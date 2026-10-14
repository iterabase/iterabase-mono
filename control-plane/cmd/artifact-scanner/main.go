// Command artifact-scanner is the isolated scanner adapter
// (DES-HOR-460-07, DES-HOR-461-01). It runs beside a pod-local clamd with no
// database, object-store, Kubernetes, or customer credential and no egress.
// It accepts exactly one bounded stream at a time over HTTP/2 mTLS from the
// coordinator identity, returns a malware verdict plus the admission policy
// result, and is ready only while clamd passes its self-test on a signature
// bundle no older than 24 hours.
//
// Configuration is environment-only; it never loads the control-plane config
// (which would carry database settings):
//
//	SCANNER_ADDR              mTLS processing listener (default :8443)
//	SCANNER_METRICS_ADDR      plaintext health/metrics listener (default :9090)
//	SCANNER_TLS_CERT_FILE     server certificate (cert-manager, rotated)
//	SCANNER_TLS_KEY_FILE      server key
//	SCANNER_CLIENT_CA_FILE    CA bundle that verifies the coordinator
//	SCANNER_ALLOWED_CLIENT    exact coordinator SPIFFE id
//	SCANNER_CLAMD_ADDR        pod-local clamd (default 127.0.0.1:3310)
//	SCANNER_SIGNATURE_STATE   updater state file on the read-only volume
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/scanner"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/logging"
	cpmetrics "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/metrics"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/version"
)

func main() { os.Exit(realMain()) }

func realMain() int {
	logger, _ := logging.New(env("LOG_LEVEL", "info"), "json")
	logger.Info("starting artifact scanner", "version", version.Version(), "commit", version.Commit())
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, logger); err != nil {
		logger.Error("artifact scanner failed", "error", err)
		return 1
	}
	return 0
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func run(ctx context.Context, logger *slog.Logger) error {
	certFile, keyFile, caFile := os.Getenv("SCANNER_TLS_CERT_FILE"), os.Getenv("SCANNER_TLS_KEY_FILE"), os.Getenv("SCANNER_CLIENT_CA_FILE")
	allowed, statePath := os.Getenv("SCANNER_ALLOWED_CLIENT"), os.Getenv("SCANNER_SIGNATURE_STATE")
	if certFile == "" || keyFile == "" || caFile == "" || allowed == "" || statePath == "" {
		return errors.New("SCANNER_TLS_CERT_FILE, SCANNER_TLS_KEY_FILE, SCANNER_CLIENT_CA_FILE, SCANNER_ALLOWED_CLIENT, and SCANNER_SIGNATURE_STATE are required")
	}
	tlsCfg, err := serverTLS(caFile, certFile, keyFile)
	if err != nil {
		return err
	}
	m := cpmetrics.New("artifact-scanner", version.Version(), version.Commit())
	adapter := &scanner.Adapter{
		Engine:        scanner.Clamd{Addr: env("SCANNER_CLAMD_ADDR", "127.0.0.1:3310")},
		StatePath:     statePath,
		AllowedClient: allowed,
		Metrics:       m,
		Log:           logger,
	}
	go func() {
		for {
			adapter.Refresh(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}()

	processing := &http.Server{Addr: env("SCANNER_ADDR", ":8443"), Handler: adapter, TLSConfig: tlsCfg,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 150 * time.Second}

	metricsSrv := &http.Server{Addr: env("SCANNER_METRICS_ADDR", ":9090"), Handler: healthMux(adapter, m), ReadHeaderTimeout: 5 * time.Second}
	return serve(ctx, processing, metricsSrv)
}

// serverTLS requires and verifies a client certificate from the platform
// workload CA and re-reads the server key pair on each handshake so
// cert-manager rotation needs no restart. The exact coordinator identity is
// checked per request by the adapter.
func serverTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	caPEM, err := os.ReadFile(caFile) //nolint:gosec // operator-configured certificate mount path.
	if err != nil {
		return nil, fmt.Errorf("read client CA: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("client CA bundle contains no certificate")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2"},
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientCAs,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			return &cert, err
		},
	}, nil
}

// healthMux serves liveness, readiness (a clean verdict is possible), and
// metrics on the plaintext metrics-only port; never the processing port.
func healthMux(adapter *scanner.Adapter, m *cpmetrics.Metrics) http.Handler {
	health := http.NewServeMux()
	health.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	health.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ok, _ := adapter.Ready(); !ok {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	health.Handle("/metrics", m.Handler())
	return health
}

func serve(ctx context.Context, processing, metricsSrv *http.Server) error {
	errc := make(chan error, 2)
	go func() {
		if err := processing.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = processing.Shutdown(shutdown)
	_ = metricsSrv.Shutdown(shutdown)
	return nil
}
