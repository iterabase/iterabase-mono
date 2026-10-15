package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/billing"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/config"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/notify"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/opauth"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/pki"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/registry"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/store"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/testpg"
)

// Content markers that must never leave the request path (REQ-036).
const (
	secretPrompt   = "SECRET-PROMPT-7f3a"
	secretResponse = "SECRET-RESPONSE-9c1e"
	providerKey    = "sk-provider-test-key"
)

const testConfig = `public_host: localhost
models:
  managed-model:
    base_url: %s/v1
    provider_model: provider/model-v1
    currency: USD
    input_per_million: "1.00"
    cached_input_per_million: "0.10"
    output_per_million: "1.00"
    effective_from: "2026-10-01"
tiers:
  starter: {workflow_limit: 2, monthly_credits: 5000}
  growth: {workflow_limit: 6, monthly_credits: 20000}
  scale: {workflow_limit: 15, monthly_credits: 60000}
trial: {tier: starter, days_basis: 30}
limits:
  provider: {rpm: %d, tpm: 0}
  installation: {rpm: %d, tpm: 0}
`

// syncBuffer collects log output from concurrent handlers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeVerifier maps header values to callers.
type fakeVerifier map[string]opauth.Caller

func (f fakeVerifier) Verify(_ context.Context, header string) (opauth.Caller, error) {
	if c, ok := f[header]; ok {
		return c, nil
	}
	if header == "forbidden" {
		return opauth.Caller{}, opauth.ErrForbidden
	}
	return opauth.Caller{}, opauth.ErrUnauthenticated
}

// recordingPublisher captures alerts.
type recordingPublisher struct {
	mu       sync.Mutex
	subjects []string
}

func (p *recordingPublisher) Publish(_ context.Context, subject, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subjects = append(p.subjects, subject)
	return nil
}

func (p *recordingPublisher) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.subjects...)
}

// providerCall is what the fake provider saw.
type providerCall struct {
	header http.Header
	body   map[string]any
}

type fixture struct {
	t         *testing.T
	db        *testpg.DB
	store     *store.Store
	authority *pki.Authority
	central   *httptest.Server
	server    *Server
	logs      *syncBuffer
	metrics   *metrics.Metrics
	published *recordingPublisher
	registry  *registry.Cache
	provider  *httptest.Server
	mu        sync.Mutex
	calls     []providerCall
	now       time.Time
}

func newFixture(t *testing.T, installationRPM int) *fixture {
	t.Helper()
	return newFixtureLimits(t, 1000, installationRPM)
}

func newFixtureLimits(t *testing.T, providerRPM, installationRPM int) *fixture {
	t.Helper()
	f := &fixture{t: t, logs: &syncBuffer{}, published: &recordingPublisher{}, now: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	f.provider = httptest.NewTLSServer(http.HandlerFunc(f.serveProvider))
	t.Cleanup(f.provider.Close)

	cfg, err := config.Parse([]byte(fmt.Sprintf(testConfig, f.provider.URL, providerRPM, installationRPM)))
	require.NoError(t, err)
	cfg.ProviderKey = providerKey

	f.db = testpg.Start(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, store.Migrate(ctx, f.db.Pool))
	f.store = store.New(f.db.Pool, billing.NewPolicy(cfg.File))
	f.store.SetClock(f.clock)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ca, err := pki.NewCA(key, time.Now())
	require.NoError(t, err)
	f.authority, err = pki.NewAuthority(ca, key)
	require.NoError(t, err)

	logger := slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reg := registry.New(f.store, f.db.URL, logger)
	f.registry = reg
	require.NoError(t, reg.Start(ctx))

	f.metrics = metrics.New(prometheus.NewRegistry())
	limiter := ratelimit.NewPostgresLimiter(f.db.Pool, store.RateTable)
	limiter.SetClock(f.clock)
	f.server, err = New(Deps{
		Config: cfg, Store: f.store, Registry: reg, Authority: f.authority,
		Limiter: limiter,
		Verifier: fakeVerifier{
			"admin":   {RoleARN: "arn:aws:iam::1:role/forge", Session: "nuno", Scope: config.ScopeAdmin},
			"preview": {RoleARN: "arn:aws:iam::2:role/ci-preview", Session: "gha", Scope: config.ScopePreview},
		},
		Alerts:         &notify.Alerts{Store: f.store, Publisher: f.published, Logger: logger},
		Ops:            f.published,
		Metrics:        f.metrics,
		ProviderClient: f.provider.Client(),
		Logger:         logger,
		Now:            f.clock,
	})
	require.NoError(t, err)

	f.central = httptest.NewUnstartedServer(f.server.Handler())
	f.central.TLS = f.server.TLSConfig()
	f.central.EnableHTTP2 = true
	f.central.StartTLS()
	t.Cleanup(f.central.Close)
	return f
}

func (f *fixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fixture) serveProvider(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.calls = append(f.calls, providerCall{header: r.Header.Clone(), body: body})
	f.mu.Unlock()
	usage := `{"prompt_tokens":1000000,"completion_tokens":1000000,"total_tokens":2000000,"prompt_tokens_details":{"cached_tokens":0}}`
	if body["stream"] == true {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"model\":\"provider/model-v1\",\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", secretResponse)
		fmt.Fprintf(w, "data: {\"model\":\"provider/model-v1\",\"choices\":[],\"usage\":%s}\n\n", usage)
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"model":"provider/model-v1","choices":[{"message":{"content":%q}}],"usage":%s}`, secretResponse, usage)
}

func (f *fixture) lastCall() providerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(f.t, f.calls)
	return f.calls[len(f.calls)-1]
}

// installation registers an installation and returns an mTLS client for it.
func (f *fixture) installation(id, deployment, plan, currency string) *http.Client {
	f.t.Helper()
	ctx := context.Background()
	reg := store.Registration{ID: id, Deployment: deployment, Plan: plan, BillingCurrency: currency}
	if plan == "trial" {
		d := 7
		reg.TrialDays = &d
	}
	_, _, err := f.store.Register(ctx, reg, store.Actor{ARN: "test"})
	require.NoError(f.t, err)
	cert := f.issue(id)
	// Registered behind the operator API's back: apply it now rather than
	// waiting for the NOTIFY round trip.
	require.NoError(f.t, f.registry.Refresh(ctx))
	return f.client(&cert)
}

func (f *fixture) issue(id string) tls.Certificate {
	f.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(f.t, err)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: id}}, key)
	require.NoError(f.t, err)
	var der []byte
	_, err = f.store.RecordCertificate(context.Background(), id, func() (store.IssuedCertificate, error) {
		issued, err := f.authority.IssueInstallation(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}), id)
		if err != nil {
			return store.IssuedCertificate{}, err
		}
		der = issued.DER
		return store.IssuedCertificate{Serial: issued.Serial, Fingerprint: issued.Fingerprint, NotBefore: issued.NotBefore, NotAfter: issued.NotAfter}, nil
	}, store.Actor{ARN: "test"})
	require.NoError(f.t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (f *fixture) client(cert *tls.Certificate) *http.Client {
	roots := x509.NewCertPool()
	roots.AddCert(f.authority.CA)
	cfg := &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: true}, Timeout: 10 * time.Second}
}

func (f *fixture) chat(c *http.Client, stream bool, headers map[string]string) *http.Response {
	f.t.Helper()
	body := fmt.Sprintf(`{"model":"managed-model","stream":%t,"messages":[{"role":"user","content":%q}]}`, stream, secretPrompt)
	req, err := http.NewRequest(http.MethodPost, f.central.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(f.t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (f *fixture) operator(auth, method, path string, body any) (*http.Response, map[string]any) {
	f.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(f.t, err)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, f.central.URL+path, reader)
	require.NoError(f.t, err)
	if auth != "" {
		req.Header.Set(opauth.Header, auth)
	}
	resp, err := f.client(nil).Do(req)
	require.NoError(f.t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"_raw": string(raw)}
	}
	return resp, out
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(raw, &e), string(raw))
	return e.Error.Code
}

func countRecords(t *testing.T, f *fixture, installationID string) int {
	var n int
	require.NoError(t, f.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM central.usage_records WHERE installation_id = $1`, installationID).Scan(&n))
	return n
}

func TestCentral_InferenceIsProxiedAndMetered(t *testing.T) {
	f := newFixture(t, 1000)
	usd := f.installation("usd-co", "hosted", "starter", "USD")

	for _, stream := range []bool{false, true} {
		resp := f.chat(usd, stream, map[string]string{
			"X-Iterabase-Run-Id": "run-1", "X-Iterabase-Workflow-Key": "invoices/intake",
			"X-Iterabase-Channel": "chat", "X-Iterabase-Turn-Id": "has spaces",
		})
		raw, _ := io.ReadAll(resp.Body)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		assert.Contains(t, string(raw), `"model":"managed-model"`, "responses carry the central model name")
		if stream {
			assert.Contains(t, string(raw), "data: [DONE]")
		}

		call := f.lastCall()
		assert.Equal(t, "Bearer "+providerKey, call.header.Get("Authorization"), "central holds the provider key")
		assert.Equal(t, "provider/model-v1", call.body["model"])
		for k := range call.header {
			assert.False(t, strings.HasPrefix(k, "X-Iterabase"), "attribution never reaches the provider: %s", k)
		}
	}

	var credits, channel, workflowKey, turnID, classification string
	require.NoError(t, f.db.Pool.QueryRow(context.Background(), `
		SELECT sum(credits)::text, min(channel), min(workflow_key), min(turn_id), min(classification)
		FROM central.usage_records WHERE installation_id = 'usd-co'`).Scan(&credits, &channel, &workflowKey, &turnID, &classification))
	assert.Equal(t, "400.000000", credits, "2 × (2M tokens at 1.00 USD per million) for a USD installation")
	assert.Equal(t, "chat", channel)
	assert.Equal(t, "invoices/intake", workflowKey)
	assert.Empty(t, turnID, "an invalid attribution value is dropped")
	assert.Equal(t, "chargeable", classification)

	// The installation reads its own figures only.
	f.installation("other-co", "hosted", "growth", "USD")
	resp, err := usd.Get(f.central.URL + "/v1/installation")
	require.NoError(t, err)
	defer resp.Body.Close()
	var read map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&read))
	assert.Equal(t, "usd-co", read["installation_id"])
	assert.EqualValues(t, 2, read["workflow_limit"])
	period := read["period"].(map[string]any)
	assert.Equal(t, "5000", period["allowance_credits"])
	assert.Equal(t, "400.000000", period["used_credits"])
	assert.Equal(t, "month", period["kind"])

	models, err := usd.Get(f.central.URL + "/v1/models")
	require.NoError(t, err)
	defer models.Body.Close()
	raw, _ := io.ReadAll(models.Body)
	assert.Contains(t, string(raw), `"id":"managed-model"`)
}

func TestCentral_AuthenticationAndRevocation(t *testing.T) {
	f := newFixture(t, 1000)
	acme := f.installation("acme", "hosted", "starter", "EUR")
	require.NoError(t, f.store.RecordObservation(context.Background(), time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), billing.MustDec("1.1403")))

	resp := f.chat(f.client(nil), false, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "no client certificate")
	assert.Equal(t, "installation_revoked", errorCode(t, resp))

	resp = f.chat(acme, false, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Revocation through the operator API is refused on the very next request.
	opResp, _ := f.operator("admin", http.MethodPost, "/operator/v1/installations/acme/revoke", map[string]string{"reason": "test"})
	require.Equal(t, http.StatusOK, opResp.StatusCode)
	calls := f.callCount()
	resp = f.chat(acme, false, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "the very next request is refused")
	assert.Equal(t, "installation_revoked", errorCode(t, resp))
	assert.Equal(t, calls, f.callCount(), "a revoked installation never reaches the provider")

	// A certificate from another CA is refused at the TLS layer.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	foreign, _ := pki.NewCA(key, time.Now())
	strangerAuthority, _ := pki.NewAuthority(foreign, key)
	csrKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, csrKey)
	issued, err := strangerAuthority.IssueInstallation(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), "acme")
	require.NoError(t, err)
	stranger := f.client(&tls.Certificate{Certificate: [][]byte{issued.DER}, PrivateKey: csrKey})
	body := `{"model":"managed-model"}`
	_, err = stranger.Post(f.central.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	assert.Error(t, err, "a foreign CA's certificate fails the handshake")
}

func TestCentral_FailsClosed(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	eur := f.installation("eur-co", "hosted", "starter", "EUR")

	t.Run("a missing FX rate refuses before the provider", func(t *testing.T) {
		resp := f.chat(eur, false, nil)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Equal(t, "upstream_unavailable", errorCode(t, resp))
		assert.Zero(t, f.callCount())
		assert.Contains(t, f.logs.String(), "fx_rate_unavailable")
		assert.Contains(t, strings.Join(f.published.all(), "\n"), "FX rate unavailable")
	})

	require.NoError(t, f.store.RecordObservation(ctx, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), billing.MustDec("1.1403")))

	t.Run("the installation limit returns retryable upstream_rate_limited", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			require.Equal(t, http.StatusOK, f.chat(eur, false, nil).StatusCode)
		}
		resp := f.chat(eur, false, nil)
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
		assert.NotEmpty(t, resp.Header.Get("Retry-After"))
		assert.Equal(t, "upstream_rate_limited", errorCode(t, resp))
	})

	t.Run("a usage write failure withholds the response", func(t *testing.T) {
		f.mu.Lock()
		f.now = f.now.Add(time.Minute) // a fresh rate window
		f.mu.Unlock()
		_, err := f.db.Pool.Exec(ctx, `ALTER TABLE central.usage_records RENAME TO usage_records_gone`)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = f.db.Pool.Exec(ctx, `ALTER TABLE central.usage_records_gone RENAME TO usage_records`)
		})

		resp := f.chat(eur, false, nil)
		raw, _ := io.ReadAll(resp.Body)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.NotContains(t, string(raw), secretResponse, "unmetered inference is never served")

		resp = f.chat(eur, true, nil)
		raw, _ = io.ReadAll(resp.Body)
		assert.Contains(t, string(raw), `"code":"upstream_unavailable"`)
		assert.NotContains(t, string(raw), "[DONE]", "the stream does not complete")
	})

	t.Run("a rate-counter failure refuses before the provider", func(t *testing.T) {
		before := f.callCount()
		_, err := f.db.Pool.Exec(ctx, `ALTER TABLE central.rate_counters RENAME TO rate_counters_gone`)
		require.NoError(t, err)
		resp := f.chat(eur, false, nil)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Equal(t, before, f.callCount())
		_, err = f.db.Pool.Exec(ctx, `ALTER TABLE central.rate_counters_gone RENAME TO rate_counters`)
		require.NoError(t, err)
	})
}

func TestCentral_AlertsAndTrial(t *testing.T) {
	f := newFixture(t, 1000)
	trial := f.installation("trial-co", "hosted", "trial", "USD")
	// Allowance 1166.666667; each request is 200 credits: the 5th crosses 80%
	// (1000 ≥ 933.33) and the 6th 100% (1200).
	for i := 0; i < 7; i++ {
		require.Equal(t, http.StatusOK, f.chat(trial, false, nil).StatusCode)
	}
	resp, err := trial.Get(f.central.URL + "/v1/installation")
	require.NoError(t, err)
	defer resp.Body.Close()
	var read struct {
		Plan          string `json:"plan"`
		WorkflowLimit int    `json:"workflow_limit"`
		Period        struct {
			Kind             string `json:"kind"`
			AllowanceCredits string `json:"allowance_credits"`
			OverageCredits   string `json:"overage_credits"`
		} `json:"period"`
		Alerts []struct {
			Threshold int `json:"threshold"`
		} `json:"alerts"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&read))
	assert.Equal(t, "trial", read.Plan)
	assert.Equal(t, 2, read.WorkflowLimit)
	assert.Equal(t, "trial", read.Period.Kind)
	assert.Equal(t, "1166.666667", read.Period.AllowanceCredits)
	assert.Equal(t, "233.333333", read.Period.OverageCredits)
	require.Len(t, read.Alerts, 2)
	assert.Equal(t, []int{80, 100}, []int{read.Alerts[0].Threshold, read.Alerts[1].Threshold})

	subjects := f.published.all()
	assert.Contains(t, subjects, "Iterabase credits: trial-co reached 80%")
	assert.Contains(t, subjects, "Iterabase credits: trial-co reached 100%")
	assert.Len(t, subjects, 2, "each alert is published once")
}

// TestCentral_LocalRejectionsDoNotBurnProviderQuota: an installation above its
// own limit cannot exhaust the shared provider-wide limit (DES-HOR-661-07).
func TestCentral_LocalRejectionsDoNotBurnProviderQuota(t *testing.T) {
	f := newFixtureLimits(t, 3, 1)
	acme := f.installation("acme", "hosted", "starter", "USD")
	beta := f.installation("beta", "hosted", "starter", "USD")
	require.Equal(t, http.StatusOK, f.chat(acme, false, nil).StatusCode)
	for i := 0; i < 2; i++ {
		resp := f.chat(acme, false, nil)
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "acme is over its own limit")
	}
	assert.Equal(t, http.StatusOK, f.chat(beta, false, nil).StatusCode, "beta still gets provider quota")
}

func TestCentral_OperatorAPI(t *testing.T) {
	f := newFixture(t, 1000)

	resp, _ := f.operator("", http.MethodPost, "/operator/v1/installations", map[string]any{})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp, body := f.operator("forbidden", http.MethodGet, "/operator/v1/installations/x", nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "operator_forbidden", body["error"].(map[string]any)["code"])

	reg := map[string]any{"id": "acme", "deployment": "hosted", "plan": "trial", "billing_currency": "EUR", "trial_days": 7}
	resp, body = f.operator("admin", http.MethodPost, "/operator/v1/installations", reg)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	assert.Equal(t, "1166.666667", body["trial_credits"])
	resp, _ = f.operator("admin", http.MethodPost, "/operator/v1/installations", reg)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "identical registration")
	reg["billing_currency"] = "USD"
	resp, body = f.operator("admin", http.MethodPost, "/operator/v1/installations", reg)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "installation_conflict", body["error"].(map[string]any)["code"])

	for name, bad := range map[string]map[string]any{
		"bad id":             {"id": "Acme!", "deployment": "hosted", "plan": "starter", "billing_currency": "EUR"},
		"trial without days": {"id": "x1", "deployment": "hosted", "plan": "trial", "billing_currency": "EUR"},
		"internal with plan": {"id": "x2", "deployment": "internal", "plan": "starter", "billing_currency": "EUR"},
		"unknown field":      {"id": "x3", "deployment": "internal", "plan": "none", "billing_currency": "EUR", "admin": true},
	} {
		resp, body := f.operator("admin", http.MethodPost, "/operator/v1/installations", bad)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, name)
		assert.Equal(t, "invalid_request", body["error"].(map[string]any)["code"], name)
	}

	// The Forge wire shape (forge/internal/central RegisterRequest): the
	// allowance is a decimal string; a JSON number is rejected.
	resp, body = f.operator("admin", http.MethodPost, "/operator/v1/installations",
		map[string]any{"id": "acme-sh", "deployment": "self_hosted_managed", "plan": "none", "billing_currency": "USD", "allowance_credits": "5000"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	resp, _ = f.operator("admin", http.MethodPost, "/operator/v1/installations",
		map[string]any{"id": "acme-sh2", "deployment": "self_hosted_managed", "plan": "none", "billing_currency": "USD", "allowance_credits": 5000})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// Preview operators manage only internal pr-*/staging installations.
	resp, _ = f.operator("preview", http.MethodPost, "/operator/v1/installations",
		map[string]any{"id": "pr-42", "deployment": "internal", "plan": "none", "billing_currency": "EUR"})
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	resp, _ = f.operator("preview", http.MethodPost, "/operator/v1/installations",
		map[string]any{"id": "pr-43", "deployment": "hosted", "plan": "starter", "billing_currency": "EUR"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp, _ = f.operator("preview", http.MethodPost, "/operator/v1/installations/acme/revoke", map[string]string{"reason": "x"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp, _ = f.operator("preview", http.MethodGet, "/operator/v1/exports/2026-10", nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// Certificates.
	resp, body = f.operator("admin", http.MethodPost, "/operator/v1/installations/acme/certificates", map[string]string{"csr_pem": "garbage"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "invalid_csr", body["error"].(map[string]any)["code"])
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))
	resp, body = f.operator("preview", http.MethodPost, "/operator/v1/installations/pr-42/certificates", map[string]string{"csr_pem": csrPEM})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	assert.Contains(t, body["certificate_pem"], "BEGIN CERTIFICATE")
	assert.Equal(t, string(f.authority.CAPEM()), body["ca_pem"])
	resp, _ = f.operator("admin", http.MethodPost, "/operator/v1/installations/nobody/certificates", map[string]string{"csr_pem": csrPEM})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// Entitlement changes.
	resp, body = f.operator("admin", http.MethodPost, "/operator/v1/installations/acme/trial-extension", map[string]int{"days": 3})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, "1666.666667", body["trial_credits"])
	resp, body = f.operator("admin", http.MethodPost, "/operator/v1/installations/acme/plan", map[string]string{"plan": "growth"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, "growth", body["plan"])
	assert.EqualValues(t, 6, body["workflow_limit"])
	resp, _ = f.operator("admin", http.MethodPost, "/operator/v1/installations/acme/plan", map[string]string{"plan": "trial"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "a plan change is to a paid tier")

	resp, body = f.operator("admin", http.MethodGet, "/operator/v1/installations/acme", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "20000", body["period"].(map[string]any)["allowance_credits"])

	var actions []string
	rows, err := f.db.Pool.Query(context.Background(), `SELECT action FROM central.audit_events WHERE installation_id = 'acme' ORDER BY id`)
	require.NoError(t, err)
	actions, err = pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"register", "extend_trial", "set_plan"}, actions)
}

func TestCentral_ExportAndAdjustment(t *testing.T) {
	f := newFixture(t, 1000)
	a := f.installation("usd-a", "hosted", "starter", "USD")
	b := f.installation("usd-b", "self_hosted_managed", "none", "USD")
	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusOK, f.chat(a, false, nil).StatusCode)
	}
	require.Equal(t, http.StatusOK, f.chat(b, false, nil).StatusCode)

	resp, body := f.operator("admin", http.MethodGet, "/operator/v1/exports/2026-10", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	rows := body["rows"].([]any)
	require.Len(t, rows, 2)
	byID := map[string]map[string]any{}
	for _, r := range rows {
		row := r.(map[string]any)
		byID[row["installation_id"].(string)] = row
	}
	assert.Equal(t, "600.000000", byID["usd-a"]["chargeable_credits"])
	assert.Equal(t, "200.000000", byID["usd-b"]["chargeable_credits"])
	assert.Equal(t, "200.000000", byID["usd-b"]["overage_credits"], "usage-only self-hosted managed inference is all billable")

	var recordID string
	require.NoError(t, f.db.Pool.QueryRow(context.Background(), `SELECT id::text FROM central.usage_records WHERE installation_id = 'usd-a' LIMIT 1`).Scan(&recordID))
	resp, body = f.operator("admin", http.MethodPost, "/operator/v1/usage/"+recordID+"/adjustment",
		map[string]string{"classification": "company_funded", "reason": "provider incident 7"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	req, _ := http.NewRequest(http.MethodGet, f.central.URL+"/operator/v1/exports/2026-10?format=csv", nil)
	req.Header.Set(opauth.Header, "admin")
	csvResp, err := f.client(nil).Do(req)
	require.NoError(t, err)
	defer csvResp.Body.Close()
	raw, _ := io.ReadAll(csvResp.Body)
	assert.Equal(t, "text/csv", csvResp.Header.Get("Content-Type"))
	assert.Contains(t, string(raw), "installation_id,deployment,plan")
	assert.Contains(t, string(raw), "usd-a,hosted,starter,USD,month,2026-10-01T00:00:00Z,2026-11-01T00:00:00Z,5000.000000,400.000000,200.000000,0.000000,3,0")
}

func TestCentral_HealthAndReadiness(t *testing.T) {
	f := newFixture(t, 1000)
	health := httptest.NewServer(f.server.HealthHandler())
	t.Cleanup(health.Close)
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(health.URL + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, path)
	}
}

// TestCentral_NoPayloadLeaks sends marked content through every path and
// inspects logs, metrics and every database row (REQ-036, HOR-661 acceptance).
func TestCentral_NoPayloadLeaks(t *testing.T) {
	f := newFixture(t, 1000)
	c := f.installation("leak-co", "hosted", "starter", "USD")
	for _, stream := range []bool{false, true} {
		resp := f.chat(c, stream, nil)
		raw, _ := io.ReadAll(resp.Body)
		require.Contains(t, string(raw), secretResponse, "the client gets its content")
	}

	scrape := httptest.NewRecorder()
	promhttp.HandlerFor(f.metrics.Registry, promhttp.HandlerOpts{}).ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	var dump strings.Builder
	rows, err := f.db.Pool.Query(context.Background(), `SELECT table_name FROM information_schema.tables WHERE table_schema = 'central'`)
	require.NoError(t, err)
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	for _, table := range tables {
		rows, err := f.db.Pool.Query(context.Background(), fmt.Sprintf(`SELECT row_to_json(t)::text FROM central.%s t`, pgx.Identifier{table}.Sanitize()))
		require.NoError(t, err)
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
		dump.WriteString(strings.Join(lines, "\n"))
	}
	require.Greater(t, countRecords(t, f, "leak-co"), 1)

	for name, text := range map[string]string{"logs": f.logs.String(), "metrics": scrape.Body.String(), "database": dump.String()} {
		for _, secret := range []string{secretPrompt, secretResponse, providerKey} {
			assert.NotContains(t, text, secret, "%s must not contain %s", name, secret)
		}
	}
}

func (f *fixture) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}
