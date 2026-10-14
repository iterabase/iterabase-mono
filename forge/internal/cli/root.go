// Package cli implements the forge command-line interface (Cobra).
package cli

import "github.com/spf13/cobra"

// NewRootCmd builds the root forge command with all subcommands registered.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "forge",
		Short: "Provision and manage single-node k3s clusters",
		Long: `forge is the installer for the Horizonshift / Iterabase platform.

It bootstraps a production-ready single-node k3s cluster on a VM or host over
SSH, with dual-stack networking and prod-ready defaults, and applies an exact
platform release plus the customer's overlay, rendered on this machine. With
provider.aws it also provisions and tears down dedicated environments in the
hosted AWS account, reached with SSH over SSM. It does not provision bare
metal, Proxmox, or network appliances.`,
		SilenceUsage: true,
	}

	root.PersistentFlags().String("config", "forge.yaml", "path to forge.yaml config")
	root.PersistentFlags().String("log-level", "info", "log level (debug|info|warn|error)")
	root.PersistentFlags().String("log-format", "text", "log format (text|json)")

	root.AddCommand(
		newInitCmd(),
		newApplyCmd(),
		newUpgradeCmd(),
		newDestroyCmd(),
		newKubeconfigCmd(),
		newStatusCmd(),
		newVersionCmd(),
		newTunnelCmd(),
		newOverlayCmd(),
		newFleetCmd(),
	)

	return root
}

// Execute runs the root command.
func Execute() error {
	return NewRootCmd().Execute()
}
