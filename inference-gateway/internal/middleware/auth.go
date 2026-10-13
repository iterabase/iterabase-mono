package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// identityIDKey is the context key for the authenticated identity id.
type identityIDKey struct{}

// WithIdentityID stores the authenticated identity id in the context.
func WithIdentityID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, identityIDKey{}, id)
}

// IdentityIDFromContext returns the authenticated identity id, or "" if unset.
func IdentityIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(identityIDKey{}).(string)
	return id
}

// Auth guards the customer listener. Legacy scoped API keys retired with the
// V2-only control-plane authority (HOR-582, DES-HOR-582-01), and live V2
// credential enforcement is delivered by HOR-513. Until then every customer
// request is denied before any backend call: there is no snapshot-cached key
// lookup and no fallback identity. The workload mTLS listener is unaffected.
func Auth(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if extractBearerToken(r) == "" {
				writeAuthError(w, http.StatusUnauthorized, "missing or invalid Authorization header", "invalid_api_key")
				return
			}
			logger.Debug("customer bearer credential refused: V2 customer inference is not enabled")
			writeAuthError(w, http.StatusUnauthorized, "invalid API key", "invalid_api_key")
		})
	}
}

// AdminAuth validates the admin API key from the X-Admin-Key header (env-configured).
// Used only for the debug endpoint; the legacy admin CRUD API is gone.
func AdminAuth(adminKey string, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-Admin-Key")
			if key == "" || key != adminKey {
				writeAuthError(w, http.StatusUnauthorized, "invalid or missing admin key", "invalid_admin_key")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func extractBearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func writeAuthError(w http.ResponseWriter, status int, message string, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "authentication_error",
			"code":    code,
		},
	}); err != nil {
		slog.Error("failed to write auth error response", "error", err)
	}
}
