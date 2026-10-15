package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/middleware"
)

// settleTimeout bounds the Settle hook, which runs after the client may have
// gone away and so cannot use the request's own context.
const settleTimeout = 10 * time.Second

// Target is one resolved upstream for a request. Both binaries build one: the
// installation gateway from its catalog entry, the central gateway from its
// price book. Forward never forwards the client's own headers.
type Target struct {
	URL    string
	Client *http.Client
	// Header is added to the upstream request (provider credential or
	// installation attribution).
	Header http.Header
	// ResponseModel replaces the model field of every response.
	ResponseModel string
	// MetricsModel and BackendRef label metrics and error logs. They must be
	// bounded values (catalog alias, price-book name), never client input.
	MetricsModel string
	BackendRef   string
}

// Usage is the provider's token accounting for one request.
type Usage struct {
	PromptTokens       int
	CachedPromptTokens int
	CompletionTokens   int
	TotalTokens        int
}

// Disposition is how a forwarded request ended.
type Disposition string

const (
	DispositionCompleted Disposition = "completed"
	DispositionFailed    Disposition = "failed"
	DispositionCanceled  Disposition = "canceled"
)

// Outcome is what Forward learned about a request. Usage is nil when the
// provider reported none.
type Outcome struct {
	Disposition    Disposition
	ErrorCode      string
	UpstreamStatus int
	Usage          *Usage
}

// Hooks let a caller account for a request without seeing its content.
type Hooks struct {
	// OnStreamTokens receives the growth of a stream's total token count as
	// chunks arrive (live TPM accounting).
	OnStreamTokens func(delta int)
	// Settle is called exactly once per forwarded request with its outcome.
	// For a completed request, an error replaces the response: a buffered
	// response becomes upstream_unavailable, and a stream ends with the
	// terminal upstream_unavailable event instead of [DONE]. The central
	// gateway uses this to never serve unmetered inference (DES-HOR-661-08).
	Settle func(ctx context.Context, o Outcome) error
}

// Forwarder is the provider round trip shared by the installation and central
// gateways: request construction, error classification (DES-HOR-597-02),
// buffered and SSE response handling, model-name rewrite, usage extraction and
// metrics. It never logs or records request or response content.
type Forwarder struct {
	metrics *metrics.Metrics
	logger  *slog.Logger
}

// NewForwarder creates a Forwarder. m may be nil.
func NewForwarder(m *metrics.Metrics, logger *slog.Logger) *Forwarder {
	return &Forwarder{metrics: m, logger: logger}
}

// NewUpstreamClient is the HTTP client for provider calls: a long timeout for
// inference and no redirects. transport may be nil for the default.
func NewUpstreamClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: transport,
		Timeout:   300 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Forward sends body to the target and writes the provider's answer to w.
func (f *Forwarder) Forward(w http.ResponseWriter, r *http.Request, body []byte, stream bool, t *Target, hooks Hooks) {
	if stream {
		f.forwardStreaming(w, r, injectStreamOptions(body), t, hooks)
		return
	}
	f.forwardBuffered(w, r, body, t, hooks)
}

// settle runs the Settle hook detached from the request's cancellation.
func (f *Forwarder) settle(r *http.Request, hooks Hooks, o Outcome) error {
	if hooks.Settle == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), settleTimeout)
	defer cancel()
	err := hooks.Settle(ctx, o)
	if err != nil {
		f.logger.Error("request accounting failed", "disposition", o.Disposition, "error", err)
	}
	return err
}

func (f *Forwarder) newRequest(ctx context.Context, t *Target, body []byte) (*http.Request, *upstreamError) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
	if err != nil {
		return nil, &upstreamError{status: http.StatusInternalServerError, errType: "api_error", code: "server_error", message: "failed to create proxy request"}
	}
	req.Header.Set("Content-Type", "application/json")
	if v := middleware.RequestIDFromContext(ctx); v != "" {
		req.Header.Set("X-Request-ID", v)
	}
	for k, vv := range t.Header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}

func (f *Forwarder) client(t *Target) *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return NewUpstreamClient(nil)
}

func (f *Forwarder) forwardBuffered(w http.ResponseWriter, r *http.Request, body []byte, t *Target, hooks Hooks) {
	proxyReq, ue := f.newRequest(r.Context(), t, body)
	if ue != nil {
		f.WriteUpstreamError(w, t, 0, ue, nil)
		return
	}

	backendStart := time.Now()
	resp, err := f.client(t).Do(proxyReq)
	if err != nil {
		f.failRoundTrip(w, r, t, hooks, 0, err)
		return
	}
	defer resp.Body.Close()

	// The status decides a classified failure; its body is discarded unread,
	// so a truncated or stalled error body cannot reclassify it.
	if ue := classifyUpstreamStatus(resp.StatusCode, resp.Header); ue != nil {
		_ = f.settle(r, hooks, Outcome{Disposition: DispositionFailed, ErrorCode: ue.code, UpstreamStatus: resp.StatusCode})
		f.WriteUpstreamError(w, t, resp.StatusCode, ue, nil)
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	backendDuration := time.Since(backendStart).Seconds()
	if err != nil {
		f.failRoundTrip(w, r, t, hooks, resp.StatusCode, err)
		return
	}
	if f.metrics != nil {
		f.metrics.BackendRequestDuration.WithLabelValues(t.MetricsModel, t.BackendRef).Observe(backendDuration)
	}

	respBody = rewriteModelInResponse(respBody, t.ResponseModel)
	usage := usageFromResponse(respBody)
	outcome := Outcome{Disposition: DispositionCompleted, UpstreamStatus: resp.StatusCode, Usage: usage}
	if resp.StatusCode != http.StatusOK {
		// A client error forwarded unchanged (other 4xx).
		outcome.Disposition = DispositionFailed
		outcome.ErrorCode = "upstream_client_error"
	}
	if err := f.settle(r, hooks, outcome); err != nil && outcome.Disposition == DispositionCompleted {
		f.WriteUpstreamError(w, t, 0, upstreamUnavailable(""), err)
		return
	}
	f.recordUsageMetrics(t.MetricsModel, usage, backendDuration, backendDuration)

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	// The response body may have been rewritten above (model-name alias),
	// which can change its length. Discard the backend's Content-Length so
	// Go recomputes it from the actual bytes we write — a stale value would
	// make ingress-nginx read past EOF and return 502 to the client.
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(respBody); err != nil {
		f.logger.Error("failed to write backend response", "model", t.MetricsModel, "error", err)
	}
}

// failRoundTrip handles a provider call that failed before a response could be
// relayed: a provider outage, or the client going away (not an outage).
func (f *Forwarder) failRoundTrip(w http.ResponseWriter, r *http.Request, t *Target, hooks Hooks, status int, err error) {
	ue := transportError(r.Context())
	if ue == nil {
		_ = f.settle(r, hooks, Outcome{Disposition: DispositionCanceled, UpstreamStatus: status})
		return
	}
	_ = f.settle(r, hooks, Outcome{Disposition: DispositionFailed, ErrorCode: ue.code, UpstreamStatus: status})
	f.WriteUpstreamError(w, t, status, ue, err)
}

// usageFromResponse extracts the provider's usage from a buffered response.
func usageFromResponse(body []byte) *Usage {
	var resp struct {
		Usage *wireUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return nil
	}
	return resp.Usage.usage()
}

// wireUsage is the OpenAI usage object, including the cached prompt tokens.
type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

func (u *wireUsage) usage() *Usage {
	if u == nil {
		return nil
	}
	out := &Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
	if u.PromptTokensDetails != nil {
		out.CachedPromptTokens = u.PromptTokensDetails.CachedTokens
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}

// recordUsageMetrics records token metrics for a request that reported usage.
func (f *Forwarder) recordUsageMetrics(model string, u *Usage, promptDuration, decodeDuration float64) {
	if f.metrics == nil || u == nil {
		return
	}
	if u.PromptTokens > 0 {
		f.metrics.PromptTokensTotal.WithLabelValues(model).Add(float64(u.PromptTokens))
		f.metrics.TokensPerRequest.WithLabelValues(model, "prompt").Observe(float64(u.PromptTokens))
	}
	if u.CompletionTokens > 0 {
		f.metrics.CompletionTokensTotal.WithLabelValues(model).Add(float64(u.CompletionTokens))
		f.metrics.TokensPerRequest.WithLabelValues(model, "completion").Observe(float64(u.CompletionTokens))
	}
	if promptDuration > 0 && u.PromptTokens > 0 {
		f.metrics.TokensPerSecond.WithLabelValues(model, "prompt").Observe(float64(u.PromptTokens) / promptDuration)
	}
	if decodeDuration > 0 && u.CompletionTokens > 0 {
		completionTokensPerSecond := float64(u.CompletionTokens) / decodeDuration
		f.metrics.TokensPerSecond.WithLabelValues(model, "completion").Observe(completionTokensPerSecond)
		f.metrics.CompletionTokensPerSecondByPromptBucket.
			WithLabelValues(model, promptTokensBucket(u.PromptTokens)).
			Observe(completionTokensPerSecond)
	}
}

// WriteUpstreamError records and writes a classified provider failure.
func (f *Forwarder) WriteUpstreamError(w http.ResponseWriter, t *Target, upstreamStatus int, ue *upstreamError, cause error) {
	f.recordUpstreamError(t, upstreamStatus, ue, cause)
	if ue.retryAfter != "" {
		w.Header().Set("Retry-After", ue.retryAfter)
	}
	writeTypedError(w, ue.status, ue.errType, ue.message, ue.code)
}

// recordUpstreamError emits the payload-free log line and counter (REQ-036).
func (f *Forwarder) recordUpstreamError(t *Target, upstreamStatus int, ue *upstreamError, cause error) {
	attrs := []any{"model", t.MetricsModel, "backend", t.BackendRef, "code", ue.code}
	if upstreamStatus != 0 {
		attrs = append(attrs, "upstream_status", upstreamStatus)
	}
	if cause != nil {
		attrs = append(attrs, "error", cause)
	}
	f.logger.Warn("upstream request failed", attrs...)
	if f.metrics != nil {
		f.metrics.UpstreamErrorsTotal.WithLabelValues(t.MetricsModel, ue.code).Inc()
	}
}

// streamErrorEvent is the terminal SSE event for a provider failure after the
// stream has started; the stream then closes without [DONE].
func streamErrorEvent(ue *upstreamError) string {
	data, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": ue.message, "type": ue.errType, "code": ue.code},
	})
	return fmt.Sprintf("data: %s\n\n", data)
}
