package middleware

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAuth_DeniesEveryCustomerCredential proves the customer listener serves no
// request until V2 credential enforcement (HOR-513): legacy keys retired with
// HOR-582 and no bearer value can reach the handler.
func TestAuth_DeniesEveryCustomerCredential(t *testing.T) {
	h := Auth(slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no customer request may reach the handler")
	}))
	for _, header := range []string{"Bearer secret-1", "Bearer cp-ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), "invalid_api_key")
	}
}

func TestAuth_MissingHeader(t *testing.T) {
	h := Auth(slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not call next")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
