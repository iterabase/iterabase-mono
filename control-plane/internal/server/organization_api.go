package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
)

// Organisation settings (DES-HOR-670-02): the deployment-wide default
// language. Reads and writes are Admin-only cookie-session routes mounted with
// the other Admin routes in registerAuthRoutes; writes also need CSRF and are
// logged with the Admin's identity.

type organizationSettingsResponse struct {
	// DefaultLocale is empty until an Admin chooses one; people who follow the
	// organisation then fall back to their browser language.
	DefaultLocale string    `json:"defaultLocale"`
	UpdatedBy     string    `json:"updatedBy,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func toOrganizationSettingsResponse(settings identity.OrganizationSettings) organizationSettingsResponse {
	return organizationSettingsResponse{
		DefaultLocale: settings.DefaultLocale,
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
		DefaultLocale     string     `json:"defaultLocale"`
		ExpectedUpdatedAt *time.Time `json:"expectedUpdatedAt,omitempty"`
	}
	if err := decodeJSON(r, &req); err != nil {
		authError(w, http.StatusBadRequest, "invalid_request", "Enter the requested details.")
		return
	}
	settings, err := h.store.UpdateOrganizationDefaultLocale(r.Context(), session.IdentityID, req.DefaultLocale, req.ExpectedUpdatedAt, h.authCfg.now())
	switch {
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
