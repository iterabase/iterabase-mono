package server_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/server"
)

func TestDashboardStatic(t *testing.T) {
	router := server.New(server.Services{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	router.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Header().Get("Content-Security-Policy"), "connect-src 'self'")
	assert.Equal(t, "no-cache", rr.Header().Get("Cache-Control"))
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
