package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/lifecycle"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/overlay"
)

func newApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Provision or reconcile the cluster from forge.yaml",
		Long: `apply bootstraps k3s on the target host(s) and converges the cluster to the
state declared in forge.yaml and its overlay. Safe to re-run: it reconciles from
reality and skips work already done. Immutable field changes are refused (use
'forge destroy' then 'forge apply'); version changes are routed to 'forge
upgrade'. The overlay is rendered on this machine and only its result is sent
to the host.`,
		RunE: runApply,
	}
	cmd.Flags().Bool("dry-run", false, "preflight and print the plan without mutating")
	cmd.Flags().Bool("skip-chart", false, "skip the platform chart phase (k3s only)")
	cmd.Flags().Bool("skip-gpu", false, "skip the GPU readiness phase")
	cmd.Flags().Bool("skip-overlay", false, "skip the overlay phase (values, tools and object instances)")
	cmd.Flags().Bool("skip-secrets", false, "skip the secret-sync phase (materialize declared Secrets)")
	cmd.Flags().String("kubeconfig-out", "", "path to write the fetched kubeconfig (default ~/.forge/<install>/kubeconfig.yaml)")
	return cmd
}

func runApply(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	return applyInstallation(context.Background(), cmd, cfg, applyOptsFromFlags(cmd), dryRun)
}

// applyOptsFromFlags reads the apply phase flags a command defines; commands
// without them (forge init --provider aws) get a full apply.
func applyOptsFromFlags(cmd *cobra.Command) lifecycle.ApplyOpts {
	var opts lifecycle.ApplyOpts
	flags := cmd.Flags()
	if flags.Lookup("kubeconfig-out") != nil {
		opts.KubeconfigOut, _ = flags.GetString("kubeconfig-out")
	}
	for name, dst := range map[string]*bool{
		"skip-chart":   &opts.SkipChart,
		"skip-gpu":     &opts.SkipGPU,
		"skip-overlay": &opts.SkipOverlay,
		"skip-secrets": &opts.SkipSecrets,
	} {
		if flags.Lookup(name) != nil {
			*dst, _ = flags.GetBool(name)
		}
	}
	return opts
}

// applyInstallation renders the overlay locally, resolves the host, and runs
// the lifecycle apply (or prints the plan).
func applyInstallation(ctx context.Context, cmd *cobra.Command, cfg *config.Cluster, opts lifecycle.ApplyOpts, dryRun bool) error {
	log := newLogger(cmd)
	if cfg.Spec.Overlay.Path != "" && !opts.SkipOverlay {
		rendered, err := overlay.Render(cfg.Dir, cfg.Spec.Overlay)
		if err != nil {
			return err
		}
		opts.Overlay = rendered
	}

	t, p, err := connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer t.Close()
	defer p.Close()
	opts.ExtraValueFiles = t.ValueFiles
	opts.ExtraManifests = t.Manifests

	if dryRun {
		plan, err := lifecycle.Plan(ctx, t.Cluster, p, opts.Overlay)
		if err != nil {
			return err
		}
		printPlan(cmd, plan)
		return nil
	}

	if t.Prepare != nil {
		if err := t.Prepare(ctx, p); err != nil {
			return err
		}
	}
	opts.SecretResolver = cliSecretResolver{interactive: isTTY(), prompter: termPasswordPrompter{}, out: os.Stderr}
	log.Info("applying", "install", t.Cluster.Metadata.Name)
	res, err := lifecycle.Apply(ctx, t.Cluster, p, p, p, opts)
	if err != nil {
		return err
	}
	printApplyResult(cmd.OutOrStdout(), t.Cluster, res)
	return nil
}

// printApplyResult writes the apply outcome summary.
func printApplyResult(out io.Writer, cfg *config.Cluster, res *lifecycle.Result) {
	fmt.Fprintln(out, "apply complete")
	fmt.Fprintf(out, "  action:     %s\n", res.Plan.Action)
	fmt.Fprintf(out, "  kubeconfig: %s\n", res.KubeconfigPath)
	fmt.Fprintf(out, "  node ready: %v\n", res.NodeReady)
	if res.DataStorage != nil {
		fmt.Fprintf(out, "  data storage: %s (%s, devices=%d, uuid=%s, free=%d/%d)\n", res.DataStorage.VGName, res.DataStorage.State, len(res.DataStorage.Devices), res.DataStorage.VGUUID, res.DataStorage.FreeBytes, res.DataStorage.SizeBytes)
	}
	if res.HostInotify != nil {
		fmt.Fprintf(out, "  inotify:    %s\n", hostInotifySummary(res.HostInotify))
	}
	if res.LVMStorageReady != nil {
		fmt.Fprintf(out, "  LVM storage ready: %v (node=%s, vg=%s, lvs=%d)\n", res.LVMStorageReady.Ready, res.LVMStorageReady.NodeName, res.LVMStorageReady.VGName, res.LVMStorageReady.LVCount)
	}
	if cfg.Spec.Chart.Version != "" {
		fmt.Fprintf(out, "  chart:      %s\n", cfg.Spec.Chart.Version)
		fmt.Fprintf(out, "  certificate substrate applied: %v\n", res.CertificateSubstrateApplied)
		fmt.Fprintf(out, "  LVM storage substrate applied: %v\n", res.LVMStorageSubstrateApplied)
		fmt.Fprintf(out, "  chart applied: %v\n", res.ChartApplied)
	}
	if cfg.Spec.GPU.Enabled {
		fmt.Fprintf(out, "  gpu operator: %v\n", res.GPUOperatorApplied)
		fmt.Fprintf(out, "  gpu ready: %v\n", res.GPUReady)
		if v := res.GPUDriverVersion; v != "" {
			fmt.Fprintf(out, "  gpu driver: %s\n", v)
		} else {
			fmt.Fprintf(out, "  gpu driver: (chart default)\n")
		}
	}
	if res.OverlayApplied {
		fmt.Fprintf(out, "  overlay:        %s\n", cfg.Spec.Overlay.Path)
		fmt.Fprintf(out, "  overlay revision: %s\n", res.OverlayRevision)
		fmt.Fprintf(out, "  overlay tools:  %s\n", res.ToolsDigest)
		for _, pruned := range res.OverlayPruned {
			fmt.Fprintf(out, "  overlay pruned: %s\n", pruned)
		}
	}
	if res.SecretsApplied {
		fmt.Fprintf(out, "  secrets applied: true\n")
	}
}

func printPlan(cmd *cobra.Command, plan *lifecycle.ReconcilePlan) {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "plan:")
	fmt.Fprintf(out, "  installed: %v\n", plan.Preflight.Installed)
	fmt.Fprintf(out, "  action:    %s\n", plan.Action)
	if plan.Reason != "" {
		fmt.Fprintf(out, "  reason:    %s\n", plan.Reason)
	}
	if len(plan.ImmutableDiff) > 0 {
		fmt.Fprintf(out, "  drift:     %s\n", plan.ImmutableDiff)
	}
	if plan.HaveVersion != "" {
		fmt.Fprintf(out, "  have:      %s\n", plan.HaveVersion)
	}
	fmt.Fprintf(out, "  want:      %s\n", plan.WantVersion)
	if plan.DataStorage != nil {
		fmt.Fprintf(out, "  data storage: %s (%s, devices=%d, uuid=%s, free=%d/%d)\n", plan.DataStorage.VGName, plan.DataStorage.State, len(plan.DataStorage.Devices), plan.DataStorage.VGUUID, plan.DataStorage.FreeBytes, plan.DataStorage.SizeBytes)
		for _, device := range plan.DataStorage.Devices {
			fmt.Fprintf(out, "    %s (model=%s serial=%s transport=%s size=%d pv=%s)\n", device.Path, device.Model, device.Serial, device.Transport, device.SizeBytes, device.PVUUID)
		}
	}
	if plan.HostInotify != nil {
		fmt.Fprintf(out, "  inotify:   %s\n", hostInotifySummary(plan.HostInotify))
	}
	if plan.ChartVersion != "" {
		fmt.Fprintf(out, "  chart:     %s\n", plan.ChartVersion)
	}
	if plan.GPUEnabled {
		fmt.Fprintf(out, "  gpu:       %s (enabled)\n", plan.GPUOperatorVersion)
		if v := plan.GPUDriverVersion; v != "" {
			fmt.Fprintf(out, "  gpu driver: %s\n", v)
		} else {
			fmt.Fprintf(out, "  gpu driver: (chart default)\n")
		}
		fmt.Fprintf(out, "  gpu pci:   %v\n", plan.Preflight.HasNVIDIAGPU)
		fmt.Fprintf(out, "  gpu build: headers=%v dkms=%v gcc=%v make=%v\n",
			plan.Preflight.KernelHeadersInstalled, plan.Preflight.HasDKMS,
			plan.Preflight.HasGCC, plan.Preflight.HasMake)
	}
	if plan.OverlayRevision != "" {
		fmt.Fprintf(out, "  overlay:  %s\n", plan.OverlayRevision)
	}
}
