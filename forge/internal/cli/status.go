package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/lifecycle"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/provisioner"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show cluster health and drift against forge.yaml",
		RunE:  runStatus,
	}
}

//nolint:gocyclo // bounded status sections intentionally retain independent evidence/failure output.
func runStatus(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}

	t, p, err := connect(context.Background(), cfg)
	if err != nil {
		return err
	}
	defer t.Close()
	defer p.Close()
	cfg = t.Cluster

	ctx := context.Background()
	plan, err := lifecycle.Plan(ctx, cfg, p, nil)
	if err != nil {
		return err
	}
	ready, _ := p.NodeReady(ctx)

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "install:    %s\n", cfg.Metadata.Name)
	fmt.Fprintf(out, "installed:  %v\n", plan.Preflight.Installed)
	fmt.Fprintf(out, "action:     %s\n", plan.Action)
	if plan.Reason != "" {
		fmt.Fprintf(out, "reason:     %s\n", plan.Reason)
	}
	if plan.HaveVersion != "" {
		fmt.Fprintf(out, "have:       %s\n", plan.HaveVersion)
	}
	fmt.Fprintf(out, "want:       %s\n", plan.WantVersion)
	fmt.Fprintf(out, "node ready: %v\n", ready)
	printDataStorageStatus(out, plan.DataStorage)
	printHostInotifyStatus(out, plan.HostInotify)
	if cfg.Spec.Chart.Version != "" {
		cs, _ := p.Status(ctx, cfg.Spec.Chart.Release, cfg.Spec.Chart.Namespace)
		if cs != nil && cs.Installed {
			fmt.Fprintf(out, "chart:      %s (%s)\n", cs.Version, cs.Status)
		} else {
			fmt.Fprintf(out, "chart:      not installed (want %s)\n", cfg.Spec.Chart.Version)
		}
		if ss, _ := p.Status(ctx, cfg.Spec.Chart.Release+"-lvm-storage", cfg.Spec.Chart.Namespace); ss != nil && ss.Installed {
			fmt.Fprintf(out, "LVM storage substrate: %s (%s)\n", ss.Version, ss.Status)
			readiness, readinessErr := p.WaitForLVMStorageReady(ctx, cfg.Spec.Chart.Namespace, plan.DataStorage)
			if readinessErr != nil {
				return readinessErr
			}
			fmt.Fprintf(out, "LVM storage readiness: ready=%v node=%s vg=%s uuid=%s free=%d/%d lvs=%d pvs=%d\n", readiness.Ready, readiness.NodeName, readiness.VGName, readiness.VGUUID, readiness.FreeBytes, readiness.SizeBytes, readiness.LVCount, readiness.PVCount)
		} else {
			fmt.Fprintf(out, "LVM storage substrate: not installed (want %s)\n", cfg.Spec.Chart.Version)
		}
	}
	if cfg.Spec.GPU.Enabled {
		g := cfg.Spec.GPU.Operator
		fmt.Fprintln(out, "gpu:")
		fmt.Fprintf(out, "  enabled:       true\n")
		fmt.Fprintf(out, "  pci present:   %v\n", plan.Preflight.HasNVIDIAGPU)
		fmt.Fprintf(out, "  headers:       %s\n", boolLabel(plan.Preflight.KernelHeadersInstalled, "installed", "absent"))
		fmt.Fprintf(out, "  dkms:          %s\n", boolLabel(plan.Preflight.HasDKMS, "installed", "absent"))
		fmt.Fprintf(out, "  gcc:           %s\n", boolLabel(plan.Preflight.HasGCC, "installed", "absent"))
		fmt.Fprintf(out, "  make:          %s\n", boolLabel(plan.Preflight.HasMake, "installed", "absent"))
		if cs, _ := p.Status(ctx, g.Release, g.Namespace); cs != nil && cs.Installed {
			fmt.Fprintf(out, "  operator:      %s (%s)\n", g.Version, cs.Status)
		} else {
			fmt.Fprintf(out, "  operator:      not installed (want %s)\n", g.Version)
		}
		if v := cfg.Spec.GPU.Driver.Version; v != "" {
			fmt.Fprintf(out, "  driver:        %s\n", v)
		} else {
			fmt.Fprintf(out, "  driver:        (chart default)\n")
		}
		readiness, readinessErr := p.ReadGPUReadiness(ctx, cfg.Spec.GPU.Driver.Version)
		if readinessErr != nil {
			fmt.Fprintln(out, "  clusterpolicy: unavailable")
			fmt.Fprintf(out, "  evidence:      %v\n", readinessErr)
		} else {
			fmt.Fprintf(out, "  clusterpolicy: %s\n", boolLabel(readiness.Ready, "ready", "notReady"))
			fmt.Fprintf(out, "  evidence:      %s\n", readiness)
		}
	}
	return nil
}

func printDataStorageStatus(out io.Writer, storage *provisioner.DataStorageState) {
	if storage == nil {
		return
	}
	fmt.Fprintf(out, "data storage: %s (%s)\n", storage.VGName, storage.State)
	fmt.Fprintf(out, "  VG UUID: %s\n", storage.VGUUID)
	fmt.Fprintf(out, "  capacity: %d total, %d free\n", storage.SizeBytes, storage.FreeBytes)
	for _, device := range storage.Devices {
		fmt.Fprintf(out, "  device: %s resolved=%s model=%s serial=%s transport=%s size=%d pv=%s\n", device.Path, device.Resolved, device.Model, device.Serial, device.Transport, device.SizeBytes, device.PVUUID)
	}
}

func boolLabel(b bool, on, off string) string {
	if b {
		return on
	}
	return off
}
