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
	plan, err := Plan(context.Background(), testConfig(), p, nil)
	require.NoError(t, err)
	assert.Equal(t, ActionInstall, plan.Action)
	assert.False(t, plan.Preflight.Installed)
}

func TestPlan_Skip(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	plan, err := Plan(context.Background(), testConfig(), p, nil)
	require.NoError(t, err)
	assert.Equal(t, ActionSkip, plan.Action)
	assert.Equal(t, "v1.31.5+k3s1", plan.HaveVersion)
}

func TestPlan_RefuseImmutable(t *testing.T) {
	st := inSyncState()
	st.ClusterCIDR = "10.99.0.0/16,fd42::/48"
	p := &fakeProv{pf: readyPf(), state: st}
	p.pf.Installed = true
	plan, err := Plan(context.Background(), testConfig(), p, nil)
	require.NoError(t, err)
	assert.Equal(t, ActionRefuseImmutable, plan.Action)
	assert.Contains(t, plan.ImmutableDiff, "k3s.clusterCIDR")
}

func TestPlan_RefuseUpgrade(t *testing.T) {
	st := inSyncState()
	st.Version = "v1.30.0+k3s1"
	p := &fakeProv{pf: readyPf(), state: st}
	p.pf.Installed = true
	plan, err := Plan(context.Background(), testConfig(), p, nil)
	require.NoError(t, err)
	assert.Equal(t, ActionRefuseUpgrade, plan.Action)
}

func TestPlan_PreflightNoSudo(t *testing.T) {
	pf := readyPf()
	pf.HasSudo = false
	p := &fakeProv{pf: pf}
	_, err := Plan(context.Background(), testConfig(), p, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sudo")
}

func TestPlan_PreflightNoIPv6DualStack(t *testing.T) {
	pf := readyPf()
	pf.HasIPv6 = false
	p := &fakeProv{pf: pf}
	_, err := Plan(context.Background(), testConfig(), p, nil)
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
	res, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{
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
	res, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{DryRun: true})
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
	_, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{})
	require.Error(t, err)
	assert.Empty(t, p.installs)
	assert.Contains(t, err.Error(), "immutable")
}

func TestApply_NodeNotReady(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), ready: false}
	_, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{
		ReadyTimeout: 100 * time.Millisecond, ReadyInterval: 20 * time.Millisecond,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready")
}

func TestApply_KubeconfigOut(t *testing.T) {
	useTempHome(t)
	out := filepath.Join(t.TempDir(), "kc.yaml")
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	res, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{
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
	timeout                                 string
}
type repoCall struct{ name, url string }
type uninstallCall struct{ release, namespace string }

// metalLBHookOwnershipCall records a transferMetalLBHookOwnership invocation (release + namespace).
type metalLBHookOwnershipCall struct{ release, namespace string }
type applyOwnedCall struct{ manifest, ownerLabel, ownerValue string }
type pruneOwnedCall struct {
	kinds    []string
	selector string
	keep     []deployer.ObjectRef
}

// fakeDeployer is a controllable deployer.Deployer for lifecycle chart tests.
type fakeDeployer struct {
	applyCalls                      []applyCall
	repoCalls                       []repoCall
	uninstallCalls                  []uninstallCall
	applyOwnedCalls                 []applyOwnedCall
	pruneOwnedCalls                 []pruneOwnedCall
	applyManifestCalls              []string // captured manifests (JSON) piped via stdin
	metallbHookOwnershipTransfers   []metalLBHookOwnershipCall
	order                           []string // ordered op log for phase-ordering assertions
	statusStates                    map[string]deployer.ChartState
	statusErr                       error
	applyErr                        error
	applyManifestErr                error
	applyOwnedErr                   error
	pruneOwnedErr                   error
	pruneOwnedResult                []string
	uninstallErrors                 map[string]error
	metallbHookOwnershipTransferErr error
}

func (f *fakeDeployer) Apply(_ context.Context, opts deployer.ApplyOpts) error {
	f.applyCalls = append(f.applyCalls, applyCall{
		release: opts.Release, repository: opts.Repository,
		version: opts.Version, namespace: opts.Namespace, checksum: opts.Checksum,
		values: opts.Values, valueFiles: opts.ValueFiles, timeout: opts.Timeout,
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

func (f *fakeDeployer) ApplyOwned(_ context.Context, manifest, ownerLabel, ownerValue string) error {
	f.applyOwnedCalls = append(f.applyOwnedCalls, applyOwnedCall{manifest: manifest, ownerLabel: ownerLabel, ownerValue: ownerValue})
	f.order = append(f.order, "apply-owned")
	return f.applyOwnedErr
}

func (f *fakeDeployer) PruneOwned(_ context.Context, kinds []string, selector string, keep []deployer.ObjectRef) ([]string, error) {
	f.pruneOwnedCalls = append(f.pruneOwnedCalls, pruneOwnedCall{kinds: kinds, selector: selector, keep: keep})
	f.order = append(f.order, "prune-owned:"+selector)
	if f.pruneOwnedErr != nil {
		return nil, f.pruneOwnedErr
	}
	return f.pruneOwnedResult, nil
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
func (f *fakeDeployer) TransferMetalLBHookOwnership(_ context.Context, release, namespace string) error {
	f.metallbHookOwnershipTransfers = append(f.metallbHookOwnershipTransfers, metalLBHookOwnershipCall{release, namespace})
	f.order = append(f.order, "metallb-hook-transfer")
	return f.metallbHookOwnershipTransferErr
}
func (f *fakeDeployer) UninstallChart(_ context.Context, release, ns string) error {
	f.uninstallCalls = append(f.uninstallCalls, uninstallCall{release, ns})
	return f.uninstallErrors[release]
}

// fakeOverlayer is a controllable overlayer.Overlayer for lifecycle overlay tests.
type fakeOverlayer struct {
	stageCalls  []stageCall
	stageErr    error
	removeCalls []string
	order       *[]string // optional shared op log with the deployer
}

type stageCall struct {
	dir   string
	files []overlayer.File
}

func (f *fakeOverlayer) StageFiles(_ context.Context, dir string, files []overlayer.File) ([]string, error) {
	f.stageCalls = append(f.stageCalls, stageCall{dir: dir, files: files})
	if f.order != nil {
		*f.order = append(*f.order, "stage")
	}
	if f.stageErr != nil {
		return nil, f.stageErr
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, dir+"/"+file.Name)
	}
	return paths, nil
}

func (f *fakeOverlayer) Remove(_ context.Context, dir string) error {
	f.removeCalls = append(f.removeCalls, dir)
	return nil
}

// testConfigWithChart pins the first platform chart whose tool runner reads the
// Forge-written overlay tools ConfigMaps: 0.3.x/0.4.x charts expect the removed
// Flux artifact and are refused (DES-HOR-593-04), so every chart-applying test
// targets a current chart that carries both substrates.
func testConfigWithChart() *config.Cluster {
	c := testConfig()
	c.Spec.Chart = config.Chart{
		Version:    overlayToolsFirstVersion,
		Repository: "oci://ghcr.io/nunocgoncalves/iterabase-platform",
		Release:    "opo1",
		Namespace:  "iterabase-system",
	}
	return c
}

func testConfigWithLVMChart() *config.Cluster {
	c := testConfig()
	c.Spec.Chart = config.Chart{
		Version:    overlayToolsFirstVersion,
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
	res, err := Apply(context.Background(), testConfigWithChart(), p, d, nil, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.True(t, res.CertificateSubstrateApplied)
	assert.True(t, res.ChartApplied)
	require.Len(t, d.applyCalls, 3)
	substrate, platform := d.applyCalls[0], d.applyCalls[2]
	assert.Equal(t, overlayToolsFirstVersion, substrate.version)
	assert.Equal(t, "opo1-cert-manager", substrate.release)
	assert.Equal(t, "oci://ghcr.io/nunocgoncalves/cert-manager-substrate", substrate.repository)
	assert.Equal(t, []string{
		"cert-manager.prometheus.servicemonitor.enabled=false",
		"global.internalTLS.platformRelease=opo1",
	}, substrate.values)
	assert.Equal(t, "opo1-lvm-storage", d.applyCalls[1].release)
	assert.Equal(t, overlayToolsFirstVersion, platform.version)
	assert.Equal(t, "opo1", platform.release)
	assert.Equal(t, "iterabase-system", platform.namespace)
	assert.Equal(t, []string{overlayToolsValue + "=false"}, platform.values, "no overlay => the tool runner reads no overlay tools archive")
}

func TestApplyRefusesPreLVMPlatformBeforeHostOrDiskMutation(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState(), ready: true, kubeconfig: []byte(minKubeconfig)}
	p.pf.Installed = true
	d := &fakeDeployer{statusStates: map[string]deployer.ChartState{"opo1": {Installed: true, Status: "deployed", Version: "0.3.23"}}}
	_, err := Apply(context.Background(), testConfigWithLVMChart(), p, d, nil, ApplyOpts{})
	require.ErrorContains(t, err, "supports only a clean destroy and fresh install")
	assert.Zero(t, p.workspaceToolsCalls)
	assert.Zero(t, p.workspaceApplyCalls)
	assert.Empty(t, d.applyCalls)
}

func TestApplyChartOrdersBothSubstratesBeforePlatform(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	res, err := Apply(context.Background(), testConfigWithLVMChart(), p, d, nil, ApplyOpts{
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

func TestApply_Chart_PreSubstrateVersionRemainsCompatible(t *testing.T) {
	useTempHome(t)
	cfg := testConfigWithChart()
	cfg.Spec.Chart.Version = "0.2.2"
	d := &fakeDeployer{}
	res, err := Apply(context.Background(), cfg,
		&fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true},
		d, nil, ApplyOpts{ReadyTimeout: time.Second, ReadyInterval: 10 * time.Millisecond})
	require.NoError(t, err)
	assert.False(t, res.CertificateSubstrateApplied)
	require.Len(t, d.applyCalls, 1)
	assert.Equal(t, "opo1", d.applyCalls[0].release)
}

func TestApply_SkipChart(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	_, err := Apply(context.Background(), testConfigWithChart(), p, d, nil, ApplyOpts{
		SkipChart: true, ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.Empty(t, d.applyCalls)
}

func TestDestroy_Chart(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	// Destroying an installation still on a Flux-era 0.3 chart stays supported:
	// only apply refuses those charts.
	cfg := testConfigWithChart()
	cfg.Spec.Chart.Version = "0.3.0"
	require.NoError(t, Destroy(context.Background(), cfg, p, d, nil))
	require.Len(t, d.uninstallCalls, 2)
	assert.Equal(t, "opo1", d.uninstallCalls[0].release)
	assert.Equal(t, "opo1-cert-manager", d.uninstallCalls[1].release)
	assert.False(t, p.state.Installed) // k3s uninstalled too
}

func TestDestroyCurrentChartRemovesPlatformThenLVMThenCertificateWithoutPurgingVG(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	require.NoError(t, Destroy(context.Background(), testConfigWithLVMChart(), p, d, nil))
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
	require.NoError(t, Destroy(context.Background(), testConfig(), p, d, nil))
	assert.Empty(t, d.uninstallCalls) // no chart configured
	assert.False(t, p.state.Installed)
	assert.Equal(t, []string{"uninstall"}, p.destroyCalls, "ordinary destroy must preserve the workspace and must not reboot")
}

func TestDestroyWithOptions_PurgeThenReboot(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	require.NoError(t, DestroyWithOptions(context.Background(), testConfig(), p, nil, nil, DestroyOpts{
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
	err := DestroyWithOptions(context.Background(), testConfig(), p, nil, nil, DestroyOpts{
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
	plan, err := Plan(context.Background(), testConfigWithGPU(), p, nil)
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
	_, err := Plan(context.Background(), testConfigWithGPU(), p, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no NVIDIA GPU")
}

func TestPlan_GPUEnabledNonUbuntu(t *testing.T) {
	pf := gpuReadyPf()
	pf.OS = "Debian GNU/Linux 12 (bookworm)"
	p := &fakeProv{pf: pf}
	_, err := Plan(context.Background(), testConfigWithGPU(), p, nil)
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
	res, err := Apply(context.Background(), testConfigWithGPU(), p, d, nil, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		GPUReadyTimeout: 1 * time.Second, GPUReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	// GPU operator and both substrates are applied before the platform.
	require.Len(t, d.applyCalls, 4)
	op, chart := d.applyCalls[0], d.applyCalls[3]
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
	assert.Equal(t, overlayToolsFirstVersion, chart.version)
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
	plan, err := Plan(context.Background(), cfg, p, nil)
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
	res, err := Apply(context.Background(), cfg, p, d, nil, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		GPUReadyTimeout: 1 * time.Second, GPUReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Len(t, d.applyCalls, 4)
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
	_, err := Apply(context.Background(), testConfigWithGPU(), p, d, nil, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		GPUReadyTimeout: 1 * time.Second, GPUReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Len(t, d.applyCalls, 4)
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
	res, err := Apply(context.Background(), testConfigWithGPU(), p, d, nil, ApplyOpts{
		SkipGPU: true, ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.Empty(t, d.repoCalls)          // no GPU operator repo
	assert.Equal(t, 0, p.ensureDepsCalls) // no build deps
	require.Len(t, d.applyCalls, 3)       // certificate + LVM substrates + platform
	assert.Equal(t, "opo1", d.applyCalls[2].release)
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
	res, err := Apply(context.Background(), cfg, p, d, nil, ApplyOpts{
		SkipGPU: true, ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Len(t, d.applyCalls, 3) // certificate + LVM substrates + platform; GPU skipped
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
	_, err := Apply(context.Background(), testConfigWithGPU(), p, d, nil, ApplyOpts{
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
	_, err := Apply(context.Background(), testConfigWithGPU(), p, d, nil, ApplyOpts{
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
	require.NoError(t, Destroy(context.Background(), testConfigWithGPU(), p, d, nil))
	require.Len(t, d.uninstallCalls, 4)
	assert.Equal(t, "opo1", d.uninstallCalls[0].release)
	assert.Equal(t, "opo1-lvm-storage", d.uninstallCalls[1].release)
	assert.Equal(t, "opo1-cert-manager", d.uninstallCalls[2].release)
	assert.Equal(t, "opo1-gpu-operator", d.uninstallCalls[3].release)
	assert.False(t, p.state.Installed)
}
