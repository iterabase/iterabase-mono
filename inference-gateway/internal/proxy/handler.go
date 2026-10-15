package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/attribution"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/snapshot"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/upstreamauth"
)

// tpmBatchInterval is how often a stream's tokens are flushed to the limiter.
const tpmBatchInterval = 500 * time.Millisecond

// Central is an installation's route to the central Iterabase gateway
// (DES-HOR-661-03): its base URL and an mTLS client carrying the
// installation certificate.
type Central struct {
	URL    string
	Client *http.Client
}

// Handler is the installation gateway's proxy handler for OpenAI-compatible
// endpoints. It routes by alias (from the control-plane catalog snapshot) to a
// single backend, rewrites the model field to the backend's id, applies
// per-alias transforms, and forwards through the shared Forwarder. No weighted
// LB / health checker — the catalog's `available` flag is the health signal
// (set by the control-plane ModelBackend reconciler).
type Handler struct {
	cache     snapshot.Reader
	limiter   ratelimit.Limiter
	creds     upstreamauth.Resolver
	client    *http.Client
	central   *Central
	forwarder *Forwarder
	logger    *slog.Logger
}

// NewHandler creates a new proxy handler. creds resolves keyed external
// backends' credentials; when nil, requests to a keyed backend fail closed
// with upstream_auth_failed.
func NewHandler(cache snapshot.Reader, limiter ratelimit.Limiter, m *metrics.Metrics, creds upstreamauth.Resolver, logger *slog.Logger) *Handler {
	return &Handler{
		cache:     cache,
		limiter:   limiter,
		creds:     creds,
		client:    NewUpstreamClient(nil),
		forwarder: NewForwarder(m, logger),
		logger:    logger,
	}
}

// WithCentral routes managed models to the central gateway. Without it, a
// managed model fails closed with upstream_unavailable.
func (h *Handler) WithCentral(c *Central) *Handler {
	h.central = c
	return h
}

// ChatCompletions handles POST /v1/chat/completions.
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}
	defer r.Body.Close()

	var req chatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}

	identityID := middleware.IdentityIDFromContext(r.Context())

	// Workload (supervisor mTLS) path: the requested model must equal the
	// turn's durably-assigned model — deny model-mismatched assignments without
	// fallback (ARCH-010/REQ-010). The API-key path has no WorkloadContext and
	// skips this check, relying on capability authorization below.
	if wc, ok := middleware.WorkloadContextFromContext(r.Context()); ok {
		if req.Model != wc.AssignedModel {
			writeError(w, http.StatusForbidden,
				fmt.Sprintf("requested model '%s' does not match the assigned model for this turn", req.Model),
				"model_mismatch")
			return
		}
	}

	md := &middleware.MetricsData{Model: req.Model, Streaming: req.Stream}
	ctx := middleware.SetMetricsData(r.Context(), md)
	// Mutate r in place so outer middleware (Logging/Metrics) observe the
	// MetricsData when the request completes — a local reassignment would only
	// flow downstream and the completion log/metrics would miss model/streaming
	// (and, on the workload path, the run/turn outcome attribution).
	*r = *r.WithContext(ctx)

	// Capability check: broad-default wildcard allows all; model access is a
	// capability resource "model:<alias>" / "model:*" (v1 broad-default = all).
	if !h.allowsModel(identityID, req.Model) {
		writeError(w, http.StatusForbidden,
			fmt.Sprintf("identity is not allowed to access model '%s'", req.Model),
			"model_not_allowed")
		return
	}

	entry, ok := h.cache.CatalogEntry(req.Model)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("model '%s' not found", req.Model), "model_not_found")
		return
	}
	if !entry.Available {
		writeError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("model '%s' is not available", req.Model), "service_unavailable")
		return
	}

	target, ue := h.target(r.Context(), &entry, identityID)
	if ue != nil {
		h.forwarder.WriteUpstreamError(w, target, 0, ue, nil)
		return
	}
	md.BackendURL = target.URL

	body = ApplyRequestTransforms(body, &entry)
	// A managed model always reaches central under its central name.
	if (entry.Transforms.RewriteModelName || entry.BackendKind == backendKindManaged) && entry.BackendModelID != "" {
		body = rewriteModelInRequest(body, entry.BackendModelID)
	}

	h.forwarder.Forward(w, r, body, req.Stream, target, h.hooks(identityID, req.Stream))
}

// target resolves the upstream for a catalog entry. A keyed external backend
// gets its credential as the bearer token; a managed backend goes to central
// over mTLS with payload-free attribution and no bearer; nothing from the
// client's own Authorization header is ever forwarded. On error the returned
// target still labels the failure.
func (h *Handler) target(ctx context.Context, entry *snapshot.CatalogEntry, identityID string) (*Target, *upstreamError) {
	t := &Target{
		URL:           chatCompletionsURL(entry),
		Client:        h.client,
		Header:        http.Header{},
		ResponseModel: entry.ModelID,
		MetricsModel:  entry.ModelID,
		BackendRef:    entry.BackendRef,
	}
	switch {
	case entry.BackendKind == backendKindManaged:
		if h.central == nil || h.central.URL == "" || h.central.Client == nil {
			h.logger.Error("managed model requested but the central gateway is not configured",
				"model", entry.ModelID, "backend", entry.BackendRef)
			return t, upstreamUnavailable("")
		}
		t.URL = strings.TrimRight(h.central.URL, "/") + "/v1/chat/completions"
		t.Client = h.central.Client
		t.Header = requestAttribution(ctx, identityID).Header()
	case entry.BackendAuthRef != "":
		if h.creds == nil {
			h.logger.Error("upstream credential unavailable: no Kubernetes credential resolver",
				"model", entry.ModelID, "backend", entry.BackendRef)
			return t, upstreamAuthFailed("model provider credential is unavailable")
		}
		key, err := h.creds.Credential(ctx, entry.Namespace, entry.BackendAuthRef)
		if err != nil {
			// The error names the Secret, never its value.
			h.logger.Error("upstream credential unavailable", "model", entry.ModelID, "backend", entry.BackendRef, "error", err)
			return t, upstreamAuthFailed("model provider credential is unavailable")
		}
		t.Header.Set("Authorization", "Bearer "+key)
	}
	return t, nil
}

// requestAttribution is the payload-free attribution sent to central.
func requestAttribution(ctx context.Context, identityID string) attribution.Attribution {
	a := attribution.Attribution{
		RequestID:  middleware.RequestIDFromContext(ctx),
		IdentityID: identityID,
		Channel:    attribution.ChannelAPI,
	}
	if wc, ok := middleware.WorkloadContextFromContext(ctx); ok {
		a.RunID = wc.RunID
		a.TurnID = wc.TurnID
		a.WorkflowKey = wc.WorkflowKey
		a.WorkItemID = wc.WorkItemID
		a.Channel = attribution.Channel(wc.Channel)
	}
	return a
}

// hooks wires per-identity TPM accounting. The installation gateway's own
// accounting never fails a request (rate limits fail open).
func (h *Handler) hooks(identityID string, stream bool) Hooks {
	if h.limiter == nil || identityID == "" {
		return Hooks{}
	}
	if stream {
		batcher := ratelimit.NewTPMBatcher(h.limiter, identityID, tpmBatchInterval)
		batcher.Start()
		return Hooks{
			OnStreamTokens: batcher.Add,
			Settle: func(ctx context.Context, _ Outcome) error {
				batcher.Stop(ctx)
				return nil
			},
		}
	}
	return Hooks{Settle: func(ctx context.Context, o Outcome) error {
		if o.Usage != nil && o.Usage.TotalTokens > 0 {
			if err := h.limiter.IncrementTPM(ctx, identityID, o.Usage.TotalTokens); err != nil {
				h.logger.Error("failed to increment TPM", "identity_id", identityID, "error", err)
			}
		}
		return nil
	}}
}

// allowsModel reports whether the identity's capabilities grant access to the
// model. Wildcard '*' / 'model:*' allow any; 'model:<alias>' allows one.
func (h *Handler) allowsModel(identityID, model string) bool {
	for _, c := range h.cache.Capabilities(identityID) {
		if c.Resource == "*" || c.Resource == "model:*" {
			return true
		}
		if c.Resource == "model:"+model {
			return true
		}
	}
	return false
}

// ListModels handles GET /v1/models — returns available aliases from the snapshot.
func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	entries := h.cache.ListCatalog()
	data := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if !e.Available {
			continue
		}
		data = append(data, map[string]any{
			"id":       e.ModelID,
			"object":   "model",
			"created":  0,
			"owned_by": "inference-gateway",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data}); err != nil {
		h.logger.Error("failed to encode models response", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Request/Response helpers
// ---------------------------------------------------------------------------

type chatCompletionRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// ParseChatRequest extracts the model and stream flag from a chat request.
func ParseChatRequest(body []byte) (model string, stream bool, err error) {
	var req chatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", false, err
	}
	return req.Model, req.Stream, nil
}

// RewriteModelInRequest replaces the request's "model" field.
func RewriteModelInRequest(body []byte, modelID string) []byte {
	return rewriteModelInRequest(body, modelID)
}

// rewriteModelInRequest replaces the "model" field with the backend's HF id.
func rewriteModelInRequest(body []byte, modelID string) []byte {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	parsed["model"] = modelID
	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return rewritten
}

// rewriteModelInResponse replaces the "model" field with the alias so the client
// sees a consistent name.
func rewriteModelInResponse(body []byte, aliasName string) []byte {
	if aliasName == "" {
		return body
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	if _, ok := parsed["model"]; ok {
		parsed["model"] = aliasName
	}
	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return rewritten
}

// WriteError writes an OpenAI-compatible error envelope.
func WriteError(w http.ResponseWriter, status int, errType, message, code string) {
	writeTypedError(w, status, errType, message, code)
}

func writeError(w http.ResponseWriter, status int, message string, code string) {
	writeTypedError(w, status, "invalid_request_error", message, code)
}

func writeTypedError(w http.ResponseWriter, status int, errType, message, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}); err != nil {
		slog.Error("failed to write error response", "error", err)
	}
}
