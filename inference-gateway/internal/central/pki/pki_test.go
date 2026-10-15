package pki

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeKMS signs with an in-memory key the way KMS does (digest in, DER out).
type fakeKMS struct {
	key   *ecdsa.PrivateKey
	signs int
}

func (f *fakeKMS) GetPublicKey(context.Context, *kms.GetPublicKeyInput, ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
	der, err := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
	if err != nil {
		return nil, err
	}
	return &kms.GetPublicKeyOutput{PublicKey: der, KeySpec: types.KeySpecEccNistP256}, nil
}

func (f *fakeKMS) Sign(_ context.Context, in *kms.SignInput, _ ...func(*kms.Options)) (*kms.SignOutput, error) {
	f.signs++
	if in.MessageType != types.MessageTypeDigest || in.SigningAlgorithm != types.SigningAlgorithmSpecEcdsaSha256 {
		panic("unexpected KMS sign request")
	}
	sig, err := ecdsa.SignASN1(rand.Reader, f.key, in.Message)
	return &kms.SignOutput{Signature: sig}, err
}

func newTestAuthority(t *testing.T) (*Authority, *fakeKMS) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	fk := &fakeKMS{key: key}
	signer, err := NewKMSSigner(context.Background(), fk, "arn:aws:kms:eu-west-1:111111111111:key/test")
	require.NoError(t, err)
	ca, err := NewCA(signer, time.Now())
	require.NoError(t, err)
	a, err := NewAuthority(ca, signer)
	require.NoError(t, err)
	return a, fk
}

func csrFor(t *testing.T, signer crypto.Signer, cn string) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn},
		URIs:    []*url.URL{{Scheme: "spiffe", Host: "evil.example", Path: "/admin"}},
	}, signer)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestCA_IsKMSSignedAndConstrained(t *testing.T) {
	a, fk := newTestAuthority(t)
	assert.True(t, a.CA.IsCA)
	assert.True(t, a.CA.MaxPathLenZero)
	assert.Equal(t, "Iterabase Central CA", a.CA.Subject.CommonName)
	assert.NoError(t, a.CA.CheckSignatureFrom(a.CA))
	assert.Positive(t, fk.signs, "signed in KMS")
	assert.WithinDuration(t, time.Now().Add(CAValidity), a.CA.NotAfter, time.Hour)
}

func TestIssueInstallation(t *testing.T) {
	a, _ := newTestAuthority(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	issued, err := a.IssueInstallation(csrFor(t, key, "spoofed-name"), "acme")
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(issued.DER)
	require.NoError(t, err)

	roots := x509.NewCertPool()
	roots.AddCert(a.CA)
	_, err = cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	require.NoError(t, err)
	assert.Equal(t, "acme", cert.Subject.CommonName, "the CSR's subject is ignored")
	require.Len(t, cert.URIs, 1, "the CSR's SANs are ignored")
	assert.Equal(t, "spiffe://central.iterabase.com/installations/acme", cert.URIs[0].String())
	assert.True(t, key.PublicKey.Equal(cert.PublicKey))
	assert.Equal(t, 127, cert.SerialNumber.BitLen(), "16 random bytes, positive in DER")
	assert.WithinDuration(t, time.Now().Add(InstallationValidity), cert.NotAfter, time.Hour)

	id, err := IdentityFromCert(cert)
	require.NoError(t, err)
	assert.Equal(t, Identity{InstallationID: "acme", Serial: issued.Serial, NotAfter: cert.NotAfter}, id)
	assert.Equal(t, Fingerprint(issued.DER), issued.Fingerprint)
}

func TestIssueInstallation_RejectsBadCSRs(t *testing.T) {
	a, _ := newTestAuthority(t)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	good := csrFor(t, p256, "x")
	block, _ := pem.Decode(good)
	tampered := append([]byte(nil), block.Bytes...)
	tampered[len(tampered)-5] ^= 0xff

	for name, csr := range map[string][]byte{
		"not PEM":       []byte("hello"),
		"P-384":         csrFor(t, p384, "x"),
		"RSA":           csrFor(t, rsaKey, "x"),
		"bad signature": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: tampered}),
		"wrong type":    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := a.IssueInstallation(csr, "acme")
			assert.ErrorIs(t, err, ErrInvalidCSR)
		})
	}
	_, err = a.IssueInstallation(good, "Bad_ID")
	assert.Error(t, err)
}

func TestIssueServing(t *testing.T) {
	a, _ := newTestAuthority(t)
	cert, err := a.IssueServing("central.test")
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(a.CA)
	_, err = cert.Leaf.Verify(x509.VerifyOptions{DNSName: "central.test", Roots: roots})
	require.NoError(t, err)
	assert.Len(t, cert.Certificate, 2, "leaf plus CA")
}

func TestIdentityFromCert_RejectsOtherShapes(t *testing.T) {
	a, _ := newTestAuthority(t)
	serving, err := a.IssueServing("central.test")
	require.NoError(t, err)
	_, err = IdentityFromCert(serving.Leaf)
	assert.ErrorIs(t, err, ErrNotIssued)
	for _, raw := range []string{
		"spiffe://iterabase.local/installations/acme",
		"spiffe://central.iterabase.com/pools/acme",
		"spiffe://central.iterabase.com/installations/acme?x=1",
	} {
		u, _ := url.Parse(raw)
		_, err := IdentityFromCert(&x509.Certificate{URIs: []*url.URL{u}, Subject: pkix.Name{CommonName: "acme"}})
		assert.ErrorIs(t, err, ErrNotIssued, raw)
	}
}

func TestNewAuthority_KeyMismatch(t *testing.T) {
	a, _ := newTestAuthority(t)
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	_, err = NewAuthority(a.CA, other)
	assert.Error(t, err)
}
