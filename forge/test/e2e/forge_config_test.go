package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// forgeConfigSpec is the shared config fixture for cloud E2E stages. Scenario
// writers vary only the fields relevant to the contract they exercise, so the
// common k3s/host recipe cannot drift across files.
const dataStorageDeviceEnv = "FORGE_E2E_DATA_STORAGE_DEVICES"

var dataStorageDevicesByAddress sync.Map

func rememberDataStorageDevice(address, device string) {
	if address != "" && device != "" {
		dataStorageDevicesByAddress.Store(address, device)
	}
}

type forgeConfigSpec struct {
	Name              string
	Address           string
	SSHUser           string
	SSHKeyPath        string
	SSHHostKey        string
	DataStorageDevice string
	RunLabel          bool
	DualStack         bool
	GPU               bool
	GPUDriverVersion  string
	GPUDriverSHA256   string
	ChartVersion      string
	ChartRepository   string
	ChartRelease      string
	ChartNamespace    string
	OverlayRepo       string
	OverlayRef        string
	Flux              bool
}

func e2eK3sVersion() string { return "v1.34.10+k3s1" }

func dataStorageDevice(spec forgeConfigSpec) string {
	if spec.DataStorageDevice != "" {
		return spec.DataStorageDevice
	}
	if device := os.Getenv(dataStorageDeviceEnv); device != "" {
		return device
	}
	if device, ok := dataStorageDevicesByAddress.Load(spec.Address); ok {
		return device.(string)
	}
	return "/dev/disk/by-id/scsi-forge-e2e-data-storage"
}

func writeForgeConfigSpec(t *testing.T, spec forgeConfigSpec) string {
	t.Helper()
	if spec.SSHUser == "" {
		spec.SSHUser = fixtureSSHUser()
	}
	if spec.SSHHostKey == "" {
		spec.SSHHostKey = strings.TrimSpace(os.Getenv(permanentFixtureHostKeyEnv))
	}
	trustFile := writeForgeTrustFile(t, spec.Address, spec.SSHHostKey)
	var cfg strings.Builder
	fmt.Fprintf(&cfg, `apiVersion: forge.horizonshift.io/v1alpha1
kind: Cluster
metadata:
  name: %s
spec:
  mode: single-node
  dataStorage:
    devices:
`, spec.Name)
	devices := strings.Split(dataStorageDevice(spec), ",")
	for i := range devices {
		devices[i] = strings.TrimSpace(devices[i])
	}
	slices.Sort(devices)
	for _, device := range devices {
		fmt.Fprintf(&cfg, "      - %s\n", device)
	}
	fmt.Fprintf(&cfg, `  hosts:
    - address: %s
      sshUser: %s
      sshKeyPath: %s
      sshTrustFile: %q
`, spec.Address, spec.SSHUser, spec.SSHKeyPath, trustFile)
	cfg.WriteString("      role: control-plane+worker\n")
	if spec.RunLabel {
		fmt.Fprintf(&cfg, `      labels:
        e2e.horizonshift.io/run: %q
`, spec.Name)
	}
	fmt.Fprintf(&cfg, `  k3s:
    version: %s
    clusterCIDR: 10.42.0.0/16
    serviceCIDR: 10.43.0.0/16
    dualStack: %t
`, e2eK3sVersion(), spec.DualStack)
	if spec.DualStack {
		cfg.WriteString(`    clusterCIDRv6: fd42::/48
    serviceCIDRv6: fd43::/112
`)
	}
	cfg.WriteString("    disable: [traefik, servicelb, local-storage]\n")
	if spec.GPU {
		if spec.GPUDriverVersion == "" {
			spec.GPUDriverVersion = gpuUpgradeBaselineDriver
		}
		if spec.GPUDriverSHA256 == "" {
			spec.GPUDriverSHA256 = e2eGPUDriverSHA256(t, spec.GPUDriverVersion)
		}
		fmt.Fprintf(&cfg, `  gpu:
    enabled: true
    driver:
      version: %q
      sha256: %s
`, spec.GPUDriverVersion, spec.GPUDriverSHA256)
	}
	if spec.ChartVersion != "" {
		fmt.Fprintf(&cfg, `  chart:
    version: %s
`, spec.ChartVersion)
		if spec.ChartRepository != "" {
			fmt.Fprintf(&cfg, "    repository: %s\n", spec.ChartRepository)
		}
		if spec.ChartRelease != "" {
			fmt.Fprintf(&cfg, "    release: %s\n", spec.ChartRelease)
		}
		if spec.ChartNamespace != "" {
			fmt.Fprintf(&cfg, "    namespace: %s\n", spec.ChartNamespace)
		}
	}
	if spec.OverlayRepo != "" {
		fmt.Fprintf(&cfg, `  overlay:
    repo: %s
    ref: %s
`, spec.OverlayRepo, spec.OverlayRef)
	}
	if spec.Flux {
		cfg.WriteString(`  flux:
    enabled: true
    version: "v2.4.0"
`)
	}

	path := filepath.Join(t.TempDir(), "forge.yaml")
	if err := os.WriteFile(path, []byte(cfg.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// writeForgeTrustFile materializes the non-Git host trust file consumed by
// spec.hosts[].sshTrustFile. The founder-verified key comes from the fixture
// environment and never enters Git, command arguments, or logs; an empty pin
// leaves a comments-only file so the built Forge binary fails closed instead of
// falling back to insecure verification.
func writeForgeTrustFile(t *testing.T, address, pin string) string {
	t.Helper()
	content := "# founder-verified host key is supplied by the fixture environment\n"
	if strings.TrimSpace(pin) != "" {
		content += address + " " + strings.TrimSpace(pin) + "\n"
	}
	path := filepath.Join(t.TempDir(), "forge-host-trust")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write host trust file: %v", err)
	}
	return path
}
