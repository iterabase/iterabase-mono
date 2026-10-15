package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/provisioner"
)

func driftingHostInotify() *provisioner.HostInotifyState {
	return &provisioner.HostInotifyState{Effective: 128, Persisted: 0, DropInPresent: false}
}

func completeDataStorage() *provisioner.DataStorageState {
	return &provisioner.DataStorageState{State: "complete", VGName: provisioner.DataVolumeGroupName, VGUUID: "vg-uuid"}
}

func TestPlanReportsHostInotifyWithoutMutation(t *testing.T) {
	p := &fakeProv{pf: readyPf(), hostInotify: driftingHostInotify()}
	plan, err := Plan(context.Background(), testConfig(), p, nil)
	require.NoError(t, err)
	require.NotNil(t, plan.HostInotify)
	assert.False(t, plan.HostInotify.Ready())
	assert.False(t, plan.HostInotify.DropInPresent)
	assert.Equal(t, 128, plan.HostInotify.Effective)
	assert.Equal(t, 1, p.hostInotifyInspectCalls)
	assert.Zero(t, p.hostInotifyCalls)
	assert.Empty(t, p.mutationOrder)
}

func TestPlanHostInotifyInspectionFailureFailsClosed(t *testing.T) {
	p := &fakeProv{pf: readyPf(), hostInotifyInspectErr: errors.New("sudo unavailable")}
	_, err := Plan(context.Background(), testConfig(), p, nil)
	require.ErrorContains(t, err, "sudo unavailable")
	assert.Zero(t, p.hostInotifyCalls)
	assert.Empty(t, p.mutationOrder)
}

func TestApplyReconcilesHostInotifyBeforeInstallAndStorage(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), readyAfterInstall: true, kubeconfig: []byte(minKubeconfig), hostInotify: driftingHostInotify()}
	res, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{ReadyTimeout: time.Second, ReadyInterval: time.Millisecond})
	require.NoError(t, err)
	assert.Equal(t, 1, p.hostInotifyCalls)
	assert.Equal(t, []string{"host-inotify", "host-swap", "data-storage-tools", "data-storage-reconcile", "k3s-install"}, p.mutationOrder)
	require.NotNil(t, res.HostInotify)
}

func TestApplyInspectsButDoesNotReconcileHostInotifyDuringDryRun(t *testing.T) {
	p := &fakeProv{pf: readyPf(), hostInotify: driftingHostInotify()}
	res, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, 1, p.hostInotifyInspectCalls)
	assert.Zero(t, p.hostInotifyCalls)
	assert.Empty(t, p.mutationOrder)
	require.NotNil(t, res.Plan.HostInotify)
	assert.False(t, res.Plan.HostInotify.Ready())
	assert.Zero(t, p.installs)
}

func TestApplyHealsHostInotifyOnInstalledReapply(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), state: inSyncState(), ready: true, kubeconfig: []byte(minKubeconfig), hostInotify: driftingHostInotify()}
	p.pf.Installed = true
	res, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{ReadyTimeout: time.Second, ReadyInterval: time.Millisecond})
	require.NoError(t, err)
	assert.Equal(t, 1, p.hostInotifyCalls)
	assert.Zero(t, p.hostSwapCalls, "install-only swap hardening must not run on an installed reapply")
	assert.Empty(t, p.installs)
	require.NotNil(t, res.HostInotify)
}

func TestApplyHostInotifyFailureStopsBeforeDownstreamMutation(t *testing.T) {
	p := &fakeProv{pf: readyPf(), hostInotifyErr: errors.New("read-back effective value 128 is below the required 8192")}
	_, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{})
	require.ErrorContains(t, err, "host inotify capacity: read-back effective value 128 is below the required 8192")
	assert.Contains(t, err.Error(), "observed before reconcile: effective=", "failure evidence must report observed versus required capacity")
	assert.Equal(t, 1, p.hostInotifyCalls)
	assert.Zero(t, p.hostSwapCalls)
	assert.Zero(t, p.workspaceToolsCalls)
	assert.Zero(t, p.workspaceApplyCalls)
	assert.Empty(t, p.installs)
	assert.Equal(t, []string{"host-inotify"}, p.mutationOrder)
}

func TestUpgradeReconcilesHostInotifyBeforeK3sUpgrade(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), state: inSyncState(), ready: true, kubeconfig: []byte(minKubeconfig), workspaceInspectState: completeDataStorage(), hostInotify: driftingHostInotify()}
	p.pf.Installed = true
	res, err := Upgrade(context.Background(), testConfig(), p, nil, "v1.32.0+k3s1", ApplyOpts{ReadyTimeout: time.Second, ReadyInterval: time.Millisecond})
	require.NoError(t, err)
	assert.Equal(t, 1, p.hostInotifyCalls)
	assert.Equal(t, []string{"host-inotify", "k3s-install"}, p.mutationOrder)
	require.NotNil(t, res.HostInotify)
}

func TestUpgradeHostInotifyFailureStopsBeforeK3sUpgrade(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), state: inSyncState(), ready: true, kubeconfig: []byte(minKubeconfig), workspaceInspectState: completeDataStorage(), hostInotifyErr: errors.New("cannot atomically replace the inotify drop-in")}
	p.pf.Installed = true
	_, err := Upgrade(context.Background(), testConfig(), p, nil, "", ApplyOpts{ReadyTimeout: time.Second, ReadyInterval: time.Millisecond})
	require.ErrorContains(t, err, "host inotify capacity: cannot atomically replace the inotify drop-in")
	assert.Contains(t, err.Error(), "observed before reconcile: effective=", "failure evidence must report observed versus required capacity")
	assert.Equal(t, []string{"host-inotify"}, p.mutationOrder)
	assert.Empty(t, p.installs)
}

func TestUpgradeHostInotifyInspectionFailureStopsBeforeK3sUpgrade(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), state: inSyncState(), ready: true, kubeconfig: []byte(minKubeconfig), workspaceInspectState: completeDataStorage(), hostInotifyInspectErr: errors.New("effective inotify state is not readable")}
	p.pf.Installed = true
	_, err := Upgrade(context.Background(), testConfig(), p, nil, "", ApplyOpts{ReadyTimeout: time.Second, ReadyInterval: time.Millisecond})
	require.ErrorContains(t, err, "effective inotify state is not readable")
	assert.Zero(t, p.hostInotifyCalls, "a failed observation must not attempt reconciliation")
	assert.Empty(t, p.mutationOrder)
	assert.Empty(t, p.installs)
}
