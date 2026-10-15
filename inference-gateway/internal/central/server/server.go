// Package server is the central gateway's HTTP surface
// (docs/architecture/v2-central-inference-gateway.md): the public mTLS
// listener for installation inference, the installation read and the operator
// API; the in-VPC health listener; and the metrics listener.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/billing"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/config"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/notify"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/opauth"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/pki"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/registry"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/store"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/metrics"
	gatewaymw "github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/proxy"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/ratelimit"
)

// ServingRenewInterval is how often the serving certificate is re-issued.
const ServingRenewInterval = 24 * time.Hour

// maxBodyBytes bounds a chat request (images included).
const maxBodyBytes = 32 << 20

// OperatorVerifier authenticates operator requests.
type OperatorVerifier interface {
	Verify(ctx context.Context, header string) (opauth.Caller, error)
}

// Deps are the server's collaborators.
type Deps struct {
	Config         *config.Config
	Store          *store.Store
	Registry       *registry.Cache
	Authority      *pki.Authority
	Limiter        *ratelimit.PostgresLimiter
	Verifier       OperatorVerifier
	Alerts         *notify.Alerts
	Ops            notify.Publisher
	Metrics        *metrics.Metrics
	ProviderClient *http.Client
	Logger         *slog.Logger
	Now            func() time.Time
}

// Server is the central gateway server.
type Server struct {
	deps      Deps
	policy    billing.Policy
	forwarder *proxy.Forwarder
	cm        *centralMetrics
	serving   atomic.Pointer[tls.Certificate]
	fxAlerted sync.Map // month -> time of the last FX-unavailable ops alert
}

// New creates the server and issues its first serving certificate.
func New(deps Deps) (*Server, error) {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.ProviderClient == nil {
		deps.ProviderClient = proxy.NewUpstreamClient(nil)
	}
	s := &Server{
		deps:      deps,
		policy:    billing.NewPolicy(deps.Config.File),
		forwarder: proxy.NewForwarder(deps.Metrics, deps.Logger),
		cm:        newCentralMetrics(deps.Metrics),
	}
	if err := s.RenewServing(); err != nil {
		return nil, err
	}
	return s, nil
}

// RenewServing issues a fresh serving certificate.
func (s *Server) RenewServing() error {
	cert, err := s.deps.Authority.IssueServing(s.deps.Config.PublicHost)
	if err != nil {
		return err
	}
	s.serving.Store(cert)
	return nil
}

// TLSConfig is the public listener's TLS: 1.3 only, client certificates
// verified against the central CA when given (installation routes require
// one, operator routes do not).
func (s *Server) TLSConfig() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(s.deps.Authority.CA)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  pool,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return s.serving.Load(), nil
		},
	}
}

// Handler is the public listener's router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(chimiddleware.Recoverer)
	r.Use(sanitizeRequestID)
	r.Use(gatewaymw.RequestID)
	r.Use(s.logging)
	if s.deps.Metrics != nil {
		r.Use(gatewaymw.Metrics(s.deps.Metrics, "central"))
	}

	r.Group(func(r chi.Router) {
		r.Use(s.installationAuth)
		r.Post("/v1/chat/completions", s.chatCompletions)
		r.Get("/v1/models", s.listModels)
		r.Get("/v1/installation", s.installationRead)
	})
	r.Route("/operator/v1", func(r chi.Router) {
		r.Use(s.operatorAuth)
		r.Post("/installations", s.registerInstallation)
		r.Get("/installations/{id}", s.showInstallation)
		r.Post("/installations/{id}/certificates", s.issueCertificate)
		r.Post("/installations/{id}/revoke", s.revokeInstallation)
		r.Post("/installations/{id}/plan", s.setPlan)
		r.Post("/installations/{id}/trial-extension", s.extendTrial)
		r.Post("/usage/{record_id}/adjustment", s.adjustUsage)
		r.Get("/exports/{period}", s.export)
	})
	return r
}

// HealthHandler is the in-VPC health router: /healthz (process) and /readyz
// (database reachable, registry fresh, serving certificate valid).
func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		dbOK := s.deps.Store.Ping(ctx) == nil
		registryOK := s.deps.Registry.Fresh()
		cert := s.serving.Load()
		certOK := cert != nil && s.deps.Now().Before(cert.Leaf.NotAfter)
		status := http.StatusOK
		if !dbOK || !registryOK || !certOK {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]bool{"database": dbOK, "registry": registryOK, "serving_certificate": certOK})
	})
	return mux
}

// Run serves until ctx is done: the public TLS listener, health, metrics and
// the background loops (serving-certificate renewal, alert retries, FX
// fetches are started by the caller).
func (s *Server) Run(ctx context.Context) error {
	cfg := s.deps.Config
	public := &http.Server{
		Addr: cfg.ListenAddr, Handler: s.Handler(), TLSConfig: s.TLSConfig(),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second,
		ErrorLog: slog.NewLogLogger(s.deps.Logger.Handler(), slog.LevelWarn),
	}
	health := &http.Server{Addr: cfg.HealthAddr, Handler: s.HealthHandler(), ReadHeaderTimeout: 5 * time.Second}
	servers := []*http.Server{public, health}
	if s.deps.Metrics != nil {
		servers = append(servers, &http.Server{
			Addr: cfg.MetricsAddr, Handler: promhttp.HandlerFor(s.deps.Metrics.Registry, promhttp.HandlerOpts{}),
			ReadHeaderTimeout: 5 * time.Second,
		})
	}

	go s.renewLoop(ctx)
	go s.deps.Alerts.Run(ctx, time.Minute)
	go s.deps.Limiter.RunPruner(ctx, time.Minute, func(err error) { s.deps.Logger.Warn("rate counter prune failed", "error", err) })

	errCh := make(chan error, len(servers))
	for i, srv := range servers {
		go func() {
			var err error
			if i == 0 {
				err = srv.ListenAndServeTLS("", "")
			} else {
				err = srv.ListenAndServe()
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("listen %s: %w", srv.Addr, err)
			}
		}()
	}
	s.deps.Logger.Info("central gateway serving", "public", cfg.ListenAddr, "health", cfg.HealthAddr, "host", cfg.PublicHost)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
	return runErr
}

func (s *Server) renewLoop(ctx context.Context) {
	ticker := time.NewTicker(ServingRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RenewServing(); err != nil {
				s.deps.Logger.Error("serving certificate renewal failed", "error", err)
			}
		}
	}
}
