package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const workspaceBarrierTimeout = 2 * time.Minute

var (
	requests         atomic.Int64
	cancelled        atomic.Int64
	capacityWaiting  atomic.Int64
	capacityRelease  = make(chan struct{})
	capacityOnce     sync.Once
	workspaceBarrier = newWorkspaceTwoPartyBarrier(workspaceBarrierTimeout)
	uuidRE           = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	bashRE           = regexp.MustCompile(`E2E_BASH:([A-Za-z0-9+/=]+)`)
	participantRE    = regexp.MustCompile(`E2E_WORKSPACE_PARTICIPANT:([a-z0-9-]+)`)
	attemptRE        = regexp.MustCompile(`"attemptId"\s*:\s*"([^"]+)"`)
	childProofRE     = regexp.MustCompile(`workspace-child-proof participant=([a-z0-9-]+) session=([A-Za-z0-9._-]+) uid=([0-9]+) gid=([0-9]+) marker_sha256=([a-f0-9]{64}) invariants=groups-cleared,caps-cleared,no-new-privs,umask-0077,pool-root-0:0:0711,session-tree-0700,sibling-EACCES,tls-key-EACCES`)
)

type completionRequest struct {
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	Stream   bool             `json:"stream"`
}

type workspaceExpectedParticipant struct {
	Participant string `json:"participant"`
	WorkItemID  string `json:"work_item_id"`
	AttemptID   string `json:"attempt_id"`
	SessionID   string `json:"session_id"`
}

type workspaceBarrierArrival struct {
	Participant  string `json:"participant"`
	WorkItemID   string `json:"work_item_id"`
	AttemptID    string `json:"attempt_id"`
	SessionID    string `json:"session_id"`
	UID          uint32 `json:"uid"`
	GID          uint32 `json:"gid"`
	MarkerSHA256 string `json:"marker_sha256"`
}

type workspaceBarrierStatus struct {
	State    string                         `json:"state"`
	Ready    bool                           `json:"ready"`
	Failure  string                         `json:"failure,omitempty"`
	Expected []workspaceExpectedParticipant `json:"expected"`
	Arrivals []workspaceBarrierArrival      `json:"arrivals"`
}

type workspaceBarrierConfiguration struct {
	Participants []workspaceExpectedParticipant `json:"participants"`
}

type workspaceTwoPartyBarrier struct {
	mu           sync.Mutex
	timeout      time.Duration
	configured   bool
	released     bool
	failure      string
	expected     map[string]workspaceExpectedParticipant
	arrivals     map[string]workspaceBarrierArrival
	configuredCh chan struct{}
	releasedCh   chan struct{}
	failedCh     chan struct{}
}

func newWorkspaceTwoPartyBarrier(timeout time.Duration) *workspaceTwoPartyBarrier {
	return &workspaceTwoPartyBarrier{
		timeout: timeout, expected: map[string]workspaceExpectedParticipant{}, arrivals: map[string]workspaceBarrierArrival{},
		configuredCh: make(chan struct{}), releasedCh: make(chan struct{}), failedCh: make(chan struct{}),
	}
}

func (b *workspaceTwoPartyBarrier) configure(participants []workspaceExpectedParticipant) error {
	if len(participants) != 2 {
		return b.fail(fmt.Sprintf("workspace barrier requires exactly two expected participants (got %d)", len(participants)))
	}
	seenParticipant := map[string]bool{}
	seenWork := map[string]bool{}
	seenAttempt := map[string]bool{}
	seenSession := map[string]bool{}
	for _, participant := range participants {
		if participant.Participant == "" || participant.WorkItemID == "" || participant.AttemptID == "" || participant.SessionID == "" {
			return b.fail("workspace barrier expected identity is incomplete")
		}
		if seenParticipant[participant.Participant] || seenWork[participant.WorkItemID] || seenAttempt[participant.AttemptID] || seenSession[participant.SessionID] {
			return b.fail("workspace barrier expected identities must be pairwise distinct")
		}
		seenParticipant[participant.Participant] = true
		seenWork[participant.WorkItemID] = true
		seenAttempt[participant.AttemptID] = true
		seenSession[participant.SessionID] = true
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failure != "" {
		return errors.New(b.failure)
	}
	if b.configured {
		return errors.New("workspace barrier is already configured")
	}
	for _, participant := range participants {
		b.expected[participant.Participant] = participant
	}
	b.configured = true
	close(b.configuredCh)
	return nil
}

func (b *workspaceTwoPartyBarrier) waitConfigured(ctx context.Context) error {
	b.mu.Lock()
	if b.failure != "" {
		err := errors.New(b.failure)
		b.mu.Unlock()
		return err
	}
	if b.configured {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	timer := time.NewTimer(b.timeout)
	defer timer.Stop()
	select {
	case <-b.configuredCh:
		return nil
	case <-b.failedCh:
		return errors.New(b.status().Failure)
	case <-ctx.Done():
		return b.fail("workspace barrier request ended before participant configuration")
	case <-timer.C:
		return b.fail("workspace barrier participant configuration timed out")
	}
}

func (b *workspaceTwoPartyBarrier) arriveAndWait(ctx context.Context, arrival workspaceBarrierArrival) error {
	if err := b.waitConfigured(ctx); err != nil {
		return err
	}

	b.mu.Lock()
	if err := b.acceptArrivalLocked(&arrival); err != nil {
		b.mu.Unlock()
		return err
	}
	b.mu.Unlock()

	timer := time.NewTimer(b.timeout)
	defer timer.Stop()
	select {
	case <-b.releasedCh:
		return nil
	case <-b.failedCh:
		return errors.New(b.status().Failure)
	case <-ctx.Done():
		return b.fail(fmt.Sprintf("workspace barrier participant %q disconnected before release", arrival.Participant))
	case <-timer.C:
		return b.fail(b.timeoutFailure())
	}
}

func (b *workspaceTwoPartyBarrier) acceptArrivalLocked(arrival *workspaceBarrierArrival) error {
	if b.failure != "" {
		return errors.New(b.failure)
	}
	expected, ok := b.expected[arrival.Participant]
	if !ok {
		b.failLocked(fmt.Sprintf("workspace barrier received unattributed participant %q", arrival.Participant))
		return errors.New(b.failure)
	}
	if _, duplicate := b.arrivals[arrival.Participant]; duplicate {
		b.failLocked(fmt.Sprintf("workspace barrier received duplicate participant %q", arrival.Participant))
		return errors.New(b.failure)
	}
	if arrival.AttemptID != expected.AttemptID || arrival.SessionID != expected.SessionID {
		b.failLocked(fmt.Sprintf("workspace barrier identity mismatch for participant %q: expected attempt/session %s/%s, got %s/%s",
			arrival.Participant, expected.AttemptID, expected.SessionID, arrival.AttemptID, arrival.SessionID))
		return errors.New(b.failure)
	}
	if arrival.UID == 0 || arrival.UID != arrival.GID || arrival.MarkerSHA256 == "" {
		b.failLocked(fmt.Sprintf("workspace barrier child identity is invalid for participant %q: uid=%d gid=%d marker_present=%t",
			arrival.Participant, arrival.UID, arrival.GID, arrival.MarkerSHA256 != ""))
		return errors.New(b.failure)
	}
	for _, existing := range b.arrivals {
		if existing.UID == arrival.UID {
			b.failLocked(fmt.Sprintf("workspace barrier child UID collision: participants %q and %q both reported %d", existing.Participant, arrival.Participant, arrival.UID))
			return errors.New(b.failure)
		}
		if existing.MarkerSHA256 == arrival.MarkerSHA256 {
			b.failLocked(fmt.Sprintf("workspace barrier marker collision: participants %q and %q reported the same digest", existing.Participant, arrival.Participant))
			return errors.New(b.failure)
		}
	}
	arrival.WorkItemID = expected.WorkItemID
	b.arrivals[arrival.Participant] = *arrival
	return nil
}

func (b *workspaceTwoPartyBarrier) release() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failure != "" {
		return errors.New(b.failure)
	}
	if !b.configured || len(b.arrivals) != len(b.expected) || len(b.expected) != 2 {
		return errors.New("workspace barrier cannot release before both expected participants are held")
	}
	if b.released {
		return nil
	}
	b.released = true
	close(b.releasedCh)
	return nil
}

func (b *workspaceTwoPartyBarrier) timeoutFailure() string {
	status := b.status()
	arrived := map[string]bool{}
	for _, arrival := range status.Arrivals {
		arrived[arrival.Participant] = true
	}
	missing := make([]string, 0, len(status.Expected))
	for _, expected := range status.Expected {
		if !arrived[expected.Participant] {
			missing = append(missing, expected.Participant)
		}
	}
	if len(missing) > 0 {
		return "workspace barrier timed out with missing participants: " + strings.Join(missing, ",")
	}
	return "workspace barrier timed out while both participants awaited validated release"
}

func (b *workspaceTwoPartyBarrier) fail(message string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failLocked(message)
	return errors.New(b.failure)
}

func (b *workspaceTwoPartyBarrier) failLocked(message string) {
	if b.failure != "" {
		return
	}
	b.failure = message
	close(b.failedCh)
}

func (b *workspaceTwoPartyBarrier) status() workspaceBarrierStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	status := workspaceBarrierStatus{Failure: b.failure}
	for _, expected := range b.expected {
		status.Expected = append(status.Expected, expected)
	}
	for _, arrival := range b.arrivals {
		status.Arrivals = append(status.Arrivals, arrival)
	}
	sort.Slice(status.Expected, func(i, j int) bool { return status.Expected[i].Participant < status.Expected[j].Participant })
	sort.Slice(status.Arrivals, func(i, j int) bool { return status.Arrivals[i].Participant < status.Arrivals[j].Participant })
	status.Ready = b.failure == "" && b.configured && len(b.expected) == 2 && len(b.arrivals) == 2
	switch {
	case b.failure != "":
		status.State = "failed"
	case b.released:
		status.State = "released"
	case status.Ready:
		status.State = "ready"
	case b.configured:
		status.State = "waiting"
	default:
		status.State = "configuration_pending"
	}
	return status
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/stats", stats)
	mux.HandleFunc("/workspace/barrier", workspaceBarrierStatusHandler)
	mux.HandleFunc("/workspace/barrier/configure", configureWorkspaceBarrier)
	mux.HandleFunc("/workspace/barrier/release", releaseWorkspaceBarrier)
	mux.HandleFunc("/release/capacity", func(w http.ResponseWriter, _ *http.Request) {
		capacityOnce.Do(func() { close(capacityRelease) })
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1/chat/completions", completions)
	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("deterministic E2E model listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func stats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int64{
		"requests": requests.Load(), "cancelled": cancelled.Load(), "capacity_waiting": capacityWaiting.Load(),
	})
}

func workspaceBarrierStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(workspaceBarrier.status())
}

func configureWorkspaceBarrier(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var configuration workspaceBarrierConfiguration
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&configuration); err != nil {
		http.Error(w, "invalid workspace barrier configuration", http.StatusBadRequest)
		return
	}
	if err := workspaceBarrier.configure(configuration.Participants); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func releaseWorkspaceBarrier(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := workspaceBarrier.release(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func completions(w http.ResponseWriter, r *http.Request) {
	requests.Add(1)
	var input completionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	transcriptBytes, _ := json.Marshal(input.Messages)
	transcript := string(transcriptBytes)
	if !waitForFixtureBoundary(w, r, input.Messages, transcript) {
		return
	}

	choice := nextChoice(transcript)
	if !input.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "fixture-completion", "object": "chat.completion", "model": input.Model,
			"choices": []any{map[string]any{
				"index": 0, "message": map[string]any{"role": "assistant", "content": "deterministic completion"}, "finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	frame := map[string]any{"id": "fixture-completion", "object": "chat.completion.chunk", "model": input.Model, "choices": []any{choice}}
	data, _ := json.Marshal(frame)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
}

func waitForFixtureBoundary(w http.ResponseWriter, r *http.Request, messages []map[string]any, transcript string) bool {
	if strings.Contains(transcript, "E2E_SLOW") {
		select {
		case <-r.Context().Done():
			cancelled.Add(1)
			return false
		case <-time.After(30 * time.Second):
		}
	}
	currentTurn := currentTurnTranscript(transcript)
	if strings.Contains(currentTurn, "E2E_MODE:workspace-barrier") {
		if err := workspaceBarrier.waitConfigured(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return false
		}
		if strings.Contains(currentTurn, `"role":"tool"`) && !strings.Contains(currentTurn, "Workflow step completion recorded") {
			arrival, err := parseWorkspaceBarrierArrival(messages)
			if err != nil {
				_ = workspaceBarrier.fail("workspace barrier could not attribute child proof: " + err.Error())
				http.Error(w, workspaceBarrier.status().Failure, http.StatusConflict)
				return false
			}
			if err := workspaceBarrier.arriveAndWait(r.Context(), arrival); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return false
			}
		}
	}
	if strings.Contains(currentTurn, `"role":"tool"`) {
		return true
	}
	if strings.Contains(currentTurn, "E2E_MODE:capacity-active") {
		capacityWaiting.Add(1)
		defer capacityWaiting.Add(-1)
		select {
		case <-capacityRelease:
		case <-r.Context().Done():
			cancelled.Add(1)
			return false
		case <-time.After(2 * time.Minute):
			http.Error(w, "capacity release timeout", http.StatusGatewayTimeout)
			return false
		}
	}
	return true
}

func parseWorkspaceBarrierArrival(messages []map[string]any) (workspaceBarrierArrival, error) {
	plain := messageStrings(messages)
	participantMatches := participantRE.FindAllStringSubmatch(plain, -1)
	proofMatches := childProofRE.FindAllStringSubmatch(plain, -1)
	attemptMatches := attemptRE.FindAllStringSubmatch(plain, -1)
	if len(participantMatches) == 0 || len(proofMatches) == 0 || len(attemptMatches) == 0 {
		return workspaceBarrierArrival{}, errors.New("participant, attempt, or child identity is missing")
	}
	participant := participantMatches[len(participantMatches)-1][1]
	proof := proofMatches[len(proofMatches)-1]
	if proof[1] != participant {
		return workspaceBarrierArrival{}, errors.New("prompt and child participant markers disagree")
	}
	attempt := attemptMatches[len(attemptMatches)-1][1]
	uid64, uidErr := strconv.ParseUint(proof[3], 10, 32)
	gid64, gidErr := strconv.ParseUint(proof[4], 10, 32)
	if uidErr != nil || gidErr != nil {
		return workspaceBarrierArrival{}, errors.New("child UID/GID is invalid")
	}
	return workspaceBarrierArrival{
		Participant: participant, AttemptID: attempt, SessionID: proof[2], UID: uint32(uid64), GID: uint32(gid64), MarkerSHA256: proof[5],
	}, nil
}

func messageStrings(messages []map[string]any) string {
	var values []string
	var collect func(any)
	collect = func(value any) {
		switch typed := value.(type) {
		case string:
			values = append(values, typed)
		case []any:
			for _, child := range typed {
				collect(child)
			}
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				collect(typed[key])
			}
		}
	}
	for _, message := range messages {
		collect(message)
	}
	return strings.Join(values, "\n")
}

func currentTurnTranscript(transcript string) string {
	if index := strings.LastIndex(transcript, "E2E_MODE:"); index >= 0 {
		return transcript[index:]
	}
	return transcript
}

func nextChoice(transcript string) map[string]any {
	currentTurn := currentTurnTranscript(transcript)
	if strings.Contains(currentTurn, "Workflow step completion recorded") {
		return textChoice("Deterministic workflow step complete.")
	}
	toolResults := strings.Count(currentTurn, `"role":"tool"`)
	if strings.Contains(currentTurn, "E2E_MODE:outcome-unknown") && toolResults > 0 {
		return textChoice("The write outcome is unknown; do not repeat it.")
	}
	if toolResults == 0 {
		if choice := initialToolChoice(currentTurn); choice != nil {
			return choice
		}
	}

	artifactID := ""
	if strings.Contains(currentTurn, "artifact_refs") {
		if matches := uuidRE.FindAllString(currentTurn, -1); len(matches) > 0 {
			// The latest tool result is appended after assignment/run context. Its
			// published artifact is therefore the final UUID in the transcript.
			artifactID = matches[len(matches)-1]
		}
	}
	args := map[string]any{
		"outcome": "completed",
		"summary": "Deterministic deployed execution completed.",
		"output":  map[string]any{"result": "verified"},
	}
	if artifactID != "" {
		args["artifact_refs"] = []any{map[string]any{"artifact_id": artifactID, "role": "evidence", "metadata": map[string]any{"label": "Deterministic evidence"}}}
	}
	encoded, _ := json.Marshal(args)
	return toolChoice("fixture-complete", "complete_step", string(encoded))
}

func initialToolChoice(currentTurn string) map[string]any {
	switch {
	case strings.Contains(currentTurn, "E2E_MODE:read-artifact"):
		return toolChoice("fixture-read", "platform.fixture_read", `{"message":"produce attributable evidence"}`)
	case strings.Contains(currentTurn, "E2E_MODE:product-diagnostic"):
		return toolChoice("product-diagnostic", "platform.diagnostic", `{"message":"release-versioned product layer"}`)
	case strings.Contains(currentTurn, "E2E_MODE:barrier"):
		return toolChoice("fixture-barrier", "platform.fixture_barrier", `{}`)
	case strings.Contains(currentTurn, "E2E_MODE:consequence"):
		return toolChoice("fixture-write", "platform.fixture_write", `{"target":"synthetic-record","mode":"success"}`)
	case strings.Contains(currentTurn, "E2E_MODE:outcome-unknown"):
		return toolChoice("fixture-write-unknown", "platform.fixture_write", `{"target":"ambiguous-record","mode":"crash"}`)
	case strings.Contains(currentTurn, "E2E_MODE:idempotent-race"):
		return duplicateToolChoice("fixture-upsert", "platform.fixture_upsert", `{"record":"same-logical-write"}`)
	case strings.Contains(currentTurn, "E2E_MODE:isolation"),
		strings.Contains(currentTurn, "E2E_MODE:workspace-barrier"),
		strings.Contains(currentTurn, "E2E_MODE:capacity-active"):
		match := bashRE.FindStringSubmatch(currentTurn)
		command := "printf isolation-default"
		if len(match) == 2 {
			if decoded, err := base64.StdEncoding.DecodeString(match[1]); err == nil {
				command = string(decoded)
			}
		}
		args, _ := json.Marshal(map[string]string{"command": command})
		return toolChoice("fixture-bash", "bash", string(args))
	default:
		return nil
	}
}

func duplicateToolChoice(id, name, arguments string) map[string]any {
	calls := make([]any, 0, 2)
	for index := range 2 {
		calls = append(calls, map[string]any{
			"index": index, "id": id, "type": "function",
			"function": map[string]string{"name": name, "arguments": arguments},
		})
	}
	return map[string]any{
		"index":         0,
		"delta":         map[string]any{"role": "assistant", "tool_calls": calls},
		"finish_reason": "tool_calls",
	}
}

func textChoice(content string) map[string]any {
	return map[string]any{
		"index":         0,
		"delta":         map[string]any{"role": "assistant", "content": content},
		"finish_reason": "stop",
	}
}

func toolChoice(id, name, arguments string) map[string]any {
	return map[string]any{
		"index": 0,
		"delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"index": 0, "id": id, "type": "function", "function": map[string]string{"name": name, "arguments": arguments},
		}}},
		"finish_reason": "tool_calls",
	}
}

func init() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	_ = os.Setenv("TZ", "UTC")
}
