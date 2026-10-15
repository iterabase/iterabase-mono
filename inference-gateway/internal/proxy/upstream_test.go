package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/snapshot"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/upstreamauth"
)

const (
	testKey       = "sk-test-provider-key"
	testNamespace = "iterabase-system"
	testAlias     = "hosted"
)

// fakeResolver serves one credential for one authRef.
type fakeResolver struct {
	ref, key string
	calls    int
}

func (f *fakeResolver) Credential(_ context.Context, namespace, authRef string) (string, error) {
	f.calls++
	if namespace != testNamespace || authRef != f.ref {
		return "", fmt.Errorf("%w: secret %s/%s not found", upstreamauth.ErrUnavailable, namespace, authRef)
	}
	return f.key, nil
}

// upstreamFixture is a gateway handler routing testAlias to backend.
type upstreamFixture struct {
	h       *Handler
	m       *metrics.Metrics
	logs    *bytes.Buffer
	backend *httptest.Server
}

func newUpstreamFixture(t *testing.T, authRef string, creds upstreamauth.Resolver, backend http.HandlerFunc) *upstreamFixture {
	t.Helper()
	srv := httptest.NewServer(backend)
	t.Cleanup(srv.Close)
	cache := &fakeReader{
		catalog: map[string]snapshot.CatalogEntry{testAlias: {
			ModelID: testAlias, Namespace: testNamespace, BackendRef: "hosted-backend", BackendKind: "external",
			BackendURL: srv.URL, BackendAuthRef: authRef, Available: true,
		}},
		caps: map[string][]snapshot.Capability{"identity-1": {{Resource: "*", Action: "*"}}},
	}
	logs := &bytes.Buffer{}
	m := metrics.New(nil)
	return &upstreamFixture{
		h:       NewHandler(cache, nil, m, creds, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		m:       m,
		logs:    logs,
		backend: srv,
	}
}

func (f *upstreamFixture) do(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer ml-client-key")
	// Bounded so a gateway blocked on a stalled provider fails, not hangs.
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	req = req.WithContext(middleware.WithIdentityID(ctx, "identity-1"))
	rec := httptest.NewRecorder()
	f.h.ChatCompletions(rec, req)
	return rec
}

func chatBody(stream bool) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, testAlias, stream)
}

func completion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"model":"m","choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":4}}`)
}

type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeError(t *testing.T, raw []byte) errorBody {
	t.Helper()
	var e errorBody
	require.NoError(t, json.Unmarshal(raw, &e), "body: %s", raw)
	return e
}

func TestUpstreamAuth_SendsCredential(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var gotAuth []string
			creds := &fakeResolver{ref: "hosted-llm", key: testKey}
			f := newUpstreamFixture(t, "hosted-llm", creds, func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Values("Authorization")
				completion(w, r)
			})

			rec := f.do(t, chatBody(stream))

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, []string{"Bearer " + testKey}, gotAuth, "only the provider credential goes upstream, never the client key")
			if stream {
				assert.Contains(t, rec.Body.String(), "data: [DONE]")
			}
			assert.NotContains(t, rec.Body.String(), testKey)
			assert.NotContains(t, f.logs.String(), testKey, "the credential never reaches logs")
		})
	}
}

func TestChatCompletionsURL(t *testing.T) {
	for _, tc := range []struct{ kind, base, want string }{
		{"external", "https://provider.example/v1", "https://provider.example/v1/chat/completions"},
		{"external", "https://provider.example/v1/", "https://provider.example/v1/chat/completions"},
		{"external", "https://provider.example/api/openai/v1", "https://provider.example/api/openai/v1/chat/completions"},
		{"vllm", "http://qwen.iterabase-system.svc:8000", "http://qwen.iterabase-system.svc:8000/v1/chat/completions"},
		{"vllm", "http://qwen.iterabase-system.svc:8000/", "http://qwen.iterabase-system.svc:8000/v1/chat/completions"},
	} {
		got := chatCompletionsURL(&snapshot.CatalogEntry{BackendKind: tc.kind, BackendURL: tc.base})
		assert.Equal(t, tc.want, got, "%s %s", tc.kind, tc.base)
	}
}

func TestUpstreamURL_ExternalBaseKeepsVersionPath(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var gotPath string
			f := newUpstreamFixture(t, "", nil, func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				completion(w, r)
			})
			entry := f.h.cache.(*fakeReader).catalog[testAlias]
			entry.BackendURL = f.backend.URL + "/v1"
			f.h.cache.(*fakeReader).catalog[testAlias] = entry

			rec := f.do(t, chatBody(stream))

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, "/v1/chat/completions", gotPath, "an SDK-style base is not given a second /v1")
		})
	}
}

func TestUpstreamAuth_UnkeyedBackendSendsNoAuthorization(t *testing.T) {
	var gotAuth []string
	creds := &fakeResolver{ref: "unused", key: testKey}
	f := newUpstreamFixture(t, "", creds, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Values("Authorization")
		completion(w, r)
	})

	rec := f.do(t, chatBody(false))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, gotAuth, "in-cluster backends get no Authorization, and the client key is not forwarded")
	assert.Zero(t, creds.calls)
}

func TestUpstreamAuth_CredentialUnavailableFailsClosed(t *testing.T) {
	for name, creds := range map[string]upstreamauth.Resolver{
		"missing secret": &fakeResolver{ref: "other", key: testKey},
		"no resolver":    nil,
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", name, stream), func(t *testing.T) {
				called := false
				f := newUpstreamFixture(t, "hosted-llm", creds, func(w http.ResponseWriter, r *http.Request) {
					called = true
					completion(w, r)
				})

				rec := f.do(t, chatBody(stream))

				assert.Equal(t, http.StatusBadGateway, rec.Code)
				assert.Equal(t, codeUpstreamAuthFailed, decodeError(t, rec.Body.Bytes()).Error.Code)
				assert.Empty(t, rec.Header().Get("Retry-After"), "a misconfigured credential is not retryable")
				assert.False(t, called, "no unauthenticated request reaches the provider")
				assert.Equal(t, 1.0, testutil.ToFloat64(f.m.UpstreamErrorsTotal.WithLabelValues(testAlias, codeUpstreamAuthFailed)))
			})
		}
	}
}

func TestUpstreamErrors_Classified(t *testing.T) {
	cases := []struct {
		upstream, status int
		retryAfterIn     string
		errType, code    string
		retryAfter       string
	}{
		{500, 503, "", "api_error", codeUpstreamUnavailable, "5"},
		{502, 503, "", "api_error", codeUpstreamUnavailable, "5"},
		{503, 503, "17", "api_error", codeUpstreamUnavailable, "17"},
		{504, 503, "", "api_error", codeUpstreamUnavailable, "5"},
		{529, 503, "", "api_error", codeUpstreamUnavailable, "5"},
		{501, 503, "", "api_error", codeUpstreamUnavailable, "5"},
		{520, 503, "", "api_error", codeUpstreamUnavailable, "5"},
		{523, 503, "12", "api_error", codeUpstreamUnavailable, "12"},
		{599, 503, "", "api_error", codeUpstreamUnavailable, "5"},
		{429, 429, "30", "rate_limit_error", codeUpstreamRateLimited, "30"},
		{429, 429, "Wed, 21 Oct 2026 07:28:00 GMT", "rate_limit_error", codeUpstreamRateLimited, "Wed, 21 Oct 2026 07:28:00 GMT"},
		{503, 503, "Wed, 21 Oct 2026 07:28:00 GMT", "api_error", codeUpstreamUnavailable, "Wed, 21 Oct 2026 07:28:00 GMT"},
		{429, 429, "soon", "rate_limit_error", codeUpstreamRateLimited, "5"},
		{503, 503, "-1", "api_error", codeUpstreamUnavailable, "5"},
		{401, 502, "", "api_error", codeUpstreamAuthFailed, ""},
		{403, 502, "", "api_error", codeUpstreamAuthFailed, ""},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/stream=%t", tc.upstream, stream), func(t *testing.T) {
				f := newUpstreamFixture(t, "hosted-llm", &fakeResolver{ref: "hosted-llm", key: testKey},
					func(w http.ResponseWriter, _ *http.Request) {
						if tc.retryAfterIn != "" {
							w.Header().Set("Retry-After", tc.retryAfterIn)
						}
						w.WriteHeader(tc.upstream)
						_, _ = io.WriteString(w, `{"error":{"message":"provider-internal-detail"}}`)
					})

				rec := f.do(t, chatBody(stream))

				assert.Equal(t, tc.status, rec.Code)
				e := decodeError(t, rec.Body.Bytes())
				assert.Equal(t, tc.code, e.Error.Code)
				assert.Equal(t, tc.errType, e.Error.Type)
				assert.Equal(t, tc.retryAfter, rec.Header().Get("Retry-After"))
				assert.NotContains(t, rec.Body.String(), "provider-internal-detail", "the provider body is not passed through")
				assert.Equal(t, 1.0, testutil.ToFloat64(f.m.UpstreamErrorsTotal.WithLabelValues(testAlias, tc.code)))
			})
		}
	}
}

// A classified status is decided before the body is read: a provider that
// truncates or stalls its error body still gets the classified response.
func TestUpstreamErrors_ClassifiedBeforeBody(t *testing.T) {
	for _, tc := range []struct {
		upstream, status int
		code             string
	}{
		{401, 502, codeUpstreamAuthFailed},
		{403, 502, codeUpstreamAuthFailed},
		{429, 429, codeUpstreamRateLimited},
	} {
		for _, ending := range []string{"truncated", "stalled"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/%s/stream=%t", tc.upstream, ending, stream), func(t *testing.T) {
					release := make(chan struct{})
					f := newUpstreamFixture(t, "hosted-llm", &fakeResolver{ref: "hosted-llm", key: testKey},
						func(w http.ResponseWriter, _ *http.Request) {
							conn, buf, err := w.(http.Hijacker).Hijack()
							if err != nil {
								return
							}
							defer conn.Close()
							_, _ = fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nRetry-After: 30\r\nContent-Length: 100\r\n\r\n{", tc.upstream, http.StatusText(tc.upstream))
							_ = buf.Flush()
							if ending == "stalled" {
								<-release
							}
						})
					t.Cleanup(func() { close(release) }) // runs before the backend's Close

					rec := f.do(t, chatBody(stream))

					assert.Equal(t, tc.status, rec.Code)
					assert.Equal(t, tc.code, decodeError(t, rec.Body.Bytes()).Error.Code)
					if tc.upstream == 429 {
						assert.Equal(t, "30", rec.Header().Get("Retry-After"))
					}
				})
			}
		}
	}
}

func TestUpstreamErrors_ClientErrorForwarded(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			f := newUpstreamFixture(t, "", nil, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"context length exceeded","code":"context_length_exceeded"}}`)
			})

			rec := f.do(t, chatBody(stream))

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "context_length_exceeded", decodeError(t, rec.Body.Bytes()).Error.Code)
		})
	}
}

func TestUpstreamErrors_Unreachable(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			f := newUpstreamFixture(t, "", nil, completion)
			f.backend.Close() // connection refused

			rec := f.do(t, chatBody(stream))

			assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
			assert.Equal(t, codeUpstreamUnavailable, decodeError(t, rec.Body.Bytes()).Error.Code)
			assert.Equal(t, "5", rec.Header().Get("Retry-After"))
		})
	}
}

func TestUpstreamErrors_MidStreamFailure(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter){
		"connection dropped": func(w http.ResponseWriter) {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
			chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
			_, _ = fmt.Fprintf(buf, "%x\r\n%s\r\n", len(chunk), chunk)
			_ = buf.Flush()
			_ = conn.Close() // no terminating chunk: an unexpected EOF
		},
		"ended without [DONE]": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		},
		"provider error then close": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"overloaded, key "+testKey+"\",\"type\":\"server_error\"}}\n\n")
		},
		"provider error then [DONE]": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"overloaded, key "+testKey+"\",\"type\":\"server_error\"}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		},
	}
	// Every valid SSE framing of a provider error, each ending both ways.
	partial := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
	providerErrors := map[string]string{
		"no space after colon":  "data:{\"error\":{\"message\":\"overloaded, key " + testKey + "\"}}\n\n",
		"multiline data":        "data: {\"error\":\ndata: {\"message\":\"overloaded, key " + testKey + "\",\ndata: \"type\":\"server_error\"}}\n\n",
		"CRLF lines":            "data: {\"error\":{\"message\":\"overloaded, key " + testKey + "\"}}\r\n\r\n",
		"CR lines":              "data: {\"error\":{\"message\":\"overloaded, key " + testKey + "\"}}\r\r",
		"error event":           "event: error\ndata: {\"message\":\"overloaded, key " + testKey + "\"}\n\n",
		"[DONE] as error event": ": overloaded, key " + testKey + "\nevent: error\ndata: [DONE]\n\n",
		"non-JSON data":         "data: overloaded, key " + testKey + "\n\n",
		"JSON non-object":       "data: \"overloaded, key " + testKey + "\"\n\n",
	}
	for framing, event := range providerErrors {
		for ending, tail := range map[string]string{"close": "", "[DONE]": "data:[DONE]\n\n"} {
			body := partial + event + tail
			cases[framing+" then "+ending] = func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, body)
			}
		}
	}
	for name, backend := range cases {
		t.Run(name, func(t *testing.T) {
			f := newUpstreamFixture(t, "hosted-llm", &fakeResolver{ref: "hosted-llm", key: testKey},
				func(w http.ResponseWriter, _ *http.Request) { backend(w) })

			rec := f.do(t, chatBody(true))

			require.Equal(t, http.StatusOK, rec.Code, "the stream had already started")
			events := sseData(t, rec.Body.String())
			require.Len(t, events, 2, "the partial chunk, then exactly one classified error: %s", rec.Body.String())
			assert.Contains(t, events[0], "partial")
			assert.Equal(t, codeUpstreamUnavailable, decodeError(t, []byte(events[1])).Error.Code)
			assert.NotContains(t, rec.Body.String(), "[DONE]")
			assert.NotContains(t, rec.Body.String(), "overloaded", "the provider's error body is not forwarded")
			assert.NotContains(t, rec.Body.String(), testKey)
			assert.NotContains(t, f.logs.String(), testKey)
			assert.Equal(t, 1.0, testutil.ToFloat64(f.m.UpstreamErrorsTotal.WithLabelValues(testAlias, codeUpstreamUnavailable)))
		})
	}
}

// Valid chunks in any SSE framing reach the client, normalized, with [DONE];
// a provider comment is forwarded only as a keep-alive.
func TestStreaming_ProviderFraming(t *testing.T) {
	cases := map[string]string{
		"no space after colon": "data:{\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata:[DONE]\n\n",
		"multiline data":       "data: {\"model\":\"m\",\ndata: \"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
		"CRLF lines":           "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\ndata: [DONE]\r\n\r\n",
		"named message events": "event: message\ndata: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\nevent: message\ndata: [DONE]\n\n",
		"comment and id":       ": provider note " + testKey + "\nid: 7\ndata: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newUpstreamFixture(t, "hosted-llm", &fakeResolver{ref: "hosted-llm", key: testKey},
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, body)
				})

			rec := f.do(t, chatBody(true))

			require.Equal(t, http.StatusOK, rec.Code)
			events := sseData(t, rec.Body.String())
			require.Equal(t, []string{`{"choices":[{"delta":{"content":"hi"}}],"model":"` + testAlias + `"}`, "[DONE]"}, events, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), testKey)
			assert.Equal(t, 0.0, testutil.ToFloat64(f.m.UpstreamErrorsTotal.WithLabelValues(testAlias, codeUpstreamUnavailable)))
		})
	}
}

func sseData(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, data)
		}
	}
	return out
}

// imageMessages is a vision request: a text part plus a base64 image part.
const imageMessages = `[{"role":"system","content":[{"type":"text","text":"Read invoices."}]},` +
	`{"role":"user","content":[{"type":"text","text":"What is the total?"},` +
	`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==","detail":"high"}}]}]`

func TestImagePassThrough(t *testing.T) {
	for _, prefix := range []string{"", "You are helpful."} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("prefix=%t/stream=%t", prefix != "", stream), func(t *testing.T) {
				var got []any
				f := newUpstreamFixture(t, "hosted-llm", &fakeResolver{ref: "hosted-llm", key: testKey},
					func(w http.ResponseWriter, r *http.Request) {
						raw, _ := io.ReadAll(r.Body)
						var req map[string]any
						require.NoError(t, json.Unmarshal(raw, &req))
						got, _ = req["messages"].([]any)
						r.Body = io.NopCloser(bytes.NewReader(raw))
						completion(w, r)
					})
				e := f.h.cache.(*fakeReader).catalog[testAlias]
				e.Transforms.SystemPromptPrefix = prefix
				f.h.cache.(*fakeReader).catalog[testAlias] = e

				rec := f.do(t, fmt.Sprintf(`{"model":%q,"stream":%t,"messages":%s}`, testAlias, stream, imageMessages))
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

				var want []any
				require.NoError(t, json.Unmarshal([]byte(imageMessages), &want))
				if prefix != "" {
					system := want[0].(map[string]any)
					system["content"] = append([]any{map[string]any{"type": "text", "text": prefix}}, system["content"].([]any)...)
				}
				assert.Equal(t, want, got, "image content parts reach the provider unchanged")
			})
		}
	}
}

// A managed model with no central route configured fails closed and
// retryably; nothing is sent anywhere (DES-HOR-661-03).
func TestManaged_CentralUnconfiguredFailsClosed(t *testing.T) {
	called := false
	f := newUpstreamFixture(t, "", nil, func(w http.ResponseWriter, r *http.Request) { called = true })
	entry := f.h.cache.(*fakeReader).catalog[testAlias]
	entry.BackendKind = backendKindManaged
	entry.BackendModelID = "central-model"
	f.h.cache.(*fakeReader).catalog[testAlias] = entry

	for _, stream := range []bool{false, true} {
		rec := f.do(t, chatBody(stream))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Equal(t, codeUpstreamUnavailable, decodeError(t, rec.Body.Bytes()).Error.Code)
		assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	}
	assert.False(t, called)
}

// A managed model goes to central's chat endpoint under its central name,
// with attribution and without any bearer.
func TestManaged_RoutesToCentralWithAttribution(t *testing.T) {
	var got *http.Request
	var gotModel string
	central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		completion(w, r)
	}))
	t.Cleanup(central.Close)
	f := newUpstreamFixture(t, "", nil, completion)
	entry := f.h.cache.(*fakeReader).catalog[testAlias]
	entry.BackendKind, entry.BackendModelID, entry.BackendURL = backendKindManaged, "central-model", ""
	f.h.cache.(*fakeReader).catalog[testAlias] = entry
	f.h.WithCentral(&Central{URL: central.URL + "/", Client: central.Client()})

	rec := f.do(t, chatBody(false))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, got)
	assert.Equal(t, "/v1/chat/completions", got.URL.Path)
	assert.Equal(t, "central-model", gotModel, "central receives its own model name")
	assert.Empty(t, got.Header.Get("Authorization"))
	assert.Equal(t, "identity-1", got.Header.Get("X-Iterabase-Identity-Id"))
	assert.Equal(t, "api", got.Header.Get("X-Iterabase-Channel"))
}

// A failed Settle withholds a completed response (DES-HOR-661-08).
func TestForward_SettleErrorWithholdsResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(completion))
	t.Cleanup(backend.Close)
	fw := NewForwarder(nil, slog.Default())
	target := &Target{URL: backend.URL + "/v1/chat/completions", ResponseModel: "m", MetricsModel: "m"}
	failing := Hooks{Settle: func(context.Context, Outcome) error { return fmt.Errorf("database down") }}

	for _, stream := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		rec := httptest.NewRecorder()
		fw.Forward(rec, req, []byte(chatBody(stream)), stream, target, failing)
		if stream {
			assert.Contains(t, rec.Body.String(), `"code":"upstream_unavailable"`)
			assert.NotContains(t, rec.Body.String(), "[DONE]")
		} else {
			assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
			assert.NotContains(t, rec.Body.String(), "choices")
		}
	}

	var outcome Outcome
	rec := httptest.NewRecorder()
	fw.Forward(rec, httptest.NewRequest(http.MethodPost, "/", nil), []byte(chatBody(true)), true, target,
		Hooks{Settle: func(_ context.Context, o Outcome) error { outcome = o; return nil }})
	assert.Equal(t, DispositionCompleted, outcome.Disposition)
	require.NotNil(t, outcome.Usage)
	assert.Equal(t, Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4}, *outcome.Usage)
}
