package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/attribution"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/billing"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/opauth"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/pki"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/registry"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/metrics"
	gatewaymw "github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/proxy"
)

// Error codes beyond the shared upstream ones.
const (
	codeInstallationRevoked     = "installation_revoked"
	codeModelNotFound           = "model_not_found"
	codeOperatorUnauthenticated = "operator_unauthenticated"
	codeOperatorForbidden       = "operator_forbidden"
	codeInstallationNotFound    = "installation_not_found"
	codeInstallationConflict    = "installation_conflict"
	codeInvalidCSR              = "invalid_csr"
	codeInvalidRequest          = "invalid_request"
)

type ctxKey int

const (
	installationKey ctxKey = iota
	callerKey
	logScopeKey
)

// logScope collects payload-free attributes for the completion log line.
type logScope struct {
	installationID string
	operator       string
}

func scope(ctx context.Context) *logScope {
	if s, ok := ctx.Value(logScopeKey).(*logScope); ok {
		return s
	}
	return &logScope{}
}

func installationFrom(ctx context.Context) billing.Installation {
	inst, _ := ctx.Value(installationKey).(billing.Installation)
	return inst
}

func callerFrom(ctx context.Context) opauth.Caller {
	c, _ := ctx.Value(callerKey).(opauth.Caller)
	return c
}

// sanitizeRequestID drops an incoming X-Request-ID that is not a valid
// attribution value, so the RequestID middleware generates one instead.
func sanitizeRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get(attribution.HeaderRequestID); v != "" && !attribution.Valid(v) {
			r.Header.Del(attribution.HeaderRequestID)
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status for the log line and keeps streaming
// working.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logging emits one payload-free line per request.
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ls := &logScope{}
		*r = *r.WithContext(context.WithValue(r.Context(), logScopeKey, ls))
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		attrs := []slog.Attr{
			slog.String("request_id", gatewaymw.RequestIDFromContext(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		}
		if ls.installationID != "" {
			attrs = append(attrs, slog.String("installation_id", ls.installationID))
		}
		if ls.operator != "" {
			attrs = append(attrs, slog.String("operator", ls.operator))
		}
		if md := gatewaymw.GetMetricsData(r.Context()); md != nil && md.Model != "" {
			attrs = append(attrs, slog.String("model", md.Model), slog.Bool("streaming", md.Streaming))
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		} else if rec.status >= 400 {
			level = slog.LevelWarn
		}
		s.deps.Logger.LogAttrs(r.Context(), level, "request completed", attrs...)
	})
}

// installationAuth authenticates the installation by its verified client
// certificate and authorizes it against the registry on every request
// (DES-HOR-661-02). A stale registry fails closed but retryably.
func (s *Server) installationAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
			s.deny(w, "an installation client certificate is required")
			return
		}
		id, err := pki.IdentityFromCert(r.TLS.PeerCertificates[0])
		if err != nil {
			s.deny(w, "not an installation certificate")
			return
		}
		scope(r.Context()).installationID = id.InstallationID
		inst, err := s.deps.Registry.Authorize(id.InstallationID, id.Serial)
		switch {
		case errors.Is(err, registry.ErrStale):
			s.cm.denials.WithLabelValues("registry_stale").Inc()
			s.deps.Logger.Error("installation registry is stale; refusing installation requests")
			proxy.WriteUpstreamUnavailable(w, "")
			return
		case err != nil:
			s.deny(w, "the installation or its certificate is revoked, expired or unknown")
			return
		}
		*r = *r.WithContext(context.WithValue(r.Context(), installationKey, inst))
		next.ServeHTTP(w, r)
	})
}

func (s *Server) deny(w http.ResponseWriter, message string) {
	s.cm.denials.WithLabelValues(codeInstallationRevoked).Inc()
	proxy.WriteError(w, http.StatusForbidden, "permission_error", message, codeInstallationRevoked)
}

// operatorAuth authenticates the operator with a presigned STS proof.
func (s *Server) operatorAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := s.deps.Verifier.Verify(r.Context(), r.Header.Get(opauth.Header))
		switch {
		case errors.Is(err, opauth.ErrUnauthenticated):
			s.deps.Logger.Warn("operator authentication failed", "error", err)
			proxy.WriteError(w, http.StatusUnauthorized, "authentication_error", "operator authentication failed", codeOperatorUnauthenticated)
			return
		case errors.Is(err, opauth.ErrForbidden):
			s.deps.Logger.Warn("operator not allowed", "error", err)
			proxy.WriteError(w, http.StatusForbidden, "permission_error", "caller is not an operator", codeOperatorForbidden)
			return
		case err != nil:
			s.deps.Logger.Error("operator authentication unavailable", "error", err)
			proxy.WriteUpstreamUnavailable(w, "")
			return
		}
		scope(r.Context()).operator = caller.RoleARN + "/" + caller.Session
		*r = *r.WithContext(context.WithValue(r.Context(), callerKey, caller))
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// centralMetrics are central's own bounded counters.
type centralMetrics struct {
	usageRecords       *prometheus.CounterVec
	meteringFailures   prometheus.Counter
	denials            *prometheus.CounterVec
	operatorRequests   *prometheus.CounterVec
	fxUnavailable      prometheus.Counter
	attributionDropped prometheus.Counter
	alertsCrossed      *prometheus.CounterVec
}

func newCentralMetrics(m *metrics.Metrics) *centralMetrics {
	cm := &centralMetrics{
		usageRecords: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "central_usage_records_total", Help: "Metered requests by classification.",
		}, []string{"classification"}),
		meteringFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "central_metering_failures_total", Help: "Requests that could not be metered (never served).",
		}),
		denials: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "central_installation_denials_total", Help: "Installation requests refused by code.",
		}, []string{"code"}),
		operatorRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "central_operator_requests_total", Help: "Operator API requests by action and status class.",
		}, []string{"action", "status_class"}),
		fxUnavailable: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "central_fx_rate_unavailable_total", Help: "Requests refused because a needed FX rate is not locked.",
		}),
		attributionDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "central_attribution_values_dropped_total", Help: "Invalid attribution header values dropped.",
		}),
		alertsCrossed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "central_credit_alerts_total", Help: "Allowance thresholds crossed.",
		}, []string{"threshold"}),
	}
	if m != nil {
		m.Registry.MustRegister(cm.usageRecords, cm.meteringFailures, cm.denials, cm.operatorRequests,
			cm.fxUnavailable, cm.attributionDropped, cm.alertsCrossed)
	}
	return cm
}
