package sshprovisioner

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh"
)

// maxTrustedHostKeys bounds one host's trust set so planned rotation can
// overlap old and new keys without unbounded growth.
const maxTrustedHostKeys = 8

// hostKeyAlgorithms maps an accepted pinned key type to the host key
// algorithms that may be negotiated for it. RSA pins are restricted to
// SHA-2 signatures; SHA-1 ssh-rsa is never offered.
var hostKeyAlgorithms = map[string][]string{
	ssh.KeyAlgoED25519:  {ssh.KeyAlgoED25519},
	ssh.KeyAlgoECDSA256: {ssh.KeyAlgoECDSA256},
	ssh.KeyAlgoECDSA384: {ssh.KeyAlgoECDSA384},
	ssh.KeyAlgoECDSA521: {ssh.KeyAlgoECDSA521},
	ssh.KeyAlgoRSA:      {ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512},
}

func supportedHostKeyTypes() string {
	types := make([]string, 0, len(hostKeyAlgorithms))
	for keyType, algorithms := range hostKeyAlgorithms {
		if keyType == ssh.KeyAlgoRSA {
			types = append(types, fmt.Sprintf("%s (negotiated as %s)", keyType, strings.Join(algorithms, "/")))
			continue
		}
		types = append(types, keyType)
	}
	sort.Strings(types)
	return strings.Join(types, ", ")
}

// ValidateHostTrust loads and validates a host's explicit non-Git trust file
// without opening a connection. It fails closed on missing, unreadable,
// insecure, malformed, or empty trust material.
func ValidateHostTrust(address, trustFile string) error {
	_, _, err := loadHostTrust(address, trustFile)
	return err
}

// EnrollHostTrust validates one founder-verified OpenSSH host public key line
// and writes it as the exact known_hosts entry for address in path (file 0600,
// created parent directories 0700). Re-running with a key the file already
// trusts is a no-op; an existing file that trusts different keys is preserved
// and reported as an error because Forge never replaces trust material
// silently.
func EnrollHostTrust(path, address, keyLine string) error {
	address = strings.TrimSpace(address)
	path = expandPath(strings.TrimSpace(path))
	if address == "" {
		return fmt.Errorf("cannot enroll SSH host trust without a host address")
	}
	if path == "" {
		return fmt.Errorf("cannot enroll SSH host trust without a trust file path")
	}
	publicKey, err := parseEnrollmentKey(keyLine)
	if err != nil {
		return err
	}
	alreadyTrusted, err := enrollIntoExistingTrustFile(path, address, publicKey)
	if err != nil || alreadyTrusted {
		return err
	}
	entry := address + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey))) + "\n"
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create SSH host trust directory %q: %w", dir, err)
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("SSH host trust file %q appeared while enrolling; re-run init to reconcile it deliberately", path)
		}
		return fmt.Errorf("create SSH host trust file %q: %w", path, err)
	}
	if _, err := file.WriteString(entry); err != nil {
		file.Close()
		return fmt.Errorf("write SSH host trust file %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close SSH host trust file %q: %w", path, err)
	}
	return nil
}

func parseEnrollmentKey(keyLine string) (ssh.PublicKey, error) {
	publicKey, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(keyLine) + "\n"))
	if err != nil {
		return nil, fmt.Errorf("parse founder-verified SSH host key: %w", err)
	}
	if len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 || strings.ContainsAny(comment, "\r\n") {
		return nil, fmt.Errorf("founder-verified SSH host key must be exactly one OpenSSH public key line")
	}
	if _, ok := hostKeyAlgorithms[publicKey.Type()]; !ok {
		return nil, fmt.Errorf("founder-verified SSH host key type %q is unsupported; allowed types are %s", publicKey.Type(), supportedHostKeyTypes())
	}
	return publicKey, nil
}

// enrollIntoExistingTrustFile reports whether path already trusts publicKey. A
// missing file is not an error; an existing file trusting different keys is.
func enrollIntoExistingTrustFile(path, address string, publicKey ssh.PublicKey) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read SSH host trust file %q: %w", path, err)
	}
	keys, _, err := parseHostTrust(path, address, string(data))
	if err != nil {
		return false, err
	}
	for _, trusted := range keys {
		if bytes.Equal(trusted.Marshal(), publicKey.Marshal()) {
			return true, nil
		}
	}
	return false, fmt.Errorf("SSH host trust file %q already exists with different trusted keys for %s; add the verified key to that file deliberately instead of replacing trust material", path, address)
}

// loadHostTrust reads path and returns the host-key callback and the restricted
// host-key algorithm list derived from its exact entries for address.
func loadHostTrust(address, trustFile string) (ssh.HostKeyCallback, []string, error) {
	path := expandPath(strings.TrimSpace(trustFile))
	if path == "" {
		return nil, nil, fmt.Errorf("no SSH host trust file configured for %s: set spec.hosts[].sshTrustFile to the non-Git file holding the founder-verified host public key (see docs/architecture/forge-ssh-host-trust.md)", address)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("SSH host trust file %q (spec.hosts[].sshTrustFile) is unavailable: %w; enroll the founder-verified host key out of band (see docs/architecture/forge-ssh-host-trust.md)", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("SSH host trust file %q must be a regular file, not a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("SSH host trust file %q must be a regular file", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, nil, fmt.Errorf("SSH host trust file %q must not be group- or world-writable (mode %o)", path, info.Mode().Perm())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if uid := os.Geteuid(); int64(stat.Uid) != int64(uid) {
			return nil, nil, fmt.Errorf("SSH host trust file %q must be owned by the invoking user (uid %d), got uid %d", path, uid, stat.Uid)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read SSH host trust file %q: %w", path, err)
	}
	keys, algorithms, err := parseHostTrust(path, address, string(data))
	if err != nil {
		return nil, nil, err
	}
	return pinnedHostKeyCallback(address, path, keys), algorithms, nil
}

// parseHostTrust applies the approved known_hosts strict subset: only exact
// address entries are trust material, key types are allowlisted, the set is
// bounded, and CA/certificate markers for this host fail closed.
func parseHostTrust(path, address, content string) ([]ssh.PublicKey, []string, error) {
	address = strings.TrimSpace(address)
	var keys []ssh.PublicKey
	seen := make(map[string]struct{})
	var algorithms []string
	seenAlgorithms := make(map[string]struct{})
	for lineNumber, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		hostField := fields[0]
		if strings.HasPrefix(hostField, "@") {
			if len(fields) >= 2 && fields[1] == address {
				return nil, nil, fmt.Errorf("SSH host trust file %q line %d declares %s trust for %s: Forge supports only exact pinned host public keys and fails closed on CA/certificate host authentication", path, lineNumber+1, hostField, address)
			}
			continue
		}
		if hostField != address {
			continue
		}
		publicKey, allowed, err := parseTrustedHostKeyLine(path, lineNumber, fields)
		if err != nil {
			return nil, nil, err
		}
		marshaled := string(publicKey.Marshal())
		if _, exists := seen[marshaled]; exists {
			return nil, nil, fmt.Errorf("SSH host trust file %q line %d: duplicate trusted host key for %s", path, lineNumber+1, address)
		}
		seen[marshaled] = struct{}{}
		keys = append(keys, publicKey)
		algorithms = appendAlgorithms(algorithms, seenAlgorithms, allowed)
	}
	if len(keys) == 0 {
		return nil, nil, fmt.Errorf("SSH host trust file %q has no exact trusted host key entry for %s; enroll the founder-verified host key out of band (see docs/architecture/forge-ssh-host-trust.md)", path, address)
	}
	if len(keys) > maxTrustedHostKeys {
		return nil, nil, fmt.Errorf("SSH host trust file %q trusts %d keys for %s; at most %d are supported", path, len(keys), address, maxTrustedHostKeys)
	}
	return keys, algorithms, nil
}

func parseTrustedHostKeyLine(path string, lineNumber int, fields []string) (ssh.PublicKey, []string, error) {
	if len(fields) < 3 {
		return nil, nil, fmt.Errorf("SSH host trust file %q line %d is not an exact known_hosts host key entry", path, lineNumber+1)
	}
	publicKey, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(strings.Join(fields[1:], " ") + "\n"))
	if err != nil {
		return nil, nil, fmt.Errorf("SSH host trust file %q line %d: parse trusted host key: %w", path, lineNumber+1, err)
	}
	if len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, fmt.Errorf("SSH host trust file %q line %d: trusted host key entry has unsupported options or trailing content", path, lineNumber+1)
	}
	allowed, ok := hostKeyAlgorithms[publicKey.Type()]
	if !ok {
		return nil, nil, fmt.Errorf("SSH host trust file %q line %d: trusted host key type %q is unsupported; allowed types are %s", path, lineNumber+1, publicKey.Type(), supportedHostKeyTypes())
	}
	return publicKey, allowed, nil
}

func appendAlgorithms(algorithms []string, seen map[string]struct{}, allowed []string) []string {
	for _, algorithm := range allowed {
		if _, exists := seen[algorithm]; !exists {
			seen[algorithm] = struct{}{}
			algorithms = append(algorithms, algorithm)
		}
	}
	return algorithms
}

// pinnedHostKeyCallback accepts exactly the pinned key bytes and reports
// fingerprint-only diagnostics. It never echoes raw key material.
func pinnedHostKeyCallback(address, path string, keys []ssh.PublicKey) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		presented := key.Marshal()
		for _, trusted := range keys {
			if bytes.Equal(presented, trusted.Marshal()) {
				return nil
			}
		}
		return &hostKeyVerificationError{address: address, trustFile: path, presented: key, trusted: keys}
	}
}

// hostKeyVerificationError is the key-exchange failure for an unknown or
// mismatched host key.
type hostKeyVerificationError struct {
	address   string
	trustFile string
	presented ssh.PublicKey
	trusted   []ssh.PublicKey
}

func (e *hostKeyVerificationError) Error() string {
	types := make([]string, 0, len(e.trusted))
	seen := make(map[string]struct{}, len(e.trusted))
	for _, trusted := range e.trusted {
		if _, exists := seen[trusted.Type()]; exists {
			continue
		}
		seen[trusted.Type()] = struct{}{}
		types = append(types, trusted.Type())
	}
	sort.Strings(types)
	detail := "is not among the trusted keys"
	for _, trusted := range e.trusted {
		if trusted.Type() == e.presented.Type() {
			detail = "does not match the trusted key of the same type"
			break
		}
	}
	return fmt.Sprintf("ssh host key verification failed for %s during key exchange: server presented %s %s which %s; trusted keys: %d (%s) from %q; zero user-authentication attempts, commands, and stdin bytes were sent — verify the host identity out of band and update the trust file",
		e.address, e.presented.Type(), ssh.FingerprintSHA256(e.presented), detail, len(e.trusted), strings.Join(types, ", "), e.trustFile)
}
