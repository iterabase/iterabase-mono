// Package lifecycle orchestrates the forge apply phases against a
// provisioner.Provisioner and computes the reconcile plan.
package lifecycle

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/artifacts"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/deployer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/fluxer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/k3s"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/kubeconfig"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/overlayer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/provisioner"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/version"
)

// Action is what apply will do for the host.
type Action int

const (
	ActionInstall         Action = iota // k3s not installed
	ActionSkip                          // installed, in sync
	ActionRefuseUpgrade                 // version changed -> forge upgrade
	ActionRefuseImmutable               // immutable field changed -> destroy + reapply
)

func (a Action) String() string {
	switch a {
	case ActionInstall:
		return "install"
	case ActionSkip:
		return "skip"
	case ActionRefuseUpgrade:
		return "refuse-upgrade"
	case ActionRefuseImmutable:
		return "refuse-immutable"
	default:
		return "unknown"
	}
}

// ReconcilePlan is the read-only reconcile decision (also returned by --dry-run).
type ReconcilePlan struct {
	Preflight          *provisioner.PreflightResult
	HostInotify        *provisioner.HostInotifyState // live inotify instance capacity + Forge drop-in evidence
	Installed          bool
	Action             Action
	Reason             string
	ImmutableDiff      []string
	HaveVersion        string
	WantVersion        string
	ChartVersion       string // platform chart version to apply (empty => skip)
	GPUEnabled         bool   // gpu.enabled; the GPU readiness phase will run
	GPUOperatorVersion string // nvidia/gpu-operator chart version to install (empty => GPU disabled)
	GPUDriverVersion   string // nvidia driver version to pin (empty => gpu-operator chart default)
	OverlayRepo        string // overlay.repo (empty => overlay phase skipped)
	OverlayRef         string // overlay.ref (branch or tag)
	FluxEnabled        bool   // flux.enabled; the Flux GitOps phase will run
	FluxVersion        string // flux2 release tag to install (empty => Flux disabled)
	DataStorage        *provisioner.DataStorageState
}

// Result is the outcome of a mutating apply.
type Result struct {
	Plan                        *ReconcilePlan
	HostInotify                 *provisioner.HostInotifyState // post-reconcile host inotify capacity evidence
	KubeconfigPath              string
	NodeReady                   bool
	CertificateSubstrateApplied bool
	LVMStorageSubstrateApplied  bool
	DataStorage                 *provisioner.DataStorageState
	LVMStorageReady             *provisioner.LVMStorageReadiness
	ChartApplied                bool
	GPUOperatorApplied          bool   // nvidia/gpu-operator release installed/upgraded
	GPUDriverVersion            string // nvidia driver version pinned via driver.version (empty => chart default)
	GPUReady                    bool   // operator conditions and live node evidence passed the GPU readiness gate
	OverlayApplied              bool   // overlay cloned + chart applied with overlay values + CRD instances applied
	OverlayCommit               string // resolved overlay commit SHA
	SecretsApplied              bool   // declared Secrets materialized from operator env vars
	FluxInstalled               bool   // Flux components installed + GitRepository/Kustomization applied
	GitRepositoryStatus         string // gated Ready revision/digest of the forge-applied GitRepository
}

// ApplyOpts configures an apply run.
type ApplyOpts struct {
	KubeconfigOut    string
	DryRun           bool
	ReadyTimeout     time.Duration  // default 120s
	ReadyInterval    time.Duration  // default 2s
	SkipChart        bool           // skip the platform chart phase (k3s-only)
	SkipGPU          bool           // skip the GPU readiness phase
	SkipOverlay      bool           // skip the overlay phase (clone + chart values + CRD instances)
	SkipSecrets      bool           // skip the secret-sync phase (materialize declared Secrets)
	SkipFlux         bool           // skip the Flux GitOps phase (install + sync resources)
	SecretResolver   SecretResolver // resolves declared secret values (env-or-prompt); nil => env-only fallback
	GPUReadyTimeout  time.Duration  // default 15m (driver compile is slow on first boot)
	GPUReadyInterval time.Duration  // default 5s
	OverlayToken     []byte         // https git token for a private overlay repo (nil => public/file://)
}

// withDefaults fills zero-valued timeouts/intervals with their defaults.
func (o ApplyOpts) withDefaults() ApplyOpts {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 120 * time.Second
	}
	if o.ReadyInterval == 0 {
		o.ReadyInterval = 2 * time.Second
	}
	if o.GPUReadyTimeout == 0 {
		o.GPUReadyTimeout = 15 * time.Minute
	}
	if o.GPUReadyInterval == 0 {
		o.GPUReadyInterval = 5 * time.Second
	}
	return o
}

// Plan runs preflight + read-state and returns the reconcile decision. It does
// not mutate the host.
func Plan(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner) (*ReconcilePlan, error) {
	host := cfg.Spec.Hosts[0]
	pf, err := p.Preflight(ctx)
	if err != nil {
		return nil, fmt.Errorf("preflight: %w", err)
	}
	if !pf.HasSudo {
		return nil, fmt.Errorf("preflight: passwordless sudo required for user %q", host.SSHUser)
	}

	storage, err := inspectDataStorage(ctx, cfg, p)
	if err != nil {
		return nil, err
	}
	inotify, err := p.InspectHostInotify(ctx)
	if err != nil {
		return nil, err
	}
	plan := &ReconcilePlan{Preflight: pf, HostInotify: inotify, WantVersion: cfg.Spec.K3s.Version, ChartVersion: cfg.Spec.Chart.Version, DataStorage: storage}

	if cfg.Spec.GPU.Enabled {
		if !pf.HasNVIDIAGPU {
			return nil, fmt.Errorf("preflight: gpu.enabled is true but no NVIDIA GPU is present on the PCI bus (PCI passthrough is an OPO1/S11 concern, not forge)")
		}
		if !isUbuntu(pf.OS) {
			return nil, fmt.Errorf("preflight: gpu.enabled requires an Ubuntu host in v1, got %q", pf.OS)
		}
		plan.GPUEnabled = true
		plan.GPUOperatorVersion = cfg.Spec.GPU.Operator.Version
		plan.GPUDriverVersion = cfg.Spec.GPU.Driver.Version
	}
	plan.OverlayRepo = cfg.Spec.Overlay.Repo
	plan.OverlayRef = cfg.Spec.Overlay.Ref
	plan.FluxEnabled = cfg.Spec.Flux.Enabled
	plan.FluxVersion = cfg.Spec.Flux.Version

	if !pf.Installed {
		if err := validateFreshInstallPreflight(cfg, pf); err != nil {
			return nil, err
		}
		plan.Action = ActionInstall
		plan.Reason = "k3s is not installed"
		return plan, nil
	}

	st, err := p.ReadState(ctx)
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	plan.Installed = true
	plan.HaveVersion = st.Version

	if diff := immutableDiff(cfg, st); len(diff) > 0 {
		plan.Action = ActionRefuseImmutable
		plan.ImmutableDiff = diff
		plan.Reason = "immutable field(s) changed: " + strings.Join(diff, ", ")
		return plan, nil
	}
	if versionDrift(st.Version, cfg.Spec.K3s.Version) {
		plan.Action = ActionRefuseUpgrade
		plan.Reason = "k3s version changed; use 'forge upgrade'"
		return plan, nil
	}

	plan.Action = ActionSkip
	plan.Reason = "in sync"
	return plan, nil
}

// validateFreshInstallPreflight keeps the flat reconcile plan readable by
// grouping the read-only fresh-install prerequisites in one place.
func validateFreshInstallPreflight(cfg *config.Cluster, pf *provisioner.PreflightResult) error {
	if !pf.HasCurl {
		return fmt.Errorf("preflight: curl is required to install k3s")
	}
	if !pf.HasSystemd {
		return fmt.Errorf("preflight: systemd is required to run k3s")
	}
	if cfg.Spec.K3s.DualStack && !pf.HasIPv6 {
		return fmt.Errorf("preflight: dualStack enabled but host has no IPv6")
	}
	return nil
}

// Apply runs Plan and, unless DryRun, executes the install/reconcile, fetches
// and stores the kubeconfig, and waits for the node to be Ready.
func Apply(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, d deployer.Deployer, o overlayer.Overlayer, f fluxer.Fluxer, opts ApplyOpts) (*Result, error) {
	opts = opts.withDefaults()

	plan, err := Plan(ctx, cfg, p)
	if err != nil {
		return nil, err
	}
	res := &Result{Plan: plan}
	if opts.DryRun {
		return res, nil
	}

	switch plan.Action {
	case ActionRefuseImmutable:
		return res, fmt.Errorf("%s; run 'forge destroy' then 'forge apply'", plan.Reason)
	case ActionRefuseUpgrade:
		return res, fmt.Errorf("%s", plan.Reason)
	}

	storage, err := prepareHostBeforeK3s(ctx, cfg, p, d, plan, res)
	if err != nil {
		return res, err
	}
	res.DataStorage = storage

	switch plan.Action {
	case ActionInstall:
		if err := p.Install(ctx, cfg.Spec.K3s.Version, k3s.ServerArgs(cfg)); err != nil {
			auditFail(cfg, "apply", err)
			return res, err
		}
	case ActionSkip:
		// nothing to install
	}

	outPath, err := storeKubeconfig(ctx, cfg, p, opts.KubeconfigOut)
	if err != nil {
		auditFail(cfg, "apply", err)
		return res, err
	}
	res.KubeconfigPath = outPath

	ready, err := waitForReady(ctx, p, opts.ReadyTimeout, opts.ReadyInterval)
	if err != nil {
		auditFail(cfg, "apply", err)
		return res, err
	}
	res.NodeReady = ready
	if !ready {
		err = fmt.Errorf("node not ready after %s", opts.ReadyTimeout)
		auditFail(cfg, "apply", err)
		return res, err
	}

	if err := applyGPU(ctx, cfg, p, d, opts, res); err != nil {
		return res, err
	}

	// Overlay delivery is deliberately phased: clone + secrets, certificate
	// substrate, LVM storage substrate, exact Flux source artifact, platform chart, CR instances, then
	// enable continuous Flux reconciliation.
	if err := applyOverlayPhase(ctx, cfg, p, o, f, d, opts, res); err != nil {
		return res, err
	}

	_ = artifacts.AppendAudit(cfg.Metadata.Name, artifacts.AuditRecord{
		Action: "apply", Result: "success", Version: version.String(),
	})
	return res, nil
}

func prepareHostBeforeK3s(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, d deployer.Deployer, plan *ReconcilePlan, res *Result) (*provisioner.DataStorageState, error) {
	if err := refusePreLVMPlatform(ctx, cfg, d, plan); err != nil {
		return nil, err
	}
	// Host inotify capacity is reconciled before K3s installation and on every
	// installed reapply, so existing clusters heal without reboot or restart.
	inotify, err := p.ReconcileHostInotify(ctx)
	if err != nil {
		auditFail(cfg, "apply-host-inotify", err)
		return nil, fmt.Errorf("host inotify capacity: %w (observed before reconcile: %s)", err, plan.HostInotify)
	}
	res.HostInotify = inotify
	if plan.Action == ActionInstall {
		if err := p.EnsureHostSwapDisabled(ctx); err != nil {
			auditFail(cfg, "apply-host-swap", err)
			return nil, fmt.Errorf("host swap hardening: %w", err)
		}
	}
	return reconcileDataStorage(ctx, cfg, p)
}

const (
	certificateSubstrateChart        = "cert-manager-substrate"
	certificateSubstrateFirstVersion = "0.3.0"
	certificateCRDLabelSelector      = "app.kubernetes.io/name=cert-manager"
	certificateMigrationAnnotation   = "forge.horizonshift.io/certificate-substrate-migration"
	certificateMigrationComplete     = "0.3.0"
	fluxArtifactFirstVersion         = "0.3.0"
	lvmStorageSubstrateChart         = provisioner.LVMStorageSubstrateChart
	lvmStorageSubstrateFirstVersion  = provisioner.LVMStorageSubstrateFirstVersion
)

func canonicalChartVersion(version string) (string, error) {
	canonical := "v" + strings.TrimPrefix(version, "v")
	if !semver.IsValid(canonical) {
		return "", fmt.Errorf("invalid chart version %q: must be SemVer", version)
	}
	return canonical, nil
}

func chartVersionAtLeast(version, boundary string) (bool, error) {
	canonical, err := canonicalChartVersion(version)
	if err != nil {
		return false, err
	}
	return semver.Compare(canonical, "v"+boundary) >= 0, nil
}

func certificateSubstrateRequired(version string) (bool, error) {
	return chartVersionAtLeast(version, certificateSubstrateFirstVersion)
}

func certificateSubstrateRepository(platformRepository string) (string, error) {
	i := strings.LastIndex(platformRepository, "/")
	if i < 0 || platformRepository[i+1:] != "iterabase-platform" {
		return "", fmt.Errorf("platform chart repository %q must end in /iterabase-platform to resolve its certificate substrate companion", platformRepository)
	}
	return platformRepository[:i+1] + certificateSubstrateChart, nil
}

func certificateSubstrateRelease(platformRelease string) string {
	return platformRelease + "-cert-manager"
}

func lvmStorageSubstrateRequired(version string) (bool, error) {
	return chartVersionAtLeast(version, lvmStorageSubstrateFirstVersion)
}

func lvmStorageSubstrateRepository(platformRepository string) (string, error) {
	i := strings.LastIndex(platformRepository, "/")
	if i < 0 || platformRepository[i+1:] != "iterabase-platform" {
		return "", fmt.Errorf("platform chart repository %q must end in /iterabase-platform to resolve its LVM storage companion", platformRepository)
	}
	return platformRepository[:i+1] + lvmStorageSubstrateChart, nil
}

func lvmStorageSubstrateRelease(platformRelease string) string {
	return platformRelease + "-lvm-storage"
}

// lvmStorageManagerIdentity is the exact userInfo.username of the control-plane
// manager that owns AgentPool workspaces (DES-HOR-545-01). The substrate's
// admission policy fails closed unless a request creating/updating a PVC in the
// agentpool class arrives under this service-account identity, so it must match
// the manager's rendered serviceAccountName exactly.
func lvmStorageManagerIdentity(platformRelease, namespace string) string {
	return "system:serviceaccount:" + namespace + ":" + platformRelease + "-control-plane-manager"
}

// refusePreLVMPlatform enforces HOR-545's clean-install-only boundary before
// Forge mutates host packages, modules, PVs, or the VG. There is deliberately no
// local-path-to-LVM chart/PVC migration path.
func refusePreLVMPlatform(ctx context.Context, cfg *config.Cluster, d deployer.Deployer, plan *ReconcilePlan) error {
	if plan == nil || !plan.Installed || cfg.Spec.Chart.Version == "" {
		return nil
	}
	required, err := lvmStorageSubstrateRequired(cfg.Spec.Chart.Version)
	if err != nil || !required {
		return err
	}
	if d == nil {
		return fmt.Errorf("read installed platform before LVM storage reconciliation: deployer observation is unavailable")
	}
	state, err := d.Status(ctx, cfg.Spec.Chart.Release, cfg.Spec.Chart.Namespace)
	if err != nil {
		return fmt.Errorf("read installed platform before LVM storage reconciliation: %w", err)
	}
	if !state.Installed {
		return nil
	}
	have, err := canonicalChartVersion(state.Version)
	if err != nil {
		return fmt.Errorf("installed platform before LVM storage reconciliation: %w", err)
	}
	if semver.Compare(have, "v"+lvmStorageSubstrateFirstVersion) < 0 {
		return fmt.Errorf("installed platform %s predates the OpenEBS LVM storage contract; HOR-545 supports only a clean destroy and fresh install, never local-path migration", state.Version)
	}
	return nil
}

func certificateHookLabelSelector(platformRelease string) string {
	return "app.kubernetes.io/instance=" + platformRelease + ",app.kubernetes.io/managed-by=Helm"
}

func certificateOwnershipMigrationRequired(ctx context.Context, d deployer.Deployer, ch config.Chart) (bool, error) {
	requiresSubstrate, err := certificateSubstrateRequired(ch.Version)
	if err != nil || !requiresSubstrate {
		return false, err
	}
	state, err := d.Status(ctx, ch.Release, ch.Namespace)
	if err != nil {
		return false, fmt.Errorf("read platform release for certificate ownership migration: %w", err)
	}
	if !state.Installed {
		return false, nil
	}
	have, err := canonicalChartVersion(state.Version)
	if err != nil {
		return false, fmt.Errorf("installed platform release: %w", err)
	}
	if semver.Compare(have, "v"+certificateSubstrateFirstVersion) < 0 {
		return true, nil
	}

	// A 0.3 platform with CRDs still owned by the platform release is an
	// interrupted hand-off, not a completed migration. Derive that checkpoint
	// from the live annotations so a failed or partial transfer resumes.
	owned, err := d.CRDOwnedBy(ctx, certificateCRDLabelSelector, certificateSubstrateRelease(ch.Release), ch.Namespace)
	if err != nil {
		return false, fmt.Errorf("read certificate CRD ownership migration state: %w", err)
	}
	if !owned {
		return true, nil
	}
	complete, err := d.CRDsAnnotated(ctx, certificateCRDLabelSelector, certificateMigrationAnnotation, certificateMigrationComplete)
	if err != nil {
		return false, fmt.Errorf("read certificate migration completion state: %w", err)
	}
	return !complete, nil
}

// applyCertificateSubstrate installs the same-version companion release before
// any cert-manager consumers. The release owns the operator, CRDs, webhook, CSI
// driver, and ordered internal-CA bootstrap. The platform remains their Helm
// owner.
func applyCertificateSubstrate(ctx context.Context, cfg *config.Cluster, d deployer.Deployer, opts ApplyOpts, res *Result, overlayDest string) error {
	if d == nil || opts.SkipChart || cfg.Spec.Chart.Version == "" {
		return nil
	}
	ch := cfg.Spec.Chart
	required, err := certificateSubstrateRequired(ch.Version)
	if err != nil {
		return err
	}
	if !required {
		return nil // pre-0.3 platform artifacts still bundle cert-manager
	}
	repository, err := certificateSubstrateRepository(ch.Repository)
	if err != nil {
		return err
	}
	dopts := deployer.ApplyOpts{
		Release:    certificateSubstrateRelease(ch.Release),
		Repository: repository,
		Version:    ch.Version,
		Namespace:  ch.Namespace,
		// The platform observability chart owns this cross-release monitor. Keep
		// substrate installation independent of Prometheus Operator CRDs.
		Values: []string{
			"cert-manager.prometheus.servicemonitor.enabled=false",
			"global.internalTLS.platformRelease=" + ch.Release,
		},
	}
	if overlayDest != "" {
		dopts.ValueFiles = overlayValueFiles(overlayDest)
	}
	if err := d.Apply(ctx, dopts); err != nil {
		auditFail(cfg, "apply-certificate-substrate", err)
		return fmt.Errorf("certificate substrate: %w", err)
	}
	res.CertificateSubstrateApplied = true
	return nil
}

// applyLVMStorageSubstrate installs the same-version OpenEBS companion after
// Forge has prepared the exact host VG and before any platform PVC is created.
func applyLVMStorageSubstrate(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, d deployer.Deployer, opts ApplyOpts, res *Result) error {
	if d == nil || opts.SkipChart || cfg.Spec.Chart.Version == "" {
		return nil
	}
	ch := cfg.Spec.Chart
	required, err := lvmStorageSubstrateRequired(ch.Version)
	if err != nil || !required {
		return err
	}
	repository, err := lvmStorageSubstrateRepository(ch.Repository)
	if err != nil {
		return err
	}
	if err := d.Apply(ctx, deployer.ApplyOpts{
		Release:    lvmStorageSubstrateRelease(ch.Release),
		Repository: repository,
		Version:    ch.Version,
		Namespace:  ch.Namespace,
		// DES-HOR-545-01: the only principal permitted to create/update PVCs in
		// the AgentPool workspace class is the control-plane manager. Admission
		// fails closed unless the request arrives under this exact SA username.
		Values: []string{"agentpool.authorizedManagerIdentity=" + lvmStorageManagerIdentity(ch.Release, ch.Namespace)},
	}); err != nil {
		auditFail(cfg, "apply-lvm-storage-substrate", err)
		return fmt.Errorf("LVM storage substrate: %w", err)
	}
	res.LVMStorageSubstrateApplied = true
	readiness, err := p.WaitForLVMStorageReady(ctx, ch.Namespace, res.DataStorage)
	if err != nil {
		auditFail(cfg, "wait-lvm-storage-substrate", err)
		return err
	}
	res.LVMStorageReady = readiness
	return nil
}

// applyChart runs the platform chart phase (helm upgrade --install) when a chart
// version is configured and the phase is not skipped. When an overlay is cloned
// (overlayDest != ""), its value files feed the chart (-f values.yaml -f
// values.client.yaml). No-op otherwise.
func applyChart(ctx context.Context, cfg *config.Cluster, d deployer.Deployer, opts ApplyOpts, res *Result, overlayDest string, extraValues ...string) error {
	return applyChartWithWait(ctx, cfg, d, opts, res, overlayDest, false, extraValues...)
}

func applyChartWithWait(ctx context.Context, cfg *config.Cluster, d deployer.Deployer, opts ApplyOpts, res *Result, overlayDest string, noWait bool, extraValues ...string) error {
	if d == nil || opts.SkipChart || cfg.Spec.Chart.Version == "" {
		return nil
	}
	ch := cfg.Spec.Chart
	dopts := deployer.ApplyOpts{
		Release:    ch.Release,
		Repository: ch.Repository,
		Version:    ch.Version,
		Namespace:  ch.Namespace,
		Values:     append([]string(nil), extraValues...),
		NoWait:     noWait,
	}
	if overlayDest != "" {
		dopts.ValueFiles = overlayValueFiles(overlayDest)
	}
	if err := d.Apply(ctx, dopts); err != nil {
		auditFail(cfg, "apply", err)
		return fmt.Errorf("chart: %w", err)
	}
	res.ChartApplied = true
	return nil
}

// migrateCertificateOwnership performs the one-time platform-first hand-off
// from the <=0.2.2 bundled cert-manager resources to the 0.3 companion release.
func migrateCertificateOwnership(ctx context.Context, cfg *config.Cluster, d deployer.Deployer, opts ApplyOpts, res *Result, overlayDest string) (bool, error) {
	if d == nil || opts.SkipChart || cfg.Spec.Chart.Version == "" {
		return false, nil
	}
	migration, err := certificateOwnershipMigrationRequired(ctx, d, cfg.Spec.Chart)
	if err != nil || !migration {
		return false, err
	}
	ch := cfg.Spec.Chart
	// The old cert-issuers subchart created ClusterIssuers and internal CA
	// Certificates as unowned Helm hooks. Adopt them into the unchanged platform
	// release before 0.3 renders them as normal resources.
	if err := d.TransferCertificateHookOwnership(ctx, certificateHookLabelSelector(ch.Release), ch.Release, ch.Namespace); err != nil {
		auditFail(cfg, "migrate-certificate-hook-ownership", err)
		return false, fmt.Errorf("certificate hook ownership migration: %w", err)
	}
	if err := applyChart(ctx, cfg, d, opts, res, overlayDest, "control-plane.toolRunner.enabled=false"); err != nil {
		return false, err
	}
	if err := d.TransferCRDOwnership(ctx, certificateCRDLabelSelector, certificateSubstrateRelease(ch.Release), ch.Namespace); err != nil {
		auditFail(cfg, "migrate-certificate-substrate-ownership", err)
		return false, fmt.Errorf("certificate substrate ownership migration: %w", err)
	}
	return true, nil
}

// cloneOverlay clones the client fork when overlay.repo is configured, returning
// the host dest path + resolved commit. Returns ("", "", nil) when the overlay
// phase is skipped (no repo, SkipOverlay). Errors if a repo is set but no
// overlayer is wired.
func cloneOverlay(ctx context.Context, cfg *config.Cluster, o overlayer.Overlayer, opts ApplyOpts) (dest, commit string, err error) {
	if cfg.Spec.Overlay.Repo == "" || opts.SkipOverlay {
		return "", "", nil
	}
	if o == nil {
		return "", "", fmt.Errorf("overlay.repo is set but no overlayer is wired (internal error)")
	}
	if err := o.EnsureGit(ctx); err != nil {
		return "", "", fmt.Errorf("overlay git: %w", err)
	}
	dest = overlayDestPath(cfg)
	commit, err = o.Clone(ctx, cfg.Spec.Overlay.Repo, cfg.Spec.Overlay.Ref, dest, opts.OverlayToken)
	if err != nil {
		return "", "", fmt.Errorf("overlay clone: %w", err)
	}
	return dest, commit, nil
}

// applyCRDInstances runs kubectl apply -k on the overlay's crds/client dir (after
// the chart so the CRD kinds exist). No-op when no overlay is configured.
func applyCRDInstances(ctx context.Context, d deployer.Deployer, overlayDest string) error {
	if overlayDest == "" || d == nil {
		return nil
	}
	if err := d.ApplyKustomize(ctx, overlayDest+"/crds/client"); err != nil {
		return fmt.Errorf("overlay crd instances: %w", err)
	}
	return nil
}

// applyOverlayPhase runs the ordered delivery path: clone the client fork →
// secrets → certificate substrate → LVM storage substrate → Flux source → platform → CRs → Flux
// Kustomization. The source precedes Helm because the chart-managed tool runner
// is intentionally unready until it loads a valid generation.
func applyOverlayPhase(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, o overlayer.Overlayer, f fluxer.Fluxer, d deployer.Deployer, opts ApplyOpts, res *Result) error {
	overlayDest, overlayCommit, err := cloneOverlay(ctx, cfg, o, opts)
	if err != nil {
		auditFail(cfg, "apply-overlay", err)
		return err
	}
	res.OverlayCommit = overlayCommit
	if err := validateChartFluxSource(ctx, cfg, f, d, opts, overlayCommit); err != nil {
		auditFail(cfg, "apply-flux", err)
		return err
	}
	if err := applySecrets(ctx, o, d, opts, res, overlayDest); err != nil {
		auditFail(cfg, "apply-secrets", err)
		return err
	}

	// Platform <=0.2.2 owns cert-manager and CSI objects in the platform Helm
	// release. Installing the companion first would fail on those exact names.
	// Upgrade the old owner with the new runner deferred, transfer only the kept
	// CRD annotations, then install the companion. The normal platform apply
	// below restores the overlay's intended runner value after Flux is Ready.
	// MetalLB hook-ownership adoption (DES-HOR-511-01) runs before this hand-off:
	// hook-era predecessors created the pools/advertisements as unowned hooks, so
	// Helm must adopt them before any target platform apply (including the
	// certificate hand-off apply below) renders them as ordinary resources, or it
	// rejects the upgrade. Idempotent and a no-op when no hooks exist.
	if err := migrateMetalLBHookOwnership(ctx, cfg, d, opts); err != nil {
		return err
	}
	migrated, err := migrateCertificateOwnership(ctx, cfg, d, opts, res, overlayDest)
	if err != nil {
		return err
	}
	if err := applyCertificateSubstrate(ctx, cfg, d, opts, res, overlayDest); err != nil {
		return err
	}
	if err := applyLVMStorageSubstrate(ctx, cfg, p, d, opts, res); err != nil {
		return err
	}
	if err := applyFluxSourcePhase(ctx, cfg, o, f, d, opts, res, overlayCommit); err != nil {
		return err
	}
	if err := applyPlatformChartPhase(ctx, cfg, d, opts, res, overlayDest, migrated); err != nil {
		return err
	}
	if err := applyCRDInstances(ctx, d, overlayDest); err != nil {
		auditFail(cfg, "apply-overlay", err)
		return err
	}
	if err := applyFluxReconciliationPhase(ctx, cfg, d, opts); err != nil {
		return err
	}
	res.OverlayApplied = overlayDest != ""
	return nil
}

func applyPlatformChartPhase(ctx context.Context, cfg *config.Cluster, d deployer.Deployer, opts ApplyOpts, res *Result, overlayDest string, migrated bool) error {
	if migrated {
		// Routine gateway config changes roll through the chart's pod-template
		// checksum. The migration still stages an explicit restart because its
		// guarded platform apply intentionally omitted the runner, so the existing
		// gateway Pod loaded no approved runner identity. Publish the final ConfigMap
		// without waiting, restart only that gateway, then let the normal waited Helm
		// reconcile below gate on runner registration.
		if err := applyChartWithWait(ctx, cfg, d, opts, res, overlayDest, true); err != nil {
			return err
		}
		selector := "app.kubernetes.io/name=control-plane,app.kubernetes.io/instance=" + cfg.Spec.Chart.Release + ",app.kubernetes.io/component=gateway"
		if err := d.RestartDeployment(ctx, selector, cfg.Spec.Chart.Namespace); err != nil {
			auditFail(cfg, "restart-migrated-gateway", err)
			return fmt.Errorf("restart migrated gateway: %w", err)
		}
	}
	if err := applyChart(ctx, cfg, d, opts, res, overlayDest); err != nil {
		return err
	}
	if cfg.Spec.Chart.Version == "" || d == nil || opts.SkipChart {
		return nil
	}
	requiresSubstrate, err := certificateSubstrateRequired(cfg.Spec.Chart.Version)
	if err != nil || !requiresSubstrate {
		return err
	}
	complete, err := d.CRDsAnnotated(ctx, certificateCRDLabelSelector, certificateMigrationAnnotation, certificateMigrationComplete)
	if err != nil {
		return fmt.Errorf("read certificate migration completion state: %w", err)
	}
	if !complete {
		if err := d.AnnotateCRDs(ctx, certificateCRDLabelSelector, certificateMigrationAnnotation, certificateMigrationComplete); err != nil {
			auditFail(cfg, "mark-certificate-migration-complete", err)
			return fmt.Errorf("mark certificate migration complete: %w", err)
		}
	}
	return nil
}

// migrateMetalLBHookOwnership adopts legacy hook-created MetalLB pools and
// advertisements into the platform release before it renders them as ordinary
// resources (DES-HOR-511-01). It is idempotent and a no-op unless legacy
// hook-owned objects exist, and must run before Helm's upgrade (which would
// otherwise reject adopting the existing objects).
func migrateMetalLBHookOwnership(ctx context.Context, cfg *config.Cluster, d deployer.Deployer, opts ApplyOpts) error {
	if d == nil || opts.SkipChart || cfg.Spec.Chart.Version == "" || cfg.Spec.Chart.Namespace == "" {
		return nil
	}
	if err := d.TransferMetalLBHookOwnership(ctx, cfg.Spec.Chart.Release, cfg.Spec.Chart.Namespace); err != nil {
		auditFail(cfg, "migrate-metallb-hook-ownership", err)
		return fmt.Errorf("metallb hook ownership migration: %w", err)
	}
	return nil
}

// overlayDestPath is the host path where the overlay fork is cloned.
func overlayDestPath(cfg *config.Cluster) string {
	return "/var/lib/forge/overlay/" + cfg.Metadata.Name
}

// overlayValueFiles returns the -f value files from the cloned overlay, in order
// (base then client; later wins). Both files always exist in a valid overlay
// (values.client.yaml may be comment-only).
func overlayValueFiles(dest string) []string {
	return []string{dest + "/values.yaml", dest + "/values.client.yaml"}
}

// gpuOperatorRepoName is the local Helm repository name forge registers the
// NVIDIA chart repo under. Not user-facing.
const gpuOperatorRepoName = "nvidia"

// applyGPU runs the GPU node-readiness phase: ensure the host can build the
// driver, install/upgrade the NVIDIA GPU Operator release, then gate on one
// coherent operator/node observation for the requested driver. No-op when GPU
// is disabled or skipped. Runs after the k3s node is Ready and before the
// platform chart (substrate before app) so the first ModelBackend-driven GPU
// pod can schedule immediately.
func applyGPU(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, d deployer.Deployer, opts ApplyOpts, res *Result) error {
	if !cfg.Spec.GPU.Enabled || opts.SkipGPU {
		// Even when the GPU phase is skipped, surface the configured driver pin
		// in the result so the apply report reflects what the config requests
		// rather than claiming chart-default semantics (HOR-401: report the
		// pinned version). The operator did not run, so GPUOperatorApplied /
		// GPUReady stay false.
		res.GPUDriverVersion = cfg.Spec.GPU.Driver.Version
		return nil
	}
	if err := p.EnsureDriverBuildDeps(ctx); err != nil {
		auditFail(cfg, "apply-gpu", err)
		return fmt.Errorf("gpu build deps: %w", err)
	}
	g := cfg.Spec.GPU.Operator
	if err := d.EnsureRepo(ctx, gpuOperatorRepoName, g.Repository); err != nil {
		auditFail(cfg, "apply-gpu", err)
		return fmt.Errorf("gpu operator repo: %w", err)
	}
	chartRef := gpuOperatorRepoName + "/" + g.Chart
	if err := d.Apply(ctx, deployer.ApplyOpts{
		Release:    g.Release,
		Repository: chartRef,
		Version:    g.Version,
		Checksum:   g.SHA256,
		Namespace:  g.Namespace,
		Values:     gpuOperatorValues(cfg.Spec.GPU),
	}); err != nil {
		auditFail(cfg, "apply-gpu", err)
		return fmt.Errorf("gpu operator: %w", err)
	}
	res.GPUOperatorApplied = true
	res.GPUDriverVersion = cfg.Spec.GPU.Driver.Version

	readiness, err := waitForGPU(ctx, p, cfg.Spec.GPU.Driver.Version, opts)
	if err != nil {
		auditFail(cfg, "apply-gpu", err)
		return err
	}
	res.GPUReady = readiness.Ready
	return nil
}

// waitForGPU polls one coherent ClusterPolicy/node observation until the
// operator conditions and live node agree on the requested driver. Query
// errors are tolerated while the CRD is installing or k3s is restarting, but
// the final error retains the last observation (or query failure). A terminal
// driver upgrade failure stops immediately instead of consuming the timeout.
func waitForGPU(ctx context.Context, p provisioner.Provisioner, requestedDriverVersion string, opts ApplyOpts) (*provisioner.GPUReadiness, error) {
	deadline := time.Now().Add(opts.GPUReadyTimeout)
	var last *provisioner.GPUReadiness
	var lastErr error
	for {
		readiness, err := p.ReadGPUReadiness(ctx, requestedDriverVersion)
		if err == nil {
			last = readiness
			lastErr = nil
			if readiness.Ready {
				return readiness, nil
			}
			if readiness.Terminal {
				return readiness, fmt.Errorf("gpu readiness entered a terminal state: %s", readiness)
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if last != nil && lastErr != nil {
				return last, fmt.Errorf("gpu not ready after %s: %s; latest observation failed: %v", opts.GPUReadyTimeout, last, lastErr)
			}
			if last != nil {
				return last, fmt.Errorf("gpu not ready after %s: %s", opts.GPUReadyTimeout, last)
			}
			if lastErr != nil {
				return nil, fmt.Errorf("gpu not ready after %s; last observation failed: %w", opts.GPUReadyTimeout, lastErr)
			}
			return nil, fmt.Errorf("gpu not ready after %s; no readiness observation", opts.GPUReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(opts.GPUReadyInterval):
		}
	}
}

// gpuOperatorValues returns the forge-internal, prod-ready Helm --set values for
// the NVIDIA GPU Operator. These mirror the chart's defaults and are set
// explicitly so forge's intent is pinned against chart default changes; advanced
// overrides are a fast-follow. CDI is enabled so workloads request
// nvidia.com/gpu with no runtimeClassName.
//
// GPU Operator v26.3.3 embeds the NFD 0.18.3 subchart. That subchart's supported
// values can run the compatible NFD v0.19.0 image without replacing chart-owned
// resources: it retains the same NodeFeature CRDs and master/worker commands,
// while the disabled topology updater avoids v0.19.0's only mandatory image-only
// RBAC change. v0.19.0 PR #2415 makes master resyncPeriod drive a full reconcile,
// so a missed fresh-NodeFeature event is repaired within a bounded interval.
// PR #2545 also improves node-rebuild relabeling, but is not the basis for the
// fresh-NodeFeature failure mode or this periodic safety net.
//
// The validated driver identity binds both the reported module version and the
// exact driver-container digest. GPU Operator accepts tag-plus-digest in its
// version field and therefore does not append a mutable OS tag; readiness
// normalizes the digest suffix while retaining the requested module version.
//
// k3s containerd: the operator does not auto-detect k3s, so the toolkit must be
// pointed at k3s's containerd config + socket via toolkit.env (the operator
// derives the host mounts from these and rewrites them to its in-container
// paths). Without this the toolkit configures /etc/containerd and signals
// /run/containerd/containerd.sock — neither of which k3s uses — and crashes.
// forge is k3s-only in v1; revisit for HA/BYOK. CDI is enabled, but on k3s the
// nvidia runtime is what injects the GPU: workloads must set
// runtimeClassName: nvidia AND request nvidia.com/gpu (a pod that only requests
// nvidia.com/gpu, with no runtimeClassName, gets no device). nvidia stays
// non-default, so non-GPU pods run on runc. This is the Q7 RuntimeClass
// fallback — record it for HOR-306 (ModelBackend vLLM pod spec).
//
// Driver upgrade policy (HOR-411): on a single-node inference cluster a pinned
// driver bump is driven by the gpu-operator's driver.upgradePolicy. By default
// the operator refuses to delete GPU pods that carry local storage (emptyDir),
// which stalls the upgrade at pod-deletion-required and leaves the only node
// cordoned with nvidia.com/gpu-driver-upgrade-state=upgrade-failed. forge pins:
//
//   - driver.upgradePolicy.gpuPodDeletion.deleteEmptyDir=true — let the operator
//     delete GPU pods that hold emptyDir volumes so the upgrade can progress.
//   - driver.upgradePolicy.drain.enable=false — never full-node drain. On a
//     single-node cluster a drain would evict Flux, metrics-server, and the rest
//     of the control plane; only GPU pods are deleted, by the operator, not by
//     kubelet eviction.
//
// Workload contract: deleteEmptyDir permanently discards emptyDir contents.
// GPU workload emptyDir volumes MUST contain only disposable data — e.g. the
// memory-backed /dev/shm used for inference scratch and the in-memory KV cache.
// Durable data (Hugging Face model cache, etc.) must live on persistent host
// storage, not in emptyDir. A driver upgrade therefore terminates active GPU
// inference pods, discards their ephemeral state, and forces a model reload;
// there is no zero-downtime driver upgrade on a single-node cluster (a non-goal).
const (
	gpuOperatorImageVersion      = "v26.3.3@sha256:6584c36f153d18cfce284f7e5bc477887ce3c1ac566dc795bd80c9af6c6488f7"
	gpuValidatorImageVersion     = gpuOperatorImageVersion
	gpuDriverManagerImageVersion = "v0.11.0@sha256:8aec215a8b159b0162b55e688065efd58ebfa848ebc999c1797221686ff1243d"
	gpuToolkitImageVersion       = "v1.19.1@sha256:c927adbc9b7755c5cb90022fdcc5c1295f5fe5fe1f38200a2dc65e85632b029c"
	gpuDevicePluginImageVersion  = "v0.19.3@sha256:25cc340fe6fd53c101e16fc452f503e7a92c219c64a80ed5381784b522dbbf77"
	gpuDCGMExporterImageVersion  = "4.5.3-4.8.2-distroless@sha256:60d3b00ac80b4ae77f94dae2f943685605585ad9e92fdccda3154d009ae317cc"
	gpuMIGManagerImageVersion    = "v0.14.2@sha256:313586bfa5c07601a83f310dd700db0007d489df2ad42bb52c611802cbb7a278"
	gpuNFDImageRepository        = "registry.k8s.io/nfd/node-feature-discovery"
	gpuNFDImageTag               = "v0.19.0@sha256:2fa1c99ad09bdf2c8ad97706a4ad2fd548c84d5ecd70ba32a6152c667b96c4d2"
	gpuNFDMasterResyncPeriod     = "30s"
)

func gpuOperatorValues(g config.GPU) []string {
	values := []string{
		"cdi.enabled=true",
		"driver.enabled=true",
		"toolkit.enabled=true",
		"devicePlugin.enabled=true",
		"gfd.enabled=true",
		"operator.version=" + gpuOperatorImageVersion,
		"validator.version=" + gpuValidatorImageVersion,
		"driver.manager.version=" + gpuDriverManagerImageVersion,
		"toolkit.version=" + gpuToolkitImageVersion,
		"devicePlugin.version=" + gpuDevicePluginImageVersion,
		"dcgmExporter.version=" + gpuDCGMExporterImageVersion,
		"gfd.version=" + gpuDevicePluginImageVersion,
		"migManager.version=" + gpuMIGManagerImageVersion,
		"node-feature-discovery.image.repository=" + gpuNFDImageRepository,
		"node-feature-discovery.image.tag=" + gpuNFDImageTag,
		"node-feature-discovery.master.resyncPeriod=" + gpuNFDMasterResyncPeriod,
		"toolkit.env[0].name=CONTAINERD_CONFIG",
		"toolkit.env[0].value=/var/lib/rancher/k3s/agent/etc/containerd/config.toml",
		"toolkit.env[1].name=CONTAINERD_SOCKET",
		"toolkit.env[1].value=/run/k3s/containerd/containerd.sock",
		"toolkit.env[2].name=CONTAINERD_RUNTIME_CLASS",
		"toolkit.env[2].value=nvidia",
		// HOR-411: unblock driver upgrades on single-node inference clusters
		// without enabling a full-node drain. See the doc comment above.
		"driver.upgradePolicy.gpuPodDeletion.deleteEmptyDir=true",
		"driver.upgradePolicy.drain.enable=false",
	}
	if v := strings.TrimSpace(g.Driver.Version); v != "" {
		identity := v
		if checksum := strings.TrimSpace(g.Driver.SHA256); checksum != "" {
			identity += "@sha256:" + checksum
		}
		values = append(values, "driver.version="+identity)
	}
	return values
}

func isUbuntu(os string) bool { return strings.HasPrefix(os, "Ubuntu") }

// DestroyOpts controls the two explicit fixture/decommission operations that
// ordinary destroy must never imply.
type DestroyOpts struct {
	PurgeDataStorage bool
	Reboot           bool
}

// Destroy removes platform/K3s resources but deliberately preserves the
// receipt-matching data PVs, iterabase-data VG, LVs, and bytes.
func Destroy(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, d deployer.Deployer, o overlayer.Overlayer, f fluxer.Fluxer) error {
	return DestroyWithOptions(ctx, cfg, p, d, o, f, DestroyOpts{})
}

// DestroyWithOptions preserves ordinary destroy semantics and performs an
// explicitly requested purge only after the existing product/platform/K3s
// destroy path succeeds. A requested reboot is last, after any purge.
func DestroyWithOptions(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, d deployer.Deployer, o overlayer.Overlayer, f fluxer.Fluxer, opts DestroyOpts) error {
	destroyProductSubstrate(ctx, cfg, d, o, f)
	if err := p.Uninstall(ctx); err != nil {
		return err
	}
	if opts.PurgeDataStorage {
		if err := purgeDataStorage(ctx, cfg, p); err != nil {
			return err
		}
	}
	if opts.Reboot {
		if err := rebootHost(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// destroyProductSubstrate stops reconcilers and charts in reverse apply order.
// Cleanup remains best-effort and never mutates the selected data disks or VG.
func destroyProductSubstrate(ctx context.Context, cfg *config.Cluster, d deployer.Deployer, o overlayer.Overlayer, f fluxer.Fluxer) {
	if f != nil && cfg.Spec.Flux.Enabled {
		_ = f.UninstallFlux(ctx)
	}
	if o != nil && cfg.Spec.Overlay.Repo != "" {
		_ = o.Remove(ctx, overlayDestPath(cfg))
		if cfg.Spec.Flux.Enabled && strings.HasPrefix(cfg.Spec.Overlay.Repo, "file://") {
			_ = o.StopServingToFlux(ctx)
		}
	}
	if d != nil && cfg.Spec.Chart.Version != "" {
		ch := cfg.Spec.Chart
		_ = d.UninstallChart(ctx, ch.Release, ch.Namespace)
		if required, err := lvmStorageSubstrateRequired(ch.Version); err == nil && required {
			_ = d.UninstallChart(ctx, lvmStorageSubstrateRelease(ch.Release), ch.Namespace)
		}
		if required, err := certificateSubstrateRequired(ch.Version); err == nil && required {
			_ = d.UninstallChart(ctx, certificateSubstrateRelease(ch.Release), ch.Namespace)
		}
	}
	if d != nil && cfg.Spec.GPU.Enabled {
		g := cfg.Spec.GPU.Operator
		_ = d.UninstallChart(ctx, g.Release, g.Namespace)
	}
}

func purgeDataStorage(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner) error {
	purger, ok := p.(provisioner.DataStoragePurger)
	if !ok {
		return fmt.Errorf("data-storage purge is unavailable for this provisioner")
	}
	if err := purger.PurgeDataStorage(ctx, dataStorageSpec(cfg)); err != nil {
		return fmt.Errorf("purge data storage: %w", err)
	}
	return nil
}

func rebootHost(ctx context.Context, p provisioner.Provisioner) error {
	rebooter, ok := p.(provisioner.Rebooter)
	if !ok {
		return fmt.Errorf("host reboot is unavailable for this provisioner")
	}
	if err := rebooter.Reboot(ctx); err != nil {
		return fmt.Errorf("reboot host: %w", err)
	}
	return nil
}

// Upgrade re-runs the k3s install script with a new version (in-place upgrade),
// then refreshes the kubeconfig and waits for the node to be Ready. The host
// must already have k3s installed (use apply first).
func Upgrade(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, d deployer.Deployer, to string, opts ApplyOpts) (*Result, error) {
	if opts.ReadyTimeout == 0 {
		opts.ReadyTimeout = 120 * time.Second
	}
	if opts.ReadyInterval == 0 {
		opts.ReadyInterval = 2 * time.Second
	}

	plan, storage, err := validateUpgradeFoundation(ctx, cfg, p, d)
	if err != nil {
		return nil, err
	}
	if to == "" {
		to = cfg.Spec.K3s.Version
	}

	inotify, err := p.ReconcileHostInotify(ctx)
	if err != nil {
		auditFail(cfg, "upgrade-host-inotify", err)
		return nil, fmt.Errorf("host inotify capacity: %w (observed before reconcile: %s)", err, plan.HostInotify)
	}

	if err := p.Upgrade(ctx, to, k3s.ServerArgs(cfg)); err != nil {
		auditFail(cfg, "upgrade", err)
		return nil, err
	}

	res := &Result{Plan: plan, HostInotify: inotify, DataStorage: storage}
	outPath, err := storeKubeconfig(ctx, cfg, p, opts.KubeconfigOut)
	if err != nil {
		auditFail(cfg, "upgrade", err)
		return nil, err
	}
	res.KubeconfigPath = outPath

	ready, err := waitForReady(ctx, p, opts.ReadyTimeout, opts.ReadyInterval)
	if err != nil {
		auditFail(cfg, "upgrade", err)
		return nil, err
	}
	res.NodeReady = ready
	if !ready {
		err = fmt.Errorf("node not ready after %s", opts.ReadyTimeout)
		auditFail(cfg, "upgrade", err)
		return nil, err
	}

	_ = artifacts.AppendAudit(cfg.Metadata.Name, artifacts.AuditRecord{
		Action: "upgrade", Result: "success", Version: version.String(),
	})
	return res, nil
}

func validateUpgradeFoundation(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, d deployer.Deployer) (*ReconcilePlan, *provisioner.DataStorageState, error) {
	st, err := p.ReadState(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read state: %w", err)
	}
	if !st.Installed {
		return nil, nil, fmt.Errorf("k3s not installed; run 'forge apply' first")
	}
	if diff := immutableDiff(cfg, st); len(diff) > 0 {
		return nil, nil, fmt.Errorf("refuse unsupported k3s upgrade: immutable field(s) differ: %s; HOR-545 supports clean install, never local-path migration", strings.Join(diff, ", "))
	}
	storage, err := inspectDataStorage(ctx, cfg, p)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect data storage before upgrade: %w", err)
	}
	if storage == nil || storage.State != "complete" || storage.VGName != provisioner.DataVolumeGroupName || storage.VGUUID == "" {
		return nil, nil, fmt.Errorf("refuse unsupported k3s upgrade: installed cluster lacks the complete receipt-bound OpenEBS LVM storage foundation; HOR-545 supports clean install, never local-path migration")
	}
	plan := &ReconcilePlan{Installed: true, WantVersion: cfg.Spec.K3s.Version, ChartVersion: cfg.Spec.Chart.Version, DataStorage: storage}
	if err := refusePreLVMPlatform(ctx, cfg, d, plan); err != nil {
		return nil, nil, err
	}
	inotify, err := p.InspectHostInotify(ctx)
	if err != nil {
		return nil, nil, err
	}
	plan.HostInotify = inotify
	return plan, storage, nil
}

// storeKubeconfig fetches the kubeconfig from the host, rewrites the server
// URL for off-host use, and writes it to outPath (or the per-install
// artifacts dir when outPath is empty). Returns the final path.
func storeKubeconfig(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, outPath string) (string, error) {
	raw, err := p.FetchKubeconfig(ctx)
	if err != nil {
		return "", err
	}
	kc, err := kubeconfig.RewriteServer(raw, cfg.Spec.Hosts[0].Address, 6443)
	if err != nil {
		return "", err
	}
	if outPath == "" {
		if err := artifacts.WriteKubeconfig(cfg.Metadata.Name, kc); err != nil {
			return "", err
		}
		return artifacts.KubeconfigPath(cfg.Metadata.Name)
	}
	if err := os.WriteFile(outPath, kc, 0o600); err != nil {
		return "", err
	}
	return outPath, nil
}

func waitForReady(ctx context.Context, p provisioner.Provisioner, timeout, interval time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		ready, err := p.NodeReady(ctx)
		if err == nil && ready {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func immutableDiff(cfg *config.Cluster, st *provisioner.HostState) []string {
	var diff []string
	if st.ClusterCIDR != "" && st.ClusterCIDR != k3s.DesiredClusterCIDR(cfg.Spec.K3s) {
		diff = append(diff, "k3s.clusterCIDR")
	}
	if st.ServiceCIDR != "" && st.ServiceCIDR != k3s.DesiredServiceCIDR(cfg.Spec.K3s) {
		diff = append(diff, "k3s.serviceCIDR")
	}
	if st.DualStack != cfg.Spec.K3s.DualStack {
		diff = append(diff, "k3s.dualStack")
	}
	if !st.LocalStorageDisabled {
		diff = append(diff, "k3s.disable[local-storage]")
	}
	return diff
}

func versionDrift(have, want string) bool {
	if have == "" {
		return false
	}
	return normalizeVersion(have) != normalizeVersion(want)
}

func normalizeVersion(v string) string {
	if i := strings.IndexByte(v, '+'); i >= 0 {
		return v[:i]
	}
	return v
}

func auditFail(cfg *config.Cluster, action string, err error) {
	_ = artifacts.AppendAudit(cfg.Metadata.Name, artifacts.AuditRecord{
		Action: action, Result: "failure", Detail: err.Error(), Version: version.String(),
	})
}
