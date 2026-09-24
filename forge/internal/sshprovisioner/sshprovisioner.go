// Package sshprovisioner implements provisioner.Provisioner over SSH.
package sshprovisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"gopkg.in/yaml.v3"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/deployer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/k3s"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/provisioner"
)

const sshPort = "22"

// dialFunc dials an SSH client. Defaults to a context-aware ssh.Dial; tests
// override it to target an in-process fake server.
type dialFunc func(ctx context.Context, network, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error)

// SSHProvisioner implements provisioner.Provisioner over SSH to a single host.
type SSHProvisioner struct {
	host   config.Host
	cfg    *ssh.ClientConfig
	dial   dialFunc
	client *ssh.Client
}

// Option configures an SSHProvisioner (primarily for tests).
type Option func(*SSHProvisioner)

// WithDial overrides the SSH dial function (for fake-server tests).
func WithDial(d dialFunc) Option {
	return func(p *SSHProvisioner) { p.dial = d }
}

// WithSSHConfig overrides the SSH client config (for fake-server tests).
func WithSSHConfig(c *ssh.ClientConfig) Option {
	return func(p *SSHProvisioner) { p.cfg = c }
}

// New builds an SSHProvisioner for host using key-based auth (key file, or the
// SSH agent as fallback). Encrypted keys must be agent-loaded (no passphrase
// prompt). Host identity is verified during key exchange against the explicit
// non-Git trust file (spec.hosts[].sshTrustFile); missing or invalid trust
// fails closed before any connection attempt.
func New(host config.Host, opts ...Option) (*SSHProvisioner, error) {
	cfg := &ssh.ClientConfig{
		User:    host.SSHUser,
		Timeout: 10 * time.Second,
	}
	p := &SSHProvisioner{host: host, cfg: cfg, dial: defaultDial}
	for _, opt := range opts {
		opt(p)
	}
	// If no host-key callback was injected (e.g. by tests via WithSSHConfig),
	// load the host's explicit trust material. The callback runs during key
	// exchange, before user authentication, sessions, commands, or stdin.
	if p.cfg.HostKeyCallback == nil {
		hostKeyCallback, hostKeyAlgorithms, err := loadHostTrust(host.Address, host.SSHTrustFile)
		if err != nil {
			return nil, err
		}
		p.cfg.HostKeyCallback = hostKeyCallback
		p.cfg.HostKeyAlgorithms = hostKeyAlgorithms
	}
	// If no auth was injected (e.g. by tests via WithSSHConfig), derive it from
	// the key file or the SSH agent.
	if len(p.cfg.Auth) == 0 {
		if err := setAuth(p.cfg, host.SSHKeyPath); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Close releases the underlying SSH connection.
func (p *SSHProvisioner) Close() error {
	if p.client != nil {
		return p.client.Close()
	}
	return nil
}

func defaultDial(ctx context.Context, network, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	d := net.Dialer{Timeout: cfg.Timeout}
	netConn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	conn, chans, reqs, err := ssh.NewClientConn(netConn, addr, cfg)
	if err != nil {
		netConn.Close()
		return nil, err
	}
	return ssh.NewClient(conn, chans, reqs), nil
}

func setAuth(cfg *ssh.ClientConfig, keyPath string) error {
	path := expandPath(keyPath)
	if path != "" {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("ssh key %q: %w", path, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("ssh key %q is too open (mode %o); expected 0600/0644", path, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read ssh key %q: %w", path, err)
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return fmt.Errorf("parse ssh key %q: %w", path, err)
		}
		cfg.Auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
		return nil
	}
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return errors.New("no sshKeyPath and no SSH_AUTH_SOCK; cannot authenticate")
	}
	conn, err := net.Dial("unix", sock) //nolint:gosec // SSH_AUTH_SOCK is a local unix socket to the user's agent
	if err != nil {
		return fmt.Errorf("connect to ssh agent: %w", err)
	}
	ag := agent.NewClient(conn)
	cfg.Auth = []ssh.AuthMethod{ssh.PublicKeysCallback(ag.Signers)}
	return nil
}

func (p *SSHProvisioner) ensureClient(ctx context.Context) (*ssh.Client, error) {
	if p.client != nil {
		return p.client, nil
	}
	c, err := p.dial(ctx, "tcp", net.JoinHostPort(p.host.Address, sshPort), p.cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", p.host.Address, err)
	}
	p.client = c
	return c, nil
}

func (p *SSHProvisioner) run(ctx context.Context, cmd string) (string, error) {
	client, err := p.ensureClient(ctx)
	if err != nil {
		return "", err
	}
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if err := sess.Start(cmd); err != nil {
		return "", fmt.Errorf("ssh start %q: %w", cmd, err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return stdout.String(), fmt.Errorf("ssh run %q: %w; stderr: %s", cmd, err, stderr.String())
		}
		return stdout.String(), nil
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return "", ctx.Err()
	}
}

// Preflight implements provisioner.Provisioner.
func (p *SSHProvisioner) Preflight(ctx context.Context) (*provisioner.PreflightResult, error) {
	r := &provisioner.PreflightResult{}
	out, err := p.run(ctx, "cat /etc/os-release")
	if err != nil {
		return nil, fmt.Errorf("inspect operating system: %w", err)
	}
	r.OS = parseOS(out)
	if r.OS == "" {
		return nil, fmt.Errorf("inspect operating system: /etc/os-release has no PRETTY_NAME")
	}
	if _, err := p.run(ctx, "sudo -n true"); err == nil {
		r.HasSudo = true
	}
	if _, err := p.run(ctx, "command -v curl"); err == nil {
		r.HasCurl = true
	}
	if _, err := p.run(ctx, "pidof systemd"); err == nil {
		r.HasSystemd = true
	}
	if _, err := p.run(ctx, "command -v k3s"); err == nil {
		r.Installed = true
	}
	if _, err := p.run(ctx, "ip -6 addr show scope global"); err == nil {
		r.HasIPv6 = true
	}
	// GPU preflight checks (read-only; only meaningful when GPU is enabled).
	// NVIDIA GPU on the PCI bus via the vendor ID (0x10de) — no driver or
	// pciutils needed. Driver-build dependency checks use the exact paths and
	// executables consumed by the GPU Operator driver container.
	if _, err := p.run(ctx, "grep -qi 0x10de /sys/bus/pci/devices/*/vendor"); err == nil {
		r.HasNVIDIAGPU = true
	}
	if _, err := p.run(ctx, "test -f /lib/modules/$(uname -r)/build/Makefile"); err == nil {
		r.KernelHeadersInstalled = true
	}
	if _, err := p.run(ctx, "command -v dkms"); err == nil {
		r.HasDKMS = true
	}
	if _, err := p.run(ctx, "command -v gcc"); err == nil {
		r.HasGCC = true
	}
	if _, err := p.run(ctx, "command -v make"); err == nil {
		r.HasMake = true
	}
	return r, nil
}

// Install implements provisioner.Provisioner.
func (p *SSHProvisioner) Install(ctx context.Context, version string, serverArgs []string) error {
	resolvedVersion := k3s.ResolveVersion(version)
	if _, err := p.installReviewedRemoteTool(ctx, "k3s", resolvedVersion); err != nil {
		return fmt.Errorf("install reviewed k3s executable: %w", err)
	}
	if _, err := p.installReviewedRemoteTool(ctx, "k3s-images", resolvedVersion); err != nil {
		return fmt.Errorf("install reviewed k3s runtime images: %w", err)
	}
	installer, err := p.downloadVerifiedRemoteContent(ctx, "k3s-installer", k3sInstallScriptURL, k3sInstallScriptSHA256)
	if err != nil {
		return err
	}
	defer p.removeRemoteContent(ctx, installer)
	cmd := fmt.Sprintf("sudo env INSTALL_K3S_VERSION=%s INSTALL_K3S_SKIP_DOWNLOAD=true sh %s %s",
		shellQuote(resolvedVersion), shellQuote(installer), joinArgs(serverArgs))
	if _, err = p.run(ctx, cmd); err != nil {
		return err
	}
	tool, installed, err := p.installedReviewedTool(ctx, "k3s", "/usr/local/bin/k3s")
	if err != nil {
		return fmt.Errorf("verify installed k3s content identity: %w", err)
	}
	if !installed || tool.version != resolvedVersion {
		return fmt.Errorf("installed k3s does not match reviewed version %s", resolvedVersion)
	}
	return nil
}

// Upgrade implements provisioner.Provisioner. k3s supports in-place upgrade by
// re-running the install script with a new version.
func (p *SSHProvisioner) Upgrade(ctx context.Context, version string, serverArgs []string) error {
	return p.Install(ctx, version, serverArgs)
}

// Uninstall implements provisioner.Provisioner.
func (p *SSHProvisioner) Uninstall(ctx context.Context) error {
	_, err := p.run(ctx, "if test -x /usr/local/bin/k3s-uninstall.sh; then sudo /usr/local/bin/k3s-uninstall.sh; fi")
	return err
}

// Reboot implements provisioner.Rebooter. --no-block lets systemd acknowledge
// the request before the pinned SSH connection is intentionally severed.
func (p *SSHProvisioner) Reboot(ctx context.Context) error {
	_, err := p.run(ctx, "sudo systemctl reboot --no-block")
	return err
}

// FetchKubeconfig implements provisioner.Provisioner.
func (p *SSHProvisioner) FetchKubeconfig(ctx context.Context) ([]byte, error) {
	out, err := p.run(ctx, "sudo cat /etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// ReadState implements provisioner.Provisioner.
func (p *SSHProvisioner) ReadState(ctx context.Context) (*provisioner.HostState, error) {
	st := &provisioner.HostState{}
	tool, installed, err := p.installedReviewedTool(ctx, "k3s", "/usr/local/bin/k3s")
	if err != nil {
		return nil, err
	}
	if !installed {
		return st, nil
	}
	st.Installed = true
	out, err := p.run(ctx, "sudo /usr/local/bin/k3s --version")
	if err != nil {
		return nil, fmt.Errorf("read reviewed k3s version: %w", err)
	}
	st.Version = parseK3sVersion(out)
	if st.Version != tool.version {
		return nil, fmt.Errorf("reviewed k3s bytes report version %q, expected %q", st.Version, tool.version)
	}
	if out, err := p.run(ctx, "sudo cat /etc/systemd/system/k3s.service"); err == nil {
		st.ClusterCIDR, st.ServiceCIDR, st.DualStack = parseSystemdUnit(out)
		st.LocalStorageDisabled = systemdUnitDisables(out, "local-storage")
	}
	return st, nil
}

// NodeReady implements provisioner.Provisioner.
func (p *SSHProvisioner) NodeReady(ctx context.Context) (bool, error) {
	out, err := p.run(ctx, "sudo k3s kubectl get --raw=/readyz")
	if err != nil {
		return false, err
	}
	return strings.Contains(out, "ok"), nil
}

// aptLockRetryInterval is the delay between apt retries when the apt/dpkg lock
// is held. A fresh Ubuntu VM commonly has apt locked on first boot by
// cloud-init or unattended-upgrades. Overridden in tests to avoid sleeping.
var aptLockRetryInterval = 15 * time.Second

// EnsureDriverBuildDeps implements provisioner.Provisioner. It installs the
// kernel headers matching the running kernel plus the compiler, make, and DKMS
// required by the GPU Operator driver container. Ubuntu/apt in v1; idempotent.
// It retries on apt/dpkg lock contention (cloud-init/unattended-upgrades holding
// the lock on first boot) rather than failing fast, then verifies the exact
// build surface before allowing GPU Operator reconciliation to continue.
func (p *SSHProvisioner) EnsureDriverBuildDeps(ctx context.Context) error {
	cmd := "sudo apt-get update && sudo apt-get install -y linux-headers-$(uname -r) build-essential dkms"
	for attempt := 0; ; attempt++ {
		out, err := p.run(ctx, cmd)
		if err == nil {
			break
		}
		lockHeld := isAptLockHeld(err.Error()) || isAptLockHeld(out)
		if !lockHeld || attempt >= 20 {
			return fmt.Errorf("install GPU driver build dependencies: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(aptLockRetryInterval):
		}
	}
	// The driver container resolves and downloads this exact package from the
	// Ubuntu archives independently of the host. An installed-but-retired HWE
	// kernel package is therefore unusable even when its local build tree remains.
	if _, err := p.run(ctx, "apt-cache show linux-headers-$(uname -r) >/dev/null 2>&1"); err != nil {
		return fmt.Errorf("verify GPU driver build dependencies: running kernel headers are not available from configured apt repositories: %w", err)
	}
	if _, err := p.run(ctx, "test -f /lib/modules/$(uname -r)/build/Makefile && command -v dkms >/dev/null && command -v gcc >/dev/null && command -v make >/dev/null"); err != nil {
		return fmt.Errorf("verify GPU driver build dependencies: %w", err)
	}
	return nil
}

// isAptLockHeld reports whether an apt/dpkg error is lock contention
// (retryable) rather than a real failure.
func isAptLockHeld(msg string) bool {
	return strings.Contains(msg, "Could not get lock") ||
		strings.Contains(msg, "Unable to lock") ||
		strings.Contains(msg, "is held by process")
}

// ReadGPUReadiness implements provisioner.Provisioner. One remote kubectl
// command collects the ClusterPolicy and live node fields for an observation.
// Query errors are returned so the lifecycle can retain them as timeout
// diagnostics while continuing to poll through installation and k3s restarts.
func (p *SSHProvisioner) ReadGPUReadiness(ctx context.Context, requestedDriverVersion string) (*provisioner.GPUReadiness, error) {
	out, err := p.run(ctx, "sudo k3s kubectl get clusterpolicy,nodes -o json")
	if err != nil {
		return nil, fmt.Errorf("read GPU readiness resources: %w", err)
	}
	readiness, err := parseGPUReadiness(out, requestedDriverVersion)
	if err != nil {
		return nil, fmt.Errorf("parse GPU readiness resources: %w", err)
	}
	return readiness, nil
}

type gpuReadinessList struct {
	Items []gpuReadinessItem `json:"items"`
}

type gpuReadinessItem struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Unschedulable bool `json:"unschedulable"`
		Driver        struct {
			Version string `json:"version"`
		} `json:"driver"`
	} `json:"spec"`
	Status struct {
		State      string `json:"state"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

func parseGPUReadiness(out, requestedDriverVersion string) (*provisioner.GPUReadiness, error) {
	var list gpuReadinessList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, err
	}

	readiness := &provisioner.GPUReadiness{RequestedDriverVersion: requestedDriverVersion}
	for i := range list.Items {
		item := &list.Items[i]
		switch item.Kind {
		case "ClusterPolicy":
			readiness.PolicyCount++
			if readiness.PolicyCount != 1 {
				continue
			}
			readiness.PolicyName = item.Metadata.Name
			readiness.PolicyState = item.Status.State
			readiness.PolicyDriverVersion = driverVersionWithoutDigest(item.Spec.Driver.Version)
			for _, condition := range item.Status.Conditions {
				switch condition.Type {
				case "Ready":
					readiness.ReadyCondition = condition.Status
				case "Error":
					readiness.ErrorCondition = condition.Status
				}
			}
		case "Node":
			readiness.NodeCount++
			if readiness.NodeCount != 1 {
				continue
			}
			readiness.NodeName = item.Metadata.Name
			readiness.NodeSchedulable = !item.Spec.Unschedulable
			readiness.NodeDriverVersion = item.Metadata.Labels["nvidia.com/cuda.driver-version.full"]
			readiness.NodeUpgradeState = item.Metadata.Labels["nvidia.com/gpu-driver-upgrade-state"]
			for _, condition := range item.Status.Conditions {
				if condition.Type == "Ready" {
					readiness.NodeReady = condition.Status == "True"
					break
				}
			}
		}
	}
	evaluateGPUReadiness(readiness)
	return readiness, nil
}

func driverVersionWithoutDigest(version string) string {
	version, _, _ = strings.Cut(version, "@sha256:")
	return version
}

func evaluateGPUReadiness(readiness *provisioner.GPUReadiness) {
	if readiness.PolicyCount != 1 {
		readiness.Reason = fmt.Sprintf("expected one ClusterPolicy, found %d", readiness.PolicyCount)
		return
	}
	if readiness.NodeCount != 1 {
		readiness.Reason = fmt.Sprintf("expected one node, found %d", readiness.NodeCount)
		return
	}
	if readiness.NodeUpgradeState == "upgrade-failed" {
		readiness.Terminal = true
		readiness.Reason = "GPU driver upgrade entered upgrade-failed"
		return
	}
	if reason := gpuPolicyReadinessIssue(readiness); reason != "" {
		readiness.Reason = reason
		return
	}
	if reason := gpuNodeReadinessIssue(readiness); reason != "" {
		readiness.Reason = reason
		return
	}

	readiness.Ready = true
	if readiness.PolicyState == "notReady" {
		readiness.Reason = "operator conditions and live node evidence converged; legacy ClusterPolicy state remains contradictory"
	} else {
		readiness.Reason = "operator conditions and live node evidence converged"
	}
}

func gpuPolicyReadinessIssue(readiness *provisioner.GPUReadiness) string {
	switch {
	case readiness.RequestedDriverVersion != "" && readiness.PolicyDriverVersion != readiness.RequestedDriverVersion:
		return "ClusterPolicy has not selected the requested driver"
	case readiness.ReadyCondition != "True":
		return "ClusterPolicy Ready condition is not True"
	case readiness.ErrorCondition != "False":
		return "ClusterPolicy Error condition is not False"
	case readiness.PolicyState != "ready" && readiness.PolicyState != "notReady":
		return "ClusterPolicy legacy state is missing or unsupported"
	default:
		return ""
	}
}

func gpuNodeReadinessIssue(readiness *provisioner.GPUReadiness) string {
	switch {
	case !readiness.NodeReady:
		return "GPU node is not Ready"
	case !readiness.NodeSchedulable:
		return "GPU node is unschedulable"
	case readiness.NodeUpgradeState != "" && readiness.NodeUpgradeState != "upgrade-done":
		return "GPU driver upgrade is still in progress"
	case readiness.NodeDriverVersion == "":
		return "GPU node does not report a loaded driver"
	case readiness.RequestedDriverVersion != "" && readiness.NodeDriverVersion != readiness.RequestedDriverVersion:
		return "GPU node has not loaded the requested driver"
	default:
		return ""
	}
}

func parseOS(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return ""
}

func parseK3sVersion(out string) string {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) >= 3 && fields[0] == "k3s" && fields[1] == "version" {
		return fields[2]
	}
	return strings.TrimSpace(out)
}

func parseSystemdUnit(out string) (clusterCIDR, serviceCIDR string, dualStack bool) {
	// The k3s install script (get.k3s.io) writes the server flags into the
	// unit's ExecStart line, one per line with backslash-newline continuations,
	// and wraps every argument in single quotes via its quote() helper. Join the
	// continuation lines first so each ExecStart arg becomes a single
	// whitespace-delimited token.
	out = strings.ReplaceAll(out, "\\\r\n", " ")
	out = strings.ReplaceAll(out, "\\\n", " ")

	tokens := strings.Fields(out)
	for i, raw := range tokens {
		tok := unquoteShellToken(raw)
		switch {
		case tok == "--cluster-cidr" && i+1 < len(tokens):
			clusterCIDR = unquoteShellToken(tokens[i+1])
		case strings.HasPrefix(tok, "--cluster-cidr="):
			clusterCIDR = strings.TrimPrefix(tok, "--cluster-cidr=")
		case tok == "--service-cidr" && i+1 < len(tokens):
			serviceCIDR = unquoteShellToken(tokens[i+1])
		case strings.HasPrefix(tok, "--service-cidr="):
			serviceCIDR = strings.TrimPrefix(tok, "--service-cidr=")
		}
	}
	dualStack = strings.Contains(clusterCIDR, ",")
	return clusterCIDR, serviceCIDR, dualStack
}

// unquoteShellToken removes one layer of single quoting as written by the k3s
// install script's quote() helper: it wraps each argument in single quotes and
// escapes an embedded single quote with the POSIX backslash-quote sequence.
// Tokens that are not single-quoted are returned unchanged. It assumes arg
// values contain no whitespace (true for all forge k3s flags: CIDRs, addresses,
// labels, taints), so strings.Fields never splits a quoted value across tokens.
func systemdUnitDisables(out, component string) bool {
	out = strings.ReplaceAll(out, "\\\r\n", " ")
	out = strings.ReplaceAll(out, "\\\n", " ")
	tokens := strings.Fields(out)
	for i, raw := range tokens {
		tok := unquoteShellToken(raw)
		if tok == "--disable" && i+1 < len(tokens) && unquoteShellToken(tokens[i+1]) == component {
			return true
		}
		if strings.HasPrefix(tok, "--disable=") && strings.TrimPrefix(tok, "--disable=") == component {
			return true
		}
	}
	return false
}

func unquoteShellToken(tok string) string {
	if len(tok) < 2 || tok[0] != '\'' || tok[len(tok)-1] != '\'' {
		return tok
	}
	inner := tok[1 : len(tok)-1]
	return strings.ReplaceAll(inner, "'\\''", "'")
}

func expandPath(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func joinArgs(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

const (
	helmVerifyCommand      = "case $(uname -m) in x86_64|amd64) expected=aeb4645b9e6658948efa290e28dd23ae75a16fb73f137942f2294fd5c7fcb573 ;; aarch64|arm64) expected=01bdd0c90f371968326162daaa427cdd14da2641ded094131afc44fb7a538b62 ;; *) exit 1 ;; esac; actual=$(sudo sha256sum /usr/local/bin/helm 2>/dev/null | awk '{print $1}') && test \"$actual\" = \"$expected\""
	k3sKubeconfigPath      = "/etc/rancher/k3s/k3s.yaml"
	helmRegistryConfigPath = "/etc/forge/helm-registry.json"
)

// helmCmd builds a sudo helm command targeting the k3s kubeconfig and Forge's
// root-owned registry config on the host. Public pulls work without the file;
// private candidate validation creates it temporarily at this same path.
func helmCmd(args ...string) string {
	base := []string{"sudo", "helm", "--kubeconfig", k3sKubeconfigPath, "--registry-config", helmRegistryConfigPath}
	return joinArgs(append(base, args...))
}

// ensureHelm installs the repository-reviewed Helm archive after verifying its
// exact bytes. Existing host binaries are never executed as installation
// authority; each reconciliation replaces them before the first Helm command.
func (p *SSHProvisioner) ensureHelm(ctx context.Context) error {
	if _, err := p.run(ctx, helmVerifyCommand); err == nil {
		return nil
	}
	if _, err := p.installReviewedRemoteTool(ctx, "helm", helmInstallVersion); err != nil {
		return fmt.Errorf("install reviewed Helm executable: %w", err)
	}
	if _, err := p.run(ctx, helmVerifyCommand); err != nil {
		return fmt.Errorf("verify reviewed Helm installation: %w", err)
	}
	return nil
}

const prometheusOperatorVersionAnnotation = "operator.prometheus.io/version"

type chartCRDHeader struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

type chartCRD struct {
	header   chartCRDHeader
	manifest string
}

// compareNumericVersions compares the dotted numeric versions used by the
// Prometheus Operator's CRD provenance annotation.
func compareNumericVersions(a, b string) (int, error) {
	parse := func(version string) ([]int, error) {
		parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
		parsed := make([]int, len(parts))
		for i, part := range parts {
			value, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid numeric version %q", version)
			}
			parsed[i] = value
		}
		return parsed, nil
	}

	left, err := parse(a)
	if err != nil {
		return 0, err
	}
	right, err := parse(b)
	if err != nil {
		return 0, err
	}
	for i := 0; i < max(len(left), len(right)); i++ {
		var leftPart, rightPart int
		if i < len(left) {
			leftPart = left[i]
		}
		if i < len(right) {
			rightPart = right[i]
		}
		if leftPart < rightPart {
			return -1, nil
		}
		if leftPart > rightPart {
			return 1, nil
		}
	}
	return 0, nil
}

// selectChartCRD resolves conflicting Prometheus Operator CRDs using the
// operator-owned version annotation. Unknown conflicting duplicates fail rather
// than allowing Helm's unstable dependency traversal order to choose a schema.
func selectChartCRD(existing, candidate chartCRD) (chartCRD, error) {
	if existing.manifest == candidate.manifest {
		return existing, nil
	}

	existingVersion := existing.header.Metadata.Annotations[prometheusOperatorVersionAnnotation]
	candidateVersion := candidate.header.Metadata.Annotations[prometheusOperatorVersionAnnotation]
	if existingVersion == "" && candidateVersion == "" {
		return chartCRD{}, fmt.Errorf("conflicting duplicate chart CRD %q has no authoritative version annotation", existing.header.Metadata.Name)
	}
	if existingVersion == "" {
		return candidate, nil
	}
	if candidateVersion == "" {
		return existing, nil
	}

	comparison, err := compareNumericVersions(existingVersion, candidateVersion)
	if err != nil {
		return chartCRD{}, fmt.Errorf("compare duplicate chart CRD %q versions: %w", existing.header.Metadata.Name, err)
	}
	if comparison < 0 {
		return candidate, nil
	}
	if comparison > 0 {
		return existing, nil
	}
	return chartCRD{}, fmt.Errorf("conflicting duplicate chart CRD %q has equal authoritative version %q", existing.header.Metadata.Name, existingVersion)
}

// extractChartCRDs removes Helm's OCI pull-status preamble and any non-CRD YAML
// documents from `helm show crds`. Helm 4 writes "Pulled" and "Digest" to stdout
// for OCI charts, which kubectl otherwise tries to decode as a resource. Helm's
// dependency traversal order is unstable, so duplicate Prometheus Operator CRDs
// are selected by their operator-owned version and the result is sorted by name.
func extractChartCRDs(raw string) (string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	crds := make(map[string]chartCRD)
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("decode chart CRDs: %w", err)
		}
		var header chartCRDHeader
		if err := document.Decode(&header); err != nil {
			return "", fmt.Errorf("decode chart CRD header: %w", err)
		}
		if header.APIVersion != "apiextensions.k8s.io/v1" || header.Kind != "CustomResourceDefinition" {
			continue
		}
		if header.Metadata.Name == "" {
			return "", errors.New("chart CRD is missing metadata.name")
		}
		var resource any
		if err := document.Decode(&resource); err != nil {
			return "", fmt.Errorf("decode chart CRD: %w", err)
		}
		manifest, err := yaml.Marshal(resource)
		if err != nil {
			return "", fmt.Errorf("encode chart CRD: %w", err)
		}
		candidate := chartCRD{header: header, manifest: strings.TrimSpace(string(manifest))}
		if existing, duplicate := crds[header.Metadata.Name]; duplicate {
			candidate, err = selectChartCRD(existing, candidate)
			if err != nil {
				return "", err
			}
		}
		crds[header.Metadata.Name] = candidate
	}
	if len(crds) == 0 {
		return "", nil
	}

	names := make([]string, 0, len(crds))
	for name := range crds {
		names = append(names, name)
	}
	sort.Strings(names)
	manifests := make([]string, 0, len(names))
	for _, name := range names {
		manifests = append(manifests, crds[name].manifest)
	}
	return strings.Join(manifests, "\n---\n") + "\n", nil
}

// selectMetalLBCRDs filters a CRD manifest to only the MetalLB CRDs (those whose
// metadata.name ends in .metallb.io). The pre-apply is intentionally scoped to the
// MetalLB CRDs (DES-HOR-511-03/04): every other CRD the chart ships in `crds/`
// directories or renders as an ordinary template is left entirely to Helm's own
// install path, which owns them correctly on a fresh install. Pre-applying those
// other CRDs without Helm ownership made a fresh `helm install` fail to import
// them ("invalid ownership metadata"), so only the MetalLB set is established up
// front.
func selectMetalLBCRDs(raw string) (string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	var crds []string
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("decode MetalLB chart CRDs: %w", err)
		}
		var header chartCRDHeader
		if err := document.Decode(&header); err != nil {
			return "", fmt.Errorf("decode MetalLB chart CRD header: %w", err)
		}
		if header.APIVersion != "apiextensions.k8s.io/v1" || header.Kind != "CustomResourceDefinition" {
			continue
		}
		if !strings.HasSuffix(header.Metadata.Name, ".metallb.io") {
			continue
		}
		var resource any
		if err := document.Decode(&resource); err != nil {
			return "", fmt.Errorf("decode MetalLB chart CRD: %w", err)
		}
		manifest, err := yaml.Marshal(resource)
		if err != nil {
			return "", fmt.Errorf("encode MetalLB chart CRD: %w", err)
		}
		crds = append(crds, strings.TrimSpace(string(manifest)))
	}
	if len(crds) == 0 {
		return "", nil
	}
	return strings.Join(crds, "\n---\n") + "\n", nil
}

// applyChartCRDs reconciles the CRDs that the exact pinned chart release will
// depend on before Helm renders/maps the release's custom resources. It unions
// two sources: CRDs shipped in `crds/` directories (surfaced by `helm show
// crds`, e.g. observability/external-dns) and CRDs rendered as ordinary template
// resources (DES-HOR-511-03: the MetalLB CRDs, which `helm show crds` cannot
// see), discovered by rendering the exact chart with the release's value files.
// Helm installs crds/-dir CRDs only on a release's initial install; rendering
// the template CRDs and establishing all of them up front makes both fresh
// installs and upgrades deterministic. The rendered (template) CRDs are marked
// Helm-adoptable for the incoming release so a fresh `helm install` can adopt
// them (DES-HOR-511-04); crds/-dir CRDs install through Helm's own crds/ path
// and need no such marking. Server-side apply avoids the oversized last-applied
// annotation common with CRDs and makes repeated applies idempotent. CRDs
// intentionally remain on uninstall to protect custom-resource data, matching
// Helm's CRD lifecycle semantics.
func (p *SSHProvisioner) applyChartCRDs(ctx context.Context, opts deployer.ApplyOpts) (bool, error) {
	showArgs := []string{"show", "crds", opts.Repository}
	if opts.Version != "" {
		showArgs = append(showArgs, "--version", opts.Version)
	}
	raw, err := p.run(ctx, helmCmd(showArgs...))
	if err != nil {
		return false, fmt.Errorf("discover chart CRDs: %w", err)
	}
	rendered, err := p.renderChartCRDs(ctx, opts)
	if err != nil {
		return false, fmt.Errorf("discover rendered chart CRDs: %w", err)
	}
	// DES-HOR-511-03/04 pre-apply:
	//  - Every CRD shipped in a `crds/` directory (surfaced by `helm show crds`,
	//    e.g. observability/Prometheus and external-dns) is preserved in the
	//    apply/wait set. Helm only installs crds/-dir CRDs on a release's initial
	//    install, so pre-applying them is what makes an operator-feature-enable
	//    upgrade (which adds a dependency) deterministic. These CRDs go through
	//    Helm's own crds/ path and need no ownership marking.
	//  - Only the MetalLB template CRDs (rendered) receive Helm-ownership marking
	//    and are added to the apply/wait set. Non-MetalLB rendered template CRDs
	//    (e.g. control-plane's agentpools) are left entirely to Helm: pre-applying
	//    them without Helm ownership makes a fresh `helm install` fail to import
	//    them ("invalid ownership metadata").
	metallbRendered, err := selectMetalLBCRDs(rendered)
	if err != nil {
		return false, fmt.Errorf("select MetalLB chart CRDs: %w", err)
	}
	metallbEnabled := metallbRendered != ""
	if metallbRendered != "" {
		metallbRendered, err = markHelmAdoptableCRDs(metallbRendered, opts.Release, opts.Namespace)
		if err != nil {
			return false, fmt.Errorf("mark MetalLB CRDs Helm-adoptable: %w", err)
		}
		raw += "\n---\n" + metallbRendered
	}
	crds, err := extractChartCRDs(raw)
	if err != nil {
		return false, err
	}
	if crds == "" {
		return metallbEnabled, nil
	}
	if _, err := p.runStdin(ctx, kubectlCmd("apply", "--server-side", "--force-conflicts", "-f", "-"), crds); err != nil {
		return false, fmt.Errorf("apply chart CRDs: %w", err)
	}
	if _, err := p.runStdin(ctx, kubectlCmd("wait", "--for=condition=Established", "--timeout=2m", "-f", "-"), crds); err != nil {
		return false, fmt.Errorf("wait for chart CRDs: %w", err)
	}
	return metallbEnabled, nil
}

// renderChartCRDs renders the exact pinned chart with the release's value files
// and overrides and returns only the CustomResourceDefinition documents it
// renders (CRDs owned as ordinary template resources, e.g. the MetalLB CRDs). It
// returns "" when the chart renders no CRDs (e.g. MetalLB disabled in values).
func (p *SSHProvisioner) renderChartCRDs(ctx context.Context, opts deployer.ApplyOpts) (string, error) {
	// Render with the release namespace so `.Release.Namespace`-templated fields
	// (e.g. the bgppeers conversion webhook's clientConfig.service.namespace)
	// resolve identically to the subsequent `helm install`, avoiding an SSA
	// conflict when Helm adopts the pre-applied CRD.
	args := []string{"template", opts.Repository}
	if opts.Version != "" {
		args = append(args, "--version", opts.Version)
	}
	args = append(args, "-n", opts.Namespace)
	for _, f := range opts.ValueFiles {
		args = append(args, "-f", f)
	}
	for _, v := range opts.Values {
		args = append(args, "--set", v)
	}
	out, err := p.run(ctx, helmCmd(args...))
	if err != nil {
		return "", err
	}
	return extractChartCRDs(out)
}

// markHelmAdoptableCRDs injects the incoming release's Helm ownership metadata
// into every rendered (template) CustomResourceDefinition so a fresh
// `helm install` can adopt them instead of failing to import them as
// "existing ... cannot be imported into the current release: invalid ownership
// metadata" (DES-HOR-511-04). Scope is strict: only the rendered template CRDs
// (the MetalLB set) are marked for the incoming release/namespace, never the
// crds/-directory CRDs which Helm installs through its own crds/ path. Idempotent
// on upgrade (re-asserting the same ownership) and a no-op shape change only
// when a rendered CRD is absent.
func markHelmAdoptableCRDs(rendered string, release, namespace string) (string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(rendered))
	var manifests []string
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("decode rendered CRD for helm adoption: %w", err)
		}
		var header chartCRDHeader
		if err := document.Decode(&header); err != nil {
			return "", fmt.Errorf("decode rendered CRD header: %w", err)
		}
		if header.APIVersion != "apiextensions.k8s.io/v1" || header.Kind != "CustomResourceDefinition" {
			continue
		}
		// Strict founder scope (DES-HOR-511-04): only the nine MetalLB CRDs are
		// marked Helm-adoptable; other rendered template CRDs are still included in
		// the pre-apply set (established before Helm) but left unmarked.
		if strings.HasSuffix(header.Metadata.Name, ".metallb.io") {
			markCRDHelmOwnership(&document, release, namespace)
		}
		var resource any
		if err := document.Decode(&resource); err != nil {
			return "", fmt.Errorf("decode rendered CRD for helm adoption: %w", err)
		}
		manifest, err := yaml.Marshal(resource)
		if err != nil {
			return "", fmt.Errorf("encode rendered CRD for helm adoption: %w", err)
		}
		manifests = append(manifests, strings.TrimSpace(string(manifest)))
	}
	return strings.Join(manifests, "\n---\n") + "\n", nil
}

// markCRDHelmOwnership sets the release-ownership annotations and the
// app.kubernetes.io/managed-by: Helm label on a rendered CRD's metadata so Helm
// can adopt the pre-applied object on install.
func markCRDHelmOwnership(doc *yaml.Node, release, namespace string) {
	root := doc.Content[0]
	var meta *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "metadata" {
			meta = root.Content[i+1]
			break
		}
	}
	if meta == nil {
		meta = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, keyNode("metadata"), meta)
	}
	ensureMapKey(meta, "annotations", map[string]string{
		"meta.helm.sh/release-name":      release,
		"meta.helm.sh/release-namespace": namespace,
	})
	ensureMapKey(meta, "labels", map[string]string{
		"app.kubernetes.io/managed-by": "Helm",
	})
}

func keyNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

// ensureMapKey sets scalar key/value pairs on a mapping node, creating or
// overwriting them (idempotent), and preserves the existing node order otherwise.
func ensureMapKey(mapNode *yaml.Node, key string, values map[string]string) {
	var sub *yaml.Node
	found := -1
	for i := 0; i+1 < len(mapNode.Content); i += 2 {
		if mapNode.Content[i].Value == key {
			sub = mapNode.Content[i+1]
			found = i
			break
		}
	}
	if sub == nil {
		sub = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mapNode.Content = append(mapNode.Content, keyNode(key), sub)
	} else if sub.Kind != yaml.MappingNode {
		sub = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: sub.Content}
		mapNode.Content[found+1] = sub
	}
	for k, v := range values {
		var set bool
		for i := 0; i+1 < len(sub.Content); i += 2 {
			if sub.Content[i].Value == k {
				sub.Content[i+1].Value = v
				sub.Content[i+1].Tag = "!!str"
				set = true
				break
			}
		}
		if !set {
			sub.Content = append(sub.Content, keyNode(k), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
		}
	}
}

// Apply implements deployer.Deployer: idempotent helm upgrade --install of a
// Helm release, applying -f value files (ValueFiles, in order) then --set values
// (Values), ensuring helm and reconciling the pinned chart's CRDs first.
//
// When MetalLB is enabled and this is a fresh install (or an unresolved
// interrupted bootstrap) the ordinary pool/advertisement CRs would otherwise be
// admitted against a not-yet-running MetalLB webhook. Apply runs a bounded
// bootstrap-only Ignore override (DES-HOR-511-04): it first applies the release
// with metallb.crds.validationFailurePolicy=Ignore so the CRs can be created,
// then waits for the MetalLB controller and webhook backend to be ready, then
// immediately reapplies the release with the chart's steady-state Fail policy
// and asserts the live webhook failurePolicy is Fail. Failure returns so a
// later reapply re-enters and completes the bootstrap safely.
func (p *SSHProvisioner) Apply(ctx context.Context, opts deployer.ApplyOpts) error {
	if err := p.ensureHelm(ctx); err != nil {
		return err
	}
	opts, cleanup, err := p.verifiedApplyOpts(ctx, opts)
	if err != nil {
		return err
	}
	defer cleanup()
	metallbEnabled, err := p.applyChartCRDs(ctx, opts)
	if err != nil {
		return err
	}
	if !metallbEnabled {
		// MetalLB disabled/absent: a single steady-state apply, no bootstrap.
		if _, err := p.run(ctx, helmCmd(applyArgs(opts, "")...)); err != nil {
			return fmt.Errorf("helm install: %w", err)
		}
		return nil
	}

	// MetalLB enabled: reconcile the admission-webhook failurePolicy to the
	// steady-state Fail, running a bounded bootstrap-only Ignore override when the
	// release is fresh or an interrupted bootstrap awaits converge (DES-HOR-511-04).
	installed, err := p.releaseInstalled(ctx, opts)
	if err != nil {
		return fmt.Errorf("read release state for metallb bootstrap: %w", err)
	}
	policy, err := p.metalLBValidationPolicy(ctx, opts.Namespace)
	if err != nil {
		return fmt.Errorf("read metallb validation policy: %w", err)
	}
	if !installed || policy == metalLBPolicyIgnore {
		if _, err := p.run(ctx, helmCmd(applyArgs(opts, metalLBValidationPolicyValue+"="+metalLBPolicyIgnore)...)); err != nil {
			return fmt.Errorf("helm bootstrap install: %w", err)
		}
		if err := p.waitMetalLBAdmissionBackend(ctx, opts.Release, opts.Namespace); err != nil {
			return err
		}
	}
	if _, err := p.run(ctx, helmCmd(applyArgs(opts, "")...)); err != nil {
		return fmt.Errorf("helm install: %w", err)
	}

	// Assert the exact steady-state Fail policy once the MetalLB webhook is
	// present (DES-HOR-511-04): success requires a live fail-closed webhook
	// converged to `Fail`, never an absent/empty configuration.
	final, err := p.metalLBValidationPolicy(ctx, opts.Namespace)
	if err != nil {
		return fmt.Errorf("read metallb validation policy: %w", err)
	}
	if final != metalLBPolicyFail {
		return fmt.Errorf("metallb validation failurePolicy not converged to %s: got %q", metalLBPolicyFail, final)
	}
	return nil
}

func (p *SSHProvisioner) verifiedApplyOpts(ctx context.Context, opts deployer.ApplyOpts) (deployer.ApplyOpts, func(), error) {
	if opts.Checksum == "" {
		return opts, func() {}, nil
	}
	archive, cleanup, err := p.downloadVerifiedHelmChart(ctx, opts.Repository, opts.Version, opts.Checksum)
	if err != nil {
		return deployer.ApplyOpts{}, nil, err
	}
	opts.Repository = archive
	opts.Version = ""
	return opts, cleanup, nil
}

// releaseInstalled reports whether a helm release already exists for the target.
func (p *SSHProvisioner) releaseInstalled(ctx context.Context, opts deployer.ApplyOpts) (bool, error) {
	state, err := p.Status(ctx, opts.Release, opts.Namespace)
	if err != nil {
		return false, err
	}
	return state.Installed, nil
}

// waitMetalLBAdmissionBackend polls until the MetalLB controller deployment is
// available and the metallb-webhook-service has ready endpoints (or the timeout
// elapses). Admission probes return "not ready" rather than erroring while the
// controller is still installing.
func (p *SSHProvisioner) waitMetalLBAdmissionBackend(ctx context.Context, release, namespace string) error {
	deadline := time.Now().Add(metalLBBackendWaitTimeout)
	for {
		ready, err := p.metalLBAdmissionBackendReady(ctx, release, namespace)
		if err != nil {
			return fmt.Errorf("probe metallb admission backend: %w", err)
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("metallb admission backend not ready after %s", metalLBBackendWaitTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(metalLBBackendWaitInterval):
		}
	}
}

// applyArgs builds the helm upgrade --install argument list for opts, appending
// a bootstrap --set override when override is non-empty. A non-empty override is
// appended last so it wins over any -f value-file / earlier --set on the same key.
func applyArgs(opts deployer.ApplyOpts, override string) []string {
	timeout := opts.Timeout
	if timeout == "" {
		timeout = "10m"
	}
	args := []string{"upgrade", "--install", opts.Release, opts.Repository}
	if opts.Version != "" {
		args = append(args, "--version", opts.Version)
	}
	args = append(args,
		"-n", opts.Namespace,
		"--create-namespace",
		"--timeout", timeout,
	)
	if !opts.NoWait {
		args = append(args, "--wait")
	}
	for _, f := range opts.ValueFiles {
		args = append(args, "-f", f)
	}
	for _, v := range opts.Values {
		args = append(args, "--set", v)
	}
	if override != "" {
		args = append(args, "--set", override)
	}
	return args
}

// Status implements deployer.Deployer. A missing release is {Installed: false},
// not an error.
func (p *SSHProvisioner) Status(ctx context.Context, release, namespace string) (*deployer.ChartState, error) {
	if err := p.ensureHelm(ctx); err != nil {
		return nil, err
	}
	out, err := p.run(ctx, helmCmd("status", release, "-n", namespace, "-o", "json"))
	if err != nil {
		// Only Helm's exact release-absence result is authoritative absence.
		// Transport, API, RBAC, timeout, and all other observation errors must
		// remain errors so clean-install guards cannot mutate under uncertainty.
		if strings.TrimSpace(out) == "Error: release: not found" || strings.HasSuffix(strings.TrimSpace(err.Error()), "stderr: Error: release: not found") {
			return &deployer.ChartState{Installed: false}, nil
		}
		return nil, fmt.Errorf("helm status %s/%s: %w", namespace, release, err)
	}
	state, err := parseHelmStatus(out)
	if err != nil || state.Version != "" {
		return state, err
	}

	// Helm 4 removed chart metadata from `helm status` JSON. Resolve the exact
	// chart version through the dedicated metadata command while retaining the
	// live release status above. Helm 3 status still takes the fast path.
	out, err = p.run(ctx, helmCmd("get", "metadata", release, "-n", namespace, "-o", "json"))
	if err != nil {
		return nil, fmt.Errorf("helm get metadata: %w", err)
	}
	version, err := parseHelmMetadataVersion(out)
	if err != nil {
		return nil, err
	}
	state.Version = version
	return state, nil
}

const crdOwnershipJSONPath = `{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.meta\.helm\.sh/release-name}{"\t"}{.metadata.annotations.meta\.helm\.sh/release-namespace}{"\n"}{end}`

// metalLBWebhookConfigName is the ValidatingWebhookConfiguration the bundled
// metallb subchart renders; its failurePolicy mirrors metallb.crds.validationFailurePolicy.
const metalLBWebhookConfigName = "metallb-webhook-configuration"

const (
	// metalLBValidationPolicyValue is the helm --set key the bundled metallb
	// subchart maps to metallb-webhook-configuration's failurePolicy.
	metalLBValidationPolicyValue = "metallb.crds.validationFailurePolicy"
	// metalLBPolicyFail is the steady-state production validation failurePolicy.
	metalLBPolicyFail = "Fail"
	// metalLBPolicyIgnore is the bounded bootstrap-only override used while the
	// MetalLB admission webhook backend is still coming up on a fresh install.
	metalLBPolicyIgnore = "Ignore"
	// metalLBBackendWaitInterval is the poll period for the backend readiness probe.
	metalLBBackendWaitInterval = 5 * time.Second
)

// metalLBBackendWaitTimeout caps the fresh-install bootstrap wait for the
// MetalLB controller + webhook backend to be ready before converging. A package
// variable so tests can shorten it.
var metalLBBackendWaitTimeout = 10 * time.Minute

// CRDOwnedBy implements deployer.Deployer from live Helm ownership annotations.
func (p *SSHProvisioner) CRDOwnedBy(ctx context.Context, labelSelector, release, namespace string) (bool, error) {
	out, err := p.run(ctx, kubectlCmd("get", "crd", "-l", labelSelector, "-o", "jsonpath="+crdOwnershipJSONPath))
	if err != nil {
		return false, fmt.Errorf("read CRD ownership: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return false, nil
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[0] == "" || fields[1] != release || fields[2] != namespace {
			return false, nil
		}
	}
	return true, nil
}

// CRDsAnnotated implements deployer.Deployer from live CRD metadata.
func (p *SSHProvisioner) CRDsAnnotated(ctx context.Context, labelSelector, annotation, value string) (bool, error) {
	out, err := p.run(ctx, kubectlCmd("get", "crd", "-l", labelSelector, "-o", "json"))
	if err != nil {
		return false, fmt.Errorf("read CRD annotations: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return false, fmt.Errorf("decode CRD annotations: %w", err)
	}
	if len(list.Items) == 0 {
		return false, nil
	}
	for _, item := range list.Items {
		if item.Metadata.Annotations[annotation] != value {
			return false, nil
		}
	}
	return true, nil
}

// AnnotateCRDs implements deployer.Deployer with one idempotent kubectl
// annotation operation over the selected CRDs.
func (p *SSHProvisioner) AnnotateCRDs(ctx context.Context, labelSelector, annotation, value string) error {
	out, err := p.run(ctx, kubectlCmd("get", "crd", "-l", labelSelector, "-o", "name"))
	if err != nil {
		return fmt.Errorf("list CRDs for annotation: %w", err)
	}
	resources := strings.Fields(out)
	if len(resources) == 0 {
		return fmt.Errorf("no CRDs match annotation selector %q", labelSelector)
	}
	args := append([]string{"annotate", "--overwrite"}, resources...)
	args = append(args, annotation+"="+value)
	if _, err := p.run(ctx, kubectlCmd(args...)); err != nil {
		return fmt.Errorf("annotate CRDs: %w", err)
	}
	return nil
}

// TransferCertificateHookOwnership implements deployer.Deployer. Platform 0.2
// created ClusterIssuers and internal-CA Certificates as Helm hooks, so Helm did
// not attach release annotations. Platform 0.3 renders them as normal resources;
// annotate the existing objects before upgrade so Helm adopts them in place.
func (p *SSHProvisioner) TransferCertificateHookOwnership(ctx context.Context, labelSelector, release, namespace string) error {
	selections := []struct {
		kind      string
		namespace string
	}{
		{kind: "clusterissuer"},
		{kind: "certificate", namespace: namespace},
	}
	for _, selection := range selections {
		getArgs := []string{"get", selection.kind}
		if selection.namespace != "" {
			getArgs = append(getArgs, "-n", selection.namespace)
		}
		getArgs = append(getArgs, "-l", labelSelector, "-o", "name")
		out, err := p.run(ctx, kubectlCmd(getArgs...))
		if err != nil {
			return fmt.Errorf("list %s resources for ownership transfer: %w", selection.kind, err)
		}
		resources := strings.Fields(out)
		if len(resources) == 0 {
			continue
		}
		args := []string{"annotate", "--overwrite"}
		if selection.namespace != "" {
			args = append(args, "-n", selection.namespace)
		}
		args = append(args, resources...)
		args = append(args,
			"meta.helm.sh/release-name="+release,
			"meta.helm.sh/release-namespace="+namespace,
			"helm.sh/hook-",
			"helm.sh/hook-weight-",
		)
		if _, err := p.run(ctx, kubectlCmd(args...)); err != nil {
			return fmt.Errorf("transfer %s ownership to %s/%s: %w", selection.kind, namespace, release, err)
		}
	}
	return nil
}

// TransferCRDOwnership implements deployer.Deployer. Kept CRDs survive the old
// platform release's upgrade, but retain its Helm ownership annotations. Move
// only those metadata fields so the companion chart can adopt the same objects
// without deleting certificates or relying on Helm --take-ownership support.
func (p *SSHProvisioner) TransferCRDOwnership(ctx context.Context, labelSelector, release, namespace string) error {
	out, err := p.run(ctx, kubectlCmd("get", "crd", "-l", labelSelector, "-o", "name"))
	if err != nil {
		return fmt.Errorf("list CRDs for ownership transfer: %w", err)
	}
	resources := strings.Fields(out)
	if len(resources) == 0 {
		return fmt.Errorf("no CRDs match ownership-transfer selector %q", labelSelector)
	}
	args := append([]string{"annotate", "--overwrite"}, resources...)
	args = append(args,
		"meta.helm.sh/release-name="+release,
		"meta.helm.sh/release-namespace="+namespace,
	)
	if _, err := p.run(ctx, kubectlCmd(args...)); err != nil {
		return fmt.Errorf("transfer CRD ownership to %s/%s: %w", namespace, release, err)
	}
	return nil
}

// TransferMetalLBHookOwnership implements deployer.Deployer. The pre-DES-HOR-511
// platform created the IPAddressPool and L2Advertisement objects as Helm
// post-install/post-upgrade hooks, so they carry a helm.sh/hook annotation but
// no release ownership annotations. The current platform chart renders them as
// ordinary resources; annotate this release's existing objects before upgrade so
// Helm adopts them in place (preserving UIDs and desired spec/status) instead of
// deleting and recreating them. It is idempotent and non-destructive: only the
// ownership/hook metadata changes, and it is a no-op when no matching objects
// (or the MetalLB CRDs themselves) exist.
func (p *SSHProvisioner) TransferMetalLBHookOwnership(ctx context.Context, release, namespace string) error {
	for _, kind := range []string{"ipaddresspool", "l2advertisement"} {
		out, err := p.run(ctx, kubectlCmd("get", kind, "-n", namespace,
			"-l", "app.kubernetes.io/instance="+release, "-o", "name"))
		if err != nil {
			// Best-effort idempotent migration: a cloud install (or an older chart
			// that never installed the MetalLB CRDs) has no such kind/objects, so a
			// list failure is a valid no-op. A genuinely needed adoption still fails
			// loudly at the subsequent Helm upgrade if it cannot run.
			continue
		}
		resources := strings.Fields(out)
		if len(resources) == 0 {
			continue
		}
		args := append([]string{"annotate", "--overwrite", "-n", namespace}, resources...)
		args = append(args,
			"meta.helm.sh/release-name="+release,
			"meta.helm.sh/release-namespace="+namespace,
			"helm.sh/hook-",
			"helm.sh/hook-weight-",
		)
		if _, err := p.run(ctx, kubectlCmd(args...)); err != nil {
			return fmt.Errorf("transfer %s ownership to %s/%s: %w", kind, namespace, release, err)
		}
	}
	return nil
}

// metalLBValidationPolicy reads the failurePolicy of the MetalLB admission
// webhook configuration (set from metallb.crds.validationFailurePolicy by the
// bundled metallb subchart). Returns "" when the webhook configuration does
// not exist (e.g. MetalLB disabled).
func (p *SSHProvisioner) metalLBValidationPolicy(ctx context.Context, namespace string) (string, error) {
	out, err := p.run(ctx, kubectlCmd("get", "validatingwebhookconfiguration", metalLBWebhookConfigName,
		"-o", "jsonpath={.webhooks[0].failurePolicy}"))
	if err != nil {
		// A genuinely absent webhook configuration (not yet wired by MetalLB) is
		// represented as empty, distinct from a real read failure which is
		// propagated so callers don't mistake it for convergence.
		detail := err.Error() + "\n" + out
		if strings.Contains(detail, "NotFound") || strings.Contains(strings.ToLower(detail), "not found") {
			return "", nil
		}
		return "", fmt.Errorf("read metallb validation policy: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// metalLBAdmissionBackendReady reports whether the MetalLB admission webhook
// backend is serving so a fresh-install bootstrap can converge the release back
// to a Fail validation policy safely. Returns true only when the metallb
// controller Deployment is Available (it serves the webhook) and the
// metallb-webhook-service has at least one ready endpoint.
func (p *SSHProvisioner) metalLBAdmissionBackendReady(ctx context.Context, release, namespace string) (bool, error) {
	replicas, err := p.run(ctx, kubectlCmd("get", "deployment", "-n", namespace,
		"-l", "app.kubernetes.io/instance="+release+",app.kubernetes.io/name=metallb,app.kubernetes.io/component=controller",
		"-o", "jsonpath={.items[0].status.readyReplicas}"))
	if err != nil || strings.TrimSpace(replicas) == "" || strings.TrimSpace(replicas) == "0" {
		return false, nil
	}
	endpoints, err := p.run(ctx, kubectlCmd("get", "endpoints", "-n", namespace, "metallb-webhook-service",
		"-o", "jsonpath={.subsets[*].addresses[*].ip}"))
	if err != nil || strings.TrimSpace(endpoints) == "" {
		return false, nil
	}
	return true, nil
}

// RestartDeployment implements deployer.Deployer for the one migration seam
// where a ConfigMap must become live before Helm can wait on its consumer.
func (p *SSHProvisioner) RestartDeployment(ctx context.Context, labelSelector, namespace string) error {
	selector := "-l=" + labelSelector
	if _, err := p.run(ctx, kubectlCmd("rollout", "restart", "deployment", "-n", namespace, selector)); err != nil {
		return fmt.Errorf("restart deployment %q in %s: %w", labelSelector, namespace, err)
	}
	if _, err := p.run(ctx, kubectlCmd("rollout", "status", "deployment", "-n", namespace, selector, "--timeout=5m")); err != nil {
		return fmt.Errorf("wait for deployment %q in %s: %w", labelSelector, namespace, err)
	}
	return nil
}

// UninstallChart implements deployer.Deployer. A missing release is an
// idempotent success, but Helm absence, status failures, failed hooks, and
// uninstall failures are propagated so stateful callers can preserve the
// cluster instead of bypassing a retained-data refusal.
func (p *SSHProvisioner) UninstallChart(ctx context.Context, release, namespace string) error {
	if _, err := p.run(ctx, "command -v helm"); err != nil {
		return fmt.Errorf("uninstall Helm release %s/%s: helm is unavailable: %w", namespace, release, err)
	}
	statusOutput, err := p.run(ctx, helmCmd("status", release, "-n", namespace))
	if err != nil {
		detail := strings.ToLower(statusOutput + "\n" + err.Error())
		if strings.Contains(detail, "release: not found") || strings.Contains(detail, "release not found") {
			return nil
		}
		return fmt.Errorf("read Helm release %s/%s before uninstall: %w", namespace, release, err)
	}
	output, err := p.run(ctx, helmCmd("uninstall", release, "-n", namespace, "--wait"))
	if err != nil {
		return fmt.Errorf("uninstall Helm release %s/%s: %w; output: %s", namespace, release, err, output)
	}
	return nil
}

// EnsureRepo implements deployer.Deployer: idempotent `helm repo add
// --force-update`. Ensures helm first. Used for repo-based charts (the NVIDIA
// GPU Operator); a no-op concern for OCI charts like the platform chart.
func (p *SSHProvisioner) EnsureRepo(ctx context.Context, name, url string) error {
	if err := p.ensureHelm(ctx); err != nil {
		return err
	}
	if _, err := p.run(ctx, helmCmd("repo", "add", "--force-update", name, url)); err != nil {
		return fmt.Errorf("helm repo add %s: %w", name, err)
	}
	return nil
}

// kubectlCmd builds a sudo k3s kubectl command (uses the k3s kubeconfig
// automatically, like NodeReady/GPUReady).
func kubectlCmd(args ...string) string {
	return joinArgs(append([]string{"sudo", "k3s", "kubectl"}, args...))
}

// ApplyKustomize implements deployer.Deployer: `kubectl apply -k dir` against
// the k3s kubeconfig on the host. Used for overlay CRD instances (kubectl apply
// -k crds/client/), after the chart so the CRD kinds exist. The dir is rendered
// first with `kubectl kustomize`; an empty build (e.g. an overlay scaffold with
// no instances) is a no-op — `kubectl apply -k` errors with "no objects passed
// to apply" on an empty build. Idempotent.
func (p *SSHProvisioner) ApplyKustomize(ctx context.Context, dir string) error {
	rendered, err := p.run(ctx, kubectlCmd("kustomize", dir))
	if err != nil {
		return fmt.Errorf("kubectl kustomize: %w", err)
	}
	if strings.TrimSpace(rendered) == "" {
		return nil // no objects to apply (empty overlay scaffold)
	}
	if _, err := p.run(ctx, kubectlCmd("apply", "-k", dir)); err != nil {
		return fmt.Errorf("kubectl apply -k: %w", err)
	}
	return nil
}

// DeleteKustomize implements deployer.Deployer: `kubectl delete -k dir`
// (best-effort; for destroy). A missing resource is not an error.
func (p *SSHProvisioner) DeleteKustomize(ctx context.Context, dir string) error {
	_, _ = p.run(ctx, kubectlCmd("delete", "-k", dir, "--ignore-not-found=true"))
	return nil
}

// ApplyManifest implements deployer.Deployer: `kubectl apply -f -` against the
// k3s kubeconfig on the host, with the manifest piped over SSH stdin. The
// command string never contains the manifest, so secret values (stringData) are
// never exposed in ps/history. Used by the secret-sync phase to ensure
// namespaces + materialize Secrets. Idempotent (kubectl apply).
func (p *SSHProvisioner) ApplyManifest(ctx context.Context, manifest string) error {
	if _, err := p.runStdin(ctx, kubectlCmd("apply", "-f", "-"), manifest); err != nil {
		return fmt.Errorf("kubectl apply -f -: %w", err)
	}
	return nil
}

type helmStatusJSON struct {
	Info struct {
		Status string `json:"status"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Version string `json:"version"`
		} `json:"metadata"`
	} `json:"chart"`
}

type helmMetadataJSON struct {
	Version string `json:"version"`
}

func parseHelmStatus(out string) (*deployer.ChartState, error) {
	var hs helmStatusJSON
	if err := json.Unmarshal([]byte(out), &hs); err != nil {
		return nil, fmt.Errorf("parse helm status: %w", err)
	}
	return &deployer.ChartState{
		Installed: true,
		Status:    hs.Info.Status,
		Version:   hs.Chart.Metadata.Version,
	}, nil
}

func parseHelmMetadataVersion(out string) (string, error) {
	var metadata helmMetadataJSON
	if err := json.Unmarshal([]byte(out), &metadata); err != nil {
		return "", fmt.Errorf("parse helm metadata: %w", err)
	}
	if metadata.Version == "" {
		return "", fmt.Errorf("parse helm metadata: chart version is empty")
	}
	return metadata.Version, nil
}
