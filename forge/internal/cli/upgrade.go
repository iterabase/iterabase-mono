package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/lifecycle"
)

func newUpgradeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade k3s to a new version",
		RunE:  runUpgrade,
	}
	cmd.Flags().String("to", "", "target k3s version (default: version in forge.yaml)")
	return cmd
}

func runUpgrade(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	log := newLogger(cmd)

	t, p, err := connect(context.Background(), cfg)
	if err != nil {
		return err
	}
	defer t.Close()
	defer p.Close()
	cfg = t.Cluster

	to, _ := cmd.Flags().GetString("to")
	log.Info("upgrading", "install", cfg.Metadata.Name, "to", to)
	res, err := lifecycle.Upgrade(context.Background(), cfg, p, p, to, lifecycle.ApplyOpts{})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "upgrade complete")
	fmt.Fprintf(out, "  kubeconfig: %s\n", res.KubeconfigPath)
	fmt.Fprintf(out, "  node ready: %v\n", res.NodeReady)
	if res.HostInotify != nil {
		fmt.Fprintf(out, "  inotify:    %s\n", hostInotifySummary(res.HostInotify))
	}
	return nil
}
