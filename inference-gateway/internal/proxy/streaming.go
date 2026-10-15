package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// forwardStreaming proxies a streaming SSE response from the backend to the
// client, flushing each chunk immediately. The caller injected stream options
// so the provider reports usage. Model names are rewritten, and TTFT, ITL,
// active_streams, backend_request_duration and token metrics are recorded.
//
//nolint:gocyclo // SSE streaming orchestration; extraction tracked separately.
func (f *Forwarder) forwardStreaming(w http.ResponseWriter, r *http.Request, body []byte, t *Target, hooks Hooks) {
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

	if f.metrics != nil {
		f.metrics.BackendRequestDuration.WithLabelValues(t.MetricsModel, t.BackendRef).
			Observe(time.Since(backendStart).Seconds())
	}

	// A classified provider failure before the first byte uses the same
	// mapping as non-streaming (DES-HOR-597-02); any other non-200 is a client
	// error forwarded as-is (not SSE).
	if ue := classifyUpstreamStatus(resp.StatusCode, resp.Header); ue != nil {
		_ = f.settle(r, hooks, Outcome{Disposition: DispositionFailed, ErrorCode: ue.code, UpstreamStatus: resp.StatusCode})
		f.WriteUpstreamError(w, t, resp.StatusCode, ue, nil)
		return
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		respBody = rewriteModelInResponse(respBody, t.ResponseModel)
		_ = f.settle(r, hooks, Outcome{Disposition: DispositionFailed, ErrorCode: "upstream_client_error", UpstreamStatus: resp.StatusCode})
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		if _, err := w.Write(respBody); err != nil {
			f.logger.Error("failed to write backend error response", "model", t.MetricsModel, "error", err)
		}
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		_ = f.settle(r, hooks, Outcome{Disposition: DispositionFailed, ErrorCode: "server_error", UpstreamStatus: resp.StatusCode})
		writeError(w, http.StatusInternalServerError, "streaming not supported", "server_error")
		return
	}

	if f.metrics != nil {
		f.metrics.ActiveStreams.WithLabelValues(t.MetricsModel).Inc()
		defer f.metrics.ActiveStreams.WithLabelValues(t.MetricsModel).Dec()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	var (
		prevTotalTokens int
		lastChunkAt     time.Time
		firstContentAt  time.Time
		gotFirstContent bool
		finalUsage      *Usage
		sawDone         bool
		providerError   bool
	)

	events := newSSEReader(resp.Body)

	for {
		ev, ok := events.next()
		if !ok || r.Context().Err() != nil {
			break
		}

		// Comments are keep-alives; their text is the provider's, so only
		// the fact of one is forwarded.
		if ev.comment {
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
			continue
		}

		if ev.isMessage() && ev.data == "[DONE]" {
			sawDone = true
			break
		}

		// Any other non-chunk event is a mid-stream failure: its diagnostic
		// body (which may echo the credential) is never forwarded, and the
		// stream ends with the one classified event below, never [DONE].
		if !isCompletionChunk(ev) {
			providerError = true
			break
		}
		data := ev.data
		chunk, rewritten := processStreamChunk(data, t.ResponseModel)

		now := time.Now()
		if chunk != nil && chunkHasContent(data) {
			if !gotFirstContent {
				gotFirstContent = true
				firstContentAt = now
				if f.metrics != nil {
					f.metrics.TimeToFirstToken.WithLabelValues(t.MetricsModel).
						Observe(now.Sub(backendStart).Seconds())
				}
			} else if f.metrics != nil && !lastChunkAt.IsZero() {
				f.metrics.InterTokenLatency.WithLabelValues(t.MetricsModel).
					Observe(now.Sub(lastChunkAt).Seconds())
			}
			lastChunkAt = now
		}

		if chunk != nil && chunk.Usage != nil {
			finalUsage = chunk.Usage.usage()
			if hooks.OnStreamTokens != nil {
				if delta := finalUsage.TotalTokens - prevTotalTokens; delta > 0 {
					hooks.OnStreamTokens(delta)
					prevTotalTokens = finalUsage.TotalTokens
				}
			}
		}

		fmt.Fprintf(w, "data: %s\n\n", rewritten)
		flusher.Flush()
	}

	switch {
	case r.Context().Err() != nil:
		// The client went away: not a provider outage, nothing more to write.
		_ = f.settle(r, hooks, Outcome{Disposition: DispositionCanceled, UpstreamStatus: resp.StatusCode, Usage: finalUsage})
		return
	case sawDone:
		if err := f.settle(r, hooks, Outcome{Disposition: DispositionCompleted, UpstreamStatus: resp.StatusCode, Usage: finalUsage}); err != nil {
			// Accounting failed: the stream does not complete (DES-HOR-661-08).
			ue := upstreamUnavailable("")
			f.recordUpstreamError(t, 0, ue, err)
			fmt.Fprint(w, streamErrorEvent(ue))
			flusher.Flush()
			return
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	default:
		// The provider failed after the stream started: the status is already
		// sent, so end with one classified error event and no [DONE].
		ue := upstreamUnavailable("")
		cause := events.err()
		if providerError {
			cause = errProviderStreamError
		}
		_ = f.settle(r, hooks, Outcome{Disposition: DispositionFailed, ErrorCode: ue.code, UpstreamStatus: resp.StatusCode, Usage: finalUsage})
		f.recordUpstreamError(t, resp.StatusCode, ue, cause)
		fmt.Fprint(w, streamErrorEvent(ue))
		flusher.Flush()
	}

	if finalUsage != nil {
		streamDuration := time.Since(backendStart).Seconds()
		promptDuration, decodeDuration := streamDuration, streamDuration
		if !firstContentAt.IsZero() {
			promptDuration = firstContentAt.Sub(backendStart).Seconds()
			decodeDuration = time.Since(firstContentAt).Seconds()
		}
		f.recordUsageMetrics(t.MetricsModel, finalUsage, promptDuration, decodeDuration)
	}
}

type streamChunk struct {
	Usage *wireUsage `json:"usage,omitempty"`
}

func processStreamChunk(data string, aliasName string) (*streamChunk, string) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		return nil, data
	}
	if _, ok := parsed["model"]; ok && aliasName != "" {
		parsed["model"] = aliasName
	}
	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return nil, data
	}
	var chunk streamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, data
	}
	return &chunk, string(rewritten)
}

// errProviderStreamError records a provider error event without its body.
var errProviderStreamError = errors.New("provider sent an error event mid-stream")

func chunkHasContent(data string) bool {
	var parsed struct {
		Choices []struct {
			Delta struct {
				Content *string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		return false
	}
	for _, c := range parsed.Choices {
		if c.Delta.Content != nil && *c.Delta.Content != "" {
			return true
		}
	}
	return false
}

// injectStreamOptions asks the provider to report usage on the stream, and
// live usage where it supports it (vLLM continuous_usage_stats).
func injectStreamOptions(body []byte) []byte {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	streamOpts, ok := parsed["stream_options"].(map[string]any)
	if !ok {
		streamOpts = make(map[string]any)
	}
	streamOpts["include_usage"] = true
	streamOpts["continuous_usage_stats"] = true
	parsed["stream_options"] = streamOpts
	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return rewritten
}

func promptTokensBucket(tokens int) string {
	switch {
	case tokens <= 1024:
		return "le_1k"
	case tokens <= 2048:
		return "1k_2k"
	case tokens <= 4096:
		return "2k_4k"
	case tokens <= 8192:
		return "4k_8k"
	case tokens <= 16384:
		return "8k_16k"
	case tokens <= 32768:
		return "16k_32k"
	case tokens <= 65536:
		return "32k_64k"
	default:
		return "gt_64k"
	}
}
