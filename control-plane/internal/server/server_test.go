package server_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/server"
	dashboardui "github.com/nunocgoncalves/iterabase-mono/control-plane/ui"
)

func testAppRouter() http.Handler {
	return server.New(server.Services{UI: dashboardui.NewHandler(fstest.MapFS{
		"index.html":    {Data: []byte(`<!doctype html><html><head><title>Iterabase</title></head><body><div id="root"></div></body></html>`)},
		"assets/app.js": {Data: []byte("console.log('app')")},
	})})
}

func serve(router http.Handler, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

// TestAppDocumentAndDeepLinks proves the app document (with its per-request
// CSP style nonce) answers the root and every browser deep link, while assets
// and health keep their own routes (DES-HOR-459-04).
func TestAppDocumentAndDeepLinks(t *testing.T) {
	router := testAppRouter()
	nonces := map[string]bool{}
	for _, path := range []string{"/", "/work/W-1", "/team/quotes", "/settings/profile", "/auth/verify?token=abc"} {
		response := serve(router, http.MethodGet, path)
		require.Equal(t, http.StatusOK, response.Code, path)
		assert.Equal(t, "no-cache", response.Header().Get("Cache-Control"), path)
		assert.Contains(t, response.Header().Get("Content-Security-Policy"), "connect-src 'self'", path)
		match := regexp.MustCompile(`<meta property="csp-nonce" content="([^"]+)">`).FindStringSubmatch(response.Body.String())
		require.Len(t, match, 2, path)
		assert.Contains(t, response.Header().Get("Content-Security-Policy"), "style-src 'self' 'nonce-"+match[1]+"'", path)
		assert.False(t, nonces[match[1]], "%s: nonce is fresh per response", path)
		nonces[match[1]] = true
	}
	asset := serve(router, http.MethodGet, "/assets/app.js")
	assert.Equal(t, http.StatusOK, asset.Code)
	assert.Equal(t, "public, max-age=31536000, immutable", asset.Header().Get("Cache-Control"))
	assert.Equal(t, http.StatusOK, serve(router, http.MethodGet, "/healthz").Code)
}

// TestUnknownPathsKeepTheirOwnNotFound proves the deep-link fallback never
// answers an API, asset or health path with the app document.
func TestUnknownPathsKeepTheirOwnNotFound(t *testing.T) {
	router := testAppRouter()
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/v1/unknown"},
		{http.MethodGet, "/v1/work-items/W-1/unknown"},
		{http.MethodPost, "/v1/unknown"},
		{http.MethodGet, "/v1"},
	} {
		response := serve(router, request.method, request.path)
		assert.Equal(t, http.StatusNotFound, response.Code, "%s %s", request.method, request.path)
		assert.Equal(t, "application/json", response.Header().Get("Content-Type"), "%s %s", request.method, request.path)
		assert.JSONEq(t, `{"error":"not found"}`, response.Body.String(), "%s %s", request.method, request.path)
	}
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/assets/missing.js"},
		{http.MethodGet, "/assets"},
		{http.MethodGet, "/healthz/extra"},
		{http.MethodGet, "/readyz/extra"},
		{http.MethodPost, "/work/W-1"},
	} {
		response := serve(router, request.method, request.path)
		assert.Equal(t, http.StatusNotFound, response.Code, "%s %s", request.method, request.path)
		assert.NotContains(t, response.Body.String(), "csp-nonce", "%s %s", request.method, request.path)
	}
}

func TestHealthz(t *testing.T) {
	router := server.New(server.Services{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestMetricsNotExposedOnApplicationListener(t *testing.T) {
	router := server.New(server.Services{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	router.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestReadyz_NoDatabase(t *testing.T) {
	router := server.New(server.Services{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

type memoryObjects struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemoryObjects() *memoryObjects               { return &memoryObjects{m: map[string][]byte{}} }
func (m *memoryObjects) Ready(context.Context) error { return nil }
func (m *memoryObjects) Put(_ context.Context, key string, r io.Reader, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.m[key]; ok {
		return errors.New("overwrite")
	}
	b, err := io.ReadAll(r)
	if err == nil {
		m.m[key] = b
	}
	return err
}
func (m *memoryObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.m[key]
	if !ok {
		return nil, errors.New("missing")
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
}
func (m *memoryObjects) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.m, key)
	m.mu.Unlock()
	return nil
}
