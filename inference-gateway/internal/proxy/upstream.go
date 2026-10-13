package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/snapshot"
)

// Classified upstream error codes (DES-HOR-597-02). The runtime pauses a step
// on the two retryable codes (HOR-649) and fails it on upstream_auth_failed.
const (
	codeUpstreamUnavailable = "upstream_unavailable"
	codeUpstreamRateLimited = "upstream_rate_limited"
	codeUpstreamAuthFailed  = "upstream_auth_failed"

	// defaultRetryAfter applies when a retryable upstream failure carries no
	// Retry-After of its own.
	defaultRetryAfter = "5"

	backendKindExternal = "external"
)

// upstreamError is a classified provider failure, written to the client in
// the OpenAI-compatible error envelope. It never carries the upstream body.
type upstreamError struct {
	status     int
	errType    string
	code       string
	message    string
	retryAfter string
}

func upstreamUnavailable(retryAfter string) *upstreamError {
	return &upstreamError{
		status: http.StatusServiceUnavailable, errType: "api_error", code: codeUpstreamUnavailable,
		message: "model provider unavailable", retryAfter: retryAfterOrDefault(retryAfter),
	}
}

func upstreamAuthFailed(message string) *upstreamError {
	return &upstreamError{status: http.StatusBadGateway, errType: "api_error", code: codeUpstreamAuthFailed, message: message}
}

// classifyUpstreamStatus maps a non-2xx provider status to its classified
// error, or nil when the response is a client error forwarded unchanged.
func classifyUpstreamStatus(status int, header http.Header) *upstreamError {
	switch {
	case status == http.StatusTooManyRequests:
		return &upstreamError{
			status: http.StatusTooManyRequests, errType: "rate_limit_error", code: codeUpstreamRateLimited,
			message: "model provider rate limit reached", retryAfter: retryAfterOrDefault(header.Get("Retry-After")),
		}
	// Every 5xx, including non-standard ones such as 529 (overloaded) and a
	// CDN's 520-524 for an unreachable origin (DES-HOR-597-02, amended).
	case status >= 500 && status <= 599:
		return upstreamUnavailable(header.Get("Retry-After"))
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return upstreamAuthFailed("model provider rejected the gateway credential")
	}
	return nil
}

// retryAfterOrDefault keeps a provider's Retry-After in either valid form,
// delay-seconds or HTTP-date (RFC 9110 §10.2.3); an absent or invalid value
// becomes the default.
func retryAfterOrDefault(v string) string {
	v = strings.TrimSpace(v)
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return strconv.Itoa(n)
	}
	if _, err := http.ParseTime(v); err == nil {
		return v
	}
	return defaultRetryAfter
}

// chatCompletionsURL is the provider endpoint for an entry. An external
// backend's URL is an OpenAI SDK-style base that already carries its version
// path (https://provider/v1); an in-cluster backend's is the bare service URL
// (DES-HOR-597-04).
func chatCompletionsURL(entry *snapshot.CatalogEntry) string {
	base := strings.TrimRight(entry.BackendURL, "/")
	if entry.BackendKind == backendKindExternal {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

// newUpstreamRequest builds the provider request. A keyed external backend
// gets its credential as the bearer token; nothing from the client's own
// Authorization header is forwarded.
func (h *Handler) newUpstreamRequest(ctx context.Context, entry *snapshot.CatalogEntry, body []byte) (*http.Request, *upstreamError) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatCompletionsURL(entry), bytes.NewReader(body))
	if err != nil {
		return nil, &upstreamError{status: http.StatusInternalServerError, errType: "api_error", code: "server_error", message: "failed to create proxy request"}
	}
	req.Header.Set("Content-Type", "application/json")
	if v := middleware.RequestIDFromContext(ctx); v != "" {
		req.Header.Set("X-Request-ID", v)
	}
	if entry.BackendAuthRef == "" {
		return req, nil
	}
	if h.creds == nil {
		h.logger.Error("upstream credential unavailable: no Kubernetes credential resolver",
			"model", entry.ModelID, "backend", entry.BackendRef)
		return nil, upstreamAuthFailed("model provider credential is unavailable")
	}
	key, err := h.creds.Credential(ctx, entry.Namespace, entry.BackendAuthRef)
	if err != nil {
		// The error names the Secret, never its value.
		h.logger.Error("upstream credential unavailable", "model", entry.ModelID, "backend", entry.BackendRef, "error", err)
		return nil, upstreamAuthFailed("model provider credential is unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return req, nil
}

// transportError classifies a failed provider round trip. It returns nil when
// the client itself went away, which is not a provider outage.
func transportError(ctx context.Context) *upstreamError {
	if ctx.Err() != nil {
		return nil
	}
	return upstreamUnavailable("")
}

// writeUpstreamError records and writes a classified provider failure.
func (h *Handler) writeUpstreamError(w http.ResponseWriter, entry *snapshot.CatalogEntry, upstreamStatus int, ue *upstreamError, cause error) {
	h.recordUpstreamError(entry, upstreamStatus, ue, cause)
	if ue.retryAfter != "" {
		w.Header().Set("Retry-After", ue.retryAfter)
	}
	writeTypedError(w, ue.status, ue.errType, ue.message, ue.code)
}

// recordUpstreamError emits the payload-free log line and counter (REQ-036).
func (h *Handler) recordUpstreamError(entry *snapshot.CatalogEntry, upstreamStatus int, ue *upstreamError, cause error) {
	attrs := []any{"model", entry.ModelID, "backend", entry.BackendRef, "code", ue.code}
	if upstreamStatus != 0 {
		attrs = append(attrs, "upstream_status", upstreamStatus)
	}
	if cause != nil {
		attrs = append(attrs, "error", cause)
	}
	h.logger.Warn("upstream request failed", attrs...)
	if h.metrics != nil {
		h.metrics.UpstreamErrorsTotal.WithLabelValues(entry.ModelID, ue.code).Inc()
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
