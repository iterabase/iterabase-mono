// Package pki is the central installation CA (DES-HOR-661-02): a KMS-held
// signing key, the self-signed CA certificate, installation certificates
// issued from CSRs, and central's own serving certificate.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// TrustDomain is the SPIFFE trust domain of installation identities. It is
// fixed: it does not follow the central public host.
const TrustDomain = "central.iterabase.com"

// Validity periods.
const (
	CAValidity           = 10 * 365 * 24 * time.Hour
	InstallationValidity = 365 * 24 * time.Hour
	ServingValidity      = 30 * 24 * time.Hour
	// backdate tolerates small clock skew between central and its clients.
	backdate = 5 * time.Minute
)

// InstallationIDPattern is the installation id format.
var InstallationIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,61}[a-z0-9]$`)

// Errors.
var (
	ErrInvalidCSR = errors.New("pki: invalid certificate signing request")
	ErrNotIssued  = errors.New("pki: certificate is not an installation certificate")
)

// NewCA creates the self-signed CA certificate, signed by signer (the KMS key).
func NewCA(signer crypto.Signer, now time.Time) (*x509.Certificate, error) {
	serial, err := RandomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Iterabase Central CA", Organization: []string{"Iterabase"}},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(CAValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, signer.Public(), signer)
	if err != nil {
		return nil, fmt.Errorf("pki: create CA certificate: %w", err)
	}
	return x509.ParseCertificate(der)
}

// Authority issues certificates under the CA.
type Authority struct {
	CA     *x509.Certificate
	Signer crypto.Signer
	Now    func() time.Time
}

// NewAuthority checks that signer holds the CA's key.
func NewAuthority(ca *x509.Certificate, signer crypto.Signer) (*Authority, error) {
	pub, ok := signer.Public().(*ecdsa.PublicKey)
	caPub, ok2 := ca.PublicKey.(*ecdsa.PublicKey)
	if !ok || !ok2 || !pub.Equal(caPub) {
		return nil, errors.New("pki: the signing key does not match the CA certificate")
	}
	return &Authority{CA: ca, Signer: signer, Now: time.Now}, nil
}

// CAPEM is the CA certificate in PEM.
func (a *Authority) CAPEM() []byte { return EncodeCert(a.CA.Raw) }

// Issued is an issued installation certificate.
type Issued struct {
	DER         []byte
	Serial      string
	Fingerprint string
	NotBefore   time.Time
	NotAfter    time.Time
}

// IssueInstallation issues an installation's client certificate from a CSR.
// Only the CSR's public key is used (it must be ECDSA P-256 and the CSR's
// signature must verify); subject and SAN come from installationID.
func (a *Authority) IssueInstallation(csrPEM []byte, installationID string) (*Issued, error) {
	if !InstallationIDPattern.MatchString(installationID) {
		return nil, fmt.Errorf("pki: invalid installation id %q", installationID)
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("%w: not a PEM certificate request", ErrInvalidCSR)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCSR, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: signature: %v", ErrInvalidCSR, err)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: the key must be ECDSA P-256", ErrInvalidCSR)
	}
	serial, err := RandomSerial()
	if err != nil {
		return nil, err
	}
	now := a.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: installationID, Organization: []string{"Iterabase installation"}},
		URIs:         []*url.URL{InstallationURI(installationID)},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(InstallationValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.CA, pub, a.Signer)
	if err != nil {
		return nil, fmt.Errorf("pki: issue installation certificate: %w", err)
	}
	return &Issued{
		DER: der, Serial: SerialString(serial), Fingerprint: Fingerprint(der),
		NotBefore: tmpl.NotBefore.UTC().Truncate(time.Second), NotAfter: tmpl.NotAfter.UTC().Truncate(time.Second),
	}, nil
}

// IssueServing issues central's serving certificate for host. The private key
// is generated here and only ever held in memory.
func (a *Authority) IssueServing(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := RandomSerial()
	if err != nil {
		return nil, err
	}
	now := a.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(ServingValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.CA, &key.PublicKey, a.Signer)
	if err != nil {
		return nil, fmt.Errorf("pki: issue serving certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der, a.CA.Raw}, PrivateKey: key, Leaf: leaf}, nil
}

// InstallationURI is the installation's SPIFFE id.
func InstallationURI(installationID string) *url.URL {
	return &url.URL{Scheme: "spiffe", Host: TrustDomain, Path: "/installations/" + installationID}
}

// Identity is a verified installation certificate's identity.
type Identity struct {
	InstallationID string
	Serial         string
	NotAfter       time.Time
}

// IdentityFromCert extracts the installation identity from a verified leaf:
// exactly one URI SAN spiffe://central.iterabase.com/installations/<id>.
func IdentityFromCert(cert *x509.Certificate) (Identity, error) {
	if cert == nil || len(cert.URIs) != 1 {
		return Identity{}, ErrNotIssued
	}
	u := cert.URIs[0]
	id, ok := strings.CutPrefix(u.Path, "/installations/")
	if u.Scheme != "spiffe" || u.Host != TrustDomain || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		!ok || !InstallationIDPattern.MatchString(id) || cert.Subject.CommonName != id {
		return Identity{}, ErrNotIssued
	}
	return Identity{InstallationID: id, Serial: SerialString(cert.SerialNumber), NotAfter: cert.NotAfter}, nil
}

// RandomSerial is a random serial from 16 random bytes, kept positive (and so
// a fixed 16 bytes in DER) by fixing its top two bits.
func RandomSerial() (*big.Int, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	b[0] &= 0x7f
	b[0] |= 0x40
	return new(big.Int).SetBytes(b), nil
}

// SerialString renders a serial as lowercase hex.
func SerialString(serial *big.Int) string { return serial.Text(16) }

// Fingerprint is the SHA-256 of a certificate's DER, in hex.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// EncodeCert encodes DER as a PEM certificate.
func EncodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// ParseCertPEM parses one PEM certificate.
func ParseCertPEM(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("pki: not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}
