package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/artifacts"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/lifecycle"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/sshprovisioner"
)

func newDestroyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Uninstall k3s and remove local artifacts",
		RunE:  runDestroy,
	}
	cmd.Flags().Bool("purge-data-storage", false, "destructively remove the empty receipt-matching iterabase-data VG and PVs after destroy")
	cmd.Flags().Bool("reboot", false, "reboot the host after successful destroy and any requested purge")
	cmd.Flags().Bool("yes", false, "confirm the requested destroy, purge, and reboot without prompting")
	return cmd
}

func runDestroy(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	if cfg.Spec.Provider.AWS != nil {
		return runAWSDestroy(cmd, cfg)
	}
	host := cfg.Spec.Hosts[0]

	purgeDataStorage, _ := cmd.Flags().GetBool("purge-data-storage")
	reboot, _ := cmd.Flags().GetBool("reboot")
	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		message := fmt.Sprintf("Uninstall k3s on %s and remove local artifacts?", host.Address)
		if purgeDataStorage {
			message = fmt.Sprintf("DESTROY k3s and PURGE the empty receipt-matching iterabase-data VG and PVs on %s? This removes their LVM identities.", host.Address)
		}
		if reboot {
			message += " The host will reboot after successful cleanup."
		}
		if !confirm(cmd, message) {
			return fmt.Errorf("aborted")
		}
	}

	p, err := sshprovisioner.New(host)
	if err != nil {
		return err
	}
	defer p.Close()

	ctx := context.Background()
	if err := lifecycle.DestroyWithOptions(ctx, cfg, p, p, p, lifecycle.DestroyOpts{
		PurgeDataStorage: purgeDataStorage,
		Reboot:           reboot,
	}); err != nil {
		return err
	}
	if dir, err := artifacts.Dir(cfg.Metadata.Name); err == nil {
		_ = os.RemoveAll(dir)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "destroyed")
	return nil
}

func confirm(cmd *cobra.Command, msg string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", msg)
	in := bufio.NewReader(cmd.InOrStdin())
	line, _ := in.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}
