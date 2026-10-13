package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
)

// TestPeopleReadIsAdminOnlyAndMutationsAreCookieOnly proves the directory is
// the Admin-only `people.read` action (Admin session or personal Admin
// credential), that Operators cannot read it through any credential, and that
// a bearer credential can never mutate People, even beside a valid cookie.
func TestPeopleReadIsAdminOnlyAndMutationsAreCookieOnly(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	operator := h.activeUser("operator@example.com", "operator", "operator-long-password")
	operatorSession := h.signIn("operator@example.com", "operator-long-password", operator)

	response := h.do(http.MethodGet, "/v1/people", nil, operatorSession.headers())
	assert.Equal(t, http.StatusForbidden, response.status, "an Operator must not read People")

	// An Operator cannot obtain a People-reading credential at all.
	_, _, err := h.store.CreateCredentialVersion(context.Background(), identity.CreateCredentialParams{
		Kind: identity.CredentialKindPersonal, OwnerIdentity: operator.ID, ActorIdentity: operator.ID,
		Name: "escalate", Actions: []string{identity.ActionPeopleRead}, RateRPM: 1, RateTPM: 1,
		ExpiresAt: h.now.Add(time.Hour), Now: h.now,
	})
	assert.Error(t, err)

	// A personal Admin credential holding people.read reads the directory.
	key := h.personalKey(admin.user.ID, identity.ActionPeopleRead)
	bearer := map[string]string{"Authorization": "Bearer " + key}
	response = h.do(http.MethodGet, "/v1/people", nil, bearer)
	assert.Equal(t, http.StatusOK, response.status)

	// An invalid bearer never falls back to the valid Admin cookie.
	headers := admin.headers()
	headers["Authorization"] = "Bearer cp-not-a-real-key"
	response = h.do(http.MethodGet, "/v1/people", nil, headers)
	assert.Equal(t, http.StatusUnauthorized, response.status)
	assert.Equal(t, "invalid_credential", response.body["code"])

	// Mutations refuse bearer even beside a valid, recently authenticated cookie.
	h.reauthenticate(admin, "admin-long-password")
	unsafe := admin.unsafeHeaders()
	unsafe["Authorization"] = "Bearer " + key
	for _, path := range []string{
		"/v1/people/" + operator.ID + "/role",
		"/v1/people/" + operator.ID + "/disable",
		"/v1/people/" + operator.ID + "/enable",
		"/v1/people/" + operator.ID + "/sessions/revoke",
	} {
		response = h.do(http.MethodPost, path, map[string]any{"role": "admin"}, unsafe)
		assert.Equal(t, http.StatusUnauthorized, response.status, path)
		assert.Equal(t, "bearer_not_accepted", response.body["code"], path)
	}
}

// TestPeopleMutationRequiresCSRFAndRecentAuth proves the security mutations are
// recent-auth bound and that the consequences are reported from server state.
func TestPeopleMutationRequiresCSRFAndRecentAuth(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	target := h.activeUser("person@example.com", "operator", "person-long-password")

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
