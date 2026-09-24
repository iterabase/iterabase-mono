package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
)

// People administration (architecture 9.3): verified pending access requests,
// the current account directory, role change, disable/re-enable, and session
// revocation. Every route is cookie-session only, re-reads current server
// authority, and mutates only with a current Admin plus CSRF and recent
// password proof. Bearer authentication is refused even when a valid cookie is
// also presented (requireBrowser).
func (h *Handler) registerPeopleRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.requireBrowser, h.requireAuthorityV2, h.requireAdmin)
		r.Get("/v1/people", h.listPeople)
		r.Get("/v1/people/{id}", h.getPerson)

		r.Group(func(r chi.Router) {
			r.Use(h.requireCSRF, h.requireRecentAuth)
			r.Post("/v1/people/{id}/role", h.changePersonRole)
			r.Post("/v1/people/{id}/disable", h.disablePerson)
			r.Post("/v1/people/{id}/enable", h.enablePerson)
			r.Post("/v1/people/{id}/sessions/revoke", h.revokePersonSessions)
		})
	})
}

type personResponse struct {
	ID               string     `json:"id"`
	Email            string     `json:"email"`
	DisplayName      string     `json:"displayName"`
	Role             string     `json:"role"`
	Locale           string     `json:"locale"`
	Status           string     `json:"status"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	RoleChangedAt    *time.Time `json:"roleChangedAt,omitempty"`
	AccountChangedAt *time.Time `json:"accountChangedAt,omitempty"`
}

type peopleResponse struct {
	People          []personResponse        `json:"people"`
	PendingRequests []accessRequestResponse `json:"pendingRequests"`
}

func toPersonResponse(p identity.Person) personResponse {
	return personResponse{
		ID: p.IdentityID, Email: p.Email, DisplayName: p.DisplayName, Role: p.Role,
		Locale: p.Locale, Status: p.Status, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		RoleChangedAt: p.RoleChangedAt, AccountChangedAt: p.AccountChangedAt,
	}
}

// listPeople returns the current directory plus verified pending requests. It
// reads current server authority; a hidden navigation entry is never the
// control.
func (h *Handler) listPeople(w http.ResponseWriter, r *http.Request) {
	people, err := h.store.ListPeople(r.Context())
	if err != nil {
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	pending, err := h.store.ListPendingAccessRequests(r.Context())
	if err != nil {
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	out := peopleResponse{People: make([]personResponse, 0, len(people)), PendingRequests: make([]accessRequestResponse, 0, len(pending))}
	for _, p := range people {
		out.People = append(out.People, toPersonResponse(p))
	}
	for _, request := range pending {
		out.PendingRequests = append(out.PendingRequests, accessRequestResponse{
			ID: request.ID, Email: request.Email, VerifiedAt: request.VerifiedAt, CreatedAt: request.CreatedAt,
		})
	}
	authWrite(w, http.StatusOK, out)
}

// getPerson returns one person by identity id. An unknown id is a bounded
// not-found that leaks no directory detail.
func (h *Handler) getPerson(w http.ResponseWriter, r *http.Request) {
	person, err := h.store.GetPerson(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, identity.ErrNotFound) {
		authError(w, http.StatusNotFound, "not_found", "This person is no longer available.")
		return
	}
	if err != nil {
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	authWrite(w, http.StatusOK, toPersonResponse(person))
}

// changePersonRole promotes or demotes exactly one person. Promotion never adds
// an action to an existing credential; demotion suspends the approved
// credential classes and revokes browser sessions.
func (h *Handler) changePersonRole(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	var req struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &req); err != nil {
		authError(w, http.StatusBadRequest, "invalid_request", "Choose Operator or Admin.")
		return
	}
	person, cons, err := h.store.ChangePersonRole(r.Context(), session.IdentityID, chi.URLParam(r, "id"), req.Role, h.authCfg.now())
	if h.writePeopleError(w, err) {
		return
	}
	authWrite(w, http.StatusOK, map[string]any{
		"person":               toPersonResponse(person),
		"sessionsRevoked":      cons.SessionsRevoked,
		"credentialsSuspended": cons.CredentialsSuspended,
	})
}

func (h *Handler) disablePerson(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	person, cons, err := h.store.DisablePerson(r.Context(), session.IdentityID, chi.URLParam(r, "id"), h.authCfg.now())
	if h.writePeopleError(w, err) {
		return
	}
	authWrite(w, http.StatusOK, map[string]any{
		"person":               toPersonResponse(person),
		"sessionsRevoked":      cons.SessionsRevoked,
		"credentialsSuspended": cons.CredentialsSuspended,
	})
}

func (h *Handler) enablePerson(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	person, err := h.store.EnablePerson(r.Context(), session.IdentityID, chi.URLParam(r, "id"), h.authCfg.now())
	if h.writePeopleError(w, err) {
		return
	}
	authWrite(w, http.StatusOK, map[string]any{
		"person":               toPersonResponse(person),
		"sessionsRevoked":      0,
		"credentialsSuspended": 0,
		"credentialsResumed":   0,
	})
}

func (h *Handler) revokePersonSessions(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	revoked, err := h.store.RevokePersonSessions(r.Context(), session.IdentityID, chi.URLParam(r, "id"), h.authCfg.now())
	if h.writePeopleError(w, err) {
		return
	}
	authWrite(w, http.StatusOK, map[string]any{"sessionsRevoked": revoked})
}

// writePeopleError maps People mutation failures to bounded states and returns
// true when it wrote a response.
func (h *Handler) writePeopleError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, identity.ErrNotFound):
		authError(w, http.StatusNotFound, "not_found", "This person is no longer available.")
	case errors.Is(err, identity.ErrLastAdmin):
		authError(w, http.StatusConflict, "last_admin",
			"At least one active Admin must remain. Promote another person first.")
	case errors.Is(err, identity.ErrInvalidRole):
		authError(w, http.StatusBadRequest, "invalid_role", "Choose Operator or Admin.")
	case errors.Is(err, identity.ErrPersonStateChanged):
		authError(w, http.StatusConflict, "state_changed", "This person changed. Refresh to see the current state.")
	default:
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
	}
	return true
}
