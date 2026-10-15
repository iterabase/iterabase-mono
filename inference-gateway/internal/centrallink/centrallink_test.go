package centrallink

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/config"
)

func writePair(t *testing.T, dir, cn string, mod time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	for _, f := range []string{"tls.crt", "tls.key"} {
		require.NoError(t, os.Chtimes(filepath.Join(dir, f), mod, mod))
	}
}

func TestNew(t *testing.T) {
	route, err := New(config.CentralConfig{})
	require.NoError(t, err)
	assert.Nil(t, route, "central not configured")

	dir := t.TempDir()
	writePair(t, dir, "first", time.Now().Add(-time.Minute))
	cfg := config.CentralConfig{URL: "https://central.test", ClientCertFile: filepath.Join(dir, "tls.crt"),
		ClientKeyFile: filepath.Join(dir, "tls.key"), CAFile: filepath.Join(dir, "ca.crt")}
	route, err = New(cfg)
	require.NoError(t, err)
	assert.Equal(t, "https://central.test", route.URL)

	_, err = New(config.CentralConfig{URL: "http://central.test", ClientCertFile: cfg.ClientCertFile, ClientKeyFile: cfg.ClientKeyFile, CAFile: cfg.CAFile})
	assert.Error(t, err, "plain HTTP is refused")
	_, err = New(config.CentralConfig{URL: "https://central.test", ClientCertFile: "/missing", ClientKeyFile: "/missing", CAFile: cfg.CAFile})
	assert.Error(t, err)
}

func TestKeyPair_ReloadsRenewedCertificate(t *testing.T) {
	dir := t.TempDir()
	writePair(t, dir, "first", time.Now().Add(-time.Minute))
	kp := &keyPair{certFile: filepath.Join(dir, "tls.crt"), keyFile: filepath.Join(dir, "tls.key")}
	first, err := kp.get()
	require.NoError(t, err)

	writePair(t, dir, "renewed", time.Now())
	renewed, err := kp.get()
	require.NoError(t, err)
	assert.NotEqual(t, first.Certificate[0], renewed.Certificate[0], "a renewed Secret applies without a restart")

	require.NoError(t, os.Remove(filepath.Join(dir, "tls.crt")))
	kept, err := kp.get()
	require.NoError(t, err, "a transient read failure keeps the last good certificate")
	assert.Equal(t, renewed.Certificate[0], kept.Certificate[0])
}
