package server

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/workflow"
)

// registerWorkflowRoutes mounts the customer-safe workflow catalogue
// (HOR-459). Like work reads, `workflows.read` is held by every Operator and
// Admin session and may be granted to a V2 bearer credential.
func (h *Handler) registerWorkflowRoutes(r chi.Router) {
	read := action(identity.ActionWorkflowsRead)
	h.route(r, http.MethodGet, "/v1/workflows", read, h.listWorkflows)
	h.route(r, http.MethodGet, "/v1/workflows/{key}", read, h.getWorkflow)
}

type workflowList struct {
	Workflows []workflow.Summary `json:"workflows"`
}

func (h *Handler) workflowsReady(w http.ResponseWriter) bool {
	if h.workflows == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "workflow service not configured"})
		return false
	}
	return true
}

func (h *Handler) listWorkflows(w http.ResponseWriter, r *http.Request) {
	if !h.workflowsReady(w) {
		return
	}
	summaries, err := h.workflows.ListReadySummaries(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "workflow read failed"})
		return
	}
	writeJSON(w, http.StatusOK, workflowList{Workflows: summaries})
}

func (h *Handler) getWorkflow(w http.ResponseWriter, r *http.Request) {
	if !h.workflowsReady(w) {
		return
	}
	// Workflow keys may contain '/', so clients percent-encode the key as one
	// path segment (for example default%2Fquotes).
	key, err := url.PathUnescape(chi.URLParam(r, "key"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "workflow not found"})
		return
	}
	summary, err := h.workflows.GetReadySummary(r.Context(), key)
	switch {
	case errors.Is(err, workflow.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorBody{Error: "workflow not found"})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "workflow read failed"})
	default:
		writeJSON(w, http.StatusOK, summary)
	}
}
