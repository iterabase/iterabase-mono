// Package server exposes the control-plane HTTP API (cmd/api): health and
// readiness, the browser shell, the public browser-authentication journey, and
// the customer work, artifact, and People APIs. Every customer route is
// authorized by one V2 class (see authority.go): a browser session, a V2 API
// credential holding the route's single action, or both.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	artifactstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
	cpmetrics "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/metrics"
	workstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/work"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/workflow"
	dashboardui "github.com/nunocgoncalves/iterabase-mono/control-plane/ui"
)

// Services are the dependencies injected into the HTTP API.
type Services struct {
	Pool      *pgxpool.Pool
	Store     *identity.Store
	Work      *workstore.Store
	Workflows *workflow.Store
	Artifacts *artifactstore.Service
	Metrics   *cpmetrics.Metrics
	Auth      *AuthServices
	// UI serves the browser app; nil serves the app embedded in the binary.
	UI http.Handler
}

// Handler holds the identity service dependencies used by the API handlers.
type Handler struct {
	store     *identity.Store
	work      *workstore.Store
	workflows *workflow.Store
	artifacts *artifactstore.Service
	authCfg   *AuthServices
	classes   map[string]RouteClass
}

// New builds the HTTP API router.
func New(svc Services) http.Handler {
	router, _ := NewClassified(svc)
	return router
}

// NewClassified builds the router and returns the authority class of every
// registered route, keyed by RouteKey.
func NewClassified(svc Services) (chi.Router, map[string]RouteClass) {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	if svc.Metrics != nil {
		r.Use(svc.Metrics.HTTPMiddleware("api"))
	}
	r.Use(func(next http.Handler) http.Handler {
		// SSE is a long-lived resumable stream; normal API requests retain the
		// bounded request timeout.
		timed := middleware.Timeout(30 * time.Second)(next)
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/v1/work-events" || req.URL.Path == "/v1/artifacts" ||
				strings.HasPrefix(req.URL.Path, "/v1/artifacts/") {
				next.ServeHTTP(w, req)
				return
			}
			timed.ServeHTTP(w, req)
		})
	})

	h := &Handler{
		store:     svc.Store,
		work:      svc.Work,
		workflows: svc.Workflows,
		artifacts: svc.Artifacts,
		authCfg:   svc.Auth,
		classes:   map[string]RouteClass{},
	}

	// The same-origin product shell is static; it reaches work only through
	// the browser session. Browser deep links fall back to the app document
	// (DES-HOR-459-04) while unknown API paths keep a JSON 404.
	ui := svc.UI
	if ui == nil {
		ui = dashboardui.Handler()
	}
	h.route(r, http.MethodGet, "/", probe(), ui.ServeHTTP)
	h.route(r, http.MethodGet, "/assets/*", probe(), ui.ServeHTTP)
	h.route(r, http.MethodGet, "/healthz", probe(), healthz)
	h.route(r, http.MethodGet, "/readyz", probe(), readyz(svc.Pool, svc.Artifacts))
	h.fallback(r, probe(), appFallback(ui))

	h.registerAuthRoutes(r)
	h.registerPeopleRoutes(r)
	h.registerWorkRoutes(r)
	h.registerWorkflowRoutes(r)
	h.registerArtifactRoutes(r)

	return r, h.classes
}

// appFallback answers every request no route matches. Unknown API paths get
// the API's JSON 404; a browser GET for any other path outside the asset and
// health surfaces gets the app document so the client router can resolve it.
// Machine endpoints that live elsewhere (/metrics on the metrics listener,
// /.well-known/ documents) stay plain 404s so a misdirected client never
// mistakes the app document for them.
func appFallback(ui http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case hasPathPrefix(path, "/v1"):
			writeJSON(w, http.StatusNotFound, errorBody{Error: "not found"})
		case r.Method != http.MethodGet && r.Method != http.MethodHead,
			hasPathPrefix(path, "/assets"), hasPathPrefix(path, "/healthz"), hasPathPrefix(path, "/readyz"),
			hasPathPrefix(path, "/metrics"), hasPathPrefix(path, "/.well-known"):
			http.NotFound(w, r)
		default:
			ui.ServeHTTP(w, r)
		}
	}
}

// hasPathPrefix reports whether path is prefix or a path below it.
func hasPathPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func readyz(pool *pgxpool.Pool, artifacts *artifactstore.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pool == nil {
			http.Error(w, "database not ready", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "database not ready", http.StatusServiceUnavailable)
			return
		}
		if artifacts != nil {
			if err := artifacts.Ready(ctx); err != nil {
				http.Error(w, "artifact storage not ready", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	}
}

// --- helpers ---

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
