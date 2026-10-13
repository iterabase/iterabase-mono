package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	artifactstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
)

// registerArtifactRoutes mounts the customer artifact APIs (architecture 9.3).
// Deletion is a personal current-Admin API action with no browser surface.
func (h *Handler) registerArtifactRoutes(r chi.Router) {
	h.route(r, http.MethodPost, "/v1/artifacts", action(identity.ActionArtifactsUpload), h.uploadArtifact)
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

// uploadArtifact accepts a raw streaming body. Content-Type is required;
// Content-Length and X-Artifact-SHA256 are optional integrity claims.
func (h *Handler) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.artifactPrincipal(w, r)
	if !ok {
		return
	}
	mimeType := r.Header.Get("Content-Type")
	var expectedSize *int64
	if r.ContentLength >= 0 {
		n := r.ContentLength
		expectedSize = &n
	}
	a, err := h.artifacts.Upload(r.Context(), artifactstore.UploadInput{
		SourceType: artifactstore.SourceUserUpload, SourceRef: r.Header.Get("X-Artifact-Source"),
		CreatedByIdentityID: principal.ActorID, MIMEType: mimeType, ExpectedSize: expectedSize,
		ExpectedDigest: r.Header.Get("X-Artifact-SHA256"),
	}, r.Body)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
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
	a, err := h.artifacts.Stat(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	if a.State != artifactstore.StateAvailable {
		writeArtifactError(w, artifactstore.ErrNotAvailable)
		return
	}
	setArtifactHeaders(w, a)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) deleteArtifact(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.artifactPrincipal(w, r); !ok {
		return
	}
	if _, err := h.artifacts.Delete(r.Context(), chi.URLParam(r, "id"), "admin_explicit"); err != nil {
		writeArtifactError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func setArtifactHeaders(w http.ResponseWriter, a artifactstore.Artifact) {
	w.Header().Set("Content-Type", a.MIMEType)
	w.Header().Set("X-Artifact-ID", a.ID)
	w.Header().Set("X-Artifact-State", a.State)
	if a.SizeBytes != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(*a.SizeBytes, 10))
	}
	if a.Digest != nil {
		w.Header().Set("X-Artifact-SHA256", *a.Digest)
	}
	if a.RetentionUntil != nil {
		w.Header().Set("X-Artifact-Retention-Until", a.RetentionUntil.Format("2006-01-02T15:04:05Z07:00"))
	}
}

func writeArtifactError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "artifact operation failed"
	switch {
	case errors.Is(err, artifactstore.ErrNotFound):
		status, message = http.StatusNotFound, "artifact not found"
	case errors.Is(err, artifactstore.ErrNotAvailable):
		status, message = http.StatusGone, "artifact is not available"
	case errors.Is(err, artifactstore.ErrUnauthorized):
		status, message = http.StatusForbidden, "artifact access denied"
	case errors.Is(err, artifactstore.ErrInvalidInput), errors.Is(err, artifactstore.ErrDigest), errors.Is(err, artifactstore.ErrSize):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, artifactstore.ErrTooLarge):
		status, message = http.StatusRequestEntityTooLarge, err.Error()
	}
	writeJSON(w, status, errorBody{Error: fmt.Sprint(message)})
}
