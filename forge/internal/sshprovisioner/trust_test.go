package sshprovisioner

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
)

func testPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	key, err := ssh.NewPublicKey(public)
	require.NoError(t, err)
	return key
}

func authorizedKeyLine(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func writeHostTrustFile(t *testing.T, address string, keys []ssh.PublicKey) string {
	t.Helper()
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, address+" "+authorizedKeyLine(key)+" founder-verified")
	}
	return writeTrustContent(t, filepath.Join(t.TempDir(), "10.20.0.10.trust"), strings.Join(lines, "\n")+"\n", 0o600)
}

func writeTrustContent(t *testing.T, path, content string, mode os.FileMode) string {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

// certificateKeyLine returns a host certificate public key line, an unsupported
// trust type because Forge implements no SSH CA/certificate host authentication.
func certificateKeyLine(t *testing.T) string {
	t.Helper()
	_, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	caSigner, err := ssh.NewSignerFromKey(caPrivate)
	require.NoError(t, err)
	certificate := &ssh.Certificate{
		Key:             testPublicKey(t),
		CertType:        ssh.HostCert,
		ValidPrincipals: []string{"host.example"},
		ValidBefore:     ssh.CertTimeInfinity,
	}
	require.NoError(t, certificate.SignCert(rand.Reader, caSigner))
	return authorizedKeyLine(certificate)
}

func TestLoadHostTrustPinsExactAddressAndKey(t *testing.T) {
	trusted := testPublicKey(t)
	otherHost := testPublicKey(t)
	content := strings.Join([]string{
		"# founder-verified out of band",
		"10.20.0.9 " + authorizedKeyLine(otherHost),
		"10.20.0.10 " + authorizedKeyLine(trusted) + " opo1",
		"",
	}, "\n")
	path := writeTrustContent(t, filepath.Join(t.TempDir(), "trust"), content, 0o600)

	callback, algorithms, err := loadHostTrust("10.20.0.10", path)
	require.NoError(t, err)
	assert.Equal(t, []string{ssh.KeyAlgoED25519}, algorithms)
	require.NoError(t, callback("10.20.0.10", &net.TCPAddr{}, trusted))

	// A different key of the same type is a mismatch; a different type is not
	// trusted. Neither diagnostic may disclose key material.
	untrusted := testPublicKey(t)
	err = callback("10.20.0.10", &net.TCPAddr{}, untrusted)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match the trusted key of the same type")
	assert.Contains(t, err.Error(), ssh.FingerprintSHA256(untrusted))
	assert.NotContains(t, err.Error(), authorizedKeyLine(untrusted))
	assert.NotContains(t, err.Error(), authorizedKeyLine(trusted))

	ecdsaPrivate, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ecdsaKey, err := ssh.NewPublicKey(&ecdsaPrivate.PublicKey)
	require.NoError(t, err)
	err = callback("10.20.0.10", &net.TCPAddr{}, ecdsaKey)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not among the trusted keys")
	assert.Contains(t, err.Error(), "zero user-authentication attempts, commands, and stdin bytes were sent")
}

func TestLoadHostTrustAcceptsRotationOverlapKeySet(t *testing.T) {
	oldKey := testPublicKey(t)
	newKey := testPublicKey(t)
	path := writeHostTrustFile(t, "10.20.0.10", []ssh.PublicKey{oldKey, newKey})

	callback, _, err := loadHostTrust("10.20.0.10", path)
	require.NoError(t, err)
	require.NoError(t, callback("10.20.0.10", &net.TCPAddr{}, oldKey))
	require.NoError(t, callback("10.20.0.10", &net.TCPAddr{}, newKey))
}

func TestLoadHostTrustDerivesRSASHA2Algorithms(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	key, err := ssh.NewPublicKey(&private.PublicKey)
	require.NoError(t, err)
	path := writeHostTrustFile(t, "10.20.0.10", []ssh.PublicKey{key})

	_, algorithms, err := loadHostTrust("10.20.0.10", path)
	require.NoError(t, err)
	assert.Equal(t, []string{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512}, algorithms)
	assert.NotContains(t, algorithms, ssh.KeyAlgoRSA)
}

func TestLoadHostTrustFailsClosed(t *testing.T) {
	trusted := testPublicKey(t)
	other := testPublicKey(t)
	manyKeys := make([]ssh.PublicKey, 0, maxTrustedHostKeys+1)
	for i := 0; i <= maxTrustedHostKeys; i++ {
		manyKeys = append(manyKeys, testPublicKey(t))
	}
	var manyLines []string
	for _, key := range manyKeys {
		manyLines = append(manyLines, "10.20.0.10 "+authorizedKeyLine(key))
	}

	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{name: "empty", content: "", want: "no exact trusted host key entry"},
		{name: "comments only", content: "# nothing here\n", want: "no exact trusted host key entry"},
		{name: "other host only", content: "10.20.0.9 " + authorizedKeyLine(other) + "\n", want: "no exact trusted host key entry"},
		{name: "wildcard ignored", content: "10.20.0.* " + authorizedKeyLine(other) + "\n", want: "no exact trusted host key entry"},
		{name: "hashed ignored", content: "|1|aGFzaGVk|aGFzaGVk " + authorizedKeyLine(other) + "\n", want: "no exact trusted host key entry"},
		{name: "bracketed port ignored", content: "[10.20.0.10]:22 " + authorizedKeyLine(other) + "\n", want: "no exact trusted host key entry"},
		{name: "malformed entry", content: "10.20.0.10 ssh-ed25519 !!!not-base64!!!\n", want: "parse trusted host key"},
		{name: "certificate authority marker", content: "@cert-authority 10.20.0.10 " + authorizedKeyLine(other) + "\n", want: "CA/certificate host authentication"},
		{name: "revoked marker", content: "@revoked 10.20.0.10 " + authorizedKeyLine(other) + "\n", want: "CA/certificate host authentication"},
		{name: "duplicate key", content: "10.20.0.10 " + authorizedKeyLine(trusted) + "\n10.20.0.10 " + authorizedKeyLine(trusted) + "\n", want: "duplicate trusted host key"},
		{name: "unsupported certificate key", content: "10.20.0.10 " + certificateKeyLine(t) + "\n", want: "unsupported"},
		{name: "too many keys", content: strings.Join(manyLines, "\n") + "\n", want: "at most 8 are supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTrustContent(t, filepath.Join(t.TempDir(), "trust"), tc.content, 0o600)
			_, _, err := loadHostTrust("10.20.0.10", path)
			require.ErrorContains(t, err, tc.want)
			if tc.content != "" {
				assert.NotContains(t, err.Error(), authorizedKeyLine(other))
			}
		})
	}
}

func TestLoadHostTrustRejectsMissingAndUnsafeFiles(t *testing.T) {
	trusted := testPublicKey(t)
	valid := writeHostTrustFile(t, "10.20.0.10", []ssh.PublicKey{trusted})

	_, _, err := loadHostTrust("10.20.0.10", "")
	require.ErrorContains(t, err, "no SSH host trust file configured")

	_, _, err = loadHostTrust("10.20.0.10", filepath.Join(t.TempDir(), "missing"))
	require.ErrorContains(t, err, "is unavailable")
	assert.NotContains(t, err.Error(), authorizedKeyLine(trusted))

	groupWritable := writeTrustContent(t, filepath.Join(t.TempDir(), "trust"), "10.20.0.10 "+authorizedKeyLine(trusted)+"\n", 0o600)
	require.NoError(t, os.Chmod(groupWritable, 0o666))
	_, _, err = loadHostTrust("10.20.0.10", groupWritable)
	require.ErrorContains(t, err, "must not be group- or world-writable")

	link := filepath.Join(t.TempDir(), "trust-link")
	require.NoError(t, os.Symlink(valid, link))
	_, _, err = loadHostTrust("10.20.0.10", link)
	require.ErrorContains(t, err, "not a symlink")
}

func TestEnrollHostTrustWritesNonGitMaterial(t *testing.T) {
	address := "10.20.0.10"
	key := testPublicKey(t)
	path := filepath.Join(t.TempDir(), "trust", address)

	require.NoError(t, EnrollHostTrust(address, path, authorizedKeyLine(key)+" operator-verified"))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dir, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dir.Mode().Perm())
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, address+" "+authorizedKeyLine(key)+"\n", string(content))

	// Re-enrolling the same verified key is idempotent.
	require.NoError(t, EnrollHostTrust(address, path, authorizedKeyLine(key)))

	// Trust material is never replaced silently, and only supported exact keys
	// are accepted.
	err = EnrollHostTrust(address, path, authorizedKeyLine(testPublicKey(t)))
	require.ErrorContains(t, err, "different trusted keys")
	err = EnrollHostTrust(address, path, "not-an-openssh-key")
	require.ErrorContains(t, err, "parse founder-verified SSH host key")
	err = EnrollHostTrust(address, path, certificateKeyLine(t))
	require.ErrorContains(t, err, "unsupported")
}

func TestProvisionerConnectsWithTrustedHostKey(t *testing.T) {
	obs := &fakeSSHObserver{}
	addr, hostKey, clientKeyPath, cleanup := startTrustedFakeSSH(t, func(cmd string) sshCommandResult {
		return sshCommandResult{stdout: "ok\n", code: 0}
	}, obs)
	defer cleanup()

	host := config.Host{
		Address: "fixture.example", SSHUser: "forge", SSHKeyPath: clientKeyPath,
		SSHTrustFile: writeHostTrustFile(t, "fixture.example", []ssh.PublicKey{hostKey}),
	}
	p := newTrustProvisioner(t, host, addr)
	defer p.Close()

	out, err := p.run(context.Background(), "echo ok")
	require.NoError(t, err)
	assert.Equal(t, "ok\n", out)
	assert.EqualValues(t, 1, obs.authAttempts.Load())
	assert.EqualValues(t, 1, obs.execRequests.Load())
	assert.EqualValues(t, 0, obs.stdinBytes.Load())
}

func TestProvisionerFailsClosedBeforeAuthenticationOnMismatchedHostKey(t *testing.T) {
	obs := &fakeSSHObserver{}
	addr, _, clientKeyPath, cleanup := startTrustedFakeSSH(t, func(cmd string) sshCommandResult {
		return sshCommandResult{stdout: "should-not-run\n", code: 0}
	}, obs)
	defer cleanup()

	host := config.Host{
		Address: "fixture.example", SSHUser: "forge", SSHKeyPath: clientKeyPath,
		SSHTrustFile: writeHostTrustFile(t, "fixture.example", []ssh.PublicKey{testPublicKey(t)}),
	}
	p := newTrustProvisioner(t, host, addr)
	defer p.Close()

	_, err := p.run(context.Background(), "echo should-not-run")
	require.ErrorContains(t, err, "ssh host key verification failed for fixture.example during key exchange")
	assert.EqualValues(t, 0, obs.authAttempts.Load())
	assert.EqualValues(t, 0, obs.execRequests.Load())
	assert.EqualValues(t, 0, obs.stdinBytes.Load())
}

func TestProvisionerFailsClosedBeforeDialOnMissingTrust(t *testing.T) {
	obs := &fakeSSHObserver{}
	addr, _, clientKeyPath, cleanup := startTrustedFakeSSH(t, func(cmd string) sshCommandResult {
		return sshCommandResult{stdout: "should-not-run\n", code: 0}
	}, obs)
	defer cleanup()

	_, err := New(config.Host{Address: "fixture.example", SSHUser: "forge", SSHKeyPath: clientKeyPath},
		WithDial(func(_ context.Context, _, _ string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
			return ssh.Dial("tcp", addr, cfg)
		}),
	)
	require.ErrorContains(t, err, "sshTrustFile")
	assert.EqualValues(t, 0, obs.authAttempts.Load())
	assert.EqualValues(t, 0, obs.execRequests.Load())
	assert.EqualValues(t, 0, obs.stdinBytes.Load())
}

func TestRunStdinSecretSentinelNeverReachesUntrustedHost(t *testing.T) {
	const sentinel = "OVERLAY-TOKEN-SENTINEL-8f2c1a"
	obs := &fakeSSHObserver{}
	addr, _, clientKeyPath, cleanup := startTrustedFakeSSH(t, func(cmd string) sshCommandResult {
		return sshCommandResult{stdout: "done\n", code: 0}
	}, obs)
	defer cleanup()

	host := config.Host{
		Address: "fixture.example", SSHUser: "forge", SSHKeyPath: clientKeyPath,
		SSHTrustFile: writeHostTrustFile(t, "fixture.example", []ssh.PublicKey{testPublicKey(t)}),
	}
	p := newTrustProvisioner(t, host, addr)
	defer p.Close()

	_, err := p.runStdin(context.Background(), "umask 077 && cat > /tmp/forge.sentinel", sentinel)
	require.ErrorContains(t, err, "ssh host key verification failed")
	assert.NotContains(t, err.Error(), sentinel)
	assert.EqualValues(t, 0, obs.authAttempts.Load())
	assert.EqualValues(t, 0, obs.execRequests.Load())
	assert.EqualValues(t, 0, obs.stdinBytes.Load())
}

func TestRunStdinSendsSentinelOnlyOverVerifiedConnection(t *testing.T) {
	const sentinel = "SECRET-MANIFEST-SENTINEL-41b7"
	obs := &fakeSSHObserver{}
	addr, hostKey, clientKeyPath, cleanup := startTrustedFakeSSH(t, func(cmd string) sshCommandResult {
		return sshCommandResult{stdout: "applied\n", code: 0}
	}, obs)
	defer cleanup()

	host := config.Host{
		Address: "fixture.example", SSHUser: "forge", SSHKeyPath: clientKeyPath,
		SSHTrustFile: writeHostTrustFile(t, "fixture.example", []ssh.PublicKey{hostKey}),
	}
	p := newTrustProvisioner(t, host, addr)
	defer p.Close()

	_, err := p.runStdin(context.Background(), "umask 077 && cat > /tmp/forge.sentinel", sentinel)
	require.NoError(t, err)
	assert.EqualValues(t, 1, obs.authAttempts.Load())
	assert.EqualValues(t, 1, obs.execRequests.Load())
	assert.EqualValues(t, len(sentinel), obs.stdinBytes.Load())
}

// TestNoInsecureHostKeyCallbackInNonTestSources is the durable scan for the
// HOR-521 acceptance criterion that no production/default path keeps an
// insecure host-key callback. Test-only fixtures live in _test.go files and the
// separate test module, which cannot be part of a built Forge binary.
func TestNoInsecureHostKeyCallbackInNonTestSources(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	require.NoError(t, filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "test" || entry.Name() == "bin" || entry.Name() == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), "InsecureIgnoreHostKey") {
			offenders = append(offenders, path)
		}
		return nil
	}))
	assert.Empty(t, offenders, fmt.Sprintf("insecure host-key callbacks in built sources: %v", offenders))
}
