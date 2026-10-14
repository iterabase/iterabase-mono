package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
	workstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/work"
)

// maxWorkStartBodyBytes bounds the full work-item start request body before any
// parsing, so an oversized or malformed start is rejected without starting
// durable work (HOR-425). Source-specific tighter bounds are enforced inside
// work.Store.Start.
const maxWorkStartBodyBytes = 4 << 20 // 4 MiB

func (h *Handler) workReady(w http.ResponseWriter) bool {
	if h.work == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "work service not configured"})
		return false
	}
	return true
}

// registerWorkRoutes mounts the customer work APIs (architecture 9.3). Reads
// share `work.read`; a direct start is `workflows.start`; saving feedback is
// `work.feedback.write`. Blocker responses and revisions can confirm
// consequential invocations, so they accept only a cookie browser session.
func (h *Handler) registerWorkRoutes(r chi.Router) {
	read := action(identity.ActionWorkRead)
	h.route(r, http.MethodGet, "/v1/work-items", read, h.listWorkItems)
	h.route(r, http.MethodPost, "/v1/work-items", action(identity.ActionWorkflowsStart), h.startWorkItem)
	h.route(r, http.MethodGet, "/v1/work-items/{id}", read, h.getWorkItem)
	h.route(r, http.MethodGet, "/v1/work-items/{id}/attempts", read, h.listWorkAttempts)
	h.route(r, http.MethodGet, "/v1/work-attempts/{id}/nodes", read, h.listWorkNodes)
	h.route(r, http.MethodGet, "/v1/work-items/{id}/timeline", read, h.listWorkTimeline)
	h.route(r, http.MethodGet, "/v1/work-items/{id}/artifacts", read, h.listWorkArtifacts)
	h.route(r, http.MethodGet, "/v1/work-items/{id}/consequences", read, h.getWorkConsequences)
	h.route(r, http.MethodGet, "/v1/work-items/{id}/blocker", read, h.getWorkBlocker)
	h.route(r, http.MethodGet, "/v1/work-items/{id}/feedback", read, h.listWorkFeedback)
	h.route(r, http.MethodGet, "/v1/work-items/{id}/feedback/{feedbackID}", read, h.getWorkFeedback)
	h.route(r, http.MethodPost, "/v1/work-items/{id}/feedback", action(identity.ActionWorkFeedbackWrite), h.saveWorkFeedback)
	h.route(r, http.MethodPost, "/v1/work-items/{id}/revisions", session(), h.createWorkRevision)
	h.route(r, http.MethodPost, "/v1/work-blockers/{id}/responses", session(), h.respondWorkBlocker)
	h.route(r, http.MethodPost, "/v1/work-items/{id}/wait/stop", session(), h.stopWorkWait)
	h.route(r, http.MethodGet, "/v1/work-dashboard", read, h.getWorkDashboard)
	h.route(r, http.MethodGet, "/v1/work-events", read, h.streamWorkEvents)
}

// workPrincipal returns the authenticated principal for a work route. Action
// routes carry it from requireAction; cookie-only routes derive it from the
// resolved browser session.
func (h *Handler) workPrincipal(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	if !h.workReady(w) {
		return Principal{}, false
	}
	if p, ok := principalFromContext(r.Context()); ok {
		return p, true
	}
	if p, ok := sessionPrincipalFromContext(r.Context()); ok {
		return p, true
	}
	authError(w, http.StatusUnauthorized, "session_expired", "Your session expired. Sign in again to continue.")
	return Principal{}, false
}

// clipWorkItem removes value data unless the principal currently holds the
// Admin-only `value.read` action (architecture 9.3).
func clipWorkItem(p Principal, item *workstore.WorkItem) {
	if p.Can(identity.ActionValueRead) {
		return
	}
	item.ValueConfigured = false
	item.ValueModel = nil
	item.EstimatedValue = nil
	item.ValueCurrency = nil
	item.ValueDisputed = false
}

type startWorkRequest struct {
	WorkflowKey        string                       `json:"workflowKey"`
	WorkflowVersion    string                       `json:"workflowVersion,omitempty"`
	Title              string                       `json:"title"`
	Source             json.RawMessage              `json:"source"`
	SourcePresentation workstore.SourcePresentation `json:"sourcePresentation"`
	ArtifactRefs       []workstore.ArtifactRef      `json:"artifactRefs,omitempty"`
}

func (h *Handler) startWorkItem(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.workPrincipal(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "Idempotency-Key is required"})
		return
	}
	var req startWorkRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkStartBodyBytes)
	if err := decodeJSON(r, &req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{Error: fmt.Sprintf("request body exceeds %d bytes", maxWorkStartBodyBytes)})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid request body"})
		return
	}
	item, created, err := h.work.Start(r.Context(), workstore.StartInput{
		// The three principals come only from the authenticated boundary
		// (architecture 7.15); the request body cannot supply them.
		ActorIdentityID:           principal.ActorID,
		InitiatingHumanIdentityID: principal.InitiatingHumanID,
		AuthorizationSource:       principal.Source,
		AuthorizationAPIKeyID:     principal.APIKeyID,
		AuthorizationSessionID:    principal.SessionID,
		WorkflowKey:               req.WorkflowKey,
		WorkflowVersion:           req.WorkflowVersion,
		IdempotencyKey:            key,
		Title:                     req.Title,
		Source:                    req.Source,
		SourcePresentation:        req.SourcePresentation,
		ArtifactRefs:              req.ArtifactRefs,
	})
	if err != nil {
		writeWorkError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	clipWorkItem(principal, &item)
	writeJSON(w, status, item)
}

func (h *Handler) listWorkItems(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.workPrincipal(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	filter := workstore.WorkItemFilter{
		State: r.URL.Query().Get("state"), Search: r.URL.Query().Get("search"),
		WorkflowKey: r.URL.Query().Get("workflowKey"), Limit: limit,
	}
	for key, target := range map[string]**time.Time{"from": &filter.From, "to": &filter.To} {
		if value := r.URL.Query().Get(key); value != "" {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, errorBody{Error: key + " must be RFC3339"})
				return
			}
			*target = &parsed
		}
	}
	items, err := h.work.ListWorkItems(r.Context(), filter)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	for i := range items {
		clipWorkItem(principal, &items[i])
	}
	writeJSON(w, http.StatusOK, items)
}
func (h *Handler) getWorkItem(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.workPrincipal(w, r)
	if !ok {
		return
	}
	item, err := h.work.GetWorkItem(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	clipWorkItem(principal, &item)
	writeJSON(w, http.StatusOK, item)
}
func (h *Handler) listWorkAttempts(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.workPrincipal(w, r); !ok {
		return
	}
	items, err := h.work.ListAttempts(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}
func (h *Handler) listWorkNodes(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.workPrincipal(w, r); !ok {
		return
	}
	nodes, err := h.work.ListNodeExecutions(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}
func (h *Handler) listWorkArtifacts(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.workPrincipal(w, r); !ok {
		return
	}
	artifacts, err := h.work.ListArtifacts(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, artifacts)
}

// valueEvent reports whether a timeline event carries Admin-only value data
// (REQ-026/027): value credits and deductions.
func valueEvent(event workstore.TimelineEvent) bool {
	return strings.HasPrefix(event.Code, "value_")
}

func (h *Handler) listWorkTimeline(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.workPrincipal(w, r)
	if !ok {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	itemID := chi.URLParam(r, "id")
	if principal.Can(identity.ActionValueRead) {
		events, err := h.work.ListTimeline(r.Context(), itemID, after, limit)
		if err != nil {
			writeWorkError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, events)
		return
	}
	// Without value.read, value events are omitted. Keep reading past them so
	// a page is only short when the timeline is exhausted, and every returned
	// cursor stays a valid resume point.
	events := []workstore.TimelineEvent{}
	for len(events) < limit {
		page, err := h.work.ListTimeline(r.Context(), itemID, after, limit)
		if err != nil {
			writeWorkError(w, err)
			return
		}
		for _, event := range page {
			after = event.Cursor
			if valueEvent(event) {
				continue
			}
			events = append(events, event)
			if len(events) == limit {
				break
			}
		}
		if len(page) < limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, events)
}
func (h *Handler) getWorkConsequences(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.workPrincipal(w, r); !ok {
		return
	}
	consequences, err := h.work.ConsequencesForItem(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, consequences)
}

func (h *Handler) getWorkBlocker(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.workPrincipal(w, r); !ok {
		return
	}
	blocker, err := h.work.OpenBlockerForItem(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, blocker)
}

type blockerResponseRequest struct {
	Outcome                string                  `json:"outcome"`
	Response               json.RawMessage         `json:"response"`
	ArtifactRefs           []workstore.ArtifactRef `json:"artifactRefs,omitempty"`
	ConfirmedInvocationIDs []string                `json:"confirmedInvocationIds,omitempty"`
}

func (h *Handler) respondWorkBlocker(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.workPrincipal(w, r)
	if !ok {
		return
	}
	var req blockerResponseRequest
	if decodeJSON(r, &req) != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid request body"})
		return
	}
	b, err := h.work.RespondBlocker(r.Context(), workstore.BlockerResponseInput{BlockerID: chi.URLParam(r, "id"), ActorIdentityID: principal.ActorID, Outcome: req.Outcome, Response: req.Response, ArtifactRefs: req.ArtifactRefs, ConfirmedInvocationIDs: req.ConfirmedInvocationIDs})
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// stopWorkWait ends the item's open wait through its `cancelled` outcome
// (DES-HOR-649-02). It carries exactly the blocker-response authority, a
// cookie session with CSRF (DES-HOR-649-06), and does not cancel the work item.
func (h *Handler) stopWorkWait(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.workPrincipal(w, r)
	if !ok {
		return
	}
	item, err := h.work.StopWaiting(r.Context(), workstore.StopWaitingInput{WorkItemID: chi.URLParam(r, "id"), ActorIdentityID: principal.ActorID})
	if err != nil {
		writeWorkError(w, err)
		return
	}
	clipWorkItem(principal, &item)
	writeJSON(w, http.StatusOK, item)
}

func (h *Handler) listWorkFeedback(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.workPrincipal(w, r); !ok {
		return
	}
	feedback, err := h.work.ListFeedback(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, feedback)
}

func (h *Handler) getWorkFeedback(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.workPrincipal(w, r); !ok {
		return
	}
	feedback, err := h.work.GetFeedback(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "feedbackID"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, feedback)
}

type feedbackRequest struct {
	AttemptID       string          `json:"attemptId"`
	Category        string          `json:"category"`
	Explanation     string          `json:"explanation,omitempty"`
	CorrectedResult json.RawMessage `json:"correctedResult,omitempty"`
}

func (h *Handler) saveWorkFeedback(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.workPrincipal(w, r)
	if !ok {
		return
	}
	var req feedbackRequest
	if decodeJSON(r, &req) != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid request body"})
		return
	}
	f, err := h.work.SaveFeedback(r.Context(), workstore.FeedbackInput{WorkItemID: chi.URLParam(r, "id"), AttemptID: req.AttemptID, ActorIdentityID: principal.ActorID, Category: req.Category, Explanation: req.Explanation, CorrectedResult: req.CorrectedResult})
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

type revisionRequest struct {
	FeedbackID             string   `json:"feedbackId"`
	ActionableGuidance     string   `json:"actionableGuidance"`
	ConfirmedInvocationIDs []string `json:"confirmedInvocationIds,omitempty"`
}

func (h *Handler) createWorkRevision(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.workPrincipal(w, r)
	if !ok {
		return
	}
	var req revisionRequest
	if decodeJSON(r, &req) != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid request body"})
		return
	}
	item, err := h.work.CreateRevision(r.Context(), workstore.RevisionInput{WorkItemID: chi.URLParam(r, "id"), ActorIdentityID: principal.ActorID, FeedbackID: req.FeedbackID, ActionableGuidance: req.ActionableGuidance, ConfirmedInvocationIDs: req.ConfirmedInvocationIDs})
	if err != nil {
		writeWorkError(w, err)
		return
	}
	clipWorkItem(principal, &item)
	writeJSON(w, http.StatusCreated, item)
}

func (h *Handler) getWorkDashboard(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.workPrincipal(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -7)
	to := now.Add(time.Second)
	if v := r.URL.Query().Get("from"); v != "" {
		if parsed, err := time.Parse(time.RFC3339, v); err == nil {
			from = parsed
		} else {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "from must be RFC3339"})
			return
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if parsed, err := time.Parse(time.RFC3339, v); err == nil {
			to = parsed
		} else {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "to must be RFC3339"})
			return
		}
	}
	summary, err := h.work.Dashboard(r.Context(), from, to)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	if !principal.Can(identity.ActionValueRead) {
		summary.Value = workstore.ValueSummary{Totals: []workstore.CurrencyAmount{}, Models: []workstore.ValueModelSnapshot{}}
		summary.Trend = []workstore.ValueTrendPoint{}
	}
	writeJSON(w, http.StatusOK, summary)
}

// streamWorkEvents is a resumable customer-safe SSE stream. Last-Event-ID is
// the durable timeline cursor; polling is a fallback over the same contract.
func (h *Handler) streamWorkEvents(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.workPrincipal(w, r)
	if !ok {
		return
	}
	valueVisible := principal.Can(identity.ActionValueRead)
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "streaming unsupported"})
		return
	}
	cursor, _ := strconv.ParseInt(strings.TrimSpace(r.Header.Get("Last-Event-ID")), 10, 64)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	poll := time.NewTicker(time.Second)
	heartbeat := time.NewTicker(15 * time.Second)
	defer poll.Stop()
	defer heartbeat.Stop()
	send := func() error {
		next, err := h.writeWorkEvents(r.Context(), w, cursor, valueVisible)
		cursor = next
		if err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	if err := send(); err != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-poll.C:
			if send() != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeWorkEvents writes every timeline event after cursor as SSE frames and
// returns the new cursor. Without value.read, value events stay server-side
// but the cursor still moves past them so a resume never replays them.
func (h *Handler) writeWorkEvents(ctx context.Context, w http.ResponseWriter, cursor int64, valueVisible bool) (int64, error) {
	for {
		events, err := h.work.TimelineSince(ctx, cursor, 500)
		if err != nil {
			return cursor, err
		}
		for _, event := range events {
			cursor = event.Cursor
			if !valueVisible && valueEvent(event) {
				continue
			}
			data, _ := json.Marshal(event)
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Cursor, event.Code, data); err != nil {
				return cursor, err
			}
		}
		if len(events) < 500 {
			return cursor, nil
		}
	}
}

func writeWorkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workstore.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorBody{Error: err.Error()})
	case errors.Is(err, workstore.ErrConflict), errors.Is(err, workstore.ErrConfirmation):
		writeJSON(w, http.StatusConflict, errorBody{Error: err.Error()})
	case errors.Is(err, workstore.ErrInvalidInput), errors.Is(err, workstore.ErrInvalidTransition):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "work operation failed"})
	}
}
