package lifecycle

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/deployer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/overlay"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/overlayer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/provisioner"
)

// overlayToolsFirstVersion is the first platform chart whose tool runner reads
// customer tools from the Forge-written ConfigMaps (DES-HOR-593-05). Older 0.3+
// charts read them from a Flux artifact, which Forge no longer provides.
const overlayToolsFirstVersion = "0.6.0"

// overlayToolsValue enables the tool runner's ConfigMap overlay source.
const overlayToolsValue = "control-plane.toolRunner.overlayTools.enabled"

// overlayStageDir is the host directory holding the staged Helm value files.
func overlayStageDir(cfg *config.Cluster) string {
	return "/var/lib/forge/overlay/" + cfg.Metadata.Name
}

// validateOverlayChart refuses platform charts whose tool runner still expects
// a Flux artifact: Helm would wait forever for a generation.
func validateOverlayChart(cfg *config.Cluster, opts ApplyOpts) error {
	if opts.SkipChart || cfg.Spec.Chart.Version == "" {
		return nil
	}
	hasRunner, err := chartVersionAtLeast(cfg.Spec.Chart.Version, toolRunnerFirstVersion)
	if err != nil || !hasRunner {
		return err
	}
	current, err := overlayToolsChart(cfg.Spec.Chart.Version)
	if err != nil {
		return err
	}
	if !current {
		return fmt.Errorf("platform chart %s reads customer tools from Flux, which Forge no longer installs (DES-HOR-593-04); use chart %s or later", cfg.Spec.Chart.Version, overlayToolsFirstVersion)
	}
	return nil
}

// overlayToolsChart reports whether a platform chart reads the ConfigMap tools
// source. Unlike the substrate gates, a prerelease of the boundary release counts:
// a PR preview such as 0.6.0-pr.171.888 is built from 0.6.0's source.
func overlayToolsChart(version string) (bool, error) {
	canonical, err := canonicalChartVersion(version)
	if err != nil {
		return false, err
	}
	core, _, _ := strings.Cut(semver.Canonical(canonical), "-")
	return semver.Compare(core, "v"+overlayToolsFirstVersion) >= 0, nil
}

// overlayToolsValues tells a tool-runner chart whether Forge delivers an
// overlay tools archive.
func overlayToolsValues(cfg *config.Cluster, opts ApplyOpts) []string {
	if ok, err := overlayToolsChart(cfg.Spec.Chart.Version); err != nil || !ok {
		return nil
	}
	return []string{fmt.Sprintf("%s=%t", overlayToolsValue, deliversOverlay(opts))}
}

// reconcilesOverlay reports whether this apply owns the overlay objects'
// lifecycle: every apply except --skip-overlay. Without a configured overlay it
// prunes every overlay-owned object, so removing the overlay removes its
// objects and tools.
func reconcilesOverlay(opts ApplyOpts) bool {
	return !opts.SkipOverlay
}

func deliversOverlay(opts ApplyOpts) bool {
	return opts.Overlay != nil && !opts.SkipOverlay
}

// applyOverlayPhase runs the ordered delivery path: stage value files and
// materialize secrets → certificate substrate → LVM storage substrate → tools
// archive → platform chart → owned object instances → prune superseded overlay
// objects. The tools archive precedes Helm because the chart-managed tool
// runner is intentionally unready until it loads a valid generation.
func applyOverlayPhase(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, o overlayer.Overlayer, d deployer.Deployer, opts ApplyOpts, res *Result) error {
	valueFiles, err := stageValueFiles(ctx, cfg, o, opts)
	if err != nil {
		auditFail(cfg, "apply-overlay", err)
		return err
	}
	for _, m := range opts.ExtraManifests {
		if d == nil {
			break
		}
		if err := d.ApplyManifest(ctx, m); err != nil {
			auditFail(cfg, "apply-installation-objects", err)
			return fmt.Errorf("installation objects: %w", err)
		}
	}
	if deliversOverlay(opts) {
		res.OverlayRevision = opts.Overlay.Revision
		if err := applySecrets(ctx, d, opts, res, opts.Overlay.Secrets); err != nil {
			auditFail(cfg, "apply-secrets", err)
			return err
		}
	}

	// MetalLB hook-ownership adoption (DES-HOR-511-01): hook-era predecessors
	// created the pools/advertisements as unowned hooks, so Helm must adopt them
	// before the platform apply renders them as ordinary resources, or it
	// rejects the upgrade. Idempotent and a no-op when no hooks exist.
	if err := migrateMetalLBHookOwnership(ctx, cfg, d, opts); err != nil {
		return err
	}
	if err := applyCertificateSubstrate(ctx, cfg, d, opts, res, valueFiles); err != nil {
		return err
	}
	if err := applyLVMStorageSubstrate(ctx, cfg, p, d, opts, res); err != nil {
		return err
	}
	if err := applyOverlayTools(ctx, d, opts, res); err != nil {
		auditFail(cfg, "apply-overlay-tools", err)
		return err
	}
	if err := applyChart(ctx, cfg, d, opts, res, valueFiles); err != nil {
		return err
	}
	if err := applyOverlayObjects(ctx, d, opts, res); err != nil {
		auditFail(cfg, "apply-overlay", err)
		return err
	}
	if err := pruneOverlayTools(ctx, d, opts); err != nil {
		auditFail(cfg, "prune-overlay-tools", err)
		return err
	}
	res.OverlayApplied = deliversOverlay(opts)
	return nil
}

// stageValueFiles places the overlay value files, then Forge's own
// installation-fact values, on the host for Helm, in apply order.
func stageValueFiles(ctx context.Context, cfg *config.Cluster, o overlayer.Overlayer, opts ApplyOpts) ([]string, error) {
	var files []overlayer.File
	add := func(prefix string, values []overlay.ValueFile) {
		for _, v := range values {
			files = append(files, overlayer.File{Name: fmt.Sprintf("%02d-%s.yaml", len(files), prefix), Content: v.Content})
		}
	}
	if deliversOverlay(opts) {
		add("overlay", opts.Overlay.ValueFiles)
	}
	add("forge", opts.ExtraValueFiles)
	if len(files) == 0 {
		return nil, nil
	}
	if o == nil {
		return nil, fmt.Errorf("overlay values need an overlayer (internal error)")
	}
	paths, err := o.StageFiles(ctx, overlayStageDir(cfg), files)
	if err != nil {
		return nil, fmt.Errorf("stage overlay values: %w", err)
	}
	return paths, nil
}

// applyOverlayTools writes the tools/client archive as an immutable,
// digest-named ConfigMap and then repoints the pointer ConfigMap at it.
func applyOverlayTools(ctx context.Context, d deployer.Deployer, opts ApplyOpts, res *Result) error {
	if !deliversOverlay(opts) || d == nil {
		return nil
	}
	tools := opts.Overlay.Tools
	namespace, archive, pointer, err := overlay.ToolsManifests(tools, opts.Overlay.Revision)
	if err != nil {
		return err
	}
	for _, m := range []string{namespace, archive, pointer} {
		if err := d.ApplyManifest(ctx, m); err != nil {
			return fmt.Errorf("overlay tools: %w", err)
		}
	}
	res.ToolsDigest = tools.Digest
	return nil
}

// pruneOverlayTools removes superseded tool archives once the chart (which
// waits for the runner to load the current generation) is applied.
func pruneOverlayTools(ctx context.Context, d deployer.Deployer, opts ApplyOpts) error {
	if !reconcilesOverlay(opts) || d == nil {
		return nil
	}
	var keep []deployer.ObjectRef
	if deliversOverlay(opts) {
		keep = []deployer.ObjectRef{
			{Kind: "ConfigMap", Namespace: overlay.ToolsNamespace, Name: overlay.ToolsPointerName},
			{Kind: "ConfigMap", Namespace: overlay.ToolsNamespace, Name: opts.Overlay.Tools.ArchiveName()},
		}
	}
	if _, err := d.PruneOwned(ctx, []string{"configmap"}, overlay.ToolsSelector, keep); err != nil {
		return fmt.Errorf("prune overlay tools: %w", err)
	}
	return nil
}

// applyOverlayObjects applies the overlay's object instances under the overlay
// owner and prunes overlay-owned objects the overlay no longer renders.
// Installation-owned objects are never selected, so they are never touched.
func applyOverlayObjects(ctx context.Context, d deployer.Deployer, opts ApplyOpts, res *Result) error {
	if !reconcilesOverlay(opts) || d == nil {
		return nil
	}
	rendered := opts.Overlay
	if rendered == nil {
		rendered = &overlay.Rendered{}
	}
	if len(rendered.Objects) > 0 {
		if err := d.ApplyOwned(ctx, string(rendered.Manifest), overlay.OwnerLabel, overlay.OwnerOverlay); err != nil {
			return fmt.Errorf("overlay objects: %w", err)
		}
	}
	keep := make([]deployer.ObjectRef, 0, len(rendered.Objects))
	for _, ref := range rendered.Objects {
		keep = append(keep, deployer.ObjectRef{Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name})
	}
	// No kinds: the deployer discovers every resource type, so an overlay
	// object is pruned even when the overlay no longer renders its kind.
	pruned, err := d.PruneOwned(ctx, nil, overlay.ObjectsSelector, keep)
	if err != nil {
		return fmt.Errorf("prune overlay objects: %w", err)
	}
	res.OverlayPruned = pruned
	return nil
}
