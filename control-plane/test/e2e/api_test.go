package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

const maxAPIResponseBytes = 8 << 20

func (state *deployedState) requestJSON(t *testing.T, method, path, key string, value any) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s request: %v", path, err)
	}
	return state.request(t, method, path, key, body, map[string]string{"Content-Type": "application/json"})
}

func (state *deployedState) request(t *testing.T, method, path, key string, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	status, responseBody, err := state.doRequest(method, path, key, body, headers)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	state.recordRequest(t, requestEvidence{Method: method, Path: pathWithoutQuery(path), Status: status})
	return status, responseBody
}

func (state *deployedState) doRequest(method, path, key string, body []byte, headers map[string]string) (int, []byte, error) {
	if state.apiClient == nil || state.apiForward == nil {
		return 0, nil, fmt.Errorf("control-plane API is not connected")
	}
	req, err := http.NewRequestWithContext(state.ctx, method, state.apiForward.URL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := state.apiClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes+1))
	if err != nil {
		return 0, nil, fmt.Errorf("read response: %w", err)
	}
	if len(responseBody) > maxAPIResponseBytes {
		return 0, nil, fmt.Errorf("response exceeds %d bytes", maxAPIResponseBytes)
	}
	return resp.StatusCode, responseBody, nil
}

func pathWithoutQuery(path string) string {
	if index := strings.IndexByte(path, '?'); index >= 0 {
		return path[:index]
	}
	return path
}

func requireStatus(t *testing.T, got, want int, body []byte) {
	t.Helper()
	if got != want {
		t.Fatalf("HTTP status=%d want=%d body=%s", got, want, safeResponse(body))
	}
}

func safeResponse(body []byte) string {
	const limit = 1200
	value := string(body)
	var decoded any
	if json.Unmarshal(body, &decoded) == nil {
		scrubResponseCredentials(decoded)
		if scrubbed, err := json.Marshal(decoded); err == nil {
			value = string(scrubbed)
		}
	}
	value = redact.New().String(value)
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}

func scrubResponseCredentials(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
			if strings.Contains(normalized, "token") || normalized == "fullkey" || strings.Contains(normalized, "password") ||
				strings.Contains(normalized, "secret") || strings.Contains(normalized, "authorization") || strings.Contains(normalized, "cookie") ||
				normalized == "uploadurl" {
				typed[key] = "<redacted>"
				continue
			}
			scrubResponseCredentials(child)
		}
	case []any:
		for _, child := range typed {
			scrubResponseCredentials(child)
		}
	}
}

func mustDecode(t *testing.T, body []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

type timelineEvent struct {
	Cursor     int64           `json:"cursor"`
	ID         string          `json:"id"`
	WorkItemID string          `json:"workItemId"`
	AttemptID  *string         `json:"attemptId,omitempty"`
	Code       string          `json:"code"`
	Params     json.RawMessage `json:"params"`
	CreatedAt  time.Time       `json:"createdAt"`
}

func (state *deployedState) streamEventsAfter(t *testing.T, cursor int64, want int) []timelineEvent {
	t.Helper()
	if want == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, state.apiForward.URL+"/v1/work-events", nil)
	if err != nil {
		t.Fatalf("build SSE request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+state.workKey)
	if cursor > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(cursor, 10))
	}
	client := *state.apiClient
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1200))
		t.Fatalf("open SSE stream status=%d body=%s", resp.StatusCode, safeResponse(body))
	}
	state.recordRequest(t, requestEvidence{Method: http.MethodGet, Path: "/v1/work-events", Status: resp.StatusCode,
		Fields: map[string]any{"lastEventId": cursor}})
	events, err := readSSEEvents(resp.Body, want)
	if err != nil {
		t.Fatalf("read SSE events after %d: %v", cursor, err)
	}
	return events
}

func readSSEEvents(reader io.Reader, want int) ([]timelineEvent, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var id string
	var data strings.Builder
	result := make([]timelineEvent, 0, want)
	flush := func() error {
		if data.Len() == 0 {
			id = ""
			return nil
		}
		var event timelineEvent
		if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
			return err
		}
		if id != "" && id != strconv.FormatInt(event.Cursor, 10) {
			return fmt.Errorf("SSE id=%q does not match payload cursor=%d", id, event.Cursor)
		}
		result = append(result, event)
		id = ""
		data.Reset()
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			if len(result) >= want {
				return result, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "id":
			id = value
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(result) < want {
		return nil, fmt.Errorf("stream ended after %d events, want %d", len(result), want)
	}
	return result, nil
}

func assertStrictCursorOrder(t *testing.T, events []timelineEvent, after int64) {
	t.Helper()
	cursor := after
	seen := make(map[int64]struct{}, len(events))
	for _, event := range events {
		if event.Cursor <= cursor {
			t.Fatalf("timeline cursor %d is not after %d", event.Cursor, cursor)
		}
		if _, exists := seen[event.Cursor]; exists {
			t.Fatalf("duplicate timeline cursor %d", event.Cursor)
		}
		seen[event.Cursor] = struct{}{}
		cursor = event.Cursor
	}
}

func assertCustomerSafeJSON(t *testing.T, body []byte, forbiddenValues ...string) {
	t.Helper()
	if err := validateCustomerSafeJSON(body, forbiddenValues...); err != nil {
		t.Fatal(err)
	}
}

func validateCustomerSafeJSON(body []byte, forbiddenValues ...string) error {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return fmt.Errorf("decode customer projection: %w", err)
	}
	forbiddenKeys := map[string]struct{}{
		"prompt": {}, "credentials": {}, "credential": {}, "workerid": {}, "worker_id": {},
		"tooltrace": {}, "tool_trace": {}, "modeltrace": {}, "model_trace": {}, "rawtrace": {}, "raw_trace": {},
		"rawtooltrace": {}, "raw_tool_trace": {}, "rawmodeltrace": {}, "raw_model_trace": {},
	}
	var walk func(any, string) error
	walk = func(current any, path string) error {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
				if _, forbidden := forbiddenKeys[normalized]; forbidden {
					return fmt.Errorf("customer projection exposes forbidden key %s.%s", path, key)
				}
				if err := walk(child, path+"."+key); err != nil {
					return err
				}
			}
		case []any:
			for index, child := range typed {
				if err := walk(child, fmt.Sprintf("%s[%d]", path, index)); err != nil {
					return err
				}
			}
		case string:
			for _, forbidden := range forbiddenValues {
				if forbidden != "" && strings.Contains(typed, forbidden) {
					return fmt.Errorf("customer projection exposes private fixture value at %s", path)
				}
			}
		}
		return nil
	}
	return walk(value, "response")
}
