package cli

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/spf13/cobra"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
)

// Overlays repository layout (DES-HOR-593-03).
const (
	customersDir     = "customers"
	templateName     = "_template"
	templateNameMark = "__NAME__"
)

var customerNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$`)

func newOverlayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "overlay",
		Short: "Work with the customer overlays repository",
	}
	newCmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Scaffold customers/<name> from customers/_template",
		Long: `new copies customers/_template in the overlays repository to customers/<name>,
replacing every __NAME__ with the customer name. Review and commit the result,
then provision it with 'forge init --provider aws --config customers/<name>/forge.yaml'.`,
		Args: cobra.ExactArgs(1),
		RunE: runOverlayNew,
	}
	newCmd.Flags().String("root", ".", "overlays repository root")
	cmd.AddCommand(newCmd)
	return cmd
}

func runOverlayNew(cmd *cobra.Command, args []string) error {
	name := args[0]
	if !customerNameRe.MatchString(name) || name == templateName {
		return fmt.Errorf("customer name %q must be a lowercase DNS label of at most 32 characters", name)
	}
	root, _ := cmd.Flags().GetString("root")
	src := filepath.Join(root, customersDir, templateName)
	dst := filepath.Join(root, customersDir, name)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s already exists", dst)
	}
	if info, err := os.Stat(src); err != nil || !info.IsDir() {
		return fmt.Errorf("template %s not found; run from the overlays repository or pass --root", src)
	}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("template entry %s is not a regular file", path)
		}
		data, err := os.ReadFile(path) //nolint:gosec // template file inside the overlays repository the operator named
		if err != nil {
			return err
		}
		return os.WriteFile(target, bytes.ReplaceAll(data, []byte(templateNameMark), []byte(name)), 0o644) //nolint:gosec // overlay files are reviewed repository content, not secrets
	})
	if err != nil {
		_ = os.RemoveAll(dst)
		return err
	}
	if _, err := config.Load(filepath.Join(dst, "forge.yaml")); err != nil {
		_ = os.RemoveAll(dst)
		return fmt.Errorf("scaffolded forge.yaml is invalid: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "created %s\n", dst)
	return nil
}

func newFleetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Operate every customer in the overlays repository",
	}
	apply := &cobra.Command{
		Use:   "apply",
		Short: "Run forge apply for every customers/<name>/forge.yaml",
		Long: `apply runs 'forge apply' for each customer in the overlays repository, in name
order, so one change to a shared base or component reaches every installation
(DES-HOR-593-04). It continues past a failing customer and fails at the end,
naming every customer that failed.`,
		RunE: runFleetApply,
	}
	apply.Flags().String("root", ".", "overlays repository root")
	apply.Flags().StringSlice("only", nil, "limit to these customer names")
	apply.Flags().Bool("dry-run", false, "print each customer's plan without mutating")
	cmd.AddCommand(apply)
	return cmd
}

func runFleetApply(cmd *cobra.Command, _ []string) error {
	root, _ := cmd.Flags().GetString("root")
	only, _ := cmd.Flags().GetStringSlice("only")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	configs, err := fleetConfigs(root, only)
	if err != nil {
		return err
	}
	var failed []string
	for _, path := range configs {
		fmt.Fprintf(cmd.OutOrStdout(), "== %s\n", path)
		cfg, err := config.Load(path)
		if err == nil {
			err = applyInstallation(context.Background(), cmd, cfg, applyOptsFromFlags(cmd), dryRun)
		}
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", path, err)
			failed = append(failed, path)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("fleet apply failed for %d of %d customers: %v", len(failed), len(configs), failed)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "fleet apply complete: %d customers\n", len(configs))
	return nil
}

// fleetConfigs lists customers/<name>/forge.yaml, excluding the template.
func fleetConfigs(root string, only []string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, customersDir))
	if err != nil {
		return nil, fmt.Errorf("overlays repository %s: %w", root, err)
	}
	wanted := map[string]bool{}
	for _, name := range only {
		wanted[name] = true
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == templateName {
			continue
		}
		if len(wanted) > 0 && !wanted[e.Name()] {
			continue
		}
		delete(wanted, e.Name())
		path := filepath.Join(root, customersDir, e.Name(), "forge.yaml")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		out = append(out, path)
	}
	if len(wanted) > 0 {
		missing := make([]string, 0, len(wanted))
		for name := range wanted {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("no such customers: %v", missing)
	}
	sort.Strings(out)
	return out, nil
}
