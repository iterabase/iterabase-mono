// Package centrallink is the installation gateway's mTLS connection to the
// central Iterabase gateway (DES-HOR-661-03). The installation presents its
// central-issued client certificate and trusts only the central CA.
package centrallink

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/proxy"
)

// New returns the central route for cfg, or nil when central is not
// configured (cfg.URL empty).
func New(cfg config.CentralConfig) (*proxy.Central, error) {
	if cfg.URL == "" {
		return nil, nil
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("central url %q must be an https URL", cfg.URL)
	}
	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read central CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("central CA %q contains no certificate", cfg.CAFile)
	}
	kp := &keyPair{certFile: cfg.ClientCertFile, keyFile: cfg.ClientKeyFile}
	if _, err := kp.get(); err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    roots,
		ServerName: u.Hostname(),
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return kp.get()
		},
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     tlsCfg,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &proxy.Central{URL: cfg.URL, Client: proxy.NewUpstreamClient(transport)}, nil
}

// keyPair re-reads the certificate files when they change, so a renewed
// installation certificate (Secret update) applies on the next handshake.
type keyPair struct {
	certFile, keyFile string

	mu      sync.Mutex
	modTime time.Time
	cert    *tls.Certificate
}

func (k *keyPair) get() (*tls.Certificate, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	certInfo, err := os.Stat(k.certFile)
	if err != nil {
		return k.cached(fmt.Errorf("stat central client certificate: %w", err))
	}
	keyInfo, err := os.Stat(k.keyFile)
	if err != nil {
		return k.cached(fmt.Errorf("stat central client key: %w", err))
	}
	mod := certInfo.ModTime()
	if keyInfo.ModTime().After(mod) {
		mod = keyInfo.ModTime()
	}
	if k.cert != nil && mod.Equal(k.modTime) {
		return k.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(k.certFile, k.keyFile)
	if err != nil {
		return k.cached(fmt.Errorf("load central client certificate: %w", err))
	}
	k.cert, k.modTime = &cert, mod
	return k.cert, nil
}

// cached keeps serving the last good certificate through a transient read
// failure (for example mid-way through a Secret update).
func (k *keyPair) cached(err error) (*tls.Certificate, error) {
	if k.cert != nil {
		return k.cert, nil
	}
	return nil, err
}
