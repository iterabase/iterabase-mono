package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
)

// Organisation settings (DES-HOR-670-02, DES-HOR-459-08): the deployment-wide
// default language and the installation name. Reads and writes are Admin-only cookie-session routes mounted with
// the other Admin routes in registerAuthRoutes; writes also need CSRF and are
// logged with the Admin's identity.

type organizationSettingsResponse struct {
	// DefaultLocale is empty until an Admin chooses one; people who follow the
	// organisation then fall back to their browser language.
	DefaultLocale string `json:"defaultLocale"`
	// Name is the installation name; empty until seeded or set by an Admin.
	Name      string    `json:"name"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func toOrganizationSettingsResponse(settings identity.OrganizationSettings) organizationSettingsResponse {
	return organizationSettingsResponse{
		DefaultLocale: settings.DefaultLocale,
		Name:          settings.Name,
		UpdatedBy:     settings.UpdatedByIdentityID,
		UpdatedAt:     settings.UpdatedAt,
	}
}

func (h *Handler) getOrganizationSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := h.store.GetOrganizationSettings(r.Context())
	if err != nil {
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	authWrite(w, http.StatusOK, toOrganizationSettingsResponse(settings))
}

func (h *Handler) updateOrganizationSettings(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	var req struct {
		DefaultLocale     *string    `json:"defaultLocale,omitempty"`
		Name              *string    `json:"name,omitempty"`
		ExpectedUpdatedAt *time.Time `json:"expectedUpdatedAt,omitempty"`
	}
	if err := decodeJSON(r, &req); err != nil {
		authError(w, http.StatusBadRequest, "invalid_request", "Enter the requested details.")
		return
	}
	update := identity.OrganizationSettingsUpdate{DefaultLocale: req.DefaultLocale, Name: req.Name}
	settings, err := h.store.UpdateOrganizationSettings(r.Context(), session.IdentityID, update, req.ExpectedUpdatedAt, h.authCfg.now())
	switch {
	case errors.Is(err, identity.ErrEmptyOrganizationSettingsUpdate):
		authError(w, http.StatusBadRequest, "invalid_request", "Enter the requested details.")
	case errors.Is(err, identity.ErrInvalidOrganizationName):
		authError(w, http.StatusBadRequest, "invalid_name", "Use a name of at most 80 characters.")
	case errors.Is(err, identity.ErrInvalidLocale):
		authError(w, http.StatusBadRequest, "invalid_locale", "Choose English or Portuguese.")
	case errors.Is(err, identity.ErrOrganizationSettingsConflict):
		authError(w, http.StatusConflict, "settings_conflict", "These settings changed elsewhere. Reload to see the current values.")
	case err != nil:
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
	default:
		authWrite(w, http.StatusOK, toOrganizationSettingsResponse(settings))
	}
}
