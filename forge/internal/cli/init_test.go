package cli

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/provisioner"
)

// initTrustArgs returns a non-interactive init invocation prefix with a
// non-production fixture host key enrolled into a temporary non-Git trust file.
func initTrustArgs(t *testing.T) []string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	key, err := ssh.NewPublicKey(public)
	require.NoError(t, err)
	return []string{
		"init", "--non-interactive",
		"--ssh-trust-file", filepath.Join(t.TempDir(), "trust", "192.0.2.10"),
		"--ssh-host-key", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))),
	}
}

func TestResolveDataStorageSourcesCanonicalizesEquivalentFlagAndEnvironmentSets(t *testing.T) {
	flags := []string{"/dev/disk/by-id/scsi-b", "/dev/disk/by-id/scsi-a"}
	environment := "/dev/disk/by-id/scsi-a,/dev/disk/by-id/scsi-b"
	got, err := resolveDataStorageSources(flags, environment)
	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/disk/by-id/scsi-a", "/dev/disk/by-id/scsi-b"}, got)

	_, err = resolveDataStorageSources(flags, "/dev/disk/by-id/scsi-c")
	require.ErrorContains(t, err, "conflicting data-storage device sets")
}

func TestResolveDataStorageSourcesRejectsDuplicatesAndMalformedCommaFlag(t *testing.T) {
	_, err := resolveDataStorageSources([]string{"/dev/disk/by-id/scsi-a", "/dev/disk/by-id/scsi-a"}, "")
	require.ErrorContains(t, err, "duplicate")
	_, err = resolveDataStorageSources([]string{"/dev/disk/by-id/scsi-a,/dev/disk/by-id/scsi-b"}, "")
	require.ErrorContains(t, err, "repeat the flag")
	_, err = resolveDataStorageSources(nil, "/dev/disk/by-id/scsi-a,,/dev/disk/by-id/scsi-b")
	require.ErrorContains(t, err, "non-empty")
}

func TestInitNonInteractiveMaterializesCanonicalEnvironmentDeviceSet(t *testing.T) {
	t.Setenv(dataStorageDevicesEnv, "/dev/disk/by-id/scsi-b, /dev/disk/by-id/scsi-a")
	path := filepath.Join(t.TempDir(), "forge.yaml")
	cmd := NewRootCmd()
	cmd.SetArgs(append(initTrustArgs(t), "--path", path, "--address", "192.0.2.10"))
	cmd.SetOut(&bytes.Buffer{})
	require.NoError(t, cmd.Execute())

	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/disk/by-id/scsi-a", "/dev/disk/by-id/scsi-b"}, cfg.Spec.DataStorage.Devices)
	assert.Contains(t, cfg.Spec.K3s.Disable, "local-storage")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestInitNonInteractiveMaterializesRepeatedFlags(t *testing.T) {
	t.Setenv(dataStorageDevicesEnv, "")
	path := filepath.Join(t.TempDir(), "forge.yaml")
	cmd := NewRootCmd()
	cmd.SetArgs(append(initTrustArgs(t), "--path", path, "--address", "192.0.2.10", "--data-storage-device", "/dev/disk/by-id/scsi-b", "--data-storage-device", "/dev/disk/by-id/scsi-a"))
	cmd.SetOut(&bytes.Buffer{})
	require.NoError(t, cmd.Execute())
	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/disk/by-id/scsi-a", "/dev/disk/by-id/scsi-b"}, cfg.Spec.DataStorage.Devices)
}

func TestInitRequiresExplicitHostTrust(t *testing.T) {
	t.Setenv(dataStorageDevicesEnv, "/dev/disk/by-id/scsi-a")
	path := filepath.Join(t.TempDir(), "forge.yaml")
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"init", "--non-interactive", "--path", path, "--address", "192.0.2.10"})
	cmd.SetOut(&bytes.Buffer{})
	err := cmd.Execute()
	require.ErrorContains(t, err, "no SSH host key was supplied")
	require.ErrorContains(t, err, "sshTrustFile")
}

func TestInitWritesTrustMaterialOutsideGitConfig(t *testing.T) {
	t.Setenv(dataStorageDevicesEnv, "/dev/disk/by-id/scsi-a")
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	key, err := ssh.NewPublicKey(public)
	require.NoError(t, err)
	keyLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	trustFile := filepath.Join(t.TempDir(), "trust", "192.0.2.10")
	path := filepath.Join(t.TempDir(), "forge.yaml")
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"init", "--non-interactive", "--path", path, "--address", "192.0.2.10", "--ssh-trust-file", trustFile, "--ssh-host-key", keyLine})
	cmd.SetOut(&bytes.Buffer{})
	require.NoError(t, cmd.Execute())

	encoded, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "sshTrustFile: "+trustFile)
	assert.NotContains(t, string(encoded), keyLine)

	info, err := os.Stat(trustFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	content, err := os.ReadFile(trustFile)
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.10 "+keyLine+"\n", string(content))
}

func TestInitNonInteractiveRequiresDataStorageSource(t *testing.T) {
	t.Setenv(dataStorageDevicesEnv, "")
	cmd := NewRootCmd()
	cmd.SetArgs(append(initTrustArgs(t), "--path", filepath.Join(t.TempDir(), "forge.yaml"), "--address", "192.0.2.10"))
	err := cmd.Execute()
	require.ErrorContains(t, err, dataStorageDevicesEnv)
}

func TestSelectDataStorageDevicesShowsConsequenceAndAcceptsMultiple(t *testing.T) {
	var out bytes.Buffer
	selected, err := selectDataStorageDevices(bufio.NewReader(strings.NewReader("2,1\n")), &out, []provisioner.DataStorageDevice{
		{Path: "/dev/disk/by-id/scsi-a", Model: "model-a", Serial: "serial-a", Transport: "sata", SizeBytes: 100 << 30},
		{Path: "/dev/disk/by-id/nvme-b", Model: "model-b", Serial: "serial-b", Transport: "nvme", SizeBytes: 200 << 30},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/disk/by-id/nvme-b", "/dev/disk/by-id/scsi-a"}, selected)
	text := out.String()
	assert.Contains(t, text, "fixed thick iterabase-data")
	assert.Contains(t, text, "sole first-write authorization")
	assert.Contains(t, text, "never creates platform filesystems")
	assert.Contains(t, text, `transport="nvme"`)
	assert.Contains(t, text, "model-b")
	assert.Contains(t, text, "serial-b")
	assert.Contains(t, text, "200.0 GiB")
}

func TestSelectDataStorageDevicesRejectsDuplicateAndEmptySelection(t *testing.T) {
	devices := []provisioner.DataStorageDevice{{Path: "/dev/disk/by-id/scsi-a"}}
	_, err := selectDataStorageDevices(bufio.NewReader(strings.NewReader("1,1\n")), &bytes.Buffer{}, devices)
	require.ErrorContains(t, err, "duplicate")
	_, err = selectDataStorageDevices(bufio.NewReader(strings.NewReader("\n")), &bytes.Buffer{}, devices)
	require.Error(t, err)
}
