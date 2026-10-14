package sshprovisioner

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

const (
	k3sInstallScriptURL    = "https://raw.githubusercontent.com/k3s-io/k3s/2977c525a2e7a487886107ce4df43630ae9b03b2/install.sh"
	k3sInstallScriptSHA256 = "e5cc3b3d9dfc1662c2d9be6da5abc9a4cd317d6abc3a5ffc02e3dd3248207fee"
	helmInstallVersion     = "v4.2.3"
)

type reviewedRemoteTool struct {
	name            string
	version         string
	platform        string
	url             string
	sha256          string
	archiveMember   string
	installedSHA256 string
	installPath     string
	installMode     string
}

var reviewedRemoteTools = []reviewedRemoteTool{
	{name: "k3s", version: "v1.31.5+k3s1", platform: "linux-amd64", url: "https://github.com/k3s-io/k3s/releases/download/v1.31.5%2Bk3s1/k3s", sha256: "399b87b432ce55013fa81adad572a8e4ecf56e0df97369cf02d4b8a41f039091", installedSHA256: "399b87b432ce55013fa81adad572a8e4ecf56e0df97369cf02d4b8a41f039091", installPath: "/usr/local/bin/k3s", installMode: "0755"},
	{name: "k3s", version: "v1.31.5+k3s1", platform: "linux-arm64", url: "https://github.com/k3s-io/k3s/releases/download/v1.31.5%2Bk3s1/k3s-arm64", sha256: "b719566c43ab1379fe2a7ce477e02bc1c79ea106bdaa7223fa9f6e19a735b477", installedSHA256: "b719566c43ab1379fe2a7ce477e02bc1c79ea106bdaa7223fa9f6e19a735b477", installPath: "/usr/local/bin/k3s", installMode: "0755"},
	{name: "k3s", version: "v1.34.10+k3s1", platform: "linux-amd64", url: "https://github.com/k3s-io/k3s/releases/download/v1.34.10%2Bk3s1/k3s", sha256: "e63a3511b2603fd1436a1ea8d228348a3b47334b45024801d41a8c0e2d22e8c4", installedSHA256: "e63a3511b2603fd1436a1ea8d228348a3b47334b45024801d41a8c0e2d22e8c4", installPath: "/usr/local/bin/k3s", installMode: "0755"},
	{name: "k3s", version: "v1.34.10+k3s1", platform: "linux-arm64", url: "https://github.com/k3s-io/k3s/releases/download/v1.34.10%2Bk3s1/k3s-arm64", sha256: "e96eb8b095600cb790f776ae3c619d266ad83a3c3d7b56166f8f2605ca898d65", installedSHA256: "e96eb8b095600cb790f776ae3c619d266ad83a3c3d7b56166f8f2605ca898d65", installPath: "/usr/local/bin/k3s", installMode: "0755"},
	{name: "k3s-images", version: "v1.31.5+k3s1", platform: "linux-amd64", url: "https://github.com/k3s-io/k3s/releases/download/v1.31.5%2Bk3s1/k3s-airgap-images-amd64.tar.zst", sha256: "94f127423c8dd3fe5834559d2c88f1afde7681ce36f139c82b5e46d0c2b33ba3", installedSHA256: "94f127423c8dd3fe5834559d2c88f1afde7681ce36f139c82b5e46d0c2b33ba3", installPath: "/var/lib/rancher/k3s/agent/images/iterabase-reviewed-k3s-images.tar.zst", installMode: "0644"},
	{name: "k3s-images", version: "v1.31.5+k3s1", platform: "linux-arm64", url: "https://github.com/k3s-io/k3s/releases/download/v1.31.5%2Bk3s1/k3s-airgap-images-arm64.tar.zst", sha256: "f848ede8c5e945312412efb46e1e02bf2bcbf5c190afcf7eb599c9ce11ede587", installedSHA256: "f848ede8c5e945312412efb46e1e02bf2bcbf5c190afcf7eb599c9ce11ede587", installPath: "/var/lib/rancher/k3s/agent/images/iterabase-reviewed-k3s-images.tar.zst", installMode: "0644"},
	{name: "k3s-images", version: "v1.34.10+k3s1", platform: "linux-amd64", url: "https://github.com/k3s-io/k3s/releases/download/v1.34.10%2Bk3s1/k3s-airgap-images-amd64.tar.zst", sha256: "7e59087b6a72e00db45a3bcb4f144114b13eb72dc8947c2b7908a0e1482b42b3", installedSHA256: "7e59087b6a72e00db45a3bcb4f144114b13eb72dc8947c2b7908a0e1482b42b3", installPath: "/var/lib/rancher/k3s/agent/images/iterabase-reviewed-k3s-images.tar.zst", installMode: "0644"},
	{name: "k3s-images", version: "v1.34.10+k3s1", platform: "linux-arm64", url: "https://github.com/k3s-io/k3s/releases/download/v1.34.10%2Bk3s1/k3s-airgap-images-arm64.tar.zst", sha256: "dc1a8c0d10a40f2d0dc1d53c5c42f2b4e00a996b1573227bcbf2855c644d3025", installedSHA256: "dc1a8c0d10a40f2d0dc1d53c5c42f2b4e00a996b1573227bcbf2855c644d3025", installPath: "/var/lib/rancher/k3s/agent/images/iterabase-reviewed-k3s-images.tar.zst", installMode: "0644"},
	{name: "helm", version: "v4.2.3", platform: "linux-amd64", url: "https://get.helm.sh/helm-v4.2.3-linux-amd64.tar.gz", sha256: "e9b88b4ee95b18c706839c28d3a0220e5bc470e9cd9262410c90793c45ff8b7c", archiveMember: "linux-amd64/helm", installedSHA256: "aeb4645b9e6658948efa290e28dd23ae75a16fb73f137942f2294fd5c7fcb573", installPath: "/usr/local/bin/helm", installMode: "0755"},
	{name: "helm", version: "v4.2.3", platform: "linux-arm64", url: "https://get.helm.sh/helm-v4.2.3-linux-arm64.tar.gz", sha256: "21abd9354d39b2cd79a8d76be6912cd137a983cbf997193503fb8a6a6e2f2785", archiveMember: "linux-arm64/helm", installedSHA256: "01bdd0c90f371968326162daaa427cdd14da2641ded094131afc44fb7a538b62", installPath: "/usr/local/bin/helm", installMode: "0755"},
}

var canonicalSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// downloadVerifiedRemoteContent downloads one reviewed input without executing
// or extracting it, then compares the exact repository-owned SHA-256 on-host.
// A transport retry may fetch the same URL again; a byte mismatch is terminal.
func (p *SSHProvisioner) downloadVerifiedRemoteContent(ctx context.Context, prefix, url, checksum string) (string, error) {
	if !strings.HasPrefix(url, "https://") || !canonicalSHA256.MatchString(checksum) {
		return "", fmt.Errorf("invalid remote content identity for %s", prefix)
	}
	path, err := p.run(ctx, "mktemp /tmp/forge-"+prefix+".XXXXXX")
	if err != nil {
		return "", fmt.Errorf("prepare %s download: %w", prefix, err)
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("prepare %s download: mktemp returned an empty path", prefix)
	}
	command := fmt.Sprintf(
		"curl -fsSL --retry 4 --retry-delay 2 --retry-all-errors --connect-timeout 10 -o %s %s",
		shellQuote(path), shellQuote(url),
	)
	if _, err := p.run(ctx, command); err != nil {
		_, _ = p.run(ctx, "rm -f "+shellQuote(path))
		return "", fmt.Errorf("download %s: %w", prefix, err)
	}
	verify := fmt.Sprintf("printf '%%s  %%s\\n' %s %s | sha256sum --check --status", shellQuote(checksum), shellQuote(path))
	if _, err := p.run(ctx, verify); err != nil {
		_, _ = p.run(ctx, "rm -f "+shellQuote(path))
		return "", fmt.Errorf("verify %s content checksum: %w", prefix, err)
	}
	return path, nil
}

func (p *SSHProvisioner) removeRemoteContent(ctx context.Context, path string) {
	_, _ = p.run(ctx, "rm -f "+shellQuote(path))
}

func (p *SSHProvisioner) remoteToolPlatform(ctx context.Context) (string, error) {
	out, err := p.run(ctx, "uname -m")
	if err != nil {
		return "", fmt.Errorf("detect remote tool platform: %w", err)
	}
	switch strings.TrimSpace(out) {
	case "x86_64", "amd64":
		return "linux-amd64", nil
	case "aarch64", "arm64":
		return "linux-arm64", nil
	default:
		return "", fmt.Errorf("unsupported remote tool architecture %q", strings.TrimSpace(out))
	}
}

func reviewedTool(name, version, platform string) (reviewedRemoteTool, error) {
	for _, tool := range reviewedRemoteTools {
		if tool.name == name && tool.version == version && tool.platform == platform {
			return tool, nil
		}
	}
	return reviewedRemoteTool{}, fmt.Errorf("%s %s has no repository-reviewed %s content identity", name, version, platform)
}

// installReviewedRemoteTool verifies the downloaded archive or executable before
// extracting or placing any bytes in the privileged executable/runtime path.
func (p *SSHProvisioner) installReviewedRemoteTool(ctx context.Context, name, version string) (reviewedRemoteTool, error) {
	platform, err := p.remoteToolPlatform(ctx)
	if err != nil {
		return reviewedRemoteTool{}, err
	}
	tool, err := reviewedTool(name, version, platform)
	if err != nil {
		return reviewedRemoteTool{}, err
	}
	artifact, err := p.downloadVerifiedRemoteContent(ctx, name, tool.url, tool.sha256)
	if err != nil {
		return reviewedRemoteTool{}, err
	}
	defer p.removeRemoteContent(ctx, artifact)
	source := artifact
	if tool.archiveMember != "" {
		directory, err := p.run(ctx, "mktemp -d /tmp/forge-"+name+".XXXXXX")
		if err != nil {
			return reviewedRemoteTool{}, fmt.Errorf("prepare %s extraction: %w", name, err)
		}
		directory = strings.TrimSpace(directory)
		if directory == "" {
			return reviewedRemoteTool{}, fmt.Errorf("prepare %s extraction: mktemp returned an empty path", name)
		}
		defer func() { _, _ = p.run(ctx, "rm -rf "+shellQuote(directory)) }()
		if _, err := p.run(ctx, fmt.Sprintf("tar -xzf %s -C %s %s", shellQuote(artifact), shellQuote(directory), shellQuote(tool.archiveMember))); err != nil {
			return reviewedRemoteTool{}, fmt.Errorf("extract reviewed %s archive: %w", name, err)
		}
		source = directory + "/" + tool.archiveMember
		verify := fmt.Sprintf("printf '%%s  %%s\\n' %s %s | sha256sum --check --status", shellQuote(tool.installedSHA256), shellQuote(source))
		if _, err := p.run(ctx, verify); err != nil {
			return reviewedRemoteTool{}, fmt.Errorf("verify extracted %s executable: %w", name, err)
		}
	}
	parent := tool.installPath[:strings.LastIndex(tool.installPath, "/")]
	command := fmt.Sprintf("sudo install -d %s && sudo install -m %s %s %s", shellQuote(parent), shellQuote(tool.installMode), shellQuote(source), shellQuote(tool.installPath))
	if _, err := p.run(ctx, command); err != nil {
		return reviewedRemoteTool{}, fmt.Errorf("install reviewed %s: %w", name, err)
	}
	verify := fmt.Sprintf("printf '%%s  %%s\\n' %s %s | sudo sha256sum --check --status", shellQuote(tool.installedSHA256), shellQuote(tool.installPath))
	if _, err := p.run(ctx, verify); err != nil {
		return reviewedRemoteTool{}, fmt.Errorf("verify installed %s executable: %w", name, err)
	}
	return tool, nil
}

func (p *SSHProvisioner) installedReviewedTool(ctx context.Context, name, path string) (reviewedRemoteTool, bool, error) {
	if _, err := p.run(ctx, "sudo test -f "+shellQuote(path)); err != nil {
		return reviewedRemoteTool{}, false, nil
	}
	platform, err := p.remoteToolPlatform(ctx)
	if err != nil {
		return reviewedRemoteTool{}, false, err
	}
	out, err := p.run(ctx, "sudo sha256sum "+shellQuote(path)+" | awk '{print $1}'")
	if err != nil {
		return reviewedRemoteTool{}, false, fmt.Errorf("hash installed %s: %w", name, err)
	}
	checksum := strings.TrimSpace(out)
	for _, tool := range reviewedRemoteTools {
		if tool.name == name && tool.platform == platform && tool.installedSHA256 == checksum {
			return tool, true, nil
		}
	}
	return reviewedRemoteTool{}, false, fmt.Errorf("installed %s does not match a repository-reviewed executable", name)
}

// downloadVerifiedHelmChart resolves a repo-index chart into a local archive,
// then verifies repository-owned bytes before Helm may inspect or apply it.
func (p *SSHProvisioner) downloadVerifiedHelmChart(ctx context.Context, repository, version, checksum string) (string, func(), error) {
	if !canonicalSHA256.MatchString(checksum) || version == "" || !strings.Contains(repository, "/") {
		return "", nil, fmt.Errorf("invalid Helm chart content identity")
	}
	directory, err := p.run(ctx, "mktemp -d /tmp/forge-chart.XXXXXX")
	if err != nil {
		return "", nil, fmt.Errorf("prepare verified Helm chart: %w", err)
	}
	directory = strings.TrimSpace(directory)
	cleanup := func() { _, _ = p.run(ctx, "sudo rm -rf "+shellQuote(directory)) }
	if directory == "" {
		cleanup()
		return "", nil, fmt.Errorf("prepare verified Helm chart: mktemp returned an empty path")
	}
	if _, err := p.run(ctx, helmCmd("pull", repository, "--version", version, "--destination", directory)); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("download verified Helm chart: %w", err)
	}
	chart := repository[strings.LastIndex(repository, "/")+1:]
	archive := directory + "/" + chart + "-" + version + ".tgz"
	verify := fmt.Sprintf("printf '%%s  %%s\\n' %s %s | sha256sum --check --status", shellQuote(checksum), shellQuote(archive))
	if _, err := p.run(ctx, verify); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("verify Helm chart content checksum: %w", err)
	}
	return archive, cleanup, nil
}
