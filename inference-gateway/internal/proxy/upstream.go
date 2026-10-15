package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"

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
	// backendKindManaged routes to the central Iterabase gateway over mTLS
	// (DES-HOR-661-03).
	backendKindManaged = "managed"
)

// Exported codes for the central gateway, which shares this contract.
const (
	CodeUpstreamUnavailable = codeUpstreamUnavailable
	CodeUpstreamRateLimited = codeUpstreamRateLimited
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

func upstreamRateLimited(retryAfter string) *upstreamError {
	return &upstreamError{
		status: http.StatusTooManyRequests, errType: "rate_limit_error", code: codeUpstreamRateLimited,
		message: "model provider rate limit reached", retryAfter: retryAfterOrDefault(retryAfter),
	}
}

func upstreamAuthFailed(message string) *upstreamError {
	return &upstreamError{status: http.StatusBadGateway, errType: "api_error", code: codeUpstreamAuthFailed, message: message}
}

// WriteUpstreamUnavailable writes the retryable upstream_unavailable error.
func WriteUpstreamUnavailable(w http.ResponseWriter, retryAfter string) {
	writeClassified(w, upstreamUnavailable(retryAfter))
}

// WriteUpstreamRateLimited writes the retryable upstream_rate_limited error.
func WriteUpstreamRateLimited(w http.ResponseWriter, retryAfter string) {
	writeClassified(w, upstreamRateLimited(retryAfter))
}

func writeClassified(w http.ResponseWriter, ue *upstreamError) {
	if ue.retryAfter != "" {
		w.Header().Set("Retry-After", ue.retryAfter)
	}
	writeTypedError(w, ue.status, ue.errType, ue.message, ue.code)
}

// classifyUpstreamStatus maps a non-2xx provider status to its classified
// error, or nil when the response is a client error forwarded unchanged.
func classifyUpstreamStatus(status int, header http.Header) *upstreamError {
	switch {
	case status == http.StatusTooManyRequests:
		return upstreamRateLimited(header.Get("Retry-After"))
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

// ChatCompletionsURL is the chat-completions endpoint under an OpenAI
// SDK-style base URL that already carries its version path
// (https://provider/v1, DES-HOR-597-04).
func ChatCompletionsURL(base string) string {
	return strings.TrimRight(base, "/") + "/chat/completions"
}

// chatCompletionsURL is the provider endpoint for an entry. An external
// backend's URL is an OpenAI SDK-style base; an in-cluster backend's is the
// bare service URL.
func chatCompletionsURL(entry *snapshot.CatalogEntry) string {
	if entry.BackendKind == backendKindExternal {
		return ChatCompletionsURL(entry.BackendURL)
	}
	return strings.TrimRight(entry.BackendURL, "/") + "/v1/chat/completions"
}

// transportError classifies a failed provider round trip. It returns nil when
// the client itself went away, which is not a provider outage.
func transportError(ctx context.Context) *upstreamError {
	if ctx.Err() != nil {
		return nil
	}
	return upstreamUnavailable("")
}
