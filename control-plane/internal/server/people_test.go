package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
)

// cutoverAuthority flips the installation to the V2 authority epoch with the
// operator evidence the preflight requires.
func (h *authAPIHarness) cutoverAuthority(t *testing.T) {
	t.Helper()
	_, err := h.store.CutoverAuthority(context.Background(), identity.CutoverOptions{
		Operator:          "ops@example.com",
		Release:           "test-release",
		BackupEvidence:    "rehearsal-2026-09-24",
		RehearsalEvidence: "rehearsal-2026-09-24",
		Manifest:          identity.CutoverManifest{DefaultRPM: 60, DefaultTPM: 60000, DefaultExpiryDays: 30},
		Now:               h.now,
	})
	require.NoError(t, err)
}

// TestPeopleAdministrationRequiresTheV2Epoch proves the People surface fails
// closed before the epoch and is reachable after it, through current server
// authority only.
func TestPeopleAdministrationRequiresTheV2Epoch(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")

	before := h.do(http.MethodGet, "/v1/people", nil, admin.headers())
	assert.Equal(t, http.StatusServiceUnavailable, before.status)
	assert.Equal(t, "authority_migration_pending", before.body["code"])

	h.cutoverAuthority(t)

	after := h.do(http.MethodGet, "/v1/people", nil, admin.headers())
	require.Equal(t, http.StatusOK, after.status, "body: %v", after.body)
	people, ok := after.body["people"].([]any)
	require.True(t, ok, "people array is required")
	require.Len(t, people, 1)
	first, _ := people[0].(map[string]any)
	assert.Equal(t, "admin@example.com", first["email"])
	assert.Equal(t, "admin", first["role"])
	assert.Equal(t, "active", first["status"])
}

// TestPeopleIsAdminCookieOnly proves Operator sessions and bearer credentials
// cannot read or mutate People, even with a valid cookie present.
func TestPeopleIsAdminCookieOnly(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	operator := h.activeUser("operator@example.com", "operator", "operator-long-password")
	operatorSession := h.signIn("operator@example.com", "operator-long-password", operator)
	h.cutoverAuthority(t)

	response := h.do(http.MethodGet, "/v1/people", nil, operatorSession.headers())
	assert.Equal(t, http.StatusForbidden, response.status, "an Operator must not read People")

	// A bearer key alongside the Admin cookie is still refused.
	_, key, err := h.store.CreateAPIKey(context.Background(), admin.user.ID, "machine", identity.ScopeAdmin, nil)
	require.NoError(t, err)
	headers := admin.headers()
	headers["Authorization"] = "Bearer " + key.Prefix
	response = h.do(http.MethodGet, "/v1/people", nil, headers)
	assert.Equal(t, http.StatusUnauthorized, response.status)
	assert.Equal(t, "bearer_not_accepted", response.body["code"])

	// A bearer-only caller is refused before any People data is produced.
	response = h.do(http.MethodGet, "/v1/people", nil, map[string]string{"Authorization": "Bearer " + key.Prefix})
	assert.Equal(t, http.StatusUnauthorized, response.status)
}

// TestPeopleMutationRequiresCSRFAndRecentAuth proves the security mutations are
// recent-auth bound and that the consequences are reported from server state.
func TestPeopleMutationRequiresCSRFAndRecentAuth(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	target := h.activeUser("person@example.com", "operator", "person-long-password")
	h.cutoverAuthority(t)

	// A safe-method read needs no recent proof.
	require.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/people", nil, admin.headers()).status)

	// An unsafe mutation without CSRF is refused.
	noCSRF := h.do(http.MethodPost, "/v1/people/"+target.ID+"/role", map[string]any{"role": "admin"}, admin.headers())
	assert.Equal(t, http.StatusForbidden, noCSRF.status)

	// With CSRF but no recent password proof, the mutation is refused.
	stale := h.do(http.MethodPost, "/v1/people/"+target.ID+"/role", map[string]any{"role": "admin"}, admin.unsafeHeaders())
	assert.Equal(t, http.StatusForbidden, stale.status)
	assert.Equal(t, "recent_auth_required", stale.body["code"])

	h.reauthenticate(admin, "admin-long-password")
	promoted := h.do(http.MethodPost, "/v1/people/"+target.ID+"/role", map[string]any{"role": "admin"}, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, promoted.status, "body: %v", promoted.body)
	person, _ := promoted.body["person"].(map[string]any)
	assert.Equal(t, "admin", person["role"])

	// Promotion is idempotent and reports no invented consequence.
	again := h.do(http.MethodPost, "/v1/people/"+target.ID+"/role", map[string]any{"role": "admin"}, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, again.status)
	assert.EqualValues(t, 0, again.body["credentialsSuspended"])

	// An invalid role is rejected before any state changes.
	invalid := h.do(http.MethodPost, "/v1/people/"+target.ID+"/role", map[string]any{"role": "owner"}, admin.unsafeHeaders())
	assert.Equal(t, http.StatusBadRequest, invalid.status)
	assert.Equal(t, "invalid_role", invalid.body["code"])

	// Disable then re-enable never resumes credentials, and the re-enable path
	// reports zero resumed.
	disabled := h.do(http.MethodPost, "/v1/people/"+target.ID+"/disable", nil, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, disabled.status, "body: %v", disabled.body)
	disabledPerson, _ := disabled.body["person"].(map[string]any)
	assert.Equal(t, "disabled", disabledPerson["status"])

	enabled := h.do(http.MethodPost, "/v1/people/"+target.ID+"/enable", nil, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, enabled.status, "body: %v", enabled.body)
	assert.EqualValues(t, 0, enabled.body["credentialsResumed"])

	// Revoking another person's sessions is bounded to a count.
	revoked := h.do(http.MethodPost, "/v1/people/"+target.ID+"/sessions/revoke", nil, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, revoked.status)
	assert.EqualValues(t, 0, revoked.body["sessionsRevoked"])
}

// TestPeopleNeverLeavesZeroActiveAdmins proves the last-active-Admin invariant
// is enforced by the product API and durable in the audit trail.
func TestPeopleNeverLeavesZeroActiveAdmins(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	h.cutoverAuthority(t)
	h.reauthenticate(admin, "admin-long-password")

	demote := h.do(http.MethodPost, "/v1/people/"+admin.user.ID+"/role", map[string]any{"role": "operator"}, admin.unsafeHeaders())
	require.Equal(t, http.StatusConflict, demote.status)
	assert.Equal(t, "last_admin", demote.body["code"])

	disable := h.do(http.MethodPost, "/v1/people/"+admin.user.ID+"/disable", nil, admin.unsafeHeaders())
	assert.Equal(t, http.StatusConflict, disable.status)
	assert.Equal(t, "last_admin", disable.body["code"])

	// The refusal is durable evidence and the Admin keeps its authority.
	var denied int
	require.NoError(t, h.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM identity.security_events
		WHERE event = $1 AND outcome = $2`, identity.EventLastAdminMutationDenied, identity.OutcomeDenied).Scan(&denied))
	assert.GreaterOrEqual(t, denied, 2)

	stillAdmin := h.do(http.MethodGet, "/v1/profile", nil, admin.headers())
	require.Equal(t, http.StatusOK, stillAdmin.status)
	profile, _ := stillAdmin.body["profile"].(map[string]any)
	assert.Equal(t, "admin", profile["role"])
}

// TestPeopleRoleChangeRevokesTargetSessions proves a role change takes effect
// on the target's next request rather than in previously issued sessions.
func TestPeopleRoleChangeRevokesTargetSessions(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	target := h.activeUser("person@example.com", "operator", "person-long-password")
	targetSession := h.signIn("person@example.com", "person-long-password", target)
	h.cutoverAuthority(t)
	h.reauthenticate(admin, "admin-long-password")

	require.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/profile", nil, targetSession.headers()).status)

	promoted := h.do(http.MethodPost, "/v1/people/"+target.ID+"/role", map[string]any{"role": "admin"}, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, promoted.status, "body: %v", promoted.body)
	assert.GreaterOrEqual(t, promoted.body["sessionsRevoked"], float64(1), "the target must sign in again under the new role")

	after := h.do(http.MethodGet, "/v1/profile", nil, targetSession.headers())
	assert.Equal(t, http.StatusUnauthorized, after.status, "the revoked session must not authorize")
	assert.Equal(t, "session_expired", after.body["code"])
}

// TestEnableReportsNoResumedCredentials keeps the "re-enable never resumes"
// contract explicit at the API boundary.
func TestEnableReportsNoResumedCredentials(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	target := h.activeUser("person@example.com", "operator", "person-long-password")
	h.cutoverAuthority(t)
	h.reauthenticate(admin, "admin-long-password")

	require.Equal(t, http.StatusOK, h.do(http.MethodPost, "/v1/people/"+target.ID+"/disable", nil, admin.unsafeHeaders()).status)
	enabled := h.do(http.MethodPost, "/v1/people/"+target.ID+"/enable", nil, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, enabled.status)
	assert.EqualValues(t, 0, enabled.body["credentialsResumed"])

	var suspended int
	require.NoError(t, h.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM identity.api_keys
		WHERE owner_user_identity_id = $1 AND status = 'suspended'`, target.ID).Scan(&suspended))
	assert.Zero(t, suspended)

	// An unknown person is a bounded not-found.
	missing := h.do(http.MethodGet, "/v1/people/00000000-0000-0000-0000-000000000000", nil, admin.headers())
	assert.Equal(t, http.StatusNotFound, missing.status)
}
