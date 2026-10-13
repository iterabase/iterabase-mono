package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/metrics"
	gatewaymw "github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/snapshot"
)

func newRouter(logger *slog.Logger, m *metrics.Metrics, deps *Deps) http.Handler {
	r := chi.NewRouter()

	// Base middleware — order matters.
	r.Use(chimiddleware.Recoverer)
	r.Use(chimiddleware.RealIP) //nolint:staticcheck // Preserve existing proxy-header behavior during source migration.
	r.Use(gatewaymw.RequestID)
	r.Use(gatewaymw.Logging(logger))

	// Liveness — no auth required.
	r.Get("/health", healthHandler)

	if deps != nil {
		// Readiness — snapshot freshness gate (a stale pod drops from the LB).
		r.Get("/readyz", readinessHandler(deps.Cache, deps.ReadinessStaleness, m))

		// OpenAI-compatible endpoints — fully wired.
		r.Route("/v1", func(r chi.Router) {
			if m != nil {
				r.Use(gatewaymw.Metrics(m, "api"))
			}
			r.Use(gatewaymw.Auth(logger))
			if deps.Limiter != nil {
				r.Use(gatewaymw.RateLimit(deps.Cache, deps.Limiter, m, logger))
			}
			r.Get("/models", deps.ProxyHandler.ListModels)
			r.Post("/chat/completions", deps.ProxyHandler.ChatCompletions)
		})

		// Admin/debug endpoints — the legacy admin CRUD API is gone; this is a
		// read-only view of the consumed snapshot for ops and owner E2E smoke.
		r.Route("/admin/v1", func(r chi.Router) {
			if deps.AdminKey != "" {
				r.Use(gatewaymw.AdminAuth(deps.AdminKey, logger))
			}
			r.Get("/health", healthHandler)
			r.Get("/snapshot", snapshotHandler(deps.Cache, deps.ReadinessStaleness))
		})
	} else {
		// Stub mode — no dependencies wired.
		r.Route("/v1", func(r chi.Router) {
			r.Get("/models", notImplementedHandler)
			r.Post("/chat/completions", notImplementedHandler)
		})
		r.Route("/admin/v1", func(r chi.Router) {
			r.Get("/health", healthHandler)
		})
	}

	return r
}

// newWorkloadRouter wires the supervisor mTLS path (HOR-398; ARCH-010/011).
// It reuses the existing proxy handler + streaming pipeline behind the
// WorkloadAuth middleware, which resolves the SPIFFE identity + active durable
// turn scope and stamps the effective identity (so capability/usage/rate-limit
// logic is shared with the API-key path). The model-mismatch denial is applied
// in the proxy handler via WorkloadContextFromContext. No admin/snapshot routes
// are exposed on this listener.
func newWorkloadRouter(logger *slog.Logger, m *metrics.Metrics, deps *Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(chimiddleware.Recoverer)
	r.Use(chimiddleware.RealIP) //nolint:staticcheck // Preserve existing proxy-header behavior during source migration.
	r.Use(gatewaymw.RequestID)
	r.Use(gatewaymw.Logging(logger))
	if m != nil {
		r.Use(gatewaymw.Metrics(m, "workload"))
	}
	r.Use(gatewaymw.WorkloadAuth(deps.WorkloadStore, deps.TrustDomain, logger))
	if deps.Limiter != nil {
		r.Use(gatewaymw.RateLimit(deps.Cache, deps.Limiter, m, logger))
	}
	r.Get("/v1/models", deps.ProxyHandler.ListModels)
	r.Post("/v1/chat/completions", deps.ProxyHandler.ChatCompletions)
	return r
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
		slog.Error("failed to write health response", "error", err)
	}
}

// readinessHandler reports 200 if the snapshot is fresh, 503 otherwise — so a
// pod with a dead LISTEN (stale beyond ReadinessStaleness) drops from the LB.
func readinessHandler(cache snapshot.Reader, staleness time.Duration, m *metrics.Metrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fresh := cache != nil && cache.Fresh(staleness)
		if m != nil {
			if fresh {
				m.SnapshotFresh.Set(1)
			} else {
				m.SnapshotFresh.Set(0)
			}
			m.BackendHealth.Reset()
			if cache != nil {
				m.SnapshotLastRefresh.Set(float64(cache.LastRefresh().Unix()))
				for _, entry := range cache.ListCatalog() {
					available := float64(0)
					if entry.Available {
						available = 1
					}
					m.BackendHealth.WithLabelValues(entry.ModelID, entry.BackendRef).Set(available)
				}
			}
		}
		status := http.StatusOK
		if !fresh {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		resp := map[string]any{"fresh": fresh}
		if cache != nil {
			resp["last_refresh"] = cache.LastRefresh()
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// snapshotHandler exposes the gateway's consumed catalog snapshot (read-only,
// for ops and owner E2E smoke). No secrets — API key hashes are not exposed,
// only counts.
func snapshotHandler(cache snapshot.Reader, staleness time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"catalog":      cache.ListCatalog(),
			"fresh":        cache.Fresh(staleness),
			"last_refresh": cache.LastRefresh(),
		})
	}
}

func notImplementedHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "not implemented",
			"type":    "not_implemented_error",
			"code":    "not_implemented",
		},
	}); err != nil {
		slog.Error("failed to write not-implemented response", "error", err)
	}
}
