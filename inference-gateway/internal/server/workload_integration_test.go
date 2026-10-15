package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/proxy"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/snapshot"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/spiffe"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/spiffe/testca"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/workload"
)

// workloadFixture applies both the snapshot (catalog/identity/permissions) and
// workload (pools/turns/runs/assignments) test schemas to a fresh Postgres.
func workloadFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, snapshot.FixtureSchema)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, workload.FixtureSchema)
	require.NoError(t, err)
}

// startPG starts a testcontainers Postgres and returns the container + a pool + conn string.
func startPG(t *testing.T, ctx context.Context) (testcontainers.Container, *pgxpool.Pool, string) {
	t.Helper()
	pgC, err := postgres.Run(ctx, "postgres:16-alpine@sha256:cf78e76683b9ca8c5733cbbdce6c9262b45b6767934dd0a95e671f9a0fc20685",
		postgres.WithDatabase("gw"), postgres.WithUsername("t"), postgres.WithPassword("t"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(30*time.Second)))
	require.NoError(t, err)
	pgConn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, pgConn)
	require.NoError(t, err)
	return pgC, pool, pgConn
}

const (
	wlSpiffePrefix = "spiffe://iterabase.local/pools/pool-1/"
	wlSupervisorID = "spiffe://iterabase.local/pools/pool-1/workers/pod-abc"
	wlModel        = "qwen3-27b"
)

// wlSeed inserts a pool + run + assignment + active turn (model=wlModel) plus
// a healthy backend, available alias, and a real scope identity (so the
// API-key regression can bind an api_key to it). Returns the run id, turn id,
// and scope identity id.
func wlSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, vllmURL, turnState string) (runID, turnID, scopeID string) {
	t.Helper()
	var poolID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO toolgateway.pools (key, name, spiffe_id_prefix)
		VALUES ('default/pool-1', 'pool-1', $1) RETURNING id::text`, wlSpiffePrefix).Scan(&poolID))
	// A real identity row so api_keys FK + the snapshot capability view resolve.
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO identity.identities (key, kind) VALUES ('workflow:default/scope', 'workflow') RETURNING id::text`).Scan(&scopeID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO runtime.workflow_runs (kind, scope_identity_id, session_id, session_dir)
		VALUES ('chat', $1::uuid, 'sess-1', '/tmp/sess') RETURNING id::text`, scopeID).Scan(&runID))
	_, err := pool.Exec(ctx, `INSERT INTO runtime.run_pool_assignments (run_id, pool_id) VALUES ($1, $2::uuid)`, runID, poolID)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO runtime.turns (run_id, session_id, model, state) VALUES ($1, 'sess-1', $2, $3) RETURNING id::text`,
		runID, wlModel, turnState).Scan(&turnID))

	// Active assignment bound to worker pod-abc at generation 1 (HOR-249/DEC-041):
	// the workload path binds authorization to the verified worker + generation.
	if turnState == "running" {
		_, err = pool.Exec(ctx, `
			INSERT INTO runtime.turn_assignments
			    (turn_id, run_id, pool_id, worker_id, fencing_generation, attempt_id,
			     scope_identity_id, agent_pool_key, state)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'pod-abc', 1, $2, $4::uuid, 'pool-1', 'active')`,
			turnID, runID, poolID, scopeID)
		require.NoError(t, err)
	}

	// Catalog: backend + alias (rewrite on so the backend receives the HF id).
	_, err = pool.Exec(ctx, `
		INSERT INTO catalog.backends (key, name, namespace, kind, model, service_url, deployed, healthy)
		VALUES ('default/qwen', 'qwen', 'default', 'vLLM', 'Qwen/Qwen3-27B', $1, true, true)`, vllmURL)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO catalog.models (key, namespace, model_id, backend_ref, transforms, available)
		VALUES ('default/qwen3-27b', 'default', 'qwen3-27b', 'qwen', '{"rewrite_model_name":true}'::jsonb, true)`)
	require.NoError(t, err)
	return runID, turnID, scopeID
}

// startWorkloadServer stands up a real mTLS HTTP/2 server backed by the full
// proxy pipeline + WorkloadAuth, plus an httptest vLLM backend. Returns the
// server URL, the test CA (for minting client certs), and the vLLM backend.
func startWorkloadServer(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pgConn string, vllm http.Handler) (*httptest.Server, *testca.CA) {
	t.Helper()
	logger := slog.Default()

	store := snapshot.NewPGStore(pool)
	cache := snapshot.NewCache(store, pgConn, logger, 30*time.Second)
	require.NoError(t, cache.Start(ctx))
	t.Cleanup(cache.Stop)

	m := metrics.New(prometheus.NewRegistry())
	limiter := ratelimit.NewPostgresLimiter(pool, ratelimit.Installation)
	proxyHandler := proxy.NewHandler(cache, limiter, m, nil, logger)
	deps := &Deps{
		ProxyHandler:       proxyHandler,
		Cache:              cache,
		Limiter:            limiter,
		ReadinessStaleness: 60 * time.Second,
		WorkloadStore:      workload.NewPGStore(pool),
		TrustDomain:        spiffe.DefaultTrustDomain,
	}

	ca, err := testca.New()
	require.NoError(t, err)
	serverCert, err := ca.Leaf(testca.LeafOpts{SPIFFEID: wlSpiffePrefix + "gateway", DNSNames: []string{"localhost"}, IsServer: true})
	require.NoError(t, err)
	tlsConfig := spiffe.ServerTLSConfig(serverCert, ca.Pool)

	srv := httptest.NewUnstartedServer(newWorkloadRouter(logger, m, deps))
	srv.TLS = tlsConfig
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, ca
}

// mTLSClient builds an http.Client that presents a supervisor leaf cert signed
// by the test CA. nil cert -> a client with no client cert (handshake fails).
func mTLSClient(t *testing.T, ca *testca.CA, spiffeID string) *http.Client {
	t.Helper()
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    ca.Pool,
			ServerName: "localhost", // server cert carries DNS SAN "localhost"
			NextProtos: []string{"h2"},
			MinVersion: tls.VersionTLS12,
		},
	}
	if spiffeID != "" {
		leaf, err := ca.Leaf(testca.LeafOpts{SPIFFEID: spiffeID})
		require.NoError(t, err)
		transport.TLSClientConfig.Certificates = []tls.Certificate{leaf}
	}
	transport.ForceAttemptHTTP2 = true
	return &http.Client{Transport: transport, Timeout: 15 * time.Second}
}

func TestWorkloadMTLS_ValidActiveTurn_Streams(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
	pgC, pool, pgConn := startPG(t, ctx)
	defer pool.Close()
	defer pgC.Terminate(ctx) //nolint:errcheck
	workloadFixture(t, ctx, pool)

	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"Qwen/Qwen3-27B","choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":5}}`))
	}))
	t.Cleanup(vllm.Close)

	runID, turnID, _ := wlSeed(t, ctx, pool, vllm.URL, "running")
	srv, ca := startWorkloadServer(t, ctx, pool, pgConn, nil)

	body := `{"model":"qwen3-27b","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.Header.Set(middleware.HeaderRunID, runID)
	req.Header.Set(middleware.HeaderTurnID, turnID)
	req.Header.Set(middleware.HeaderFencingGeneration, "1")
	resp, err := mTLSClient(t, ca, wlSupervisorID).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	rb, _ := io.ReadAll(resp.Body)
	var r map[string]any
	require.NoError(t, json.Unmarshal(rb, &r))
	assert.Equal(t, "qwen3-27b", r["model"], "response model is the alias")
}

func TestWorkloadMTLS_StreamingCancellationClosesBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
	pgC, pool, pgConn := startPG(t, ctx)
	defer pool.Close()
	defer pgC.Terminate(ctx) //nolint:errcheck
	workloadFixture(t, ctx, pool)

	var backendCancelled atomic.Bool
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		// Stream one chunk, then block until the client cancels.
		_, _ = fmt.Fprintf(w, "data: {\"model\":\"Qwen/Qwen3-27B\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		flusher.Flush()
		<-r.Context().Done()
		backendCancelled.Store(true)
	}))
	t.Cleanup(vllm.Close)

	runID, turnID, _ := wlSeed(t, ctx, pool, vllm.URL, "running")
	srv, ca := startWorkloadServer(t, ctx, pool, pgConn, nil)

	body := `{"model":"qwen3-27b","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	reqCtx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.Header.Set(middleware.HeaderRunID, runID)
	req.Header.Set(middleware.HeaderTurnID, turnID)
	req.Header.Set(middleware.HeaderFencingGeneration, "1")

	client := mTLSClient(t, ca, wlSupervisorID)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Read a bit, then cancel the request. The upstream backend ctx must fire.
	buf := make([]byte, 64)
	_, _ = resp.Body.Read(buf) // first SSE chunk
	cancel()
	require.Eventually(t, func() bool { return backendCancelled.Load() }, 5*time.Second, 50*time.Millisecond,
		"backend context must be cancelled when the client cancels the stream")
}

func TestWorkloadMTLS_NonSupervisorSAN_Denied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
	pgC, pool, pgConn := startPG(t, ctx)
	defer pool.Close()
	defer pgC.Terminate(ctx) //nolint:errcheck
	workloadFixture(t, ctx, pool)
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("backend must not be hit") }))
	t.Cleanup(vllm.Close)
	runID, turnID, _ := wlSeed(t, ctx, pool, vllm.URL, "running")
	srv, ca := startWorkloadServer(t, ctx, pool, pgConn, nil)

	body := `{"model":"qwen3-27b","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.Header.Set(middleware.HeaderRunID, runID)
	req.Header.Set(middleware.HeaderTurnID, turnID)
	req.Header.Set(middleware.HeaderFencingGeneration, "1")
	// A tool-runner SAN: chain verifies, but not a supervisor identity.
	resp, err := mTLSClient(t, ca, "spiffe://iterabase.local/tool-runners/default/r1").Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestWorkloadMTLS_TrustDomainMismatch_Denied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
	pgC, pool, pgConn := startPG(t, ctx)
	defer pool.Close()
	defer pgC.Terminate(ctx) //nolint:errcheck
	workloadFixture(t, ctx, pool)
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("backend must not be hit") }))
	t.Cleanup(vllm.Close)
	runID, turnID, _ := wlSeed(t, ctx, pool, vllm.URL, "running")
	srv, ca := startWorkloadServer(t, ctx, pool, pgConn, nil)

	// A CA for a DIFFERENT trust domain, signing a supervisor-shaped id.
	evilCA, err := testca.New()
	require.NoError(t, err)
	evilLeaf, err := evilCA.Leaf(testca.LeafOpts{SPIFFEID: "spiffe://evil.example/pools/pool-1/workers/pod-abc"})
	require.NoError(t, err)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool, ServerName: "localhost", Certificates: []tls.Certificate{evilLeaf}, NextProtos: []string{"h2"}}, ForceAttemptHTTP2: true}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	body := `{"model":"qwen3-27b","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.Header.Set(middleware.HeaderRunID, runID)
	req.Header.Set(middleware.HeaderTurnID, turnID)
	req.Header.Set(middleware.HeaderFencingGeneration, "1")
	// The chain does not verify against the gateway's ClientCAs -> handshake fails.
	_, err = client.Do(req)
	require.Error(t, err, "mTLS handshake must fail for an untrusted client cert")
}

func TestWorkloadMTLS_TerminalTurn_Denied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
	pgC, pool, pgConn := startPG(t, ctx)
	defer pool.Close()
	defer pgC.Terminate(ctx) //nolint:errcheck
	workloadFixture(t, ctx, pool)
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("backend must not be hit") }))
	t.Cleanup(vllm.Close)
	runID, turnID, _ := wlSeed(t, ctx, pool, vllm.URL, "succeeded")
	srv, ca := startWorkloadServer(t, ctx, pool, pgConn, nil)

	body := `{"model":"qwen3-27b","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.Header.Set(middleware.HeaderRunID, runID)
	req.Header.Set(middleware.HeaderTurnID, turnID)
	req.Header.Set(middleware.HeaderFencingGeneration, "1")
	resp, err := mTLSClient(t, ca, wlSupervisorID).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestWorkloadMTLS_ModelMismatch_Denied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
	pgC, pool, pgConn := startPG(t, ctx)
	defer pool.Close()
	defer pgC.Terminate(ctx) //nolint:errcheck
	workloadFixture(t, ctx, pool)
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("backend must not be hit") }))
	t.Cleanup(vllm.Close)
	runID, turnID, _ := wlSeed(t, ctx, pool, vllm.URL, "running")
	srv, ca := startWorkloadServer(t, ctx, pool, pgConn, nil)

	// Request a model different from the turn's assigned model.
	body := `{"model":"other-model","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.Header.Set(middleware.HeaderRunID, runID)
	req.Header.Set(middleware.HeaderTurnID, turnID)
	req.Header.Set(middleware.HeaderFencingGeneration, "1")
	resp, err := mTLSClient(t, ca, wlSupervisorID).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	rb, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(rb), "model_mismatch")
}

func TestWorkloadMTLS_MissingTurnContext_Denied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
	pgC, pool, pgConn := startPG(t, ctx)
	defer pool.Close()
	defer pgC.Terminate(ctx) //nolint:errcheck
	workloadFixture(t, ctx, pool)
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("backend must not be hit") }))
	t.Cleanup(vllm.Close)
	_, _, _ = wlSeed(t, ctx, pool, vllm.URL, "running")
	srv, ca := startWorkloadServer(t, ctx, pool, pgConn, nil)

	body := `{"model":"qwen3-27b","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader([]byte(body)))
	// No run/turn headers.
	resp, err := mTLSClient(t, ca, wlSupervisorID).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// Regression: the customer listener stays a separate policy path from the
// workload listener (ARCH-010). With legacy keys retired (HOR-582) a customer
// bearer is denied before any backend call, even for an identity the workload
// path would serve.
func TestWorkloadMTLS_CustomerListenerDeniesBearer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
	pgC, pool, pgConn := startPG(t, ctx)
	defer pool.Close()
	defer pgC.Terminate(ctx) //nolint:errcheck
	workloadFixture(t, ctx, pool)

	var backendCalls atomic.Int32
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"Qwen/Qwen3-27B","choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":5}}`))
	}))
	t.Cleanup(vllm.Close)

	wlSeed(t, ctx, pool, vllm.URL, "running")

	logger := slog.Default()
	store := snapshot.NewPGStore(pool)
	cache := snapshot.NewCache(store, pgConn, logger, 30*time.Second)
	require.NoError(t, cache.Start(ctx))
	t.Cleanup(cache.Stop)
	m := metrics.New(prometheus.NewRegistry())
	limiter := ratelimit.NewPostgresLimiter(pool, ratelimit.Installation)
	proxyHandler := proxy.NewHandler(cache, limiter, m, nil, logger)
	deps := &Deps{ProxyHandler: proxyHandler, Cache: cache, Limiter: limiter, ReadinessStaleness: 60 * time.Second}
	srv := httptest.NewServer(newRouter(logger, m, deps))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"qwen3-27b","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Zero(t, backendCalls.Load())
}

// The workload listener must be HTTP/2-only (ARCH-006): an mTLS client that
// offers only HTTP/1.1 (no h2 ALPN) must be rejected. This does not need
// Postgres — it only exercises the TLS + protocol configuration.
func TestWorkloadMTLS_HTTP1_1Rejected(t *testing.T) {
	ca, err := testca.New()
	require.NoError(t, err)
	serverCert, err := ca.Leaf(testca.LeafOpts{
		SPIFFEID: wlSpiffePrefix + "gateway", DNSNames: []string{"localhost"}, IsServer: true,
	})
	require.NoError(t, err)
	tlsConfig := spiffe.ServerTLSConfig(serverCert, ca.Pool)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = tlsConfig
	srv.EnableHTTP2 = true
	// Mirror server.AttachWorkload: HTTP/2 only (NextProtos=["h2"] is not
	// sufficient on its own — Protocols must disable HTTP/1.1).
	protocols := &http.Protocols{}
	protocols.SetHTTP2(true)
	srv.Config.Protocols = protocols
	srv.StartTLS()
	t.Cleanup(srv.Close)

	// Supervisor client cert, but the client forces HTTP/1.1 ALPN only.
	leaf, err := ca.Leaf(testca.LeafOpts{SPIFFEID: wlSupervisorID})
	require.NoError(t, err)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:      ca.Pool,
			ServerName:   "localhost",
			Certificates: []tls.Certificate{leaf},
			NextProtos:   []string{"http/1.1"}, // no h2
			MinVersion:   tls.VersionTLS12,
		},
		// ForceAttemptHTTP2 is intentionally false: this client speaks HTTP/1.1.
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"qwen3-27b","messages":[]}`)))
	_, err = client.Do(req)
	require.Error(t, err, "an HTTP/1.1 mTLS request must be rejected on the HTTP/2-only workload listener")
}
