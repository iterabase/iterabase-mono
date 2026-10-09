package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/deployer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/fluxer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/overlayer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/provisioner"
)

const (
	minKubeconfig   = "apiVersion: v1\nclusters:\n- name: default\n  cluster:\n    server: https://127.0.0.1:6443\n"
	canonicalDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func testConfig() *config.Cluster {
	return &config.Cluster{
		APIVersion: config.APIVersion,
		Kind:       config.Kind,
		Metadata:   config.Metadata{Name: "opo1"},
		Spec: config.Spec{
			Mode:        config.ModeSingleNode,
			DataStorage: config.DataStorage{Devices: []string{"/dev/disk/by-id/scsi-data-a", "/dev/disk/by-id/scsi-data-b"}},
			Hosts: []config.Host{{
				Address: "10.20.0.10", SSHUser: "forge", SSHKeyPath: "/dev/null",
				Role: config.RoleControlPlaneWorker,
			}},
			K3s: config.K3s{
				Version:       "v1.31.5",
				ClusterCIDR:   "10.42.0.0/16",
				ServiceCIDR:   "10.43.0.0/16",
				DualStack:     true,
				ClusterCIDRv6: "fd42::/48",
				ServiceCIDRv6: "fd43::/112",
				Disable:       []string{"traefik", "servicelb"},
			},
		},
	}
}

type installCall struct {
	version string
	args    []string
}

// fakeProv is a controllable provisioner.Provisioner for lifecycle tests.
type fakeProv struct {
	pf                      provisioner.PreflightResult
	state                   provisioner.HostState
	ready                   bool
	readyAfterInstall       bool
	kubeconfig              []byte
	installErr              error
	installs                []installCall
	ensureDepsErr           error
	ensureDepsCalls         int
	hostSwapErr             error
	hostSwapCalls           int
	hostInotify             *provisioner.HostInotifyState
	hostInotifyErr          error
	hostInotifyCalls        int
	hostInotifyInspectErr   error
	hostInotifyInspectCalls int
	mutationOrder           []string
	workspaceInspectErr     error
	workspaceInspectState   *provisioner.DataStorageState
	workspaceReconcileErr   error
	workspaceInspectCalls   int
	workspaceToolsErr       error
	workspaceToolsCalls     int
	workspaceApplyCalls     int
	lvmReadinessErr         error
	lvmReadinessCalls       int
	gpuReady                bool
	gpuTerminal             bool
	gpuReadinessReason      string
	gpuDriverRequests       []string
	destroyCalls            []string
	purgeErr                error
	rebootErr               error
}

func (f *fakeProv) Preflight(_ context.Context) (*provisioner.PreflightResult, error) {
	return &f.pf, nil
}
func (f *fakeProv) Install(_ context.Context, version string, args []string) error {
	f.mutationOrder = append(f.mutationOrder, "k3s-install")
	f.installs = append(f.installs, installCall{version, args})
	if f.installErr != nil {
		return f.installErr
	}
	f.state.Installed = true
	if f.readyAfterInstall {
		f.ready = true
	}
	return nil
}
func (f *fakeProv) Upgrade(ctx context.Context, v string, a []string) error {
	return f.Install(ctx, v, a)
}
func (f *fakeProv) Uninstall(_ context.Context) error {
	f.destroyCalls = append(f.destroyCalls, "uninstall")
	f.state.Installed = false
	return nil
}
func (f *fakeProv) PurgeDataStorage(_ context.Context, spec provisioner.DataStorageSpec) error {
	f.destroyCalls = append(f.destroyCalls, "purge:"+spec.InstallName+":"+strings.Join(spec.Devices, ","))
	return f.purgeErr
}
func (f *fakeProv) Reboot(_ context.Context) error {
	f.destroyCalls = append(f.destroyCalls, "reboot")
	return f.rebootErr
}
func (f *fakeProv) FetchKubeconfig(_ context.Context) ([]byte, error) { return f.kubeconfig, nil }
func (f *fakeProv) ReadState(_ context.Context) (*provisioner.HostState, error) {
	s := f.state
	return &s, nil
}
func (f *fakeProv) NodeReady(_ context.Context) (bool, error) { return f.ready, nil }
func (f *fakeProv) EnsureDriverBuildDeps(_ context.Context) error {
	f.ensureDepsCalls++
	return f.ensureDepsErr
}
func (f *fakeProv) EnsureHostSwapDisabled(_ context.Context) error {
	f.hostSwapCalls++
	f.mutationOrder = append(f.mutationOrder, "host-swap")
	return f.hostSwapErr
}

// hostInotifyState returns the configured observation or a canonical ready
// host so most lifecycle tests stay focused on their own concern.
func (f *fakeProv) hostInotifyState() *provisioner.HostInotifyState {
	if f.hostInotify != nil {
		state := *f.hostInotify
		return &state
	}
	return &provisioner.HostInotifyState{
		Effective:       provisioner.InotifyMaxUserInstancesRequired,
		Persisted:       provisioner.InotifyMaxUserInstancesRequired,
		DropInPresent:   true,
		DropInRegular:   true,
		DropInOwner:     "0:0",
		DropInMode:      "644",
		DropInCanonical: true,
	}
}
func (f *fakeProv) InspectHostInotify(_ context.Context) (*provisioner.HostInotifyState, error) {
	f.hostInotifyInspectCalls++
	if f.hostInotifyInspectErr != nil {
		return nil, f.hostInotifyInspectErr
	}
	return f.hostInotifyState(), nil
}
func (f *fakeProv) ReconcileHostInotify(_ context.Context) (*provisioner.HostInotifyState, error) {
	f.hostInotifyCalls++
	f.mutationOrder = append(f.mutationOrder, "host-inotify")
	if f.hostInotifyErr != nil {
		return nil, f.hostInotifyErr
	}
	return f.hostInotifyState(), nil
}
func (f *fakeProv) ListDataStorageDevices(_ context.Context) ([]provisioner.DataStorageDevice, error) {
	return nil, nil
}
func (f *fakeProv) InspectDataStorage(_ context.Context, spec provisioner.DataStorageSpec) (*provisioner.DataStorageState, error) {
	f.workspaceInspectCalls++
	if f.workspaceInspectErr != nil {
		return nil, f.workspaceInspectErr
	}
	if f.workspaceInspectState != nil {
		state := *f.workspaceInspectState
		state.Devices = append([]provisioner.DataStorageDevice(nil), f.workspaceInspectState.Devices...)
		return &state, nil
	}
	devices := make([]provisioner.DataStorageDevice, len(spec.Devices))
	for i, device := range spec.Devices {
		devices[i] = provisioner.DataStorageDevice{Path: device, Transport: "scsi", SizeBytes: 100}
	}
	return &provisioner.DataStorageState{Devices: devices, VGName: provisioner.DataVolumeGroupName, State: "blank-candidate"}, nil
}
func (f *fakeProv) EnsureDataStorageTools(_ context.Context) error {
	f.workspaceToolsCalls++
	f.mutationOrder = append(f.mutationOrder, "data-storage-tools")
	return f.workspaceToolsErr
}
func (f *fakeProv) ReconcileDataStorage(_ context.Context, spec provisioner.DataStorageSpec) (*provisioner.DataStorageState, error) {
	f.workspaceApplyCalls++
	f.mutationOrder = append(f.mutationOrder, "data-storage-reconcile")
	if f.workspaceReconcileErr != nil {
		return nil, f.workspaceReconcileErr
	}
	devices := make([]provisioner.DataStorageDevice, len(spec.Devices))
	for i, device := range spec.Devices {
		devices[i] = provisioner.DataStorageDevice{Path: device, Transport: "scsi", SizeBytes: 100, PVUUID: fmt.Sprintf("pv-%d", i)}
	}
	return &provisioner.DataStorageState{Devices: devices, VGName: provisioner.DataVolumeGroupName, VGUUID: "vg-uuid", SizeBytes: 200, FreeBytes: 200, State: "complete"}, nil
}
func (f *fakeProv) WaitForLVMStorageReady(_ context.Context, _ string, storage *provisioner.DataStorageState) (*provisioner.LVMStorageReadiness, error) {
	f.lvmReadinessCalls++
	if f.lvmReadinessErr != nil {
		return nil, f.lvmReadinessErr
	}
	return &provisioner.LVMStorageReadiness{Ready: true, NodeName: "node", VGName: storage.VGName, VGUUID: storage.VGUUID, SizeBytes: storage.SizeBytes, FreeBytes: storage.FreeBytes, PVCount: len(storage.Devices)}, nil
}
func (f *fakeProv) ReadGPUReadiness(_ context.Context, requestedDriverVersion string) (*provisioner.GPUReadiness, error) {
	f.gpuDriverRequests = append(f.gpuDriverRequests, requestedDriverVersion)
	return &provisioner.GPUReadiness{
		Ready:                  f.gpuReady,
		Terminal:               f.gpuTerminal,
		Reason:                 f.gpuReadinessReason,
		RequestedDriverVersion: requestedDriverVersion,
	}, nil
}

func readyPf() provisioner.PreflightResult {
	return provisioner.PreflightResult{HasSudo: true, HasCurl: true, HasSystemd: true, HasIPv6: true}
}

func inSyncState() provisioner.HostState {
	return provisioner.HostState{
		Installed:            true,
		Version:              "v1.31.5+k3s1",
		ClusterCIDR:          "10.42.0.0/16,fd42::/48",
		ServiceCIDR:          "10.43.0.0/16,fd43::/112",
		DualStack:            true,
		LocalStorageDisabled: true,
	}
}

func useTempHome(t *testing.T) {
	t.Helper()
	t.Setenv("FORGE_HOME", t.TempDir())
}

func TestPlan_Install(t *testing.T) {
	p := &fakeProv{pf: readyPf()} // not installed
	plan, err := Plan(context.Background(), testConfig(), p)
	require.NoError(t, err)
	assert.Equal(t, ActionInstall, plan.Action)
	assert.False(t, plan.Preflight.Installed)
}

func TestPlan_Skip(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	plan, err := Plan(context.Background(), testConfig(), p)
	require.NoError(t, err)
	assert.Equal(t, ActionSkip, plan.Action)
	assert.Equal(t, "v1.31.5+k3s1", plan.HaveVersion)
}

func TestPlan_RefuseImmutable(t *testing.T) {
	st := inSyncState()
	st.ClusterCIDR = "10.99.0.0/16,fd42::/48"
	p := &fakeProv{pf: readyPf(), state: st}
	p.pf.Installed = true
	plan, err := Plan(context.Background(), testConfig(), p)
	require.NoError(t, err)
	assert.Equal(t, ActionRefuseImmutable, plan.Action)
	assert.Contains(t, plan.ImmutableDiff, "k3s.clusterCIDR")
}

func TestPlan_RefuseUpgrade(t *testing.T) {
	st := inSyncState()
	st.Version = "v1.30.0+k3s1"
	p := &fakeProv{pf: readyPf(), state: st}
	p.pf.Installed = true
	plan, err := Plan(context.Background(), testConfig(), p)
	require.NoError(t, err)
	assert.Equal(t, ActionRefuseUpgrade, plan.Action)
}

func TestPlan_PreflightNoSudo(t *testing.T) {
	pf := readyPf()
	pf.HasSudo = false
	p := &fakeProv{pf: pf}
	_, err := Plan(context.Background(), testConfig(), p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sudo")
}

func TestPlan_PreflightNoIPv6DualStack(t *testing.T) {
	pf := readyPf()
	pf.HasIPv6 = false
	p := &fakeProv{pf: pf}
	_, err := Plan(context.Background(), testConfig(), p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IPv6")
}

func TestApply_Install(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{
		pf:                readyPf(),
		kubeconfig:        []byte(minKubeconfig),
		readyAfterInstall: true,
	}
	res, err := Apply(context.Background(), testConfig(), p, nil, nil, nil, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Len(t, p.installs, 1)
	assert.Equal(t, "v1.31.5", p.installs[0].version)
	assert.Contains(t, p.installs[0].args, "server")
	assert.True(t, res.NodeReady)

	// kubeconfig written to artifacts with rewritten server
	kc, err := os.ReadFile(filepath.Join(os.Getenv("FORGE_HOME"), "opo1", "kubeconfig.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(kc), "https://10.20.0.10:6443")
}

func TestApply_DryRun(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig)}
	res, err := Apply(context.Background(), testConfig(), p, nil, nil, nil, ApplyOpts{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, ActionInstall, res.Plan.Action)
	assert.Empty(t, p.installs) // no install
	_, err = os.Stat(filepath.Join(os.Getenv("FORGE_HOME"), "opo1"))
	assert.True(t, os.IsNotExist(err)) // no artifacts written
}

func TestApply_RefuseImmutable(t *testing.T) {
	useTempHome(t)
	st := inSyncState()
	st.ClusterCIDR = "10.99.0.0/16,fd42::/48"
	p := &fakeProv{pf: readyPf(), state: st, kubeconfig: []byte(minKubeconfig)}
	p.pf.Installed = true
	_, err := Apply(context.Background(), testConfig(), p, nil, nil, nil, ApplyOpts{})
	require.Error(t, err)
	assert.Empty(t, p.installs)
	assert.Contains(t, err.Error(), "immutable")
}

func TestApply_NodeNotReady(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), ready: false}
	_, err := Apply(context.Background(), testConfig(), p, nil, nil, nil, ApplyOpts{
		ReadyTimeout: 100 * time.Millisecond, ReadyInterval: 20 * time.Millisecond,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready")
}

func TestApply_KubeconfigOut(t *testing.T) {
	useTempHome(t)
	out := filepath.Join(t.TempDir(), "kc.yaml")
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	res, err := Apply(context.Background(), testConfig(), p, nil, nil, nil, ApplyOpts{
		KubeconfigOut: out, ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.Equal(t, out, res.KubeconfigPath)
	kc, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Contains(t, string(kc), "https://10.20.0.10:6443")
}

func TestUpgrade(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), state: inSyncState(), kubeconfig: []byte(minKubeconfig), ready: true,
		workspaceInspectState: &provisioner.DataStorageState{State: "complete", VGName: provisioner.DataVolumeGroupName, VGUUID: "vg-uuid"}}
	p.pf.Installed = true
	res, err := Upgrade(context.Background(), testConfig(), p, &fakeDeployer{}, "v1.32.0+k3s1", ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Len(t, p.installs, 1) // Upgrade delegates to Install
	assert.Equal(t, "v1.32.0+k3s1", p.installs[0].version)
	assert.True(t, res.NodeReady)
}

func TestUpgrade_NotInstalled(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf()} // not installed
	_, err := Upgrade(context.Background(), testConfig(), p, &fakeDeployer{}, "v1.32.0", ApplyOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not installed")
}

func TestUpgradeRefusesIncompatibleOrUnknownPreLVMStateWithoutMutation(t *testing.T) {
	complete := &provisioner.DataStorageState{State: "complete", VGName: provisioner.DataVolumeGroupName, VGUUID: "vg-uuid"}
	for _, tt := range []struct {
		name      string
		mutate    func(*config.Cluster, *fakeProv, *fakeDeployer)
		wantError string
	}{
		{name: "local-storage-enabled", wantError: "k3s.disable[local-storage]", mutate: func(_ *config.Cluster, p *fakeProv, _ *fakeDeployer) {
			p.workspaceInspectState = complete
			p.state.LocalStorageDisabled = false
		}},
		{name: "storage-observation-failed", wantError: "inspect data storage", mutate: func(_ *config.Cluster, p *fakeProv, _ *fakeDeployer) {
			p.workspaceInspectErr = errors.New("receipt unavailable")
		}},
		{name: "receipt-not-complete", wantError: "clean install", mutate: func(_ *config.Cluster, _ *fakeProv, _ *fakeDeployer) {}},
		{name: "pre-lvm-platform", wantError: "predates the OpenEBS LVM", mutate: func(cfg *config.Cluster, p *fakeProv, d *fakeDeployer) {
			p.workspaceInspectState = complete
			cfg.Spec.Chart = config.Chart{Release: "opo1", Namespace: "iterabase-system", Version: "0.4.0"}
			d.statusStates["opo1"] = deployer.ChartState{Installed: true, Version: "0.3.23"}
		}},
		{name: "platform-observation-failed", wantError: "read installed platform", mutate: func(cfg *config.Cluster, p *fakeProv, d *fakeDeployer) {
			p.workspaceInspectState = complete
			cfg.Spec.Chart = config.Chart{Release: "opo1", Namespace: "iterabase-system", Version: "0.4.0"}
			d.statusErr = errors.New("helm transport unavailable")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useTempHome(t)
			cfg := testConfig()
			p := &fakeProv{pf: readyPf(), state: inSyncState()}
			d := &fakeDeployer{statusStates: map[string]deployer.ChartState{}}
			tt.mutate(cfg, p, d)
			_, err := Upgrade(context.Background(), cfg, p, d, "v1.34.10+k3s1", ApplyOpts{})
			require.ErrorContains(t, err, tt.wantError)
			assert.Empty(t, p.installs, "upgrade/install provisioner mutation must not run on refusal")
		})
	}
}

type applyCall struct {
	release, repository, version, namespace string
	checksum                                string
	values, valueFiles                      []string
	noWait                                  bool
	timeout                                 string
}
type repoCall struct{ name, url string }
type uninstallCall struct{ release, namespace string }
type ownershipTransferCall struct{ selector, release, namespace string }

// metalLBHookOwnershipCall records a transferMetalLBHookOwnership invocation (release + namespace).
type metalLBHookOwnershipCall struct{ release, namespace string }
type restartCall struct{ selector, namespace string }

// fakeDeployer is a controllable deployer.Deployer for lifecycle chart tests.
type fakeDeployer struct {
	applyCalls                      []applyCall
	repoCalls                       []repoCall
	uninstallCalls                  []uninstallCall
	applyKustomizeCalls             []string
	deleteKustomizeCalls            []string
	applyManifestCalls              []string // captured manifests (JSON) piped via stdin
	ownershipTransfers              []ownershipTransferCall
	hookOwnershipTransfers          []ownershipTransferCall
	metallbHookOwnershipTransfers   []metalLBHookOwnershipCall
	restarts                        []restartCall
	order                           []string // ordered op log for phase-ordering assertions
	statusStates                    map[string]deployer.ChartState
	statusErr                       error
	crdsOwnedByTarget               bool
	crdsMigrationComplete           bool
	applyErr                        error
	applyManifestErr                error
	uninstallErrors                 map[string]error
	ownershipTransferErr            error
	hookOwnershipTransferErr        error
	metallbHookOwnershipTransferErr error
}

func (f *fakeDeployer) Apply(_ context.Context, opts deployer.ApplyOpts) error {
	f.applyCalls = append(f.applyCalls, applyCall{
		release: opts.Release, repository: opts.Repository,
		version: opts.Version, namespace: opts.Namespace, checksum: opts.Checksum,
		values: opts.Values, valueFiles: opts.ValueFiles, noWait: opts.NoWait, timeout: opts.Timeout,
	})
	f.order = append(f.order, "apply")
	if f.applyErr != nil {
		return f.applyErr
	}
	if f.statusStates == nil {
		f.statusStates = make(map[string]deployer.ChartState)
	}
	f.statusStates[opts.Release] = deployer.ChartState{Installed: true, Status: "deployed", Version: opts.Version}
	return nil
}

func (f *fakeDeployer) ApplyKustomize(_ context.Context, dir string) error {
	f.applyKustomizeCalls = append(f.applyKustomizeCalls, dir)
	f.order = append(f.order, "kustomize")
	return nil
}

func (f *fakeDeployer) DeleteKustomize(_ context.Context, dir string) error {
	f.deleteKustomizeCalls = append(f.deleteKustomizeCalls, dir)
	return nil
}
func (f *fakeDeployer) ApplyManifest(_ context.Context, manifest string) error {
	f.applyManifestCalls = append(f.applyManifestCalls, manifest)
	f.order = append(f.order, "manifest")
	return f.applyManifestErr
}
func (f *fakeDeployer) EnsureRepo(_ context.Context, name, url string) error {
	f.repoCalls = append(f.repoCalls, repoCall{name, url})
	return nil
}
func (f *fakeDeployer) Status(_ context.Context, release, _ string) (*deployer.ChartState, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	s := f.statusStates[release]
	return &s, nil
}
func (f *fakeDeployer) CRDOwnedBy(_ context.Context, _, _, _ string) (bool, error) {
	return f.crdsOwnedByTarget, nil
}
func (f *fakeDeployer) CRDsAnnotated(_ context.Context, _, _, _ string) (bool, error) {
	return f.crdsMigrationComplete, nil
}
func (f *fakeDeployer) AnnotateCRDs(_ context.Context, _, _, _ string) error {
	f.crdsMigrationComplete = true
	f.order = append(f.order, "annotate-crds")
	return nil
}
func (f *fakeDeployer) TransferCertificateHookOwnership(_ context.Context, selector, release, namespace string) error {
	f.hookOwnershipTransfers = append(f.hookOwnershipTransfers, ownershipTransferCall{selector, release, namespace})
	f.order = append(f.order, "hook-transfer")
	return f.hookOwnershipTransferErr
}
func (f *fakeDeployer) TransferMetalLBHookOwnership(_ context.Context, release, namespace string) error {
	f.metallbHookOwnershipTransfers = append(f.metallbHookOwnershipTransfers, metalLBHookOwnershipCall{release, namespace})
	f.order = append(f.order, "metallb-hook-transfer")
	return f.metallbHookOwnershipTransferErr
}
func (f *fakeDeployer) TransferCRDOwnership(_ context.Context, selector, release, namespace string) error {
	f.ownershipTransfers = append(f.ownershipTransfers, ownershipTransferCall{selector, release, namespace})
	f.order = append(f.order, "crd-transfer")
	if f.ownershipTransferErr != nil {
		return f.ownershipTransferErr
	}
	f.crdsOwnedByTarget = true
	return nil
}
func (f *fakeDeployer) RestartDeployment(_ context.Context, selector, namespace string) error {
	f.restarts = append(f.restarts, restartCall{selector, namespace})
	f.order = append(f.order, "restart")
	return nil
}
func (f *fakeDeployer) UninstallChart(_ context.Context, release, ns string) error {
	f.uninstallCalls = append(f.uninstallCalls, uninstallCall{release, ns})
	return f.uninstallErrors[release]
}

// fakeOverlayer is a controllable overlayer.Overlayer for lifecycle overlay tests.
type fakeOverlayer struct {
	ensureGitErr    error
	cloneCommit     string
	cloneErr        error
	cloneCalls      []cloneCall
	removeCalls     []string
	readFileContent string
	readFileValues  map[string]string
	readFileErrors  map[string]error
	readFileErr     error
	readFileCalls   []readFileCall
	serveCalls      []serveCall
	stopServeCalls  int
}

type serveCall struct {
	repo, ref, publicKey string
	podCIDRs             []string
}

type cloneCall struct {
	repo, ref, dest string
	hasToken        bool
}

type readFileCall struct{ dest, relPath string }

func (f *fakeOverlayer) EnsureGit(_ context.Context) error { return f.ensureGitErr }
func (f *fakeOverlayer) ServeToFlux(_ context.Context, repo, ref, publicKey string, podCIDRs []string) (overlayer.FluxSSHSource, error) {
	f.serveCalls = append(f.serveCalls, serveCall{repo: repo, ref: ref, publicKey: publicKey, podCIDRs: podCIDRs})
	return overlayer.FluxSSHSource{URL: "ssh://iterabase-overlay@10.0.0.5/var/lib/iterabase/overlay-source/overlay.git", KnownHosts: "10.0.0.5 ssh-ed25519 AAAAhost"}, nil
}
func (f *fakeOverlayer) StopServingToFlux(_ context.Context) error {
	f.stopServeCalls++
	return nil
}
func (f *fakeOverlayer) Clone(_ context.Context, repo, ref, dest string, token []byte) (string, error) {
	f.cloneCalls = append(f.cloneCalls, cloneCall{repo, ref, dest, len(token) > 0})
	if f.cloneErr != nil {
		return "", f.cloneErr
	}
	return f.cloneCommit, nil
}
func (f *fakeOverlayer) Remove(_ context.Context, dest string) error {
	f.removeCalls = append(f.removeCalls, dest)
	return nil
}
func (f *fakeOverlayer) ReadFile(_ context.Context, dest, relPath string) (string, error) {
	f.readFileCalls = append(f.readFileCalls, readFileCall{dest, relPath})
	if err := f.readFileErrors[relPath]; err != nil {
		return "", err
	}
	if f.readFileErr != nil {
		return "", f.readFileErr
	}
	if value, ok := f.readFileValues[relPath]; ok {
		return value, nil
	}
	return f.readFileContent, nil
}

func testConfigWithChart() *config.Cluster {
	c := testConfig()
	c.Spec.Chart = config.Chart{
		Version:    "0.3.0",
		Repository: "oci://ghcr.io/nunocgoncalves/iterabase-platform",
		Release:    "opo1",
		Namespace:  "iterabase-system",
	}
	return c
}

func testConfigWithLVMChart() *config.Cluster {
	c := testConfig()
	c.Spec.Chart = config.Chart{
		Version:    "0.4.0",
		Repository: "oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform",
		Release:    "opo1",
		Namespace:  "iterabase-system",
	}
	return c
}

func TestCertificateSubstrateRepository(t *testing.T) {
	required, err := certificateSubstrateRequired("0.2.2")
	require.NoError(t, err)
	assert.False(t, required)
	required, err = certificateSubstrateRequired("v0.3.0-rc.1")
	require.NoError(t, err)
	assert.False(t, required, "SemVer prereleases sort before the 0.3.0 boundary")
	required, err = certificateSubstrateRequired("0.3.0+build.1")
	require.NoError(t, err)
	assert.True(t, required, "build metadata does not change SemVer precedence")
	_, err = certificateSubstrateRequired("0.3.0-invalid..prerelease")
	require.ErrorContains(t, err, "invalid chart version")

	repository, err := certificateSubstrateRepository("oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform")
	require.NoError(t, err)
	assert.Equal(t, "oci://ghcr.io/nunocgoncalves/iterabase-charts/cert-manager-substrate", repository)

	_, err = certificateSubstrateRepository("oci://example.invalid/custom-platform")
	require.ErrorContains(t, err, "must end in /iterabase-platform")
}

func TestApply_Chart(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	res, err := Apply(context.Background(), testConfigWithChart(), p, d, nil, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.True(t, res.CertificateSubstrateApplied)
	assert.True(t, res.ChartApplied)
	require.Len(t, d.applyCalls, 2)
	substrate, platform := d.applyCalls[0], d.applyCalls[1]
	assert.Equal(t, "0.3.0", substrate.version)
	assert.Equal(t, "opo1-cert-manager", substrate.release)
	assert.Equal(t, "oci://ghcr.io/nunocgoncalves/cert-manager-substrate", substrate.repository)
	assert.Equal(t, []string{
		"cert-manager.prometheus.servicemonitor.enabled=false",
		"global.internalTLS.platformRelease=opo1",
	}, substrate.values)
	assert.Equal(t, "0.3.0", platform.version)
	assert.Equal(t, "opo1", platform.release)
	assert.Equal(t, "iterabase-system", platform.namespace)
}

func TestApplyRefusesPreLVMPlatformBeforeHostOrDiskMutation(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState(), ready: true, kubeconfig: []byte(minKubeconfig)}
	p.pf.Installed = true
	d := &fakeDeployer{statusStates: map[string]deployer.ChartState{"opo1": {Installed: true, Status: "deployed", Version: "0.3.23"}}}
	_, err := Apply(context.Background(), testConfigWithLVMChart(), p, d, nil, nil, ApplyOpts{})
	require.ErrorContains(t, err, "supports only a clean destroy and fresh install")
	assert.Zero(t, p.workspaceToolsCalls)
	assert.Zero(t, p.workspaceApplyCalls)
	assert.Empty(t, d.applyCalls)
}

func TestApplyChartOrdersBothSubstratesBeforePlatform(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	res, err := Apply(context.Background(), testConfigWithLVMChart(), p, d, nil, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.True(t, res.CertificateSubstrateApplied)
	assert.True(t, res.LVMStorageSubstrateApplied)
	require.NotNil(t, res.LVMStorageReady)
	assert.True(t, res.LVMStorageReady.Ready)
	assert.True(t, res.ChartApplied)
	require.Len(t, d.applyCalls, 3)
	assert.Equal(t, "opo1-cert-manager", d.applyCalls[0].release)
	assert.Equal(t, "opo1-lvm-storage", d.applyCalls[1].release)
	assert.Equal(t, "oci://ghcr.io/nunocgoncalves/iterabase-charts/lvm-storage-substrate", d.applyCalls[1].repository)
	// DES-HOR-545-01: the substrate install carries the exact manager SA identity
	// so admission fails closed unless the request is the control-plane manager.
	assert.Equal(t, []string{"agentpool.authorizedManagerIdentity=system:serviceaccount:iterabase-system:opo1-control-plane-manager"}, d.applyCalls[1].values)
	assert.Equal(t, "opo1", d.applyCalls[2].release)
	assert.Equal(t, 1, p.lvmReadinessCalls)
}

func TestApply_Chart_MigratesPreSubstrateOwnershipBeforeCompanion(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{statusStates: map[string]deployer.ChartState{
		"opo1": {Installed: true, Status: "deployed", Version: "0.2.2"},
	}}
	res, err := Apply(context.Background(), testConfigWithChart(), p, d, nil, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.True(t, res.CertificateSubstrateApplied)
	assert.True(t, res.ChartApplied)
	require.Len(t, d.applyCalls, 4)
	assert.Equal(t, "opo1", d.applyCalls[0].release, "old platform owner upgrades before companion install")
	assert.Equal(t, []string{"control-plane.toolRunner.enabled=false"}, d.applyCalls[0].values)
	assert.Equal(t, "opo1-cert-manager", d.applyCalls[1].release)
	assert.Equal(t, "opo1", d.applyCalls[2].release, "final gateway config publishes without waiting on its runner")
	assert.True(t, d.applyCalls[2].noWait)
	assert.Equal(t, "opo1", d.applyCalls[3].release, "normal waited values reconcile after the gateway restarts")
	assert.Empty(t, d.applyCalls[3].values)
	require.Equal(t, []ownershipTransferCall{{
		selector: certificateHookLabelSelector("opo1"), release: "opo1", namespace: "iterabase-system",
	}}, d.hookOwnershipTransfers)
	require.Equal(t, []ownershipTransferCall{{
		selector: certificateCRDLabelSelector, release: "opo1-cert-manager", namespace: "iterabase-system",
	}}, d.ownershipTransfers)
	require.Equal(t, []restartCall{{
		selector:  "app.kubernetes.io/name=control-plane,app.kubernetes.io/instance=opo1,app.kubernetes.io/component=gateway",
		namespace: "iterabase-system",
	}}, d.restarts)
	assert.True(t, d.crdsMigrationComplete)
	require.Equal(t, []metalLBHookOwnershipCall{{
		release: "opo1", namespace: "iterabase-system",
	}}, d.metallbHookOwnershipTransfers)
	assert.Equal(t, []string{"metallb-hook-transfer", "hook-transfer", "apply", "crd-transfer", "apply", "apply", "restart", "apply", "annotate-crds"}, d.order)
}

func TestApply_Chart_ResumesAfterCRDOwnershipBeforeGatewayRestart(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{
		statusStates: map[string]deployer.ChartState{
			"opo1": {Installed: true, Status: "deployed", Version: "0.3.0"},
		},
		crdsOwnedByTarget: true,
	}
	res, err := Apply(context.Background(), testConfigWithChart(), p, d, nil, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.True(t, res.CertificateSubstrateApplied)
	assert.True(t, d.crdsMigrationComplete)
	require.Len(t, d.restarts, 1, "missing completion checkpoint must replay the staged gateway rollout")
	require.Len(t, d.applyCalls, 4)
	assert.Equal(t, []string{"control-plane.toolRunner.enabled=false"}, d.applyCalls[0].values)
	assert.True(t, d.applyCalls[2].noWait)
}

func TestApply_Chart_ResumesOwnershipTransferAfterFailure(t *testing.T) {
	useTempHome(t)
	pf := readyPf()
	pf.Installed = true
	p := &fakeProv{
		pf: pf, state: inSyncState(), kubeconfig: []byte(minKubeconfig), ready: true,
	}
	d := &fakeDeployer{
		statusStates: map[string]deployer.ChartState{
			"opo1": {Installed: true, Status: "deployed", Version: "0.2.2"},
		},
		ownershipTransferErr: errors.New("annotation interrupted"),
	}
	opts := ApplyOpts{ReadyTimeout: time.Second, ReadyInterval: 10 * time.Millisecond}

	_, err := Apply(context.Background(), testConfigWithChart(), p, d, nil, &fakeFluxer{}, opts)
	require.ErrorContains(t, err, "certificate substrate ownership migration")
	require.Len(t, d.applyCalls, 1)
	assert.Equal(t, "0.3.0", d.statusStates["opo1"].Version, "successful platform upgrade is live state on retry")
	assert.False(t, d.crdsOwnedByTarget)

	d.ownershipTransferErr = nil
	res, err := Apply(context.Background(), testConfigWithChart(), p, d, nil, &fakeFluxer{}, opts)
	require.NoError(t, err)
	assert.True(t, res.CertificateSubstrateApplied)
	assert.True(t, res.ChartApplied)
	require.Len(t, d.ownershipTransfers, 2, "retry resumes the incomplete ownership hand-off")
	assert.True(t, d.crdsOwnedByTarget)
	require.Len(t, d.applyCalls, 5)
	assert.Equal(t, []string{"control-plane.toolRunner.enabled=false"}, d.applyCalls[1].values)
	assert.Equal(t, "opo1-cert-manager", d.applyCalls[2].release)
	assert.True(t, d.applyCalls[3].noWait)
	assert.Equal(t, "opo1", d.applyCalls[4].release)
}

func TestApply_Chart_RequiresFluxArtifactBeforeSubstrate(t *testing.T) {
	useTempHome(t)
	d := &fakeDeployer{}
	_, err := Apply(context.Background(), testConfigWithChart(),
		&fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true},
		d, nil, &fakeFluxer{artifactNotReady: true}, ApplyOpts{
			ReadyTimeout: time.Second, ReadyInterval: 10 * time.Millisecond,
		})
	require.ErrorContains(t, err, "requires an exact Ready Flux artifact before Helm")
	assert.Empty(t, d.applyCalls, "unsupported fresh installs fail before certificate substrate or platform mutation")
}

func TestApply_Chart_PreSubstrateVersionRemainsCompatible(t *testing.T) {
	useTempHome(t)
	cfg := testConfigWithChart()
	cfg.Spec.Chart.Version = "0.2.2"
	d := &fakeDeployer{}
	res, err := Apply(context.Background(), cfg,
		&fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true},
		d, nil, nil, ApplyOpts{ReadyTimeout: time.Second, ReadyInterval: 10 * time.Millisecond})
	require.NoError(t, err)
	assert.False(t, res.CertificateSubstrateApplied)
	require.Len(t, d.applyCalls, 1)
	assert.Equal(t, "opo1", d.applyCalls[0].release)
}

func TestApply_SkipChart(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	_, err := Apply(context.Background(), testConfigWithChart(), p, d, nil, &fakeFluxer{}, ApplyOpts{
		SkipChart: true, ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.Empty(t, d.applyCalls)
}

func TestDestroy_Chart(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	require.NoError(t, Destroy(context.Background(), testConfigWithChart(), p, d, nil, nil))
	require.Len(t, d.uninstallCalls, 2)
	assert.Equal(t, "opo1", d.uninstallCalls[0].release)
	assert.Equal(t, "opo1-cert-manager", d.uninstallCalls[1].release)
	assert.False(t, p.state.Installed) // k3s uninstalled too
}

func TestDestroyCurrentChartRemovesPlatformThenLVMThenCertificateWithoutPurgingVG(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	require.NoError(t, Destroy(context.Background(), testConfigWithLVMChart(), p, d, nil, nil))
	require.Len(t, d.uninstallCalls, 3)
	assert.Equal(t, "opo1", d.uninstallCalls[0].release)
	assert.Equal(t, "opo1-lvm-storage", d.uninstallCalls[1].release)
	assert.Equal(t, "opo1-cert-manager", d.uninstallCalls[2].release)
	assert.Equal(t, []string{"uninstall"}, p.destroyCalls)
}

func TestDestroy_NoChart(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	require.NoError(t, Destroy(context.Background(), testConfig(), p, d, nil, nil))
	assert.Empty(t, d.uninstallCalls) // no chart configured
	assert.False(t, p.state.Installed)
	assert.Equal(t, []string{"uninstall"}, p.destroyCalls, "ordinary destroy must preserve the workspace and must not reboot")
}

func TestDestroyWithOptions_PurgeThenReboot(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	require.NoError(t, DestroyWithOptions(context.Background(), testConfig(), p, nil, nil, nil, DestroyOpts{
		PurgeDataStorage: true,
		Reboot:           true,
	}))
	assert.Equal(t, []string{
		"uninstall",
		"purge:opo1:/dev/disk/by-id/scsi-data-a,/dev/disk/by-id/scsi-data-b",
		"reboot",
	}, p.destroyCalls)
}

func TestDestroyWithOptions_StopsBeforeRebootOnPurgeFailure(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState(), purgeErr: errors.New("identity drift")}
	err := DestroyWithOptions(context.Background(), testConfig(), p, nil, nil, nil, DestroyOpts{
		PurgeDataStorage: true,
		Reboot:           true,
	})
	require.ErrorContains(t, err, "identity drift")
	assert.Equal(t, []string{
		"uninstall",
		"purge:opo1:/dev/disk/by-id/scsi-data-a,/dev/disk/by-id/scsi-data-b",
	}, p.destroyCalls)
}

const defaultGPUOperatorChecksumForTest = "59abb5852a24b3ae0ef757bfea3051f419acbf559ee5efd72f0672d28af56a68"

func testConfigWithGPU() *config.Cluster {
	c := testConfigWithChart()
	c.Spec.GPU = config.GPU{
		Enabled: true,
		Driver:  config.GPUDriver{Version: "580.126.20", SHA256: strings.Repeat("b", 64)},
	}
	c.Spec.GPU.Operator = config.GPUOperator{
		Version:    "v26.3.3",
		Repository: "https://helm.ngc.nvidia.com/nvidia",
		Chart:      "gpu-operator",
		Release:    "opo1-gpu-operator",
		Namespace:  "gpu-operator",
		SHA256:     defaultGPUOperatorChecksumForTest,
	}
	return c
}

func gpuReadyPf() provisioner.PreflightResult {
	pf := readyPf()
	pf.OS = "Ubuntu 24.04 LTS"
	pf.HasNVIDIAGPU = true
	pf.KernelHeadersInstalled = true
	return pf
}

func TestPlan_GPUEnabled(t *testing.T) {
	p := &fakeProv{pf: gpuReadyPf()} // not installed
	plan, err := Plan(context.Background(), testConfigWithGPU(), p)
	require.NoError(t, err)
	assert.True(t, plan.GPUEnabled)
	assert.Equal(t, "v26.3.3", plan.GPUOperatorVersion)
	assert.Equal(t, "580.126.20", plan.GPUDriverVersion)
	assert.Equal(t, ActionInstall, plan.Action)
}

func TestPlan_GPUEnabledNoGPU(t *testing.T) {
	pf := gpuReadyPf()
	pf.HasNVIDIAGPU = false
	p := &fakeProv{pf: pf}
	_, err := Plan(context.Background(), testConfigWithGPU(), p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no NVIDIA GPU")
}

func TestPlan_GPUEnabledNonUbuntu(t *testing.T) {
	pf := gpuReadyPf()
	pf.OS = "Debian GNU/Linux 12 (bookworm)"
	p := &fakeProv{pf: pf}
	_, err := Plan(context.Background(), testConfigWithGPU(), p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Ubuntu")
}

func TestApply_GPU(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{
		pf:                gpuReadyPf(),
		kubeconfig:        []byte(minKubeconfig),
		readyAfterInstall: true,
		gpuReady:          true,
	}
	d := &fakeDeployer{}
	res, err := Apply(context.Background(), testConfigWithGPU(), p, d, nil, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		GPUReadyTimeout: 1 * time.Second, GPUReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	// GPU operator and certificate substrate are applied before the platform.
	require.Len(t, d.applyCalls, 3)
	op, chart := d.applyCalls[0], d.applyCalls[2]
	assert.Equal(t, "opo1-gpu-operator", op.release)
	assert.Equal(t, "nvidia/gpu-operator", op.repository)
	assert.Equal(t, "v26.3.3", op.version)
	assert.Equal(t, defaultGPUOperatorChecksumForTest, op.checksum)
	assert.Equal(t, "gpu-operator", op.namespace)
	assert.Equal(t, []string{
		"cdi.enabled=true",
		"driver.enabled=true",
		"toolkit.enabled=true",
		"devicePlugin.enabled=true",
		"gfd.enabled=true",
		"operator.version=v26.3.3@sha256:6584c36f153d18cfce284f7e5bc477887ce3c1ac566dc795bd80c9af6c6488f7",
		"validator.version=v26.3.3@sha256:6584c36f153d18cfce284f7e5bc477887ce3c1ac566dc795bd80c9af6c6488f7",
		"driver.manager.version=v0.11.0@sha256:8aec215a8b159b0162b55e688065efd58ebfa848ebc999c1797221686ff1243d",
		"toolkit.version=v1.19.1@sha256:c927adbc9b7755c5cb90022fdcc5c1295f5fe5fe1f38200a2dc65e85632b029c",
		"devicePlugin.version=v0.19.3@sha256:25cc340fe6fd53c101e16fc452f503e7a92c219c64a80ed5381784b522dbbf77",
		"dcgmExporter.version=4.5.3-4.8.2-distroless@sha256:60d3b00ac80b4ae77f94dae2f943685605585ad9e92fdccda3154d009ae317cc",
		"gfd.version=v0.19.3@sha256:25cc340fe6fd53c101e16fc452f503e7a92c219c64a80ed5381784b522dbbf77",
		"migManager.version=v0.14.2@sha256:313586bfa5c07601a83f310dd700db0007d489df2ad42bb52c611802cbb7a278",
		"node-feature-discovery.image.repository=registry.k8s.io/nfd/node-feature-discovery",
		"node-feature-discovery.image.tag=v0.19.0@sha256:2fa1c99ad09bdf2c8ad97706a4ad2fd548c84d5ecd70ba32a6152c667b96c4d2",
		"node-feature-discovery.master.resyncPeriod=30s",
		"toolkit.env[0].name=CONTAINERD_CONFIG",
		"toolkit.env[0].value=/var/lib/rancher/k3s/agent/etc/containerd/config.toml",
		"toolkit.env[1].name=CONTAINERD_SOCKET",
		"toolkit.env[1].value=/run/k3s/containerd/containerd.sock",
		"toolkit.env[2].name=CONTAINERD_RUNTIME_CLASS",
		"toolkit.env[2].value=nvidia",
		"driver.upgradePolicy.gpuPodDeletion.deleteEmptyDir=true",
		"driver.upgradePolicy.drain.enable=false",
		"driver.version=580.126.20@sha256:" + strings.Repeat("b", 64),
	}, op.values)
	assert.Equal(t, "580.126.20", res.GPUDriverVersion)
	assert.Equal(t, "opo1", chart.release)
	assert.Equal(t, "0.3.0", chart.version)
	assert.Equal(t, 1, p.ensureDepsCalls) // build deps ensured once
	require.Len(t, d.repoCalls, 1)
	assert.Equal(t, "nvidia", d.repoCalls[0].name)
	assert.Equal(t, "https://helm.ngc.nvidia.com/nvidia", d.repoCalls[0].url)
	assert.True(t, res.GPUOperatorApplied)
	assert.True(t, res.GPUReady)
	assert.True(t, res.ChartApplied)
}

func TestApply_GPU_PinnedDriverEmitsSet(t *testing.T) {
	cfg := testConfigWithGPU()
	cfg.Spec.GPU.Driver = config.GPUDriver{Version: "570.186", SHA256: strings.Repeat("a", 64)}

	p := &fakeProv{pf: gpuReadyPf()}
	plan, err := Plan(context.Background(), cfg, p)
	require.NoError(t, err)
	assert.Equal(t, "570.186", plan.GPUDriverVersion)

	useTempHome(t)
	p = &fakeProv{
		pf:                gpuReadyPf(),
		kubeconfig:        []byte(minKubeconfig),
		readyAfterInstall: true,
		gpuReady:          true,
	}
	d := &fakeDeployer{}
	res, err := Apply(context.Background(), cfg, p, d, nil, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		GPUReadyTimeout: 1 * time.Second, GPUReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Len(t, d.applyCalls, 3)
	op := d.applyCalls[0]
	assert.Contains(t, op.values, "driver.version=570.186@sha256:"+strings.Repeat("a", 64))
	// ordering / other values unchanged — driver.version appended after the base set.
	assert.Equal(t, []string{
		"cdi.enabled=true",
		"driver.enabled=true",
		"toolkit.enabled=true",
		"devicePlugin.enabled=true",
		"gfd.enabled=true",
		"operator.version=v26.3.3@sha256:6584c36f153d18cfce284f7e5bc477887ce3c1ac566dc795bd80c9af6c6488f7",
		"validator.version=v26.3.3@sha256:6584c36f153d18cfce284f7e5bc477887ce3c1ac566dc795bd80c9af6c6488f7",
		"driver.manager.version=v0.11.0@sha256:8aec215a8b159b0162b55e688065efd58ebfa848ebc999c1797221686ff1243d",
		"toolkit.version=v1.19.1@sha256:c927adbc9b7755c5cb90022fdcc5c1295f5fe5fe1f38200a2dc65e85632b029c",
		"devicePlugin.version=v0.19.3@sha256:25cc340fe6fd53c101e16fc452f503e7a92c219c64a80ed5381784b522dbbf77",
		"dcgmExporter.version=4.5.3-4.8.2-distroless@sha256:60d3b00ac80b4ae77f94dae2f943685605585ad9e92fdccda3154d009ae317cc",
		"gfd.version=v0.19.3@sha256:25cc340fe6fd53c101e16fc452f503e7a92c219c64a80ed5381784b522dbbf77",
		"migManager.version=v0.14.2@sha256:313586bfa5c07601a83f310dd700db0007d489df2ad42bb52c611802cbb7a278",
		"node-feature-discovery.image.repository=registry.k8s.io/nfd/node-feature-discovery",
		"node-feature-discovery.image.tag=v0.19.0@sha256:2fa1c99ad09bdf2c8ad97706a4ad2fd548c84d5ecd70ba32a6152c667b96c4d2",
		"node-feature-discovery.master.resyncPeriod=30s",
		"toolkit.env[0].name=CONTAINERD_CONFIG",
		"toolkit.env[0].value=/var/lib/rancher/k3s/agent/etc/containerd/config.toml",
		"toolkit.env[1].name=CONTAINERD_SOCKET",
		"toolkit.env[1].value=/run/k3s/containerd/containerd.sock",
		"toolkit.env[2].name=CONTAINERD_RUNTIME_CLASS",
		"toolkit.env[2].value=nvidia",
		"driver.upgradePolicy.gpuPodDeletion.deleteEmptyDir=true",
		"driver.upgradePolicy.drain.enable=false",
		"driver.version=570.186@sha256:" + strings.Repeat("a", 64),
	}, op.values)
	assert.Equal(t, "570.186", res.GPUDriverVersion)
	require.NotEmpty(t, p.gpuDriverRequests)
	assert.Equal(t, "570.186", p.gpuDriverRequests[0])
}

// TestApply_GPU_DriverUpgradePolicy pins the HOR-411 driver-upgrade values that
// keep a single-node inference cluster schedulable across a driver bump:
// emptyDir deletion is allowed so the upgrade can pass pod-deletion-required,
// and full-node drain stays disabled so the control plane is not evicted.
func TestApply_GPU_DriverUpgradePolicy(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{
		pf:                gpuReadyPf(),
		kubeconfig:        []byte(minKubeconfig),
		readyAfterInstall: true,
		gpuReady:          true,
	}
	d := &fakeDeployer{}
	_, err := Apply(context.Background(), testConfigWithGPU(), p, d, nil, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		GPUReadyTimeout: 1 * time.Second, GPUReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Len(t, d.applyCalls, 3)
	op := d.applyCalls[0]
	assert.Contains(t, op.values,
		"driver.upgradePolicy.gpuPodDeletion.deleteEmptyDir=true")
	assert.Contains(t, op.values,
		"driver.upgradePolicy.drain.enable=false")
	// Full-node drain must never be enabled by forge.
	for _, v := range op.values {
		assert.NotContains(t, v, "drain.enable=true")
	}
}

func TestApply_SkipGPU(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{
		pf:                gpuReadyPf(),
		kubeconfig:        []byte(minKubeconfig),
		readyAfterInstall: true,
		gpuReady:          true,
	}
	d := &fakeDeployer{}
	res, err := Apply(context.Background(), testConfigWithGPU(), p, d, nil, &fakeFluxer{}, ApplyOpts{
		SkipGPU: true, ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.Empty(t, d.repoCalls)          // no GPU operator repo
	assert.Equal(t, 0, p.ensureDepsCalls) // no build deps
	require.Len(t, d.applyCalls, 2)       // certificate substrate + platform
	assert.Equal(t, "opo1", d.applyCalls[1].release)
	// Skipped phase does not claim the operator ran.
	assert.False(t, res.GPUOperatorApplied)
	assert.False(t, res.GPUReady)
	// Skipping reconciliation still reports the configured immutable driver identity.
	assert.Equal(t, "580.126.20", res.GPUDriverVersion)
}

func TestApply_SkipGPU_SurfacesConfiguredPin(t *testing.T) {
	// HOR-401: with --skip-gpu and a configured pin, the apply report must
	// surface the configured driver version rather than claiming chart-default
	// semantics — the pin is what the config requests even though no GPU
	// reconciliation ran.
	useTempHome(t)
	cfg := testConfigWithGPU()
	cfg.Spec.GPU.Driver = config.GPUDriver{Version: "570.186", SHA256: strings.Repeat("a", 64)}
	p := &fakeProv{
		pf:                gpuReadyPf(),
		kubeconfig:        []byte(minKubeconfig),
		readyAfterInstall: true,
		gpuReady:          true,
	}
	d := &fakeDeployer{}
	res, err := Apply(context.Background(), cfg, p, d, nil, &fakeFluxer{}, ApplyOpts{
		SkipGPU: true, ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Len(t, d.applyCalls, 2) // certificate substrate + platform; GPU skipped
	assert.False(t, res.GPUOperatorApplied)
	assert.Equal(t, "570.186", res.GPUDriverVersion)
}

func TestApply_GPU_NotReady(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{
		pf:                 gpuReadyPf(),
		kubeconfig:         []byte(minKubeconfig),
		readyAfterInstall:  true,
		gpuReady:           false,
		gpuReadinessReason: "ClusterPolicy Ready condition is not True",
	}
	d := &fakeDeployer{}
	_, err := Apply(context.Background(), testConfigWithGPU(), p, d, nil, nil, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		GPUReadyTimeout: 100 * time.Millisecond, GPUReadyInterval: 20 * time.Millisecond,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gpu not ready")
	assert.Contains(t, err.Error(), "Ready condition is not True")
}

func TestApply_GPU_TerminalUpgradeFailureStopsImmediately(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{
		pf:                 gpuReadyPf(),
		kubeconfig:         []byte(minKubeconfig),
		readyAfterInstall:  true,
		gpuTerminal:        true,
		gpuReadinessReason: "GPU driver upgrade entered upgrade-failed",
	}
	d := &fakeDeployer{}
	started := time.Now()
	_, err := Apply(context.Background(), testConfigWithGPU(), p, d, nil, nil, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		GPUReadyTimeout: time.Minute, GPUReadyInterval: time.Second,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "terminal state")
	assert.Contains(t, err.Error(), "upgrade-failed")
	assert.Less(t, time.Since(started), time.Second)
}

func TestDestroy_GPU(t *testing.T) {
	p := &fakeProv{pf: gpuReadyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	require.NoError(t, Destroy(context.Background(), testConfigWithGPU(), p, d, nil, nil))
	require.Len(t, d.uninstallCalls, 3)
	assert.Equal(t, "opo1", d.uninstallCalls[0].release)
	assert.Equal(t, "opo1-cert-manager", d.uninstallCalls[1].release)
	assert.Equal(t, "opo1-gpu-operator", d.uninstallCalls[2].release)
	assert.False(t, p.state.Installed)
}

func TestApply_Overlay(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef"}
	cfg := testConfigWithChart()
	cfg.Spec.Overlay = config.Overlay{Repo: "https://github.com/example/iterabase-overlay.git", Ref: "master"}

	res, err := Apply(context.Background(), cfg, p, d, o, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.True(t, res.OverlayApplied)
	assert.Equal(t, "deadbeef", res.OverlayCommit)

	// overlay cloned with the configured repo/ref + dest; no token (public).
	require.Len(t, o.cloneCalls, 1)
	assert.Equal(t, "https://github.com/example/iterabase-overlay.git", o.cloneCalls[0].repo)
	assert.Equal(t, "master", o.cloneCalls[0].ref)
	assert.Equal(t, "/var/lib/forge/overlay/opo1", o.cloneCalls[0].dest)
	assert.False(t, o.cloneCalls[0].hasToken)

	// Substrate and platform receive the same overlay value files.
	require.Len(t, d.applyCalls, 2)
	valueFiles := []string{"/var/lib/forge/overlay/opo1/values.yaml", "/var/lib/forge/overlay/opo1/values.client.yaml"}
	assert.Equal(t, valueFiles, d.applyCalls[0].valueFiles)
	assert.Equal(t, valueFiles, d.applyCalls[1].valueFiles)

	// CRD instances applied via kustomize AFTER the chart (ordering: clone -> chart -> crds).
	require.Len(t, d.applyKustomizeCalls, 1)
	assert.Equal(t, "/var/lib/forge/overlay/opo1/crds/client", d.applyKustomizeCalls[0])
}

func TestApply_Overlay_TokenPassthrough(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "abc"}
	cfg := testConfigWithChart()
	cfg.Spec.Overlay = config.Overlay{Repo: "https://github.com/example/iterabase-overlay.git", Ref: "master"}

	fx := &fakeFluxer{artifact: fluxer.GitRepositoryArtifact{
		Ready: true, Revision: "main@sha1:abc", Digest: canonicalDigest,
	}}
	_, err := Apply(context.Background(), cfg, p, d, o, fx, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		OverlayToken: []byte("ghp_secret"),
	})
	require.NoError(t, err)
	require.Len(t, o.cloneCalls, 1)
	assert.True(t, o.cloneCalls[0].hasToken, "token passed through to Clone")
}

func TestApply_Overlay_SkippedWhenNoRepo(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{}
	cfg := testConfigWithChart() // no overlay

	res, err := Apply(context.Background(), cfg, p, d, o, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.False(t, res.OverlayApplied)
	assert.Empty(t, o.cloneCalls, "no clone when overlay.repo is empty")
	assert.Empty(t, d.applyKustomizeCalls, "no kustomize apply when no overlay")
	require.Len(t, d.applyCalls, 2)
	assert.Empty(t, d.applyCalls[0].valueFiles)
	assert.Empty(t, d.applyCalls[1].valueFiles, "platform applied with no value files when no overlay")
}

func TestApply_Overlay_SkipFlag(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{}
	cfg := testConfigWithChart()
	cfg.Spec.Overlay = config.Overlay{Repo: "https://github.com/example/iterabase-overlay.git", Ref: "master"}

	res, err := Apply(context.Background(), cfg, p, d, o, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		SkipOverlay: true,
	})
	require.NoError(t, err)
	assert.False(t, res.OverlayApplied)
	assert.Empty(t, o.cloneCalls, "SkipOverlay skips the clone")
}

func TestDestroy_Overlay(t *testing.T) {
	p := &fakeProv{}
	d := &fakeDeployer{}
	o := &fakeOverlayer{}
	cfg := testConfigWithChart()
	cfg.Spec.Overlay = config.Overlay{Repo: "https://github.com/example/iterabase-overlay.git", Ref: "master"}

	require.NoError(t, Destroy(context.Background(), cfg, p, d, o, nil))
	require.Len(t, o.removeCalls, 1)
	assert.Equal(t, "/var/lib/forge/overlay/opo1", o.removeCalls[0])
}

type fakeSecretResolver struct {
	value string
	unset bool
	calls []resolveCall
}

type resolveCall struct{ name, envVar string }

func (f *fakeSecretResolver) Resolve(_ context.Context, name, envVar string) (string, error) {
	f.calls = append(f.calls, resolveCall{name, envVar})
	if f.unset {
		return "", fmt.Errorf("secret %q: env var %q is unset (set it in the operator's gitignored .env)", name, envVar)
	}
	return f.value, nil
}

func testConfigWithOverlay() *config.Cluster {
	c := testConfigWithChart()
	c.Spec.Overlay = config.Overlay{Repo: "https://github.com/example/iterabase-overlay.git", Ref: "master"}
	return c
}

// overlaySecretsYAML is a minimal overlay secrets.yaml for the secret-sync tests.
func overlaySecretsYAML() string {
	return `secrets:
  - name: cloudflare-api-token
    namespace: iterabase-system
    key: api-token
    envVar: FORGE_CLOUDFLARE_API_TOKEN
`
}

func TestApply_Secrets(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef", readFileContent: overlaySecretsYAML()}
	r := &fakeSecretResolver{value: "supersecret-token"}
	res, err := Apply(context.Background(), testConfigWithOverlay(), p, d, o, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond, SecretResolver: r,
	})
	require.NoError(t, err)
	assert.True(t, res.SecretsApplied)

	// the resolver was called for the declared secret (env-or-prompt happens there).
	require.Len(t, r.calls, 1)
	assert.Equal(t, "cloudflare-api-token", r.calls[0].name)
	assert.Equal(t, "FORGE_CLOUDFLARE_API_TOKEN", r.calls[0].envVar)

	// namespace pre-create, then the secret manifest (stringData), via stdin.
	require.Len(t, d.applyManifestCalls, 2)
	assert.Contains(t, d.applyManifestCalls[0], `"Namespace"`)
	assert.Contains(t, d.applyManifestCalls[0], `"iterabase-system"`)
	assert.Contains(t, d.applyManifestCalls[1], `"Secret"`)
	assert.Contains(t, d.applyManifestCalls[1], `"cloudflare-api-token"`)
	assert.Contains(t, d.applyManifestCalls[1], `"api-token"`)
	assert.Contains(t, d.applyManifestCalls[1], "supersecret-token", "value piped via stdin manifest")

	// the value never reaches helm values (it stays out of the release secret).
	for _, c := range d.applyCalls {
		for _, v := range c.values {
			assert.NotContains(t, v, "supersecret-token")
		}
	}

	// Only the non-secret declaration is read; storage is fixed substrate config.
	require.Len(t, o.readFileCalls, 1)
	assert.Equal(t, "secrets.yaml", o.readFileCalls[0].relPath)

	// secrets are applied AFTER the overlay clone + BEFORE the chart so
	// cert-manager finds them on first reconcile.
	manifestIdx, applyIdx := -1, -1
	for i, op := range d.order {
		if op == "manifest" && manifestIdx == -1 {
			manifestIdx = i
		}
		if op == "apply" && applyIdx == -1 {
			applyIdx = i
		}
	}
	assert.GreaterOrEqual(t, manifestIdx, 0)
	assert.GreaterOrEqual(t, applyIdx, 0)
	assert.Less(t, manifestIdx, applyIdx, "secrets applied before chart")
}

func TestApply_Secrets_UnsetEnv(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef", readFileContent: overlaySecretsYAML()}
	r := &fakeSecretResolver{unset: true} // resolver can't provide a value (env unset + non-interactive)
	_, err := Apply(context.Background(), testConfigWithOverlay(), p, d, o, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond, SecretResolver: r,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FORGE_CLOUDFLARE_API_TOKEN")
	assert.Contains(t, err.Error(), "unset")
	assert.Empty(t, d.applyCalls, "chart not applied when a secret value is missing")
	assert.Len(t, d.applyManifestCalls, 1, "namespace pre-created before the resolve fails")
}

func TestApply_SkipSecrets(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef", readFileContent: overlaySecretsYAML()}
	res, err := Apply(context.Background(), testConfigWithOverlay(), p, d, o, &fakeFluxer{}, ApplyOpts{
		SkipSecrets: true, ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.False(t, res.SecretsApplied)
	assert.Empty(t, d.applyManifestCalls, "SkipSecrets skips materialization")
	assert.Empty(t, o.readFileCalls, "SkipSecrets does not read secret declarations; storage is not overlay-selectable")
}

func TestApply_Secrets_NoSecretsFile(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	// Overlay cloned but has no secrets.yaml.
	o := &fakeOverlayer{cloneCommit: "deadbeef", readFileErrors: map[string]error{
		"secrets.yaml": errors.New("overlay read secrets.yaml: No such file or directory"),
	}}
	res, err := Apply(context.Background(), testConfigWithOverlay(), p, d, o, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.False(t, res.SecretsApplied)
	assert.Empty(t, d.applyManifestCalls, "no secrets.yaml ⇒ no materialization")
}

func TestApply_Secrets_NoOverlay(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{} // no overlay.repo ⇒ no clone
	res, err := Apply(context.Background(), testConfigWithChart(), p, d, o, &fakeFluxer{}, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.False(t, res.SecretsApplied)
	assert.Empty(t, d.applyManifestCalls, "no overlay ⇒ no secrets")
}

// fakeFluxer is a controllable fluxer.Fluxer for lifecycle flux tests.
type fakeFluxer struct {
	ensureVersion    string
	ensureErr        error
	ensureCalls      int
	uninstallCalls   int
	artifact         fluxer.GitRepositoryArtifact
	artifactErr      error
	artifactNotReady bool
}

func (f *fakeFluxer) EnsureFlux(_ context.Context, version string) error {
	f.ensureCalls++
	f.ensureVersion = version
	return f.ensureErr
}
func (f *fakeFluxer) UninstallFlux(_ context.Context) error {
	f.uninstallCalls++
	return nil
}
func (f *fakeFluxer) GitRepositoryArtifact(_ context.Context, _ string) (fluxer.GitRepositoryArtifact, error) {
	if f.artifactErr != nil {
		return fluxer.GitRepositoryArtifact{}, f.artifactErr
	}
	if f.artifactNotReady {
		return fluxer.GitRepositoryArtifact{}, nil
	}
	if f.artifact.Revision == "" {
		return fluxer.GitRepositoryArtifact{Ready: true, Revision: "main@sha1:deadbeef", Digest: canonicalDigest}, nil
	}
	return f.artifact, nil
}

// testConfigWithFlux extends the overlay config (chart + https overlay) with
// Flux enabled at a pinned version.
func testConfigWithFlux() *config.Cluster {
	c := testConfigWithOverlay()
	c.Spec.Flux = config.Flux{Enabled: true, Version: "v2.4.0"}
	return c
}

func TestApply_Flux(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef"}
	fx := &fakeFluxer{}
	cfg := testConfigWithFlux()

	res, err := Apply(context.Background(), cfg, p, d, o, fx, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		OverlayToken: []byte("ghp_secret"),
	})
	require.NoError(t, err)
	assert.True(t, res.FluxInstalled)
	assert.Equal(t, "ready=True revision=main@sha1:deadbeef digest="+canonicalDigest, res.GitRepositoryStatus)

	// EnsureFlux called with the configured version.
	require.Equal(t, 1, fx.ensureCalls)
	assert.Equal(t, "v2.4.0", fx.ensureVersion)

	// Four Flux resources are applied in lifecycle order: source credentials →
	// GitRepository → artifact policy before Helm, then Kustomization afterward.
	require.Len(t, d.applyManifestCalls, 4)
	sec, repo, policy, kust := d.applyManifestCalls[0], d.applyManifestCalls[1], d.applyManifestCalls[2], d.applyManifestCalls[3]
	assert.Contains(t, sec, `"Secret"`)
	assert.Contains(t, sec, `"overlay-git-auth"`)
	assert.Contains(t, sec, "ghp_secret", "token piped via stdin manifest (stringData)")
	assert.Contains(t, repo, `"GitRepository"`)
	assert.Contains(t, repo, `"overlay"`)
	assert.Contains(t, repo, `"secretRef"`)
	assert.Contains(t, repo, `"overlay-git-auth"`)
	assert.Contains(t, kust, `"Kustomization"`)
	assert.Contains(t, kust, `"overlay-crds"`)
	assert.Contains(t, kust, `"./crds/client"`)
	assert.Contains(t, kust, `"prune":true`)
	assert.Contains(t, policy, `"NetworkPolicy"`)
	assert.Contains(t, policy, `"app":"source-controller"`)
	assert.Contains(t, policy, `"tool-runner"`)
	assert.Contains(t, policy, `"iterabase-system"`)

	// The token never reaches helm values (it stays in the stdin Secret).
	for _, c := range d.applyCalls {
		for _, v := range c.values {
			assert.NotContains(t, v, "ghp_secret")
		}
	}

	// Source manifests precede Helm so the runner can load a generation during
	// --wait; continuous reconciliation starts only after the one-time CR apply.
	require.Equal(t, []string{"metallb-hook-transfer", "apply", "manifest", "manifest", "manifest", "apply", "annotate-crds", "kustomize", "manifest"}, d.order)
}

func TestApply_Flux_PublicRepoNoToken(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef"}
	fx := &fakeFluxer{}
	cfg := testConfigWithFlux()

	res, err := Apply(context.Background(), cfg, p, d, o, fx, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		// no OverlayToken => public repo => Flux clones anonymously.
	})
	require.NoError(t, err)
	assert.True(t, res.FluxInstalled)

	// No token Secret (GitRepository + artifact ingress policy before Helm,
	// Kustomization after); GitRepository has no secretRef.
	require.Len(t, d.applyManifestCalls, 3)
	repo, policy, kust := d.applyManifestCalls[0], d.applyManifestCalls[1], d.applyManifestCalls[2]
	assert.Contains(t, repo, `"GitRepository"`)
	assert.NotContains(t, repo, `"secretRef"`, "public repo => no secretRef")
	assert.NotContains(t, repo, "ghp_secret")
	assert.Contains(t, kust, `"Kustomization"`)
	assert.Contains(t, policy, `"NetworkPolicy"`)
}

func TestApply_Flux_FileOverlayServedOverNodeSSH(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef"}
	fx := &fakeFluxer{}
	cfg := testConfigWithFlux()
	cfg.Spec.Overlay.Repo = "file:///srv/overlay"
	cfg.Spec.K3s.DualStack = true
	cfg.Spec.K3s.ClusterCIDRv6 = "fd00:42::/56"

	res, err := Apply(context.Background(), cfg, p, d, o, fx, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		OverlayToken: []byte("ghp_unused"),
	})
	require.NoError(t, err)
	assert.True(t, res.FluxInstalled)

	// DES-HOR-632-01: the host serves the configured ref to the pod CIDRs with a
	// fresh Forge key, never the https token.
	require.Len(t, o.serveCalls, 1)
	serve := o.serveCalls[0]
	assert.Equal(t, "file:///srv/overlay", serve.repo)
	assert.Equal(t, cfg.Spec.Overlay.Ref, serve.ref)
	assert.Equal(t, []string{cfg.Spec.K3s.ClusterCIDR, "fd00:42::/56"}, serve.podCIDRs)
	assert.True(t, strings.HasPrefix(serve.publicKey, "ssh-ed25519 "), serve.publicKey)

	require.Len(t, d.applyManifestCalls, 4)
	sec, repo := d.applyManifestCalls[0], d.applyManifestCalls[1]
	assert.Contains(t, sec, `"overlay-git-ssh"`)
	assert.Contains(t, sec, "BEGIN OPENSSH PRIVATE KEY")
	assert.Contains(t, sec, `"known_hosts":"10.0.0.5 ssh-ed25519 AAAAhost"`)
	assert.Contains(t, sec, strings.TrimSuffix(strings.SplitN(serve.publicKey, " ", 3)[1], "="))
	assert.NotContains(t, sec, "ghp_unused")
	assert.Contains(t, repo, `"url":"ssh://iterabase-overlay@10.0.0.5/var/lib/iterabase/overlay-source/overlay.git"`)
	assert.Contains(t, repo, `"secretRef":{"name":"overlay-git-ssh"}`)

	// Every apply gets a new key: re-apply replaces the authorized key.
	_, err = Apply(context.Background(), cfg, p, d, o, fx, ApplyOpts{ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond})
	require.NoError(t, err)
	require.Len(t, o.serveCalls, 2)
	assert.NotEqual(t, o.serveCalls[0].publicKey, o.serveCalls[1].publicKey)
}

func TestApply_Flux_SkipFlag(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef"}
	fx := &fakeFluxer{}
	cfg := testConfigWithFlux()

	res, err := Apply(context.Background(), cfg, p, d, o, fx, ApplyOpts{
		SkipFlux: true, ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.False(t, res.FluxInstalled)
	assert.Equal(t, 0, fx.ensureCalls, "SkipFlux skips EnsureFlux")
	assert.Empty(t, d.applyManifestCalls, "SkipFlux skips the sync resources")
}

func TestApply_Flux_SkipFlagRejectsFreshChartInstall(t *testing.T) {
	useTempHome(t)
	d := &fakeDeployer{}
	_, err := Apply(context.Background(), testConfigWithFlux(),
		&fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true},
		d, &fakeOverlayer{cloneCommit: "deadbeef"}, &fakeFluxer{artifactNotReady: true}, ApplyOpts{
			SkipFlux: true, ReadyTimeout: time.Second, ReadyInterval: 10 * time.Millisecond,
		})
	require.ErrorContains(t, err, "requires an exact Ready Flux artifact before Helm")
	assert.Empty(t, d.applyCalls)
	assert.Empty(t, d.applyManifestCalls)
}

func TestApply_Flux_DisabledReusesEstablishedSource(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef"}
	fx := &fakeFluxer{}
	cfg := testConfigWithOverlay() // flux not enabled

	res, err := Apply(context.Background(), cfg, p, d, o, fx, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.False(t, res.FluxInstalled)
	assert.Equal(t, 0, fx.ensureCalls, "Flux disabled => phase skipped even with a fluxer wired")
	assert.Empty(t, d.applyManifestCalls)
}

func TestApply_Flux_NoFluxerWired(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef"}
	cfg := testConfigWithFlux()

	_, err := Apply(context.Background(), cfg, p, d, o, nil, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fluxer")
}

func TestApply_Flux_EnsureFluxFails(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef"}
	fx := &fakeFluxer{ensureErr: fmt.Errorf("flux install: network")}
	cfg := testConfigWithFlux()

	_, err := Apply(context.Background(), cfg, p, d, o, fx, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "flux install")
	assert.Empty(t, d.applyManifestCalls, "sync resources not applied when EnsureFlux fails")
}

func TestApply_Flux_WaitsForExactArtifactBeforeChart(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef"}
	fx := &fakeFluxer{artifact: fluxer.GitRepositoryArtifact{
		Ready: true, Revision: "main@sha1:stale", Digest: canonicalDigest,
	}}

	_, err := Apply(context.Background(), testConfigWithFlux(), p, d, o, fx, ApplyOpts{
		ReadyTimeout: 30 * time.Millisecond, ReadyInterval: 5 * time.Millisecond,
	})
	require.ErrorContains(t, err, `did not publish Ready commit "deadbeef"`)
	require.Len(t, d.applyCalls, 1)
	assert.Equal(t, "opo1-cert-manager", d.applyCalls[0].release)
	assert.NotEmpty(t, d.applyManifestCalls, "the source is established before polling")
}

func TestApply_Flux_RejectsNonCanonicalArtifactDigestBeforeChart(t *testing.T) {
	useTempHome(t)
	d := &fakeDeployer{}
	fx := &fakeFluxer{artifact: fluxer.GitRepositoryArtifact{
		Ready: true, Revision: "main@sha1:deadbeef", Digest: "sha256:garbage",
	}}

	_, err := Apply(context.Background(), testConfigWithFlux(),
		&fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true},
		d, &fakeOverlayer{cloneCommit: "deadbeef"}, fx, ApplyOpts{
			ReadyTimeout: 30 * time.Millisecond, ReadyInterval: 5 * time.Millisecond,
		})
	require.ErrorContains(t, err, "with a canonical sha256 digest")
	require.Len(t, d.applyCalls, 1)
	assert.Equal(t, "opo1-cert-manager", d.applyCalls[0].release)
}

func TestApply_Flux_ArtifactReadFailureStopsBeforeChart(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef"}
	fx := &fakeFluxer{artifactErr: errors.New("api unavailable")}

	_, err := Apply(context.Background(), testConfigWithFlux(), p, d, o, fx, ApplyOpts{
		ReadyTimeout: time.Second, ReadyInterval: 5 * time.Millisecond,
	})
	require.ErrorContains(t, err, "read Flux GitRepository artifact")
	require.Len(t, d.applyCalls, 1)
	assert.Equal(t, "opo1-cert-manager", d.applyCalls[0].release)
}

func TestDestroy_Flux(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	o := &fakeOverlayer{}
	fx := &fakeFluxer{}
	cfg := testConfigWithFlux()

	require.NoError(t, Destroy(context.Background(), cfg, p, d, o, fx))
	assert.Equal(t, 1, fx.uninstallCalls, "Flux uninstalled on destroy")
}

func TestDestroy_Flux_FileOverlayStopsServing(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	o := &fakeOverlayer{}
	cfg := testConfigWithFlux()
	cfg.Spec.Overlay.Repo = "file:///srv/overlay"

	require.NoError(t, Destroy(context.Background(), cfg, p, &fakeDeployer{}, o, &fakeFluxer{}))
	assert.Equal(t, 1, o.stopServeCalls, "the read-only user, key and mirror are removed")

	https := &fakeOverlayer{}
	require.NoError(t, Destroy(context.Background(), testConfigWithFlux(), p, &fakeDeployer{}, https, &fakeFluxer{}))
	assert.Zero(t, https.stopServeCalls, "an https overlay was never served from the node")
}

func TestDestroy_Flux_Disabled(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	o := &fakeOverlayer{}
	fx := &fakeFluxer{}
	cfg := testConfigWithOverlay() // flux not enabled

	require.NoError(t, Destroy(context.Background(), cfg, p, d, o, fx))
	assert.Equal(t, 0, fx.uninstallCalls, "Flux disabled => not uninstalled")
}

func TestDestroy_Flux_NoFluxer(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	o := &fakeOverlayer{}
	cfg := testConfigWithFlux()

	// nil fluxer + flux enabled => best-effort skip (k3s-uninstall wipes it).
	require.NoError(t, Destroy(context.Background(), cfg, p, d, o, nil))
}

func TestFluxManifest_GitRepository_RefBranchVsTag(t *testing.T) {
	// A branch ref => ref.branch.
	branch := gitRepositoryManifest("overlay", "flux-system", "https://example/o.git", "master", "")
	assert.Contains(t, branch, `"branch":"master"`)
	assert.NotContains(t, branch, `"tag"`)
	// A semver tag ref => ref.tag.
	tag := gitRepositoryManifest("overlay", "flux-system", "https://example/o.git", "v0.1.0", "")
	assert.Contains(t, tag, `"tag":"v0.1.0"`)
	assert.NotContains(t, tag, `"branch"`)
}

func TestFluxManifest_GitRepository_SecretRefOmission(t *testing.T) {
	withRef := gitRepositoryManifest("overlay", "flux-system", "https://example/o.git", "master", "overlay-git-auth")
	assert.Contains(t, withRef, `"secretRef"`)
	assert.Contains(t, withRef, `"overlay-git-auth"`)
	withoutRef := gitRepositoryManifest("overlay", "flux-system", "https://example/o.git", "master", "")
	assert.NotContains(t, withoutRef, `"secretRef"`)
}

func TestFluxManifest_TokenSecret(t *testing.T) {
	m := fluxTokenSecretManifest("overlay-git-auth", "flux-system", "git", []byte("ghp_secret"))
	assert.Contains(t, m, `"Secret"`)
	assert.Contains(t, m, `"Opaque"`)
	assert.Contains(t, m, `"username":"git"`)
	assert.Contains(t, m, `"password":"ghp_secret"`)
}
