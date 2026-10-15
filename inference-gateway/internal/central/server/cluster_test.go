package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/billing"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/config"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/notify"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/pki"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/registry"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/store"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/centrallink"
	gatewayconfig "github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/proxy"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/testpg"
)

// The cluster harness runs the HOR-661 fault-injection acceptance in-process
// and hermetically (DES-HOR-661-09): two central replicas share one central
// Postgres behind a TCP load balancer, the database itself sits behind a TCP
// proxy that can cut every connection, and two installations call central
// through the installation gateway's real proxy handler and mTLS link. A
// replica "kill" closes its listener and every live connection at once and
// stops its background loops, which is what an ECS task's SIGKILL looks like
// from the network: in-flight requests break and new connections are refused.

const clusterConfig = `public_host: localhost
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
  provider: {rpm: 0, tpm: 0}
  installation: {rpm: 0, tpm: 0}
`

// Per request the fake provider reports 1,000 prompt tokens (200 cached) and
// 500 completion tokens: (800 x 1.00 + 200 x 0.10 + 500 x 1.00) / 1e6 =
// USD 0.00132. A USD installation is charged 0.132 credits; a EUR one, at the
// locked 1.10 USD per EUR, EUR 0.0012 = 0.12 credits.
const (
	usdCreditsPerRequest = "0.132"
	eurCreditsPerRequest = "0.12"
	clusterUSDPerEUR     = "1.10"
	holdMarker           = "HOLD-UNTIL-RELEASED"
)

// imageMarker is an image part's payload; it must reach the provider
// unchanged and never be stored or logged.
var imageMarker = "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("SECRET-IMAGE-5d2b"))

// tcpProxy is a minimal layer-4 proxy. As the load balancer it spreads new
// connections round-robin over the targets that accept a connection (an NLB
// whose health check has removed dead targets); as the database proxy it can
// cut every connection and refuse new ones (an outage).
type tcpProxy struct {
	ln      net.Listener
	mu      sync.Mutex
	targets []string
	next    int
	cut     bool
	conns   map[net.Conn]struct{}
	wg      sync.WaitGroup
}

func newTCPProxy(t *testing.T, targets ...string) *tcpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &tcpProxy{ln: ln, targets: targets, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.closeAll()
		p.wg.Wait()
	})
	return p
}

func (p *tcpProxy) addr() string { return p.ln.Addr().String() }

func (p *tcpProxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(c)
		}()
	}
}

func (p *tcpProxy) dial() net.Conn {
	p.mu.Lock()
	if p.cut {
		p.mu.Unlock()
		return nil
	}
	targets := append([]string(nil), p.targets...)
	start := p.next
	p.next++
	p.mu.Unlock()
	for i := range targets {
		up, err := net.DialTimeout("tcp", targets[(start+i)%len(targets)], time.Second)
		if err == nil {
			return up
		}
	}
	return nil
}

func (p *tcpProxy) handle(down net.Conn) {
	up := p.dial()
	if up == nil {
		_ = down.Close()
		return
	}
	if !p.track(down, up) {
		_ = down.Close()
		_ = up.Close()
		return
	}
	defer p.untrack(down, up)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, down); done <- struct{}{} }()
	go func() { _, _ = io.Copy(down, up); done <- struct{}{} }()
	<-done
	_ = down.Close()
	_ = up.Close()
	<-done
}

func (p *tcpProxy) track(cs ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cut {
		return false
	}
	for _, c := range cs {
		p.conns[c] = struct{}{}
	}
	return true
}

func (p *tcpProxy) untrack(cs ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range cs {
		delete(p.conns, c)
	}
}

func (p *tcpProxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
}

// outage cuts every connection and refuses new ones until restore.
func (p *tcpProxy) outage() {
	p.mu.Lock()
	p.cut = true
	p.mu.Unlock()
	p.closeAll()
}

func (p *tcpProxy) restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = false
}

// replica is one central process.
type replica struct {
	name    string
	addr    string
	logs    *syncBuffer
	metrics *metrics.Metrics
	server  *Server

	cancel context.CancelFunc
	ln     net.Listener
	http   *http.Server
	pool   *pgxpool.Pool
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	killed atomic.Bool
}

// kill drops the replica abruptly: no graceful shutdown, every live
// connection is closed mid-request.
func (r *replica) kill() {
	if r.killed.Swap(true) {
		return
	}
	_ = r.ln.Close()
	r.mu.Lock()
	for c := range r.conns {
		_ = c.Close()
	}
	r.mu.Unlock()
	r.cancel()
	// Close the pool without waiting: handlers still holding connections
	// finish on their own, as they would racing a dying process.
	go r.pool.Close()
}

func (r *replica) url() string { return "https://localhost:" + portOf(r.addr) }

func portOf(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}

type cluster struct {
	t         *testing.T
	db        *testpg.DB
	dbProxy   *tcpProxy
	lb        *tcpProxy
	cfg       *config.Config
	authority *pki.Authority
	verifier  fakeVerifier
	published *recordingPublisher
	provider  *httptest.Server
	now       time.Time

	mu       sync.Mutex
	replicas []*replica
	calls    int
	images   []string
	held     chan struct{}
	release  chan struct{}
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	c := &cluster{
		t: t, published: &recordingPublisher{},
		now: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC),
		verifier: fakeVerifier{
			"admin": {RoleARN: "arn:aws:iam::1:role/forge", Session: "nuno", Scope: config.ScopeAdmin},
		},
		held: make(chan struct{}, 1), release: make(chan struct{}),
	}
	c.provider = httptest.NewTLSServer(http.HandlerFunc(c.serveProvider))
	t.Cleanup(c.provider.Close)

	var err error
	c.cfg, err = config.Parse([]byte(fmt.Sprintf(clusterConfig, c.provider.URL)))
	require.NoError(t, err)
	c.cfg.ProviderKey = providerKey

	c.db = testpg.Start(t)
	ctx := context.Background()
	require.NoError(t, store.Migrate(ctx, c.db.Pool))
	direct := store.New(c.db.Pool, billing.NewPolicy(c.cfg.File))
	rate, _ := new(big.Rat).SetString(clusterUSDPerEUR)
	require.NoError(t, direct.RecordObservation(ctx, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), rate))

	u, err := url.Parse(c.db.URL)
	require.NoError(t, err)
	c.dbProxy = newTCPProxy(t, u.Host)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ca, err := pki.NewCA(key, time.Now())
	require.NoError(t, err)
	c.authority, err = pki.NewAuthority(ca, key)
	require.NoError(t, err)

	c.lb = newTCPProxy(t)
	for _, name := range []string{"a", "b"} {
		c.start(name, "127.0.0.1:0")
	}
	return c
}

func (c *cluster) clock() time.Time { return c.now }

// proxiedDSN is the central database reached through the outage proxy.
func (c *cluster) proxiedDSN() string {
	u, _ := url.Parse(c.db.URL)
	u.Host = c.dbProxy.addr()
	return u.String()
}

// start runs a replica on addr (":0" for a fresh port, or a dead replica's
// address to restart it) and puts it behind the load balancer.
func (c *cluster) start(name, addr string) *replica {
	t := c.t
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &replica{name: name, logs: &syncBuffer{}, cancel: cancel, conns: map[net.Conn]struct{}{}}
	logger := slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	pool, err := pgxpool.New(ctx, c.proxiedDSN())
	require.NoError(t, err)
	r.pool = pool
	st := store.New(pool, billing.NewPolicy(c.cfg.File))
	st.SetClock(c.clock)
	reg := registry.New(st, c.proxiedDSN(), logger)
	require.NoError(t, reg.Start(ctx))
	limiter := ratelimit.NewPostgresLimiter(pool, store.RateTable)
	limiter.SetClock(c.clock)
	r.metrics = metrics.New(prometheus.NewRegistry())
	r.server, err = New(Deps{
		Config: c.cfg, Store: st, Registry: reg, Authority: c.authority, Limiter: limiter, Verifier: c.verifier,
		Alerts: &notify.Alerts{Store: st, Publisher: c.published, Logger: logger}, Ops: c.published,
		Metrics: r.metrics, ProviderClient: c.provider.Client(), Logger: logger, Now: c.clock,
	})
	require.NoError(t, err)

	var ln net.Listener
	require.Eventually(t, func() bool {
		ln, err = net.Listen("tcp", addr)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "listen %s", addr)
	r.ln, r.addr = ln, ln.Addr().String()
	r.http = &http.Server{
		Handler: r.server.Handler(), TLSConfig: r.server.TLSConfig(), ReadHeaderTimeout: 5 * time.Second,
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
		ConnState: func(conn net.Conn, state http.ConnState) {
			r.mu.Lock()
			defer r.mu.Unlock()
			switch state {
			case http.StateNew:
				r.conns[conn] = struct{}{}
			case http.StateClosed, http.StateHijacked:
				delete(r.conns, conn)
			}
		},
	}
	go func() { _ = r.http.ServeTLS(ln, "", "") }()
	t.Cleanup(r.kill)

	c.mu.Lock()
	c.replicas = append(c.replicas, r)
	c.mu.Unlock()
	c.lb.mu.Lock()
	c.lb.targets = dedupe(append(c.lb.targets, r.addr))
	c.lb.mu.Unlock()
	return r
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (c *cluster) replica(name string) *replica {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.replicas) - 1; i >= 0; i-- {
		if c.replicas[i].name == name {
			return c.replicas[i]
		}
	}
	c.t.Fatalf("no replica %s", name)
	return nil
}

// serveProvider is the deterministic fake OpenAI provider. A request whose
// prompt carries holdMarker blocks until the test releases it.
func (c *cluster) serveProvider(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(raw, &body)
	c.mu.Lock()
	c.calls++
	if strings.Contains(string(raw), "image_url") {
		c.images = append(c.images, string(raw))
	}
	c.mu.Unlock()
	if strings.Contains(string(raw), holdMarker) {
		c.held <- struct{}{}
		<-c.release
	}
	time.Sleep(5 * time.Millisecond) // keep requests in flight long enough to be interrupted
	usage := `{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500,"prompt_tokens_details":{"cached_tokens":200}}`
	if body.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"model\":\"provider/model-v1\",\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", secretResponse)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		fmt.Fprintf(w, "data: {\"model\":\"provider/model-v1\",\"choices\":[],\"usage\":%s}\n\n", usage)
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"model":"provider/model-v1","choices":[{"message":{"content":%q}}],"usage":%s}`, secretResponse, usage)
}

func (c *cluster) providerCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// operator calls the operator API on one replica directly.
func (c *cluster) operator(r *replica, method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(c.t, err)
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, r.url()+path, reader)
	require.NoError(c.t, err)
	req.Header.Set("X-Iterabase-Operator-Auth", "admin")
	resp, err := c.client(nil).Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	out["_raw"] = string(raw)
	return resp.StatusCode, out
}

func (c *cluster) client(cert *tls.Certificate) *http.Client {
	roots := x509.NewCertPool()
	roots.AddCert(c.authority.CA)
	cfg := &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: true}, Timeout: 10 * time.Second}
}

// installation is one customer installation: its certificate files (what
// the chart mounts) and gateway handlers that reach central through the load
// balancer, each with its own connection.
type installation struct {
	id       string
	cert     tls.Certificate
	files    gatewayconfig.CentralConfig
	handlers []http.Handler
}

// register registers the installation and issues its certificate through
// the operator API, the way Forge does.
func (c *cluster) register(id, currency string, links int) *installation {
	t := c.t
	t.Helper()
	a := c.replica("a")
	status, out := c.operator(a, http.MethodPost, "/operator/v1/installations",
		map[string]any{"id": id, "deployment": "hosted", "plan": "starter", "billing_currency": currency})
	require.Equal(t, http.StatusCreated, status, out["_raw"])

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: id}}, key)
	require.NoError(t, err)
	status, out = c.operator(a, http.MethodPost, "/operator/v1/installations/"+id+"/certificates",
		map[string]any{"csr_pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))})
	require.Equal(t, http.StatusCreated, status, out["_raw"])
	certPEM, _ := out["certificate_pem"].(string)
	block, _ := pem.Decode([]byte(certPEM))
	require.NotNil(t, block, out["_raw"])

	dir := t.TempDir()
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	caPEM, _ := out["ca_pem"].(string)
	files := map[string][]byte{
		"tls.crt": []byte(certPEM),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		"ca.crt":  []byte(caPEM),
	}
	for name, data := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	inst := &installation{
		id:   id,
		cert: tls.Certificate{Certificate: [][]byte{block.Bytes}, PrivateKey: key},
		files: gatewayconfig.CentralConfig{
			URL:            "https://localhost:" + portOf(c.lb.addr()),
			ClientCertFile: filepath.Join(dir, "tls.crt"), ClientKeyFile: filepath.Join(dir, "tls.key"),
			CAFile: filepath.Join(dir, "ca.crt"),
		},
	}
	for range links {
		link, err := centrallink.New(inst.files)
		require.NoError(t, err)
		h := proxy.NewHandler(catalog{
			"assistant": {ModelID: "assistant", BackendRef: "central", BackendKind: "managed", BackendModelID: "managed-model", Available: true},
		}, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithCentral(link)
		inst.handlers = append(inst.handlers, middleware.RequestID(http.HandlerFunc(h.ChatCompletions)))
	}
	return inst
}

// result is what the installation's caller (the runtime) saw.
type result struct {
	requestID  string
	status     int
	code       string
	retryAfter string
	body       string
}

// ok reports a delivered completion: a 200 with content and, when streaming,
// a clean [DONE] rather than a terminal error event.
func (r result) ok() bool {
	return r.status == http.StatusOK && strings.Contains(r.body, secretResponse) && !strings.Contains(r.body, `"error"`)
}

// retryable reports the classified retryable failure the runtime pauses on
// (DES-HOR-597-02, HOR-649): 503 upstream_unavailable with Retry-After, or a
// stream that ends with the upstream_unavailable event.
func (r result) retryable() bool {
	if r.status == http.StatusServiceUnavailable && r.code == "upstream_unavailable" && r.retryAfter != "" {
		return true
	}
	return r.status == http.StatusOK && strings.Contains(r.body, `"code":"upstream_unavailable"`) && !strings.Contains(r.body, "[DONE]")
}

var requestSeq atomic.Int64

// call sends one turn's model call through the installation gateway.
func (inst *installation) call(link int, stream, image bool, prompt string) result {
	id := fmt.Sprintf("req-%s-%d", inst.id, requestSeq.Add(1))
	content := fmt.Sprintf("%q", secretPrompt+" "+prompt)
	if image {
		content = fmt.Sprintf(`[{"type":"text","text":%q},{"type":"image_url","image_url":{"url":%q}}]`, secretPrompt, imageMarker)
	}
	body := fmt.Sprintf(`{"model":"assistant","stream":%t,"messages":[{"role":"user","content":%s}]}`, stream, content)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Request-ID", id)
	ctx := middleware.WithIdentityID(req.Context(), "identity-"+inst.id)
	ctx = middleware.WithWorkloadContext(ctx, middleware.WorkloadContext{
		RunID: "run-" + inst.id, TurnID: id, AssignedModel: "assistant", EffectiveIdentity: "identity-" + inst.id,
		WorkflowKey: "invoices/intake", Channel: "api",
	})
	rec := httptest.NewRecorder()
	inst.handlers[link%len(inst.handlers)].ServeHTTP(rec, req.WithContext(ctx))
	res := result{requestID: id, status: rec.Code, retryAfter: rec.Header().Get("Retry-After"), body: rec.Body.String()}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &e) == nil {
		res.code = e.Error.Code
	}
	return res
}

// installationRead is central's GET /v1/installation for this certificate.
func (c *cluster) installationRead(inst *installation, via string) map[string]any {
	c.t.Helper()
	resp, err := c.client(&inst.cert).Get(via + "/v1/installation")
	require.NoError(c.t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	require.Equal(c.t, http.StatusOK, resp.StatusCode, string(raw))
	out := map[string]any{}
	require.NoError(c.t, json.Unmarshal(raw, &out))
	return out
}

// directStatus is the status of one inference call straight to a replica.
func (c *cluster) directStatus(inst *installation, r *replica) (int, string) {
	body := fmt.Sprintf(`{"model":"managed-model","messages":[{"role":"user","content":%q}]}`, secretPrompt)
	resp, err := c.client(&inst.cert).Post(r.url()+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	return resp.StatusCode, e.Error.Code
}

// sumCredits is the sum of an installation's usage-record credits.
func (c *cluster) sumCredits(id string) *big.Rat {
	var s string
	require.NoError(c.t, c.db.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(sum(credits), 0)::text FROM central.usage_records WHERE installation_id = $1`, id).Scan(&s))
	r, ok := new(big.Rat).SetString(s)
	require.True(c.t, ok, s)
	return r
}

func (c *cluster) records(id string) map[string]int {
	rows, err := c.db.Pool.Query(context.Background(),
		`SELECT request_id, count(*) FROM central.usage_records WHERE installation_id = $1 GROUP BY request_id`, id)
	require.NoError(c.t, err)
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var rid string
		var n int
		require.NoError(c.t, rows.Scan(&rid, &n))
		out[rid] = n
	}
	require.NoError(c.t, rows.Err())
	return out
}

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return r
}

func mul(a *big.Rat, n int) *big.Rat { return new(big.Rat).Mul(a, big.NewRat(int64(n), 1)) }

// TestCentralCluster_FaultTolerance is the hermetic HOR-661 acceptance run
// (DES-HOR-661-09). The scenarios share one cluster and run in order.
//
//nolint:gocyclo,maintidx // One ordered acceptance narrative.
func TestCentralCluster_FaultTolerance(t *testing.T) {
	c := newCluster(t)
	usdCo := c.register("usd-co", "USD", 4)
	eurCo := c.register("eur-co", "EUR", 4)
	installations := []*installation{usdCo, eurCo}
	perRequest := map[string]*big.Rat{"usd-co": rat(usdCreditsPerRequest), "eur-co": rat(eurCreditsPerRequest)}

	// Every completion the runtime received is recorded exactly once.
	delivered := map[string][]string{}
	var deliveredMu sync.Mutex
	deliver := func(inst *installation, res result) {
		deliveredMu.Lock()
		defer deliveredMu.Unlock()
		delivered[inst.id] = append(delivered[inst.id], res.requestID)
	}

	t.Run("two replicas meter two installations separately", func(t *testing.T) {
		const perInstallation = 8
		for _, inst := range installations {
			for i := range perInstallation {
				res := inst.call(i, i%2 == 1, i%4 == 0, "")
				require.True(t, res.ok(), "%s: %d %s", inst.id, res.status, res.body)
				deliver(inst, res)
			}
		}
		for _, r := range []string{"a", "b"} {
			scrape := httptest.NewRecorder()
			promhttp.HandlerFor(c.replica(r).metrics.Registry, promhttp.HandlerOpts{}).
				ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			require.Contains(t, scrape.Body.String(), `route="/v1/chat/completions"`, "replica %s served traffic", r)
		}
		for _, inst := range installations {
			want := mul(perRequest[inst.id], perInstallation)
			require.Zero(t, want.Cmp(c.sumCredits(inst.id)), "%s credits %s, want %s", inst.id,
				c.sumCredits(inst.id).FloatString(6), want.FloatString(6))
			for _, via := range []string{c.replica("a").url(), c.replica("b").url()} {
				read := c.installationRead(inst, via)
				require.Equal(t, inst.id, read["installation_id"])
				period := read["period"].(map[string]any)
				require.Zero(t, want.Cmp(rat(period["used_credits"].(string))), "%s via %s: %v", inst.id, via, period)
			}
		}
		c.mu.Lock()
		require.NotEmpty(t, c.images)
		for _, raw := range c.images {
			require.Contains(t, raw, imageMarker, "image parts reach the provider unchanged")
		}
		c.mu.Unlock()
	})

	t.Run("killing one replica fails no turn beyond a retryable error", func(t *testing.T) {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		var results []result
		var succeededBeforeKill atomic.Int64
		for w := range 8 {
			inst := installations[w%2]
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					res := inst.call(w/2, i%2 == 0, false, "")
					if res.ok() {
						deliver(inst, res)
						succeededBeforeKill.Add(1)
					}
					mu.Lock()
					results = append(results, res)
					mu.Unlock()
				}
			}()
		}
		require.Eventually(t, func() bool { return succeededBeforeKill.Load() >= 40 }, 20*time.Second, 10*time.Millisecond)
		c.replica("a").kill()
		after := succeededBeforeKill.Load()
		require.Eventually(t, func() bool { return succeededBeforeKill.Load() >= after+40 }, 20*time.Second, 10*time.Millisecond,
			"traffic continues through the surviving replica")
		close(stop)
		wg.Wait()

		failures := 0
		for _, res := range results {
			if res.ok() {
				continue
			}
			failures++
			require.True(t, res.retryable(), "a failed turn must be retryable: %d %q %s", res.status, res.code, res.body)
		}
		t.Logf("kill: %d calls, %d retryable failures", len(results), failures)

		// After the kill every call succeeds through replica b.
		for i := range 20 {
			inst := installations[i%2]
			res := inst.call(i, i%2 == 0, false, "")
			require.True(t, res.ok(), "after kill: %d %s", res.status, res.body)
			deliver(inst, res)
		}
	})

	t.Run("both replicas down pauses and work resumes after restart", func(t *testing.T) {
		c.replica("b").kill()
		for _, inst := range installations {
			for i := range 4 {
				res := inst.call(i, i%2 == 0, false, "")
				require.True(t, res.retryable(), "both down: %d %q %s", res.status, res.code, res.body)
			}
		}
		c.start("a", c.replica("a").addr)
		c.start("b", c.replica("b").addr)
		require.Eventually(t, func() bool {
			res := usdCo.call(0, false, false, "")
			if res.ok() {
				deliver(usdCo, res)
			}
			return res.ok()
		}, 15*time.Second, 50*time.Millisecond)
		for i := range 8 {
			inst := installations[i%2]
			res := inst.call(i, i%2 == 0, false, "")
			require.True(t, res.ok(), "resumed: %d %s", res.status, res.body)
			deliver(inst, res)
		}
	})

	t.Run("central database outage withholds every response and recovers", func(t *testing.T) {
		before := c.providerCalls()
		c.dbProxy.outage()
		for _, inst := range installations {
			for i := range 4 {
				res := inst.call(i, i%2 == 0, false, "")
				require.True(t, res.retryable(), "db down: %d %q %s", res.status, res.code, res.body)
				require.NotContains(t, res.body, secretResponse, "no unmetered response")
			}
		}
		require.Equal(t, before, c.providerCalls(), "nothing is dispatched without the pre-dispatch limit check")
		c.dbProxy.restore()
		require.Eventually(t, func() bool {
			res := eurCo.call(0, false, false, "")
			if res.ok() {
				deliver(eurCo, res)
			}
			return res.ok()
		}, 20*time.Second, 100*time.Millisecond)

		// The database fails while the provider is answering: the usage write
		// fails, so the completion is withheld for both response modes.
		for _, stream := range []bool{false, true} {
			done := make(chan result, 1)
			go func() { done <- usdCo.call(1, stream, false, holdMarker) }()
			select {
			case <-c.held:
			case <-time.After(10 * time.Second):
				t.Fatal("provider never received the held request")
			}
			c.dbProxy.outage()
			c.release <- struct{}{}
			res := <-done
			require.True(t, res.retryable(), "write failed (stream=%t): %d %q %s", stream, res.status, res.code, res.body)
			if !stream {
				require.NotContains(t, res.body, secretResponse, "the buffered completion is withheld")
			}
			// A stream has already relayed its chunks; it ends with the
			// terminal upstream_unavailable event instead of [DONE], so the
			// runtime treats the turn as failed and retries it.
			c.dbProxy.restore()
			require.Eventually(t, func() bool {
				res := usdCo.call(2, false, false, "")
				if res.ok() {
					deliver(usdCo, res)
				}
				return res.ok()
			}, 20*time.Second, 100*time.Millisecond)
		}
	})

	t.Run("revocation propagates to every replica", func(t *testing.T) {
		revoked := c.register("revoked-co", "USD", 1)
		a, b := c.replica("a"), c.replica("b")
		for _, r := range []*replica{a, b} {
			require.Eventually(t, func() bool { s, _ := c.directStatus(revoked, r); return s == http.StatusOK },
				5*time.Second, 20*time.Millisecond, "registered installation works on replica %s", r.name)
		}
		status, out := c.operator(a, http.MethodPost, "/operator/v1/installations/revoked-co/revoke", map[string]any{"reason": "test"})
		require.Equal(t, http.StatusOK, status, out["_raw"])
		start := time.Now()
		s, code := c.directStatus(revoked, a)
		require.Equal(t, http.StatusForbidden, s, "the replica that revoked refuses the very next request")
		require.Equal(t, "installation_revoked", code)
		require.Eventually(t, func() bool {
			s, code := c.directStatus(revoked, b)
			return s == http.StatusForbidden && code == "installation_revoked"
		}, 2*time.Second, 5*time.Millisecond, "NOTIFY reaches the other replica within 2s")
		t.Logf("revocation reached replica b after %s", time.Since(start))
		res := revoked.call(0, false, false, "")
		require.Equal(t, http.StatusBadGateway, res.status, "through the installation gateway it is non-retryable")
		require.Equal(t, "upstream_auth_failed", res.code)
	})

	t.Run("every delivered completion is recorded exactly once", func(t *testing.T) {
		for _, inst := range installations {
			recs := c.records(inst.id)
			for rid, n := range recs {
				require.Equal(t, 1, n, "request %s recorded %d times", rid, n)
			}
			deliveredMu.Lock()
			for _, rid := range delivered[inst.id] {
				require.Equal(t, 1, recs[rid], "delivered request %s must have exactly one usage record", rid)
			}
			deliveredMu.Unlock()
			// Records for requests the runtime did not receive are usage the
			// provider charged for (authorised consumption), still metered.
			var withUsage int
			require.NoError(t, c.db.Pool.QueryRow(context.Background(),
				`SELECT count(*) FROM central.usage_records WHERE installation_id = $1 AND classification = 'chargeable'`,
				inst.id).Scan(&withUsage))
			want := mul(perRequest[inst.id], withUsage)
			require.Zero(t, want.Cmp(c.sumCredits(inst.id)), "%s: %s chargeable records", inst.id, want.FloatString(6))
		}
	})

	t.Run("monthly export reconciles with per-installation totals", func(t *testing.T) {
		status, out := c.operator(c.replica("b"), http.MethodGet, "/operator/v1/exports/2026-10?format=json", nil)
		require.Equal(t, http.StatusOK, status, out["_raw"])
		rows, _ := out["rows"].([]any)
		seen := map[string]bool{}
		for _, raw := range rows {
			row := raw.(map[string]any)
			id := row["installation_id"].(string)
			seen[id] = true
			var requests int64
			require.NoError(t, c.db.Pool.QueryRow(context.Background(),
				`SELECT count(*) FROM central.usage_records WHERE installation_id = $1`, id).Scan(&requests))
			require.Zero(t, c.sumCredits(id).Cmp(rat(row["chargeable_credits"].(string))), "%s: %v", id, row)
			require.EqualValues(t, requests, row["requests"], id)
		}
		for _, inst := range installations {
			require.True(t, seen[inst.id], "export covers %s", inst.id)
		}
	})

	t.Run("no payload in any replica's logs or metrics or the database", func(t *testing.T) {
		var dump strings.Builder
		rows, err := c.db.Pool.Query(context.Background(),
			`SELECT table_name FROM information_schema.tables WHERE table_schema = 'central'`)
		require.NoError(t, err)
		var tables []string
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			tables = append(tables, name)
		}
		rows.Close()
		require.NotEmpty(t, tables)
		for _, table := range tables {
			rows, err := c.db.Pool.Query(context.Background(), fmt.Sprintf(`SELECT row_to_json(t)::text FROM central.%q t`, table))
			require.NoError(t, err)
			for rows.Next() {
				var line string
				require.NoError(t, rows.Scan(&line))
				dump.WriteString(line + "\n")
			}
			rows.Close()
		}
		sources := map[string]string{"database": dump.String()}
		c.mu.Lock()
		replicas := append([]*replica(nil), c.replicas...)
		c.mu.Unlock()
		for i, r := range replicas {
			scrape := httptest.NewRecorder()
			promhttp.HandlerFor(r.metrics.Registry, promhttp.HandlerOpts{}).
				ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			sources[fmt.Sprintf("replica %s#%d logs", r.name, i)] = r.logs.String()
			sources[fmt.Sprintf("replica %s#%d metrics", r.name, i)] = scrape.Body.String()
		}
		secrets := []string{secretPrompt, secretResponse, providerKey, imageMarker, "SECRET-IMAGE-5d2b"}
		for name, text := range sources {
			require.NotEmpty(t, text, name)
			for _, secret := range secrets {
				require.NotContains(t, text, secret, "%s must not contain %s", name, secret)
			}
		}
	})
}
