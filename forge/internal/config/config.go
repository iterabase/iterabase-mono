// Package config defines the forge.yaml substrate config schema and loader.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Supported values.
const (
	APIVersion = "forge.horizonshift.io/v1alpha1"
	Kind       = "Cluster"

	ModeSingleNode    = "single-node"
	ModeHA            = "ha" // reserved, not yet supported
	ModeBYOKubeconfig = "byokubeconfig"

	RoleControlPlaneWorker = "control-plane+worker"
	RoleControlPlane       = "control-plane"
	RoleWorker             = "worker"
)

var (
	nameRe                 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dataStoragePartitionRe = regexp.MustCompile(`-part[0-9]+$`)
	contentSHA256Re        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Cluster is the top-level forge.yaml document.
type Cluster struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
	// Dir is the directory holding forge.yaml. Relative overlay paths resolve
	// against it. Load sets it; it is never read from or written to YAML.
	Dir string `yaml:"-"`
}

// Metadata identifies the install.
type Metadata struct {
	Name string `yaml:"name"`
}

// Spec is the cluster substrate specification.
type Spec struct {
	Mode        string      `yaml:"mode"`
	Provider    Provider    `yaml:"provider"`
	Hosts       []Host      `yaml:"hosts"`
	K3s         K3s         `yaml:"k3s"`
	Overlay     Overlay     `yaml:"overlay"`
	Chart       Chart       `yaml:"chart"`
	GPU         GPU         `yaml:"gpu"`
	DataStorage DataStorage `yaml:"dataStorage"`
	// FluxLegacy is the removed Flux GitOps block (DES-HOR-593-04). It is kept
	// only to reject old configs with a migration error.
	FluxLegacy *yaml.Node `yaml:"flux,omitempty"`
}

// DataStorage selects the immutable set of blank whole disks Forge prepares as
// the fixed iterabase-data volume group. The canonical by-id list is the sole
// first-write authorization; forge apply never discovers or substitutes media.
type DataStorage struct {
	Devices []string `yaml:"devices"`
}

func (s DataStorage) validate() error {
	if len(s.Devices) == 0 {
		return fmt.Errorf("dataStorage.devices must contain at least one stable whole-disk identity")
	}
	seen := make(map[string]struct{}, len(s.Devices))
	for i, device := range s.Devices {
		if !strings.HasPrefix(device, "/dev/disk/by-id/") || strings.Contains(strings.TrimPrefix(device, "/dev/disk/by-id/"), "/") {
			return fmt.Errorf("dataStorage.devices[%d] %q must be one stable /dev/disk/by-id/<identity> whole-disk path", i, device)
		}
		if strings.ContainsAny(device, " \t\r\n") || dataStoragePartitionRe.MatchString(device) {
			return fmt.Errorf("dataStorage.devices[%d] %q must identify a whole disk, not a partition or volatile path", i, device)
		}
		if _, ok := seen[device]; ok {
			return fmt.Errorf("dataStorage.devices contains duplicate identity %q", device)
		}
		seen[device] = struct{}{}
		if i > 0 && s.Devices[i-1] > device {
			return fmt.Errorf("dataStorage.devices must use canonical lexical order; order-only drift is refused")
		}
	}
	return nil
}

// Host describes a single target VM/host.
type Host struct {
	Address    string `yaml:"address"`
	SSHUser    string `yaml:"sshUser"`
	SSHKeyPath string `yaml:"sshKeyPath"`
	// SSHTrustFile is the required path to the non-Git known_hosts-format file
	// holding the founder-verified host public key(s). See
	// docs/architecture/forge-ssh-host-trust.md (DES-HOR-521-01).
	SSHTrustFile string `yaml:"sshTrustFile"`
	// SSHHostKey was the removed inline Git pin. It is retained only to reject
	// legacy configs with an actionable migration error; the value is never
	// used, echoed, or written.
	SSHHostKeyLegacy string            `yaml:"sshHostKey,omitempty"`
	Role             string            `yaml:"role"`
	Labels           map[string]string `yaml:"labels"`
	Taints           []Taint           `yaml:"taints"`
}

// Taint is a Kubernetes node taint applied at install time.
type Taint struct {
	Key    string `yaml:"key"`
	Value  string `yaml:"value"`
	Effect string `yaml:"effect"`
}

// K3s holds k3s bootstrap options.
type K3s struct {
	Version       string   `yaml:"version"`
	ClusterCIDR   string   `yaml:"clusterCIDR"`
	ServiceCIDR   string   `yaml:"serviceCIDR"`
	DualStack     bool     `yaml:"dualStack"`
	ClusterCIDRv6 string   `yaml:"clusterCIDRv6"`
	ServiceCIDRv6 string   `yaml:"serviceCIDRv6"`
	Disable       []string `yaml:"disable"`
	ExtraArgs     []string `yaml:"extraArgs"`
}

// Overlay is the customer overlay directory on the operator machine
// (DES-HOR-593-03). Forge renders it locally and ships only the rendered result
// to the installation: Helm value files, the Kustomize-built CRD instances, the
// customer tools archive and the secret declarations. An empty Path skips the
// overlay phase.
type Overlay struct {
	// Path is the overlay directory, relative to forge.yaml (or absolute). It
	// holds kustomization.yaml (CRD instances), the value files, the tool
	// directories and the optional secrets.yaml.
	Path string `yaml:"path"`
	// ValueFiles are Helm value files relative to Path, applied in order (later
	// wins). They may reach shared bases inside the same overlays repository,
	// for example ../../bases/hosted/values.yaml. Default: [values.yaml].
	ValueFiles []string `yaml:"valueFiles"`
	// ToolDirs are customer tool directories relative to Path. Each holds one
	// directory per tool; together they form the tools/client layer the tool
	// runner loads (DES-HOR-593-05). Missing directories are skipped.
	// Default: [tools].
	ToolDirs []string `yaml:"toolDirs"`
	// RepoLegacy and RefLegacy are the removed Git overlay pointer. They are kept
	// only to reject old configs with a migration error.
	RepoLegacy string `yaml:"repo,omitempty"`
	RefLegacy  string `yaml:"ref,omitempty"`
}

// applyDefaults fills the value-file and tool-directory defaults when an
// overlay is configured.
func (o *Overlay) applyDefaults() {
	if o.Path == "" {
		return
	}
	if len(o.ValueFiles) == 0 {
		o.ValueFiles = []string{"values.yaml"}
	}
	if len(o.ToolDirs) == 0 {
		o.ToolDirs = []string{"tools"}
	}
}

func (o Overlay) validate() error {
	if o.RepoLegacy != "" || o.RefLegacy != "" {
		return fmt.Errorf("overlay.repo and overlay.ref are removed: Forge renders a local overlay directory and ships only its result (DES-HOR-593-03); check out the overlay and set overlay.path")
	}
	if o.Path == "" {
		if len(o.ValueFiles) > 0 || len(o.ToolDirs) > 0 {
			return fmt.Errorf("overlay.valueFiles and overlay.toolDirs require overlay.path")
		}
		return nil
	}
	for i, file := range o.ValueFiles {
		if err := validateOverlayRelPath(file); err != nil {
			return fmt.Errorf("overlay.valueFiles[%d]: %w", i, err)
		}
	}
	for i, dir := range o.ToolDirs {
		if err := validateOverlayRelPath(dir); err != nil {
			return fmt.Errorf("overlay.toolDirs[%d]: %w", i, err)
		}
	}
	return nil
}

func validateOverlayRelPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("must be non-empty")
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("%q must be relative to overlay.path", p)
	}
	return nil
}

// Chart is the platform umbrella chart pull pointer. An empty Version means
// the chart phase is skipped (k3s-only). Defaults are applied in Validate.
type Chart struct {
	Repository string `yaml:"repository"` // OCI URL ending /iterabase-platform; Forge installs same-version certificate and LVM-storage companions first
	Version    string `yaml:"version"`    // chart version (semver) to install; empty => skip chart
	Release    string `yaml:"release"`    // helm release name (default: metadata.name)
	Namespace  string `yaml:"namespace"`  // target namespace (default: iterabase-system)
}

const (
	defaultChartRepository = "oci://ghcr.io/iterabase/iterabase-charts/iterabase-platform"
	defaultChartNamespace  = "iterabase-system"
)

// applyDefaults fills the chart pull pointer defaults when a chart version is
// set. With no version, the chart phase is skipped and defaults stay empty.
func (c *Chart) applyDefaults(install string) {
	if c.Version == "" {
		return
	}
	if c.Repository == "" {
		c.Repository = defaultChartRepository
	}
	if c.Release == "" {
		c.Release = install
	}
	if c.Namespace == "" {
		c.Namespace = defaultChartNamespace
	}
}

// GPU is the opt-in NVIDIA GPU node-readiness configuration. When Enabled,
// forge installs the NVIDIA GPU Operator (containerized driver + container
// toolkit + device plugin + RuntimeClass via CDI) as a forge-managed Helm
// release and gates apply on coherent ClusterPolicy conditions plus live node
// driver/readiness evidence. It is a forge composable dependency, not a chart
// value — the overlay owns those.
// v1: single-node, Ubuntu hosts only.
type GPU struct {
	Enabled  bool        `yaml:"enabled"`
	Operator GPUOperator `yaml:"operator"`
	Driver   GPUDriver   `yaml:"driver"`
}

// GPUDriver is the NVIDIA driver the gpu-operator installs and loads as a host
// kernel module. It is a node-readiness / substrate concern, on the same
// footing as the operator chart pointer — NOT an overlay chart value. Version
// and SHA256 are both required when GPU is enabled so the operator receives an
// immutable tag-plus-digest image identity rather than resolving a mutable tag.
// v1: the operator compiles and loads the driver on the host; forge sets the
// reviewed content identity.
type GPUDriver struct {
	Version string `yaml:"version"` // NVIDIA driver version reported by the loaded module (e.g. 580.126.20)
	SHA256  string `yaml:"sha256"`  // exact resolved driver-container image digest
}

// GPUOperator is the NVIDIA GPU Operator Helm release pointer. Defaults are
// applied in Validate when GPU is enabled; an empty Version falls back to the
// forge-pinned default.
type GPUOperator struct {
	Version    string `yaml:"version"`    // chart version (semver); default defaultGPUOperatorVersion
	Repository string `yaml:"repository"` // helm repo URL; default the NGC chart repo
	Chart      string `yaml:"chart"`      // chart name in the repo; default gpu-operator
	Release    string `yaml:"release"`    // helm release name (default: <metadata.name>-gpu-operator)
	Namespace  string `yaml:"namespace"`  // target namespace (default: gpu-operator)
	SHA256     string `yaml:"sha256"`     // exact chart archive content identity
}

const (
	defaultGPUOperatorVersion    = "v26.3.3"
	defaultGPUOperatorRepository = "https://helm.ngc.nvidia.com/nvidia"
	defaultGPUOperatorChart      = "gpu-operator"
	defaultGPUOperatorNamespace  = "gpu-operator"
	defaultGPUOperatorSHA256     = "59abb5852a24b3ae0ef757bfea3051f419acbf559ee5efd72f0672d28af56a68"
)

// applyDefaults fills the GPU operator release pointer defaults when GPU is
// enabled. When disabled, the GPU phase is skipped and defaults stay empty.
func (g *GPU) applyDefaults(install string) {
	if !g.Enabled {
		return
	}
	if g.Operator.Version == "" {
		g.Operator.Version = defaultGPUOperatorVersion
	}
	if g.Operator.Repository == "" {
		g.Operator.Repository = defaultGPUOperatorRepository
	}
	if g.Operator.Chart == "" {
		g.Operator.Chart = defaultGPUOperatorChart
	}
	if g.Operator.Release == "" {
		g.Operator.Release = install + "-gpu-operator"
	}
	if g.Operator.Namespace == "" {
		g.Operator.Namespace = defaultGPUOperatorNamespace
	}
	if g.Operator.SHA256 == "" && g.Operator.Version == defaultGPUOperatorVersion &&
		g.Operator.Repository == defaultGPUOperatorRepository && g.Operator.Chart == defaultGPUOperatorChart {
		g.Operator.SHA256 = defaultGPUOperatorSHA256
	}
}

// validate enforces v1 constraints on the GPU configuration. GPU readiness
// supports single-node only in v1 (HA is already refused by mode validation;
// this guard makes the intent explicit and keeps GPU enablement honest).
//
// A non-empty driver identity with gpu.enabled: false is rejected: the pin is
// inert when GPU is disabled (no operator runs to materialize it), so keeping
// it would be silently ignored config. GPU enablement requires both the
// operator chart archive checksum and the driver container digest.
func (g GPU) validate(mode string) error {
	if !g.Enabled {
		if g.Driver.Version != "" || g.Driver.SHA256 != "" {
			return fmt.Errorf("gpu.driver identity is set but gpu.enabled is false — clear gpu.driver or enable gpu")
		}
		return nil
	}
	if mode != ModeSingleNode {
		return fmt.Errorf("gpu.enabled requires mode %q in v1, got %q", ModeSingleNode, mode)
	}
	if !contentSHA256Re.MatchString(g.Operator.SHA256) {
		return fmt.Errorf("gpu.operator.sha256 must be the exact lowercase chart archive SHA-256")
	}
	if strings.TrimSpace(g.Driver.Version) == "" {
		return fmt.Errorf("gpu.driver.version is required for immutable driver identity")
	}
	if !contentSHA256Re.MatchString(g.Driver.SHA256) {
		return fmt.Errorf("gpu.driver.sha256 must be the exact lowercase driver image SHA-256")
	}
	return nil
}

// Load reads and parses a forge.yaml config from path and records its
// directory for relative overlay paths.
func Load(path string) (*Cluster, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config %q: %w", path, err)
	}
	c.Dir = filepath.Dir(abs)
	return c, nil
}

// Parse parses and validates a forge.yaml config from bytes.
func Parse(data []byte) (*Cluster, error) {
	var c Cluster
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the config against v1 constraints.
func (c *Cluster) Validate() error {
	if c.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", APIVersion, c.APIVersion)
	}
	if c.Kind != Kind {
		return fmt.Errorf("kind must be %q, got %q", Kind, c.Kind)
	}
	if c.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if !nameRe.MatchString(c.Metadata.Name) {
		return fmt.Errorf("metadata.name %q must be lowercase alphanumeric with hyphens", c.Metadata.Name)
	}
	c.Spec.Chart.applyDefaults(c.Metadata.Name)
	c.Spec.GPU.applyDefaults(c.Metadata.Name)
	c.Spec.Overlay.applyDefaults()
	c.Spec.Provider.applyDefaults()
	return c.Spec.validate()
}

func (s *Spec) validate() error {
	if s.FluxLegacy != nil {
		return fmt.Errorf("flux is removed: forge apply is the only overlay delivery path (DES-HOR-593-04); delete the flux block")
	}
	if err := s.Provider.validate(); err != nil {
		return err
	}
	switch s.Mode {
	case ModeSingleNode:
		if s.Provider.AWS != nil {
			// The AWS provider derives the host and the data disk from the
			// environment it provisions; forge.yaml must not pin them.
			if len(s.Hosts) != 0 || len(s.DataStorage.Devices) != 0 {
				return fmt.Errorf("provider.aws derives hosts and dataStorage from the provisioned environment; remove them from forge.yaml")
			}
			break
		}
		if len(s.Hosts) != 1 {
			return fmt.Errorf("mode %q requires exactly 1 host, got %d", s.Mode, len(s.Hosts))
		}
	case ModeHA, ModeBYOKubeconfig:
		return fmt.Errorf("mode %q is reserved and not yet supported", s.Mode)
	default:
		return fmt.Errorf("mode %q is invalid (expected %q)", s.Mode, ModeSingleNode)
	}
	for i, h := range s.Hosts {
		if err := h.validate(); err != nil {
			return fmt.Errorf("host[%d]: %w", i, err)
		}
	}
	if err := s.GPU.validate(s.Mode); err != nil {
		return err
	}
	if s.Provider.AWS == nil {
		if err := s.DataStorage.validate(); err != nil {
			return err
		}
	}
	if err := s.Overlay.validate(); err != nil {
		return err
	}
	return s.K3s.validate()
}

func (h *Host) validate() error {
	if h.Address == "" {
		return fmt.Errorf("address is required")
	}
	if h.SSHUser == "" {
		return fmt.Errorf("sshUser is required")
	}
	if h.SSHKeyPath == "" {
		return fmt.Errorf("sshKeyPath is required")
	}
	if h.SSHHostKeyLegacy != "" {
		return fmt.Errorf("sshHostKey is removed: move the founder-verified host public key out of Git, store it in a non-Git trust file, and set sshTrustFile (see docs/architecture/forge-ssh-host-trust.md)")
	}
	if h.SSHTrustFile == "" {
		return fmt.Errorf("sshTrustFile is required: Forge verifies the host against explicit non-Git trust material or fails closed")
	}
	if h.Role != RoleControlPlaneWorker {
		return fmt.Errorf("role must be %q for v1, got %q", RoleControlPlaneWorker, h.Role)
	}
	for i, t := range h.Taints {
		if err := t.validate(); err != nil {
			return fmt.Errorf("taints[%d]: %w", i, err)
		}
	}
	return nil
}

func (t Taint) validate() error {
	if t.Key == "" {
		return fmt.Errorf("key is required")
	}
	switch t.Effect {
	case "NoSchedule", "PreferNoSchedule", "NoExecute":
	default:
		return fmt.Errorf("effect %q must be NoSchedule|PreferNoSchedule|NoExecute", t.Effect)
	}
	return nil
}

func (k K3s) validate() error {
	if err := validateK3sExtraArgs(k.ExtraArgs); err != nil {
		return err
	}
	if k.Version == "" {
		return fmt.Errorf("k3s.version is required")
	}
	switch k.Version {
	case "v1.31.5", "v1.31.5+k3s1", "v1.34.10", "v1.34.10+k3s1":
	default:
		return fmt.Errorf("k3s.version %q has no repository-reviewed executable/runtime identity", k.Version)
	}
	if err := validateCIDR(k.ClusterCIDR, false, "clusterCIDR"); err != nil {
		return err
	}
	if err := validateCIDR(k.ServiceCIDR, false, "serviceCIDR"); err != nil {
		return err
	}
	if k.DualStack {
		if err := validateCIDR(k.ClusterCIDRv6, true, "clusterCIDRv6"); err != nil {
			return err
		}
		if err := validateCIDR(k.ServiceCIDRv6, true, "serviceCIDRv6"); err != nil {
			return err
		}
	}
	return nil
}

//nolint:gocyclo // ordered token-stream validation keeps split/joined security cases explicit.
func validateK3sExtraArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--disable" || strings.HasPrefix(arg, "--disable=") {
			return fmt.Errorf("k3s.extraArgs must not override component disablement; use k3s.disable (local-storage is always disabled)")
		}
		if arg == "--data-dir" || strings.HasPrefix(arg, "--data-dir=") {
			return fmt.Errorf("k3s.extraArgs must not change the fixed K3s data/kubelet directory required by the LVM storage substrate")
		}
		if strings.HasPrefix(arg, "--kubelet-arg=") {
			value := strings.TrimPrefix(arg, "--kubelet-arg=")
			if value == "" || value == "root-dir" || strings.HasPrefix(value, "root-dir=") {
				return fmt.Errorf("k3s.extraArgs must not change the fixed K3s data/kubelet directory required by the LVM storage substrate")
			}
			continue
		}
		if arg != "--kubelet-arg" {
			continue
		}
		if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "--") {
			return fmt.Errorf("k3s.extraArgs --kubelet-arg requires one following kubelet argument token")
		}
		value := args[i+1]
		if value == "root-dir" || strings.HasPrefix(value, "root-dir=") {
			return fmt.Errorf("k3s.extraArgs must not change the fixed K3s data/kubelet directory required by the LVM storage substrate")
		}
		i++
	}
	return nil
}

func validateCIDR(s string, wantV6 bool, field string) error {
	if s == "" {
		return fmt.Errorf("k3s.%s is required", field)
	}
	_, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		return fmt.Errorf("k3s.%s %q: %w", field, s, err)
	}
	isV6 := ipnet.IP.To4() == nil
	if isV6 != wantV6 {
		want := "IPv4"
		if wantV6 {
			want = "IPv6"
		}
		return fmt.Errorf("k3s.%s %q: expected %s", field, s, want)
	}
	return nil
}
