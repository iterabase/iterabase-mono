package e2e_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

// HOR-528: the internal CA root is a single authority that is created once and
// never rewritten. These helpers read the authority and the leaves a cluster
// actually issued, and prove the identity relationship directly from the
// certificate bytes: every workload leaf must chain to the exact root the
// clients mount, and a reconcile must not replace that root.
//
// The chain comparison is asserted on the certificate's raw issuer/subject and
// on an X.509 signature verification against the mounted root, so a rotated root
// that reuses the same commonName (the reported failure: "PostgreSQL served a
// leaf signed by one key while the mounted root exposed the other") is rejected.

const internalCARootSecretSuffix = "-internal-ca-root"

// internalCARootSecretName is the fixed platform-owned trust root Secret every
// internal-TLS client mounts.
func internalCARootSecretName() string {
	return testRelease + internalCARootSecretSuffix
}

// internalCABootstrapIssuerName is the self-signed bootstrap ClusterIssuer that
// mints the root. Exactly one must exist per install.
func internalCABootstrapIssuerName() string {
	return testRelease + "-internal-ca-bootstrap"
}

// coreInternalCALeafSecrets are the datastore and control-plane leaves signed by
// the internal CA ClusterIssuer in every internal-TLS composition.
func coreInternalCALeafSecrets() []string {
	return []string{
		testRelease + "-postgresql-tls",
		testRelease + "-redis-tls",
		testRelease + "-control-plane-api-tls",
	}
}

// observabilityInternalCALeafSecrets are the stack leaves signed by the same
// internal CA ClusterIssuer when the observability preset is active.
func observabilityInternalCALeafSecrets() []string {
	return []string{
		"observability-prometheus-tls",
		"observability-alertmanager-tls",
		"observability-grafana-tls",
		"observability-loki-tls",
	}
}

func parseCertificate(t *testing.T, name string, data []byte) *x509.Certificate {
	t.Helper()
	certificates, err := parsePEMCertificates(data)
	if err != nil {
		t.Fatalf("%s is not a PEM certificate chain: %v", name, err)
	}
	return certificates[0]
}

func parsePEMCertificates(data []byte) ([]*x509.Certificate, error) {
	var certificates []*x509.Certificate
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certificates = append(certificates, certificate)
	}
	if len(certificates) == 0 {
		return nil, fmt.Errorf("no CERTIFICATE PEM block found")
	}
	return certificates, nil
}

// verifyLeafChainsToRoot verifies the exact trust relationship the platform
// promises: the leaf's issuer is the mounted root and the leaf's signature
// validates against that root's public key.
func verifyLeafChainsToRoot(rootPEM, leafPEM []byte) error {
	roots, err := parsePEMCertificates(rootPEM)
	if err != nil {
		return fmt.Errorf("parse mounted root: %w", err)
	}
	root := roots[0]
	if !root.IsCA {
		return fmt.Errorf("mounted root %q is not a CA certificate", root.Subject)
	}
	leaves, err := parsePEMCertificates(leafPEM)
	if err != nil {
		return fmt.Errorf("parse issued leaf: %w", err)
	}
	leaf := leaves[0]
	if !bytes.Equal(leaf.RawIssuer, root.RawSubject) {
		return fmt.Errorf("leaf issuer %q does not match mounted root subject %q", leaf.Issuer, root.Subject)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	options := x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}
	if len(leaves) > 1 {
		// A leaf Secret may carry its issued chain; keep the verification strict
		// by trusting only the mounted root and any intermediate it shipped with.
		intermediates := x509.NewCertPool()
		for _, intermediate := range leaves[1:] {
			intermediates.AddCert(intermediate)
		}
		options.Intermediates = intermediates
	}
	if _, err := leaf.Verify(options); err != nil {
		return fmt.Errorf("leaf does not verify against the mounted root: %w", err)
	}
	return nil
}

func certificateFingerprint(data []byte) (string, error) {
	certificates, err := parsePEMCertificates(data)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(certificates[0].Raw)
	return hex.EncodeToString(digest[:]), nil
}

// internalCARootFingerprint returns the SHA-256 of the mounted root's DER bytes.
func internalCARootFingerprint(t *testing.T, state *chartState) string {
	t.Helper()
	fingerprint, err := certificateFingerprint(decodeSecretValue(t, state, internalCARootSecretName(), "ca.crt"))
	if err != nil {
		t.Fatalf("fingerprint mounted internal CA root: %v", err)
	}
	return fingerprint
}

// assertSingleInternalCARootAuthority proves the cluster holds exactly one root
// authority on its first revision: one CA root Certificate, one bootstrap
// issuer, one CA issuer bound to that Secret, and one CertificateRequest.
func assertSingleInternalCARootAuthority(t *testing.T, state *chartState) {
	t.Helper()
	rootName := internalCARootSecretName()

	root := decodeSecretValue(t, state, rootName, "ca.crt")
	rootCertificate := parseCertificate(t, rootName, root)
	if !rootCertificate.IsCA {
		t.Fatalf("%s ca.crt is not a CA certificate", rootName)
	}

	revision := strings.TrimSpace(state.kubectl(t, 30*time.Second, "get", "certificate/"+rootName, "-n", testNamespace,
		"-o", "jsonpath={.status.revision}"))
	if revision != "1" {
		t.Fatalf("internal CA root revision=%q want 1: the authority must be issued exactly once", revision)
	}
	if owner := strings.TrimSpace(state.kubectl(t, 30*time.Second, "get", "certificate/"+rootName, "-n", testNamespace,
		"-o", "jsonpath={.metadata.annotations.meta\\.helm\\.sh/release-name}")); owner != testRelease {
		t.Fatalf("internal CA root owner=%q want platform release %q", owner, testRelease)
	}

	requests := state.kubectl(t, 30*time.Second, "get", "certificaterequest", "-n", testNamespace, "-o", "name")
	var rootRequests []string
	for _, line := range strings.Fields(requests) {
		name := strings.TrimPrefix(line, "certificaterequest.cert-manager.io/")
		if strings.HasPrefix(name, rootName+"-") {
			rootRequests = append(rootRequests, name)
		}
	}
	// cert-manager names requests <certificate>-<revision> (stable request
	// naming), so a single first-revision request proves there is no overlapping
	// or superseded root issuance to adopt.
	if len(rootRequests) != 1 || rootRequests[0] != rootName+"-1" {
		t.Fatalf("internal CA root CertificateRequests=%v want exactly [%s-1]: no overlapping root request is permitted", rootRequests, rootName)
	}

	bootstrap := strings.TrimSpace(state.kubectl(t, 30*time.Second, "get", "clusterissuer/"+internalCABootstrapIssuerName(),
		"-o", "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}"))
	if bootstrap != "True" {
		t.Fatalf("internal CA bootstrap ClusterIssuer %s Ready=%q want True", internalCABootstrapIssuerName(), bootstrap)
	}
	caSecret := strings.TrimSpace(state.kubectl(t, 30*time.Second, "get", "clusterissuer/internal-ca",
		"-o", "jsonpath={.spec.ca.secretName}"))
	if caSecret != rootName {
		t.Fatalf("internal-ca ClusterIssuer ca.secretName=%q want %q: leaves must be signed by the mounted authority", caSecret, rootName)
	}
}

// assertIssuedChainsMatchMountedRoot verifies each issued workload leaf against
// the mounted internal CA root.
func assertIssuedChainsMatchMountedRoot(t *testing.T, state *chartState, leafSecrets ...string) {
	t.Helper()
	root := decodeSecretValue(t, state, internalCARootSecretName(), "ca.crt")
	for _, leafSecret := range leafSecrets {
		leaf := decodeSecretValue(t, state, leafSecret, "tls.crt")
		if err := verifyLeafChainsToRoot(root, leaf); err != nil {
			t.Fatalf("%s does not match the mounted %s: %v", leafSecret, internalCARootSecretName(), err)
		}
	}
}

// assertInternalCARootStable proves a reconcile did not replace the authority.
func assertInternalCARootStable(t *testing.T, state *chartState, uidBefore, fingerprintBefore string) {
	t.Helper()
	uidAfter := strings.TrimSpace(state.kubectl(t, 30*time.Second, "get", "certificate/"+internalCARootSecretName(), "-n", testNamespace,
		"-o", "jsonpath={.metadata.uid}"))
	if uidAfter != uidBefore {
		t.Fatalf("internal CA root object was replaced on reconcile: before=%q after=%q", uidBefore, uidAfter)
	}
	fingerprintAfter := internalCARootFingerprint(t, state)
	if fingerprintAfter != fingerprintBefore {
		t.Fatalf("internal CA root key material rotated on reconcile: before=%s after=%s", fingerprintBefore, fingerprintAfter)
	}
	assertSingleInternalCARootAuthority(t, state)
}

// assertMountedInternalCA proves a workload pod really mounts the exact root
// authority the platform issued (byte identity, not a same-named replacement).
func assertMountedInternalCA(t *testing.T, state *chartState, pod, path string) {
	t.Helper()
	root := decodeSecretValue(t, state, internalCARootSecretName(), "ca.crt")
	expected := sha256.Sum256(bytes.TrimSpace(root))
	observed := state.kubectl(t, 30*time.Second, "exec", "-n", testNamespace, pod, "--", "sha256sum", path)
	fields := strings.Fields(observed)
	if len(fields) == 0 {
		t.Fatalf("could not hash mounted internal CA %s in %s: %q", path, pod, observed)
	}
	if fields[0] != hex.EncodeToString(expected[:]) {
		t.Fatalf("mounted internal CA %s in %s does not match the issued %s", path, pod, internalCARootSecretName())
	}
}

// --- hermetic regression for the chain comparison itself ---------------------

type testCertificateAuthority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pem         []byte
}

func newTestCertificateAuthority(t *testing.T, commonName string) testCertificateAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	return testCertificateAuthority{certificate: certificate, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (authority testCertificateAuthority) issue(t *testing.T, dnsName string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, authority.certificate, &key.PublicKey, authority.key)
	if err != nil {
		t.Fatalf("issue leaf certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestUnitInternalCAChainIdentityRejectsRotatedAuthority(t *testing.T) {
	original := newTestCertificateAuthority(t, "iterabase-internal-ca")
	// A rotated root deliberately keeps the same commonName: only the key
	// material differs, which is exactly the failure this regression guards.
	rotated := newTestCertificateAuthority(t, "iterabase-internal-ca")
	leaf := original.issue(t, "iterabase-postgresql.iterabase-system.svc")

	if err := verifyLeafChainsToRoot(original.pem, leaf); err != nil {
		t.Fatalf("leaf issued by the mounted root must verify: %v", err)
	}
	if err := verifyLeafChainsToRoot(rotated.pem, leaf); err == nil {
		t.Fatal("leaf signed by a rotated root with the same commonName must be rejected")
	}
}

func TestUnitInternalCAChainIdentityRejectsUnrelatedAuthority(t *testing.T) {
	root := newTestCertificateAuthority(t, "iterabase-internal-ca")
	other := newTestCertificateAuthority(t, "unrelated-ca")
	leaf := other.issue(t, "iterabase-redis.iterabase-system.svc")

	if err := verifyLeafChainsToRoot(root.pem, leaf); err == nil {
		t.Fatal("leaf signed by an unrelated authority must be rejected")
	}
	if err := verifyLeafChainsToRoot(root.pem, []byte("not a certificate")); err == nil {
		t.Fatal("non-certificate leaf material must be rejected")
	}
}

func TestUnitInternalCAFingerprintTracksKeyMaterial(t *testing.T) {
	original := newTestCertificateAuthority(t, "iterabase-internal-ca")
	rotated := newTestCertificateAuthority(t, "iterabase-internal-ca")

	first, err := certificateFingerprint(original.pem)
	if err != nil {
		t.Fatalf("fingerprint original root: %v", err)
	}
	second, err := certificateFingerprint(rotated.pem)
	if err != nil {
		t.Fatalf("fingerprint rotated root: %v", err)
	}
	if first == second {
		t.Fatal("rotated root material must produce a different fingerprint")
	}
	if _, err := certificateFingerprint([]byte("not a certificate")); err == nil {
		t.Fatal("invalid root material must not produce a fingerprint")
	}
}

func TestUnitInternalCARootSecretIsReleaseScoped(t *testing.T) {
	if got := internalCARootSecretName(); got != testRelease+internalCARootSecretSuffix {
		t.Fatalf("internal CA root Secret=%q want release-scoped authority", got)
	}
	if got := internalCABootstrapIssuerName(); got != testRelease+"-internal-ca-bootstrap" {
		t.Fatalf("internal CA bootstrap issuer=%q want release-scoped authority", got)
	}
}
