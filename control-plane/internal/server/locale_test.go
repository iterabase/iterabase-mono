package server_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withHeader(headers map[string]string, key, value string) map[string]string {
	out := map[string]string{key: value}
	for k, v := range headers {
		out[k] = v
	}
	return out
}

// TestLanguageResolutionOverHTTP covers DES-HOR-670-02: the effective locale
// follows the person's choice, then the organisation default, then the
// browser, then English — before and after sign-in.
func TestLanguageResolutionOverHTTP(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	user := h.activeUser("rita@example.com", "operator", "rita-long-password")
	session := h.signIn("rita@example.com", "rita-long-password", user)

	// Before sign-in, with no organisation default, the browser decides.
	anonymous := h.do(http.MethodGet, "/v1/auth/session", nil, map[string]string{"Accept-Language": "pt-PT,pt;q=0.9"})
	require.Equal(t, http.StatusOK, anonymous.status)
	assert.Equal(t, "pt", anonymous.body["locale"])
	anonymous = h.do(http.MethodGet, "/v1/auth/session", nil, map[string]string{"Accept-Language": "fr-FR"})
	assert.Equal(t, "en", anonymous.body["locale"])

	// Rita chose English at setup; clearing it follows the organisation.
	cleared := h.do(http.MethodPatch, "/v1/profile", map[string]any{"locale": ""}, withHeader(session.unsafeHeaders(), "Accept-Language", "pt-PT"))
	require.Equal(t, http.StatusOK, cleared.status, "%v", cleared.body)
	profile := cleared.body["profile"].(map[string]any)
	assert.Equal(t, "", profile["localePreference"])
	assert.Equal(t, "", profile["organizationLocale"])
	assert.Equal(t, "pt", profile["locale"], "no organisation default: the browser decides")

	invalid := h.do(http.MethodPatch, "/v1/profile", map[string]any{"locale": "es"}, session.unsafeHeaders())
	assert.Equal(t, http.StatusBadRequest, invalid.status)
	assert.Equal(t, "invalid_locale", invalid.body["code"])

	// An Admin sets the organisation default; it beats the browser.
	settings := h.do(http.MethodGet, "/v1/organization/settings", nil, admin.headers())
	require.Equal(t, http.StatusOK, settings.status, "%v", settings.body)
	assert.Equal(t, "", settings.body["defaultLocale"])
	updated := h.do(http.MethodPatch, "/v1/organization/settings",
		map[string]any{"defaultLocale": "en", "expectedUpdatedAt": settings.body["updatedAt"]}, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, updated.status, "%v", updated.body)
	assert.Equal(t, "en", updated.body["defaultLocale"])
	assert.Equal(t, admin.user.ID, updated.body["updatedBy"])

	current := h.do(http.MethodGet, "/v1/auth/session", nil, withHeader(session.headers(), "Accept-Language", "pt-PT"))
	require.Equal(t, http.StatusOK, current.status)
	assert.Equal(t, "en", current.body["locale"])
	profile = current.body["profile"].(map[string]any)
	assert.Equal(t, "en", profile["locale"])
	assert.Equal(t, "en", profile["organizationLocale"])
	// The probe rotates the session's CSRF proof.
	session.csrf, _ = current.body["csrfToken"].(string)
	anonymous = h.do(http.MethodGet, "/v1/auth/session", nil, map[string]string{"Accept-Language": "pt-PT"})
	assert.Equal(t, "en", anonymous.body["locale"], "the organisation default also applies before sign-in")

	// The person's own choice beats both.
	chosen := h.do(http.MethodPatch, "/v1/profile", map[string]any{"locale": "pt"}, session.unsafeHeaders())
	require.Equal(t, http.StatusOK, chosen.status, "%v", chosen.body)
	profile = chosen.body["profile"].(map[string]any)
	assert.Equal(t, "pt", profile["locale"])
	assert.Equal(t, "pt", profile["localePreference"])

	// A stale settings version is refused.
	stale := h.do(http.MethodPatch, "/v1/organization/settings",
		map[string]any{"defaultLocale": "pt", "expectedUpdatedAt": settings.body["updatedAt"]}, admin.unsafeHeaders())
	assert.Equal(t, http.StatusConflict, stale.status)
	assert.Equal(t, "settings_conflict", stale.body["code"])
	bad := h.do(http.MethodPatch, "/v1/organization/settings", map[string]any{"defaultLocale": "xx"}, admin.unsafeHeaders())
	assert.Equal(t, http.StatusBadRequest, bad.status)
}

func TestOrganizationSettingsAreAdminOnly(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	user := h.activeUser("rita@example.com", "operator", "rita-long-password")
	operator := h.signIn("rita@example.com", "rita-long-password", user)

	read := h.do(http.MethodGet, "/v1/organization/settings", nil, operator.headers())
	assert.Equal(t, http.StatusForbidden, read.status)
	write := h.do(http.MethodPatch, "/v1/organization/settings", map[string]any{"defaultLocale": "pt"}, operator.unsafeHeaders())
	assert.Equal(t, http.StatusForbidden, write.status)

	// CSRF is required for the Admin write.
	noCSRF := h.do(http.MethodPatch, "/v1/organization/settings", map[string]any{"defaultLocale": "pt"}, admin.headers())
	assert.Equal(t, http.StatusForbidden, noCSRF.status)

	anonymous := h.do(http.MethodGet, "/v1/organization/settings", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, anonymous.status)
}

// TestInstallationNameOverHTTP covers DES-HOR-459-08: an Admin sets the
// installation name (trimmed, at most 80 characters) alone or with the default
// language, an Operator cannot, and the session probe shows it before and after
// sign-in.
func TestInstallationNameOverHTTP(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	user := h.activeUser("rita@example.com", "operator", "rita-long-password")
	operator := h.signIn("rita@example.com", "rita-long-password", user)

	settings := h.do(http.MethodGet, "/v1/organization/settings", nil, admin.headers())
	require.Equal(t, http.StatusOK, settings.status, "%v", settings.body)
	assert.Equal(t, "", settings.body["name"])
	anonymous := h.do(http.MethodGet, "/v1/auth/session", nil, nil)
	require.Equal(t, http.StatusOK, anonymous.status)
	assert.NotContains(t, anonymous.body, "organizationName", "an unset name is omitted")

	named := h.do(http.MethodPatch, "/v1/organization/settings",
		map[string]any{"name": "  Exemplo Logística  ", "expectedUpdatedAt": settings.body["updatedAt"]}, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, named.status, "%v", named.body)
	assert.Equal(t, "Exemplo Logística", named.body["name"])
	assert.Equal(t, "", named.body["defaultLocale"], "a name-only update leaves the language alone")
	assert.Equal(t, admin.user.ID, named.body["updatedBy"])

	both := h.do(http.MethodPatch, "/v1/organization/settings",
		map[string]any{"name": "Exemplo", "defaultLocale": "pt"}, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, both.status, "%v", both.body)
	assert.Equal(t, "Exemplo", both.body["name"])
	assert.Equal(t, "pt", both.body["defaultLocale"])
	localeOnly := h.do(http.MethodPatch, "/v1/organization/settings", map[string]any{"defaultLocale": "en"}, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, localeOnly.status, "%v", localeOnly.body)
	assert.Equal(t, "Exemplo", localeOnly.body["name"], "a language-only update leaves the name alone")

	tooLong := h.do(http.MethodPatch, "/v1/organization/settings",
		map[string]any{"name": strings.Repeat("n", 81)}, admin.unsafeHeaders())
	assert.Equal(t, http.StatusBadRequest, tooLong.status)
	assert.Equal(t, "invalid_name", tooLong.body["code"])
	exact := h.do(http.MethodPatch, "/v1/organization/settings",
		map[string]any{"name": strings.Repeat("ã", 80)}, admin.unsafeHeaders())
	assert.Equal(t, http.StatusOK, exact.status, "80 characters is allowed")
	empty := h.do(http.MethodPatch, "/v1/organization/settings", map[string]any{}, admin.unsafeHeaders())
	assert.Equal(t, http.StatusBadRequest, empty.status)
	assert.Equal(t, "invalid_request", empty.body["code"])
	denied := h.do(http.MethodPatch, "/v1/organization/settings", map[string]any{"name": "Mine"}, operator.unsafeHeaders())
	assert.Equal(t, http.StatusForbidden, denied.status)

	restored := h.do(http.MethodPatch, "/v1/organization/settings", map[string]any{"name": "Exemplo"}, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, restored.status, "%v", restored.body)
	anonymous = h.do(http.MethodGet, "/v1/auth/session", nil, nil)
	assert.Equal(t, false, anonymous.body["authenticated"])
	assert.Equal(t, "Exemplo", anonymous.body["organizationName"], "the sign-in page shows the name")
	current := h.do(http.MethodGet, "/v1/auth/session", nil, operator.headers())
	require.Equal(t, http.StatusOK, current.status)
	assert.Equal(t, true, current.body["authenticated"])
	assert.Equal(t, "Exemplo", current.body["organizationName"])

	cleared := h.do(http.MethodPatch, "/v1/organization/settings", map[string]any{"name": " "}, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, cleared.status, "%v", cleared.body)
	assert.Equal(t, "", cleared.body["name"], "an empty name is allowed and unsets it")
}
