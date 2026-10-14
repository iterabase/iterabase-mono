package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/hosted"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/lifecycle"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/sshprovisioner"
)

func TestEBSDevice(t *testing.T) {
	assert.Equal(t, "/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0abc", ebsDevice("vol-0abc"))
	assert.Equal(t, "/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0123456789abcdef0", ebsDevice("vol-0123456789abcdef0"))
}

func TestWithoutProviderID(t *testing.T) {
	in := []string{"--kubelet-arg=max-pods=200", "--kubelet-arg=provider-id=aws:///eu-west-1a/i-old", "--disable-cloud-controller"}
	out := withoutProviderID(in)
	assert.Equal(t, []string{"--kubelet-arg=max-pods=200", "--disable-cloud-controller"}, out)
	assert.Len(t, in, 3, "input is not modified")
	assert.Empty(t, withoutProviderID(nil))
}

func hostKeyLine(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	key, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " root@ip-10-0-0-5", key
}

func TestWriteEnvironmentSSH(t *testing.T) {
	dir := t.TempDir()
	line, key := hostKeyLine(t)
	outs := &hosted.Outputs{InstanceID: "i-0abc", HostPublicKey: line, ClientPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nkey\n"}
	keyPath, trustPath, err := writeEnvironmentSSH(dir, outs)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "ssh", "id_ed25519"), keyPath)
	assert.Equal(t, filepath.Join(dir, "ssh", "known_hosts"), trustPath)

	for _, p := range []string{keyPath, trustPath} {
		info, err := os.Stat(p)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), p)
	}
	info, err := os.Stat(filepath.Join(dir, "ssh"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	got, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	assert.Equal(t, outs.ClientPrivateKey, string(got))
	trust, err := os.ReadFile(trustPath)
	require.NoError(t, err)
	fields := strings.Fields(line)
	assert.Equal(t, "i-0abc "+fields[0]+" "+fields[1]+" forge-generated\n", string(trust), "comment replaced; trust keyed by instance ID")
	assert.Equal(t, key.Type(), fields[0])

	// The production trust path accepts the written file for the instance ID.
	require.NoError(t, sshprovisioner.ValidateHostTrust("i-0abc", trustPath))

	// Re-writing replaces, never appends.
	_, _, err = writeEnvironmentSSH(dir, outs)
	require.NoError(t, err)
	again, err := os.ReadFile(trustPath)
	require.NoError(t, err)
	assert.Equal(t, trust, again)
}

func TestWriteEnvironmentSSH_RejectsMalformedHostKey(t *testing.T) {
	for _, hostKey := range []string{"", "ssh-ed25519", "   "} {
		dir := t.TempDir()
		_, _, err := writeEnvironmentSSH(dir, &hosted.Outputs{InstanceID: "i-0abc", HostPublicKey: hostKey, ClientPrivateKey: "k"})
		require.ErrorContains(t, err, "environment host public key is malformed", hostKey)
		_, statErr := os.Stat(filepath.Join(dir, "ssh", "id_ed25519"))
		assert.True(t, os.IsNotExist(statErr), "no private key written for a malformed host key")
	}
}

func TestApplyOptsFromFlags(t *testing.T) {
	apply := newApplyCmd()
	require.NoError(t, apply.ParseFlags([]string{"--skip-chart", "--skip-gpu", "--skip-overlay", "--skip-secrets", "--kubeconfig-out", "/tmp/kc.yaml"}))
	assert.Equal(t, lifecycle.ApplyOpts{
		KubeconfigOut: "/tmp/kc.yaml",
		SkipChart:     true,
		SkipGPU:       true,
		SkipOverlay:   true,
		SkipSecrets:   true,
	}, applyOptsFromFlags(apply))

	defaults := newApplyCmd()
	require.NoError(t, defaults.ParseFlags(nil))
	assert.Equal(t, lifecycle.ApplyOpts{}, applyOptsFromFlags(defaults))

	// forge init --provider aws defines none of the phase flags: a full apply.
	initCmd := newInitCmd()
	require.NoError(t, initCmd.ParseFlags([]string{"--provider", "aws"}))
	assert.Equal(t, lifecycle.ApplyOpts{}, applyOptsFromFlags(initCmd))
}
