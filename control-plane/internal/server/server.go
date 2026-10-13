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
	dashboardui "github.com/nunocgoncalves/iterabase-mono/control-plane/ui"
)

// Services are the dependencies injected into the HTTP API.
type Services struct {
	Pool      *pgxpool.Pool
	Store     *identity.Store
	Work      *workstore.Store
	Artifacts *artifactstore.Service
	Metrics   *cpmetrics.Metrics
	Auth      *AuthServices
}

// Handler holds the identity service dependencies used by the API handlers.
type Handler struct {
	store     *identity.Store
	work      *workstore.Store
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
		artifacts: svc.Artifacts,
		authCfg:   svc.Auth,
		classes:   map[string]RouteClass{},
	}

	// The same-origin product shell is static; it reaches work only through
	// the browser session.
	ui := dashboardui.Handler()
	h.route(r, http.MethodGet, "/", probe(), ui.ServeHTTP)
	h.route(r, http.MethodGet, "/assets/*", probe(), ui.ServeHTTP)
	h.route(r, http.MethodGet, "/healthz", probe(), healthz)
	h.route(r, http.MethodGet, "/readyz", probe(), readyz(svc.Pool, svc.Artifacts))

	h.registerAuthRoutes(r)
	h.registerPeopleRoutes(r)
	h.registerWorkRoutes(r)
	h.registerArtifactRoutes(r)

	return r, h.classes
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
