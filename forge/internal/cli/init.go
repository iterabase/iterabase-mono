package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/provisioner"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/sshprovisioner"
)

const dataStorageDevicesEnv = "FORGE_DATA_STORAGE_DEVICES"

var discoverDataStorageDevices = func(ctx context.Context, host config.Host) ([]provisioner.DataStorageDevice, error) {
	p, err := sshprovisioner.New(host)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	return p.ListDataStorageDevices(ctx)
}

func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate a forge.yaml config",
		Long:  "Generate a forge.yaml substrate config, interactively or from flags (--non-interactive).",
		RunE:  runInit,
	}
	cmd.Flags().Bool("non-interactive", false, "generate without prompts using flags")
	cmd.Flags().String("path", "forge.yaml", "output path for the generated config")
	cmd.Flags().String("name", "opo1", "install name")
	cmd.Flags().String("address", "", "target host address")
	cmd.Flags().String("ssh-user", "forge", "SSH user (must have passwordless sudo)")
	cmd.Flags().String("ssh-key", "~/.ssh/forge_ed25519", "SSH private key path")
	cmd.Flags().String("ssh-trust-file", "", "non-Git known_hosts-format trust file holding the founder-verified host key (default ~/.forge/trust/<name>/<address>)")
	cmd.Flags().String("ssh-host-key", "", "founder-verified OpenSSH host public key; written to the trust file, never to forge.yaml")
	cmd.Flags().String("k3s-version", "v1.34.10+k3s1", "K3s version (full tag, e.g. v1.34.10+k3s1)")
	cmd.Flags().Bool("dual-stack", true, "enable dual-stack IPv4+IPv6")
	cmd.Flags().String("overlay", "", "overlay repo URL (client fork; https:// or file://; empty => no overlay)")
	cmd.Flags().String("overlay-ref", "master", "overlay ref (branch or tag)")
	cmd.Flags().StringArray("data-storage-device", nil, "stable blank whole disk /dev/disk/by-id/... selected for iterabase-data (repeat for multiple disks)")
	cmd.Flags().Bool("overwrite", false, "overwrite an existing config file (does not authorize disk changes)")
	return cmd
}

func runInit(cmd *cobra.Command, _ []string) error {
	nonInteractive, _ := cmd.Flags().GetBool("non-interactive")
	path, _ := cmd.Flags().GetString("path")
	overwrite, _ := cmd.Flags().GetBool("overwrite")

	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; use --overwrite to replace the config file", path)
		}
	}

	name, _ := cmd.Flags().GetString("name")
	address, _ := cmd.Flags().GetString("address")
	sshUser, _ := cmd.Flags().GetString("ssh-user")
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	sshHostKey, _ := cmd.Flags().GetString("ssh-host-key")
	sshTrustFile, _ := cmd.Flags().GetString("ssh-trust-file")
	k3sVersion, _ := cmd.Flags().GetString("k3s-version")
	dualStack, _ := cmd.Flags().GetBool("dual-stack")
	overlay, _ := cmd.Flags().GetString("overlay")
	overlayRef, _ := cmd.Flags().GetString("overlay-ref")
	flagDevices, _ := cmd.Flags().GetStringArray("data-storage-device")
	envDevices := strings.TrimSpace(os.Getenv(dataStorageDevicesEnv))
	dataDevices, err := resolveDataStorageSources(flagDevices, envDevices)
	if err != nil {
		return err
	}

	in := bufio.NewReader(cmd.InOrStdin())
	if !nonInteractive {
		name = prompt(in, "Install name", name)
		address = prompt(in, "Target host address", address)
		sshUser = prompt(in, "SSH user", sshUser)
		sshKey = prompt(in, "SSH key path", sshKey)
		sshTrustFile = prompt(in, "SSH host trust file (non-Git known_hosts file)", sshTrustFile)
		sshHostKey = prompt(in, "Founder-verified SSH host key (written to the trust file)", sshHostKey)
		k3sVersion = prompt(in, "K3s version", k3sVersion)
		overlay = prompt(in, "Overlay repo URL (optional)", overlay)
	}
	if address == "" {
		return fmt.Errorf("address is required")
	}
	sshTrustFile, err = resolveInitHostTrust(name, address, sshTrustFile, sshHostKey)
	if err != nil {
		return err
	}

	host := config.Host{
		Address: address, SSHUser: sshUser, SSHKeyPath: sshKey, SSHTrustFile: sshTrustFile,
		Role: config.RoleControlPlaneWorker, Labels: map[string]string{}, Taints: []config.Taint{},
	}
	dataDevices, err = resolveInitDataStorage(in, cmd.ErrOrStderr(), host, nonInteractive, dataDevices)
	if err != nil {
		return err
	}

	cfg := &config.Cluster{
		APIVersion: config.APIVersion,
		Kind:       config.Kind,
		Metadata:   config.Metadata{Name: name},
		Spec: config.Spec{
			Mode:        config.ModeSingleNode,
			Hosts:       []config.Host{host},
			DataStorage: config.DataStorage{Devices: dataDevices},
			K3s: config.K3s{
				Version:       k3sVersion,
				ClusterCIDR:   "10.42.0.0/16",
				ServiceCIDR:   "10.43.0.0/16",
				DualStack:     dualStack,
				ClusterCIDRv6: "fd42::/48",
				ServiceCIDRv6: "fd43::/112",
				Disable:       []string{"traefik", "servicelb", "local-storage"},
			},
		},
	}
	if overlay != "" {
		if overlayRef == "" {
			overlayRef = config.DefaultOverlayRef
		}
		cfg.Spec.Overlay = config.Overlay{Repo: overlay, Ref: overlayRef}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
	fmt.Fprintf(cmd.OutOrStdout(), "SSH host trust file: %s\n", host.SSHTrustFile)
	return nil
}

// resolveInitHostTrust enrolls the founder-verified key or validates the
// existing non-Git trust file, then returns the path recorded in forge.yaml.
func resolveInitHostTrust(name, address, sshTrustFile, sshHostKey string) (string, error) {
	sshTrustFile = strings.TrimSpace(sshTrustFile)
	if sshTrustFile == "" {
		sshTrustFile = defaultSSHTrustFile(name, address)
	}
	if strings.TrimSpace(sshHostKey) == "" {
		if err := sshprovisioner.ValidateHostTrust(address, sshTrustFile); err != nil {
			return "", fmt.Errorf("no SSH host key was supplied and no valid trust file exists: %w", err)
		}
		return sshTrustFile, nil
	}
	if err := sshprovisioner.EnrollHostTrust(address, sshTrustFile, sshHostKey); err != nil {
		return "", err
	}
	return sshTrustFile, nil
}

// defaultSSHTrustFile is the non-Git trust-file path forge init uses when the
// operator does not select one.
func defaultSSHTrustFile(name, address string) string {
	return filepath.Join("~", ".forge", "trust", name, address)
}

func resolveInitDataStorage(in *bufio.Reader, out io.Writer, host config.Host, nonInteractive bool, selected []string) ([]string, error) {
	if len(selected) == 0 && nonInteractive {
		return nil, fmt.Errorf("at least one --data-storage-device or %s entry is required in non-interactive mode", dataStorageDevicesEnv)
	}
	if nonInteractive {
		return selected, nil
	}
	devices, err := discoverDataStorageDevices(context.Background(), host)
	if err != nil {
		return nil, fmt.Errorf("discover blank data-storage disks on %s: %w", host.Address, err)
	}
	if len(selected) == 0 {
		return selectDataStorageDevices(in, out, devices)
	}
	discovered := make(map[string]struct{}, len(devices))
	for _, device := range devices {
		discovered[device.Path] = struct{}{}
	}
	for _, device := range selected {
		if _, ok := discovered[device]; !ok {
			return nil, fmt.Errorf("selected data-storage device %q is not a discovered stable non-removable blank whole disk", device)
		}
	}
	printDataStorageAuthorization(out, devices)
	fmt.Fprintf(out, "Preserving explicit data-storage set: %s\n", strings.Join(selected, ", "))
	return selected, nil
}

func resolveDataStorageSources(flagDevices []string, envValue string) ([]string, error) {
	flags, err := canonicalDataStorageDevices(flagDevices)
	if err != nil {
		return nil, fmt.Errorf("--data-storage-device: %w", err)
	}
	var environment []string
	if envValue != "" {
		environment, err = canonicalDataStorageDevices(strings.Split(envValue, ","))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dataStorageDevicesEnv, err)
		}
	}
	if len(flags) > 0 && len(environment) > 0 && strings.Join(flags, "\x00") != strings.Join(environment, "\x00") {
		return nil, fmt.Errorf("conflicting data-storage device sets: --data-storage-device=%q and %s=%q", flags, dataStorageDevicesEnv, environment)
	}
	if len(flags) > 0 {
		return flags, nil
	}
	return environment, nil
}

func canonicalDataStorageDevices(devices []string) ([]string, error) {
	if len(devices) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(devices))
	seen := make(map[string]struct{}, len(devices))
	for _, raw := range devices {
		device := strings.TrimSpace(raw)
		if device == "" {
			return nil, fmt.Errorf("device entries must be non-empty")
		}
		if strings.Contains(device, ",") {
			return nil, fmt.Errorf("device %q must be one path; repeat the flag instead of using commas", device)
		}
		if _, ok := seen[device]; ok {
			return nil, fmt.Errorf("duplicate device %q", device)
		}
		seen[device] = struct{}{}
		out = append(out, device)
	}
	sort.Strings(out)
	return out, nil
}

func selectDataStorageDevices(in *bufio.Reader, out io.Writer, devices []provisioner.DataStorageDevice) ([]string, error) {
	if len(devices) == 0 {
		return nil, fmt.Errorf("no stable non-removable blank whole disks were discovered; Forge never falls back to the root disk")
	}
	printDataStorageAuthorization(out, devices)
	choice := prompt(in, "Data disk numbers (comma-separated)", "")
	parts := strings.Split(choice, ",")
	selected := make([]string, 0, len(parts))
	seenIndexes := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		index, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || index < 1 || index > len(devices) {
			return nil, fmt.Errorf("data disk selection %q is invalid; choose one or more displayed numbers", choice)
		}
		if _, exists := seenIndexes[index]; exists {
			return nil, fmt.Errorf("data disk selection %q contains duplicate number %d", choice, index)
		}
		seenIndexes[index] = struct{}{}
		selected = append(selected, devices[index-1].Path)
	}
	return canonicalDataStorageDevices(selected)
}

func printDataStorageAuthorization(out io.Writer, devices []provisioner.DataStorageDevice) {
	fmt.Fprintln(out, "Select one or more blank whole data disks for the fixed thick iterabase-data LVM volume group.")
	fmt.Fprintln(out, "Forge will create only receipt-bound LVM physical volumes and the iterabase-data VG after fail-closed complete-set checks.")
	fmt.Fprintln(out, "This set selection is the sole first-write authorization; Forge never creates platform filesystems or falls back to the root disk.")
	for i, device := range devices {
		fmt.Fprintf(out, "  %d) %s  model=%q serial=%q transport=%q size=%s\n", i+1, device.Path, device.Model, device.Serial, displayDeviceTransport(device.Transport), formatDeviceSize(device.SizeBytes))
	}
}

func displayDeviceTransport(transport string) string {
	transport = strings.ToLower(strings.TrimSpace(transport))
	if transport == "" {
		return "unknown"
	}
	return transport
}

func formatDeviceSize(size uint64) string {
	const gib = uint64(1024 * 1024 * 1024)
	if size >= gib {
		return fmt.Sprintf("%.1f GiB", float64(size)/float64(gib))
	}
	return fmt.Sprintf("%d bytes", size)
}

func prompt(in *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}
