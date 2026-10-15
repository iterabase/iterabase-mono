package central

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// InstallationIDPattern is central's installation id rule.
var InstallationIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,61}[a-z0-9]$`)

// KeyAndCSR is a locally generated installation key and its CSR. The key
// never leaves the operator machine or the installation's Secret.
type KeyAndCSR struct {
	KeyPEM []byte
	CSRPEM []byte
}

// NewKeyAndCSR generates an ECDSA P-256 key and a CSR for installation.
// Central uses only the CSR's public key; the subject is informative.
func NewKeyAndCSR(installation string) (KeyAndCSR, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return KeyAndCSR{}, fmt.Errorf("central: generate key: %w", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: installation, Organization: []string{"Iterabase installation"}},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}, key)
	if err != nil {
		return KeyAndCSR{}, fmt.Errorf("central: create CSR: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return KeyAndCSR{}, fmt.Errorf("central: encode key: %w", err)
	}
	return KeyAndCSR{
		KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		CSRPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
	}, nil
}

// WriteCertificateFiles writes tls.key, tls.crt and ca.crt into dir, each
// 0600, creating dir 0700 when absent.
func WriteCertificateFiles(dir string, keyPEM []byte, cert IssuedCertificate) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("central: create %s: %w", dir, err)
	}
	files := []struct {
		name string
		data []byte
	}{
		{"tls.key", keyPEM},
		{"tls.crt", []byte(cert.CertificatePEM)},
		{"ca.crt", []byte(cert.CAPEM)},
	}
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, f.data, 0o600); err != nil {
			return fmt.Errorf("central: write %s: %w", path, err)
		}
		// WriteFile keeps an existing file's mode; force 0600.
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("central: chmod %s: %w", path, err)
		}
	}
	return nil
}

// SecretManifest renders a kubernetes.io/tls Secret holding the installation
// key and certificate plus the central CA under ca.crt, as the
// inference-gateway chart's central.tlsSecretName expects.
func SecretManifest(namespace, name string, keyPEM []byte, cert IssuedCertificate) string {
	enc := base64.StdEncoding.EncodeToString
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/part-of: iterabase
    iterabase.com/central-certificate-serial: %q
type: kubernetes.io/tls
data:
  tls.crt: %s
  tls.key: %s
  ca.crt: %s
`, name, namespace, cert.Serial, enc([]byte(cert.CertificatePEM)), enc(keyPEM), enc([]byte(cert.CAPEM)))
}
