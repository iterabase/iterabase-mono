package sshprovisioner

import (
	"crypto/ed25519"
	"crypto/rand"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

func testFluxPublicKey(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	key, err := ssh.NewPublicKey(public)
	require.NoError(t, err)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " forge-flux-overlay"
}

func TestFluxOverlayServeScriptRestrictsTheKey(t *testing.T) {
	publicKey := testFluxPublicKey(t)
	script, err := fluxOverlayServeScript("file:///srv/overlay", "e2e", publicKey, []string{"10.42.0.0/16", "fd00:42::/56"})
	require.NoError(t, err)

	// DES-HOR-632-01: one key, no shell/forwarding, read-only on one mirror, pod
	// CIDRs only.
	want := `restrict,from="10.42.0.0/16,fd00:42::/56",command="git-upload-pack '/var/lib/iterabase-overlay-source/overlay.git'" ` + publicKey
	assert.Contains(t, script, shellQuote(want))
	assert.Contains(t, script, `--shell /usr/bin/git-shell`)
	assert.Contains(t, script, `usermod -p '*' "$user"`)
	assert.Contains(t, script, `clone --quiet --bare --single-branch --branch "$ref" "file://$source"`)
	assert.Contains(t, script, `source='/srv/overlay'`)
	assert.Contains(t, script, `ref='e2e'`)
	assert.Contains(t, script, `> "$root/.ssh/authorized_keys.next"`)
	assert.NotContains(t, script, "authorized_keys >>", "the key is replaced, never appended")
	assert.Contains(t, script, `runuser -u "$user" -- test -r "$root/.ssh/authorized_keys"`, "sshd reads the key as the user")
	assert.NotContains(t, script, "/var/lib/iterabase/", "/var/lib/iterabase is root-only (data-storage receipt)")

	out, err := exec.Command("bash", "-n", "-c", script).CombinedOutput()
	require.NoError(t, err, "script is not valid bash: %s", out)
}

func TestFluxOverlayServeScriptRejectsUnsafeInputs(t *testing.T) {
	key := testFluxPublicKey(t)
	cidrs := []string{"10.42.0.0/16"}
	for name, tc := range map[string]struct {
		repo, ref, key string
		cidrs          []string
	}{
		"https repo":        {"https://github.com/example/overlay.git", "main", key, cidrs},
		"relative path":     {"file://srv/overlay", "main", key, cidrs},
		"unclean path":      {"file:///srv/../etc", "main", key, cidrs},
		"ref with spaces":   {"file:///srv/overlay", "main; rm -rf /", key, cidrs},
		"ref with dotdot":   {"file:///srv/overlay", "a..b", key, cidrs},
		"option-like ref":   {"file:///srv/overlay", "-uhelp", key, cidrs},
		"rsa key":           {"file:///srv/overlay", "main", "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC7 forge", cidrs},
		"uncommented key":   {"file:///srv/overlay", "main", strings.TrimSuffix(key, " forge-flux-overlay"), cidrs},
		"two keys":          {"file:///srv/overlay", "main", key + "\n" + key, cidrs},
		"no pod cidrs":      {"file:///srv/overlay", "main", key, nil},
		"invalid pod cidr":  {"file:///srv/overlay", "main", key, []string{"10.42.0.0"}},
		"injected pod cidr": {"file:///srv/overlay", "main", key, []string{`10.42.0.0/16" ,command="sh`}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := fluxOverlayServeScript(tc.repo, tc.ref, tc.key, tc.cidrs)
			require.Error(t, err)
		})
	}
}

func TestParseFluxSSHSource(t *testing.T) {
	hostKey := strings.TrimSuffix(testFluxPublicKey(t), " forge-flux-overlay")
	source, err := parseFluxSSHSource("noise\nFORGE_FLUX_SOURCE\tfd00::5 172.31.20.168\t" + hostKey + "\n")
	require.NoError(t, err)
	assert.Equal(t, "ssh://iterabase-overlay@172.31.20.168/var/lib/iterabase-overlay-source/overlay.git", source.URL,
		"IPv4 is preferred when the node is dual-stack")
	assert.Equal(t, "172.31.20.168 "+hostKey, source.KnownHosts)

	source, err = parseFluxSSHSource("FORGE_FLUX_SOURCE\tfd00::5\t" + hostKey)
	require.NoError(t, err)
	assert.Equal(t, "ssh://iterabase-overlay@[fd00::5]/var/lib/iterabase-overlay-source/overlay.git", source.URL)
	assert.Equal(t, "fd00::5 "+hostKey, source.KnownHosts)

	for name, out := range map[string]string{
		"no marker":     "nothing here",
		"no address":    "FORGE_FLUX_SOURCE\t\t" + hostKey,
		"not an ip":     "FORGE_FLUX_SOURCE\tnode-1\t" + hostKey,
		"no host key":   "FORGE_FLUX_SOURCE\t10.0.0.5\t",
		"rsa host key":  "FORGE_FLUX_SOURCE\t10.0.0.5\tssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC7",
		"missing field": "FORGE_FLUX_SOURCE\t10.0.0.5",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseFluxSSHSource(out)
			require.Error(t, err)
		})
	}
}
