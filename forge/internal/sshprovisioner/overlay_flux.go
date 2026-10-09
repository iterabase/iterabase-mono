package sshprovisioner

import (
	"context"
	"fmt"
	"net"
	"path"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/overlayer"
)

// A file:// overlay lives on the host, where the in-cluster Flux
// source-controller cannot read it. Forge serves it over read-only SSH from the
// node itself (DES-HOR-632-01): a bare mirror of the overlay ref, owned by a
// dedicated unprivileged user whose only authorized key is restricted to
// git-upload-pack on that one mirror and to connections from the pod CIDRs.
const (
	fluxOverlayUser   = "iterabase-overlay"
	fluxOverlayRoot   = "/var/lib/iterabase-overlay-source"
	fluxOverlayMirror = fluxOverlayRoot + "/overlay.git"
	fluxSourceMarker  = "FORGE_FLUX_SOURCE"
)

var overlayRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// ServeToFlux implements overlayer.Overlayer.
func (p *SSHProvisioner) ServeToFlux(ctx context.Context, repo, ref, publicKey string, podCIDRs []string) (overlayer.FluxSSHSource, error) {
	script, err := fluxOverlayServeScript(repo, ref, publicKey, podCIDRs)
	if err != nil {
		return overlayer.FluxSSHSource{}, err
	}
	out, err := p.runStdin(ctx, "sudo bash -s", script)
	if err != nil {
		return overlayer.FluxSSHSource{}, fmt.Errorf("serve overlay to flux: %w", err)
	}
	return parseFluxSSHSource(out)
}

// StopServingToFlux implements overlayer.Overlayer.
func (p *SSHProvisioner) StopServingToFlux(ctx context.Context) error {
	_, err := p.run(ctx, fmt.Sprintf("sudo userdel %s 2>/dev/null; sudo rm -rf %s", fluxOverlayUser, shellQuote(fluxOverlayRoot)))
	return err
}

// fluxOverlayServeScript renders the idempotent host script. Every input is
// validated here because the script runs as root.
func fluxOverlayServeScript(repo, ref, publicKey string, podCIDRs []string) (string, error) {
	source, ok := strings.CutPrefix(repo, "file://")
	if !ok || !path.IsAbs(source) || path.Clean(source) != source {
		return "", fmt.Errorf("overlay.repo %q is not an absolute, clean file:// path", repo)
	}
	if !overlayRefPattern.MatchString(ref) || strings.Contains(ref, "..") {
		return "", fmt.Errorf("overlay.ref %q is not a plain branch or tag name", ref)
	}
	parsed, comment, _, rest, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil || len(strings.TrimSpace(string(rest))) != 0 || parsed.Type() != ssh.KeyAlgoED25519 || comment == "" {
		return "", fmt.Errorf("flux overlay key must be one commented ssh-ed25519 public key")
	}
	if len(podCIDRs) == 0 {
		return "", fmt.Errorf("flux overlay access needs the cluster pod CIDRs")
	}
	for _, cidr := range podCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return "", fmt.Errorf("pod CIDR %q: %w", cidr, err)
		}
	}
	authorized := fmt.Sprintf(`restrict,from="%s",command="git-upload-pack '%s'" %s %s`,
		strings.Join(podCIDRs, ","), fluxOverlayMirror,
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(parsed))), comment)

	return fmt.Sprintf(`set -euo pipefail
user=%[1]s
root=%[2]s
mirror=%[3]s
source=%[4]s
ref=%[5]s

if ! id -u "$user" >/dev/null 2>&1; then
  useradd --system --home-dir "$root" --no-create-home --shell /usr/bin/git-shell "$user"
fi
# Converge an existing user too (an earlier apply may have used another home).
# No password and not locked: sshd accepts only the restricted key below.
usermod -d "$root" -s /usr/bin/git-shell -p '*' "$user"
install -d -o root -g root -m 0755 "$root" "$root/.ssh"

# Refresh the bare mirror of exactly the configured ref, then swap it in.
tmp=$(mktemp -d "$root/.mirror.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
git -c safe.directory='*' clone --quiet --bare --single-branch --branch "$ref" "file://$source" "$tmp/overlay.git"
chown -R "$user:$user" "$tmp/overlay.git"
rm -rf "$mirror.old"
if [ -e "$mirror" ]; then mv "$mirror" "$mirror.old"; fi
mv "$tmp/overlay.git" "$mirror"
rm -rf "$mirror.old"

umask 022
printf '%%s\n' %[6]s > "$root/.ssh/authorized_keys.next"
mv -f "$root/.ssh/authorized_keys.next" "$root/.ssh/authorized_keys"

# sshd reads authorized_keys as the target user, and git-upload-pack runs as
# it: prove both are reachable now rather than as a Flux authentication timeout.
runuser -u "$user" -- test -r "$root/.ssh/authorized_keys" || { echo "$user cannot read $root/.ssh/authorized_keys" >&2; exit 1; }
runuser -u "$user" -- git --git-dir="$mirror" rev-parse --verify --quiet "$ref^{commit}" >/dev/null || { echo "$user cannot read ref $ref in $mirror" >&2; exit 1; }

addresses=$(k3s kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
hostkey=$(awk '{print $1" "$2}' /etc/ssh/ssh_host_ed25519_key.pub)
printf '%[7]s\t%%s\t%%s\n' "$addresses" "$hostkey"
`, fluxOverlayUser, fluxOverlayRoot, fluxOverlayMirror, shellQuote(source), shellQuote(ref), shellQuote(authorized), fluxSourceMarker), nil
}

// parseFluxSSHSource reads the marker line: the node's InternalIP addresses
// (IPv4 preferred) and its ed25519 host key.
func parseFluxSSHSource(out string) (overlayer.FluxSSHSource, error) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 3 || fields[0] != fluxSourceMarker {
			continue
		}
		address := ""
		for _, candidate := range strings.Fields(fields[1]) {
			ip := net.ParseIP(candidate)
			if ip == nil {
				continue
			}
			if ip.To4() != nil {
				address = candidate
				break
			}
			if address == "" {
				address = candidate
			}
		}
		if address == "" {
			return overlayer.FluxSSHSource{}, fmt.Errorf("node has no InternalIP for the flux overlay source: %q", fields[1])
		}
		hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(fields[2]))
		if err != nil || hostKey.Type() != ssh.KeyAlgoED25519 {
			return overlayer.FluxSSHSource{}, fmt.Errorf("node has no ed25519 SSH host key for the flux overlay source")
		}
		host := address
		if strings.Contains(address, ":") {
			host = "[" + address + "]"
		}
		return overlayer.FluxSSHSource{
			URL:        fmt.Sprintf("ssh://%s@%s%s", fluxOverlayUser, host, fluxOverlayMirror),
			KnownHosts: address + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey))),
		}, nil
	}
	return overlayer.FluxSSHSource{}, fmt.Errorf("flux overlay source script reported no node address")
}
