package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	artifactstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
)

// registerArtifactRoutes mounts the V2 customer artifact APIs (architecture
// section 13). Deletion is a personal current-Admin API action with no browser
// surface; security retry is browser-only.
func (h *Handler) registerArtifactRoutes(r chi.Router) {
	h.route(r, http.MethodPost, "/v1/artifacts", action(identity.ActionArtifactsUpload), h.uploadArtifact)
	h.route(r, http.MethodGet, "/v1/artifacts", action(identity.ActionArtifactsRead), h.listArtifacts)
	h.route(r, http.MethodGet, "/v1/artifacts/{id}/status", action(identity.ActionArtifactsRead), h.artifactStatus)
	h.route(r, http.MethodPost, "/v1/artifacts/{id}/security-retries", session(), h.retryArtifactSecurity)
	h.route(r, http.MethodGet, "/v1/artifacts/{id}", action(identity.ActionArtifactsRead), h.getArtifact)
	h.route(r, http.MethodHead, "/v1/artifacts/{id}", action(identity.ActionArtifactsRead), h.headArtifact)
	h.route(r, http.MethodDelete, "/v1/artifacts/{id}", bearerAction(identity.ActionArtifactsDelete), h.deleteArtifact)
}

// artifactPrincipal returns the principal requireAction resolved for the route.
func (h *Handler) artifactPrincipal(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	if h.artifacts == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "artifact service not configured"})
		return Principal{}, false
	}
	p, ok := principalFromContext(r.Context())
	if !ok {
		authError(w, http.StatusUnauthorized, "session_expired", "Your session expired. Sign in again to continue.")
		return Principal{}, false
	}
	return p, true
}

// uploadHandle is the provisional upload response (section 15). It has no
// canonical type, digest, or size and cannot be used as an artifact ref.
type uploadHandle struct {
	UploadID string `json:"uploadId"`
	State    string `json:"state"`
}

// uploadArtifact accepts one raw stream into quarantine. Required headers:
// Idempotency-Key and X-Artifact-Filename (percent-encoded UTF-8).
// Content-Type, Content-Length, and X-Artifact-SHA256 are advisory claims.
func (h *Handler) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.artifactPrincipal(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		artifactError(w, http.StatusBadRequest, "idempotency_key_required", "An Idempotency-Key header is required.")
		return
	}
	name, err := url.PathUnescape(r.Header.Get("X-Artifact-Filename"))
	if err != nil || strings.TrimSpace(name) == "" {
		artifactError(w, http.StatusBadRequest, "filename_required", "An X-Artifact-Filename header is required.")
		return
	}
	var expectedSize *int64
	if r.ContentLength >= 0 {
		if r.ContentLength > artifactstore.MaxSize {
			artifactError(w, http.StatusRequestEntityTooLarge, "file_too_large", "Files can be at most 25 MB.")
			return
		}
		n := r.ContentLength
		expectedSize = &n
	}
	source := artifactstore.AuthBrowser
	if principal.Source == SourceAPI {
		source = artifactstore.AuthAPI
	}
	a, err := h.artifacts.Admit(r.Context(), artifactstore.AdmitInput{
		SourceType: artifactstore.SourceUpload,
		Attribution: artifactstore.Attribution{
			InitiatingHumanID: principal.InitiatingHumanID, RequestActorID: principal.ActorID,
			Source: source, SessionID: principal.SessionID, APIKeyID: principal.APIKeyID,
		},
		DisplayName: name, ClaimedMIME: r.Header.Get("Content-Type"),
		ExpectedSize: expectedSize, ExpectedDigest: r.Header.Get("X-Artifact-SHA256"),
		IdempotencyKey: key, ContainerDepth: 1,
	}, r.Body)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/artifacts/"+a.UploadID+"/status")
	writeJSON(w, http.StatusAccepted, uploadHandle{UploadID: a.UploadID, State: a.Status()})
}

// artifactStatusBody is the independent, customer-safe state of one artifact.
type artifactStatusBody struct {
	UploadID      string               `json:"uploadId"`
	State         string               `json:"state"`
	Reason        string               `json:"reason,omitempty"`
	Retryable     bool                 `json:"retryable"`
	DisplayName   string               `json:"displayName,omitempty"`
	Source        string               `json:"source"`
	Artifact      *artifactstore.Ref   `json:"artifact,omitempty"`
	ParentUpload  string               `json:"parentUploadId,omitempty"`
	ChildKind     string               `json:"childKind,omitempty"`
	Children      []artifactStatusBody `json:"children,omitempty"`
	CreatedAt     time.Time            `json:"createdAt"`
	ClearedAt     *time.Time           `json:"clearedAt,omitempty"`
	RetentionTill *time.Time           `json:"retentionUntil,omitempty"`
}

func statusBody(a artifactstore.Artifact) artifactStatusBody {
	b := artifactStatusBody{UploadID: a.UploadID, State: a.Status(), Source: a.SourceType, CreatedAt: a.CreatedAt,
		ClearedAt: a.ClearedAt, RetentionTill: a.RetentionUntil}
	if a.DisplayName != nil {
		b.DisplayName = *a.DisplayName
	}
	if a.ChildKind != nil {
		b.ChildKind = *a.ChildKind
	}
	switch b.State {
	case artifactstore.StatusRejected, artifactstore.StatusSecurityCheckFailed:
		if a.SecurityReason != nil {
			b.Reason = *a.SecurityReason
		}
		b.Retryable = b.State == artifactstore.StatusSecurityCheckFailed
	}
	// The canonical ref is returned only once the artifact is usable.
	if a.Cleared() {
		if ref, err := a.Ref(); err == nil {
			b.Artifact = &ref
		}
	}
	return b
}

// canSeeUnregistered is the pre-registration predicate (section 14): a
// browser Admin, the same initiating human (browser or personal key), or the
// exact automation credential that uploaded it.
func canSeeUnregistered(p Principal, a artifactstore.Artifact) bool {
	sameHuman := a.InitiatingHumanID != nil && *a.InitiatingHumanID == p.InitiatingHumanID
	switch {
	case p.Source == SourceBrowser:
		return sameHuman || p.Role == RoleAdmin
	case p.CredentialKind == identity.CredentialKindAutomation:
		return a.APIKeyID != nil && *a.APIKeyID == p.APIKeyID
	default:
		return sameHuman
	}
}

// visibleArtifact resolves an id or upload handle under the shared/registered
// or pre-registration predicate. Invisible rows are indistinguishable from
// missing ones.
func (h *Handler) visibleArtifact(w http.ResponseWriter, r *http.Request, p Principal) (artifactstore.Artifact, bool) {
	a, err := h.artifacts.Status(r.Context(), chi.URLParam(r, "id"))
	if err != nil || a.StorageState == artifactstore.StorageUploading || (!a.Registered() && !canSeeUnregistered(p, a)) {
		if err != nil && !errors.Is(err, artifactstore.ErrNotFound) {
			writeArtifactError(w, err)
			return a, false
		}
		artifactError(w, http.StatusNotFound, "not_found", "This file was not found.")
		return a, false
	}
	return a, true
}

func (h *Handler) artifactStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := h.artifactPrincipal(w, r)
	if !ok {
		return
	}
	a, ok := h.visibleArtifact(w, r, p)
	if !ok {
		return
	}
	body := statusBody(a)
	if a.ParentID != nil {
		if parent, err := h.artifacts.Stat(r.Context(), *a.ParentID); err == nil {
			body.ParentUpload = parent.UploadID
		}
	}
	children, err := h.artifacts.Store().Children(r.Context(), a.ID)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	for _, c := range children {
		if c.StorageState != artifactstore.StorageUploading {
			body.Children = append(body.Children, statusBody(c))
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, body)
}

// artifactListItem is one registered artifact in the shared library.
type artifactListItem struct {
	Artifact    artifactstore.Ref `json:"artifact"`
	UploadID    string            `json:"uploadId"`
	DisplayName string            `json:"displayName"`
	Source      string            `json:"source"`
	State       string            `json:"state"`
	Uploader    string            `json:"uploader,omitempty"`
	Snippet     string            `json:"snippet,omitempty"`
	CreatedAt   time.Time         `json:"createdAt"`
	ClearedAt   *time.Time        `json:"clearedAt,omitempty"`
}

type artifactListBody struct {
	Items      []artifactListItem `json:"items"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

// listCursor is the opaque pagination position: a keyset for recent order or
// an offset for ranked search. A stale cursor never authorizes a row; every
// page re-applies current eligibility.
type listCursor struct {
	Search string    `json:"s,omitempty"`
	Offset int       `json:"o,omitempty"`
	After  time.Time `json:"t,omitempty"`
	AfterI string    `json:"i,omitempty"`
}

// parseListQuery validates search, limit, and the opaque cursor.
func parseListQuery(r *http.Request) (artifactstore.ListQuery, string, string) {
	q := artifactstore.ListQuery{Search: strings.TrimSpace(r.URL.Query().Get("search")), Limit: 50}
	if len(q.Search) > 256 {
		return q, "invalid_search", "Search text is too long."
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return q, "invalid_limit", "limit must be between 1 and 100."
		}
		q.Limit = n
	}
	if v := r.URL.Query().Get("cursor"); v != "" {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		var c listCursor
		if err != nil || json.Unmarshal(raw, &c) != nil || c.Search != q.Search || c.Offset < 0 || c.Offset > 10_000 {
			return q, "invalid_cursor", "The cursor is not valid for this query."
		}
		q.Offset = c.Offset
		if !c.After.IsZero() {
			q.AfterTime, q.AfterID = &c.After, c.AfterI
		}
	}
	return q, "", ""
}

func (h *Handler) listArtifacts(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.artifactPrincipal(w, r); !ok {
		return
	}
	q, code, message := parseListQuery(r)
	if code != "" {
		artifactError(w, http.StatusBadRequest, code, message)
		return
	}
	items, err := h.artifacts.List(r.Context(), q)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	body := artifactListBody{Items: make([]artifactListItem, 0, len(items))}
	for _, it := range items {
		ref, err := it.Artifact.Ref()
		if err != nil {
			continue
		}
		body.Items = append(body.Items, artifactListItem{Artifact: ref, UploadID: it.Artifact.UploadID,
			DisplayName: deref(it.Artifact.DisplayName), Source: it.Artifact.SourceType, State: it.Artifact.Status(),
			Uploader: it.Uploader, Snippet: it.Snippet, CreatedAt: it.Artifact.CreatedAt, ClearedAt: it.Artifact.ClearedAt})
	}
	body.NextCursor = nextCursor(q, items)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, body)
}

// nextCursor encodes the position after a full page, or "" at the end.
func nextCursor(q artifactstore.ListQuery, items []artifactstore.ListItem) string {
	if len(items) < q.Limit {
		return ""
	}
	next := listCursor{Search: q.Search}
	if q.Search != "" {
		next.Offset = q.Offset + len(items)
	} else if last := items[len(items)-1].Artifact; last.RegisteredAt != nil {
		next.After, next.AfterI = *last.RegisteredAt, last.ID
	}
	raw, _ := json.Marshal(next)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (h *Handler) getArtifact(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.artifactPrincipal(w, r); !ok {
		return
	}
	a, body, err := h.artifacts.Open(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	defer body.Close()
	setArtifactHeaders(w, a)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		// Response status is already committed; cancellation/truncation is the
		// only safe signal. Never replace a partial artifact with JSON.
		return
	}
}

func (h *Handler) headArtifact(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.artifactPrincipal(w, r); !ok {
		return
	}
	a, _, err := h.artifacts.Store().ClearedOriginal(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	setArtifactHeaders(w, a)
	w.WriteHeader(http.StatusOK)
}

// retryArtifactSecurity creates one new security trigger after a transient
// failure. It is browser-only; the initiating human or a current Admin may
// retry.
func (h *Handler) retryArtifactSecurity(w http.ResponseWriter, r *http.Request) {
	if h.artifacts == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "artifact service not configured"})
		return
	}
	p, ok := sessionPrincipalFromContext(r.Context())
	if !ok {
		authError(w, http.StatusUnauthorized, "session_expired", "Your session expired. Sign in again to continue.")
		return
	}
	a, ok := h.visibleArtifact(w, r, p)
	if !ok {
		return
	}
	if !canSeeUnregistered(p, a) {
		artifactError(w, http.StatusNotFound, "not_found", "This file was not found.")
		return
	}
	a, err := h.artifacts.RetrySecurity(r.Context(), a.ID, p.ActorID, p.SessionID)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, statusBody(a))
}

// deleteArtifact starts the controlled deletion lifecycle. The byte purge is
// asynchronous; the response reports the deleting or deleted tombstone.
func (h *Handler) deleteArtifact(w http.ResponseWriter, r *http.Request) {
	p, ok := h.artifactPrincipal(w, r)
	if !ok {
		return
	}
	a, err := h.artifacts.RequestDelete(r.Context(), artifactstore.DeleteRequest{
		ArtifactID: chi.URLParam(r, "id"), Reason: artifactstore.DeletionAdmin,
		ActorID: p.ActorID, APIKeyID: p.APIKeyID,
	})
	var inUse *artifactstore.InUseError
	if errors.As(err, &inUse) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "This file is used by active work and cannot be deleted yet.", "code": "artifact_in_use",
			"workItemIds": inUse.WorkItemIDs,
		})
		return
	}
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, statusBody(a))
}

func setArtifactHeaders(w http.ResponseWriter, a artifactstore.Artifact) {
	w.Header().Set("Content-Type", a.MIMEType())
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": deref(a.DisplayName)}))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Artifact-ID", a.ID)
	if a.SizeBytes != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(*a.SizeBytes, 10))
	}
	if a.Digest != nil {
		w.Header().Set("X-Artifact-SHA256", *a.Digest)
	}
	if a.RetentionUntil != nil {
		w.Header().Set("X-Artifact-Retention-Until", a.RetentionUntil.Format(time.RFC3339))
	}
}

// artifactError writes a stable, customer-safe coded artifact error.
func artifactError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, authErrorBody{Error: message, Code: code})
}

func writeArtifactError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, artifactstore.ErrNotFound), errors.Is(err, artifactstore.ErrNotAvailable):
		// Quarantined, rejected, withdrawn, and deleted bytes are not
		// distinguishable from missing ones on a byte route.
		artifactError(w, http.StatusNotFound, "not_found", "This file was not found.")
	case errors.Is(err, artifactstore.ErrUnauthorized):
		artifactError(w, http.StatusForbidden, "forbidden", "This is not available with your current access.")
	case errors.Is(err, artifactstore.ErrDigest), errors.Is(err, artifactstore.ErrSize):
		artifactError(w, http.StatusBadRequest, "integrity_mismatch", "The uploaded bytes do not match the declared size or digest.")
	case errors.Is(err, artifactstore.ErrInvalidInput):
		artifactError(w, http.StatusBadRequest, "invalid_upload", "The upload request is not valid.")
	case errors.Is(err, artifactstore.ErrTooLarge):
		artifactError(w, http.StatusRequestEntityTooLarge, "file_too_large", "Files can be at most 25 MB.")
	case errors.Is(err, artifactstore.ErrConflict):
		artifactError(w, http.StatusConflict, "idempotency_conflict", "This Idempotency-Key was already used for a different file.")
	case errors.Is(err, artifactstore.ErrUploadInFlight):
		artifactError(w, http.StatusConflict, "upload_in_progress", "This upload is still in progress.")
	case errors.Is(err, artifactstore.ErrNotRetryable):
		artifactError(w, http.StatusConflict, "not_retryable", "This file's security check cannot be retried.")
	case errors.Is(err, artifactstore.ErrCapacity):
		artifactError(w, http.StatusServiceUnavailable, "processing_capacity_unavailable", "Uploads are temporarily unavailable. Try again shortly.")
	default:
		artifactError(w, http.StatusInternalServerError, "artifact_failed", "The file operation failed. Try again shortly.")
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
