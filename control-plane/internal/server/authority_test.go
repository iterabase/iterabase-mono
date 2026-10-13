package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/server"
	workstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/work"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/workflow"
)

// reviewedRouteClasses is the reviewed authority classification of every HTTP
// route (architecture 9.3, HOR-582). Adding, removing, or reclassifying a route
// must change this table in the same review.
var reviewedRouteClasses = map[string]server.RouteClass{
	"GET /":         {Kind: server.RouteProbe},
	"GET /assets/*": {Kind: server.RouteProbe},
	"GET /healthz":  {Kind: server.RouteProbe},
	"GET /readyz":   {Kind: server.RouteProbe},

	"POST /v1/auth/request-access":  {Kind: server.RoutePublic},
	"POST /v1/auth/verify":          {Kind: server.RoutePublic},
	"POST /v1/auth/request-status":  {Kind: server.RoutePublic},
	"POST /v1/auth/setup":           {Kind: server.RoutePublic},
	"POST /v1/auth/setup/resend":    {Kind: server.RoutePublic},
	"POST /v1/auth/setup/context":   {Kind: server.RoutePublic},
	"POST /v1/auth/sign-in":         {Kind: server.RoutePublic},
	"POST /v1/auth/password/forgot": {Kind: server.RoutePublic},
	"POST /v1/auth/password/reset":  {Kind: server.RoutePublic},
	"GET /v1/auth/session":          {Kind: server.RoutePublic},

	"GET /v1/profile":                 {Kind: server.RouteSession},
	"PATCH /v1/profile":               {Kind: server.RouteSession},
	"GET /v1/sessions":                {Kind: server.RouteSession},
	"DELETE /v1/sessions/{id}":        {Kind: server.RouteSession},
	"POST /v1/sessions/revoke-others": {Kind: server.RouteSession},
	"POST /v1/auth/sign-out":          {Kind: server.RouteSession},
	"POST /v1/auth/reauthenticate":    {Kind: server.RouteSession},

	"GET /v1/access-requests":               {Kind: server.RouteAction, Action: identity.ActionPeopleRead},
	"POST /v1/access-requests/{id}/approve": {Kind: server.RouteSession, Admin: true, RecentAuth: true},
	"POST /v1/access-requests/{id}/decline": {Kind: server.RouteSession, Admin: true, RecentAuth: true},
	"GET /v1/people":                        {Kind: server.RouteAction, Action: identity.ActionPeopleRead},
	"GET /v1/people/{id}":                   {Kind: server.RouteAction, Action: identity.ActionPeopleRead},
	"POST /v1/people/{id}/role":             {Kind: server.RouteSession, Admin: true, RecentAuth: true},
	"POST /v1/people/{id}/disable":          {Kind: server.RouteSession, Admin: true, RecentAuth: true},
	"POST /v1/people/{id}/enable":           {Kind: server.RouteSession, Admin: true, RecentAuth: true},
	"POST /v1/people/{id}/sessions/revoke":  {Kind: server.RouteSession, Admin: true, RecentAuth: true},
	"GET /v1/organization/settings":         {Kind: server.RouteSession, Admin: true},
	"PATCH /v1/organization/settings":       {Kind: server.RouteSession, Admin: true},

	"GET /v1/work-items":                            {Kind: server.RouteAction, Action: identity.ActionWorkRead},
	"POST /v1/work-items":                           {Kind: server.RouteAction, Action: identity.ActionWorkflowsStart},
	"GET /v1/work-items/{id}":                       {Kind: server.RouteAction, Action: identity.ActionWorkRead},
	"GET /v1/work-items/{id}/attempts":              {Kind: server.RouteAction, Action: identity.ActionWorkRead},
	"GET /v1/work-attempts/{id}/nodes":              {Kind: server.RouteAction, Action: identity.ActionWorkRead},
	"GET /v1/work-items/{id}/timeline":              {Kind: server.RouteAction, Action: identity.ActionWorkRead},
	"GET /v1/work-items/{id}/artifacts":             {Kind: server.RouteAction, Action: identity.ActionWorkRead},
	"GET /v1/work-items/{id}/consequences":          {Kind: server.RouteAction, Action: identity.ActionWorkRead},
	"GET /v1/work-items/{id}/blocker":               {Kind: server.RouteAction, Action: identity.ActionWorkRead},
	"GET /v1/work-items/{id}/feedback":              {Kind: server.RouteAction, Action: identity.ActionWorkRead},
	"GET /v1/work-items/{id}/feedback/{feedbackID}": {Kind: server.RouteAction, Action: identity.ActionWorkRead},
	"POST /v1/work-items/{id}/feedback":             {Kind: server.RouteAction, Action: identity.ActionWorkFeedbackWrite},
	"POST /v1/work-items/{id}/revisions":            {Kind: server.RouteSession},
	"POST /v1/work-blockers/{id}/responses":         {Kind: server.RouteSession},
	"POST /v1/work-items/{id}/wait/stop":            {Kind: server.RouteSession},
	"GET /v1/work-dashboard":                        {Kind: server.RouteAction, Action: identity.ActionWorkRead},
	"GET /v1/work-events":                           {Kind: server.RouteAction, Action: identity.ActionWorkRead},

	"POST /v1/artifacts":        {Kind: server.RouteAction, Action: identity.ActionArtifactsUpload},
	"GET /v1/artifacts/{id}":    {Kind: server.RouteAction, Action: identity.ActionArtifactsRead},
	"HEAD /v1/artifacts/{id}":   {Kind: server.RouteAction, Action: identity.ActionArtifactsRead},
	"DELETE /v1/artifacts/{id}": {Kind: server.RouteBearerAction, Action: identity.ActionArtifactsDelete},
}

// TestEveryRouteIsClassified is the static customer-route classification gate:
// every route the router serves must be registered through exactly one
// reviewed authority class, no class may name a wildcard or unknown action,
// and the retired legacy routes must not exist.
func TestEveryRouteIsClassified(t *testing.T) {
	router, classes := server.NewClassified(server.Services{})

	var served []string
	require.NoError(t, chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served = append(served, server.RouteKey(method, route))
		return nil
	}))
	sort.Strings(served)
	for _, key := range served {
		_, ok := classes[key]
		assert.True(t, ok, "route %s is served without an authority class", key)
	}
	assert.Len(t, served, len(classes), "every classified route is served exactly once")
	assert.Equal(t, reviewedRouteClasses, classes, "the route classification changed without review")

	for key, class := range classes {
		if class.Kind == server.RouteAction || class.Kind == server.RouteBearerAction {
			assert.True(t, identity.KnownAction(class.Action), "%s names unknown action %q", key, class.Action)
			assert.NotContains(t, class.Action, "*", "%s must not use a wildcard", key)
		}
	}

	for _, retired := range []string{
		"POST /v1/token", "GET /.well-known/jwks.json",
		"GET /v1/users", "POST /v1/users", "GET /v1/users/{id}",
		"GET /v1/api-keys", "POST /v1/api-keys", "DELETE /v1/api-keys/{id}",
		"GET /v1/permissions/identities/{id}", "POST /v1/value-models",
	} {
		_, ok := classes[retired]
		assert.False(t, ok, "retired route %s must not exist", retired)
	}
}

// TestRetiredLegacyRoutesAreGone proves the retired legacy surfaces answer as
// unknown routes rather than as an authentication challenge.
func TestRetiredLegacyRoutesAreGone(t *testing.T) {
	router := server.New(server.Services{})
	for _, request := range []struct{ method, path string }{
		{http.MethodPost, "/v1/token"},
		{http.MethodGet, "/.well-known/jwks.json"},
		{http.MethodGet, "/v1/users"},
		{http.MethodPost, "/v1/api-keys"},
		{http.MethodGet, "/v1/permissions/identities/00000000-0000-0000-0000-000000000000"},
		{http.MethodPost, "/v1/value-models"},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(request.method, request.path, nil))
		assert.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, recorder.Code, "%s %s", request.method, request.path)
	}
}

// TestCustomerActionMatrix proves direct positives and negatives for every
// credential path: Operator and Admin sessions, personal credentials holding
// only some actions, unauthenticated callers, and bearer credentials on
// cookie-only routes.
func TestCustomerActionMatrix(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	operator := h.activeUser("operator@example.com", "operator", "operator-long-password")
	operatorSession := h.signIn("operator@example.com", "operator-long-password", operator)
	itemID := "00000000-0000-0000-0000-000000000001"

	readOnly := h.personalKey(operator.ID, identity.ActionWorkRead)
	bearer := func(key string) map[string]string { return map[string]string{"Authorization": "Bearer " + key} }

	// Unauthenticated customer requests are refused.
	assert.Equal(t, http.StatusUnauthorized, h.do(http.MethodGet, "/v1/work-items", nil, nil).status)
	assert.Equal(t, http.StatusUnauthorized, h.do(http.MethodPost, "/v1/artifacts", nil, nil).status)

	// Sessions reach work by role.
	assert.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/work-dashboard", nil, operatorSession.headers()).status)
	assert.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/work-dashboard", nil, admin.headers()).status)
	assert.Equal(t, http.StatusForbidden, h.do(http.MethodGet, "/v1/people", nil, operatorSession.headers()).status)
	assert.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/people", nil, admin.headers()).status)

	// A personal credential holds exactly its actions.
	assert.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/work-items", nil, bearer(readOnly)).status)
	for _, denied := range []struct{ method, path string }{
		{http.MethodPost, "/v1/work-items"},
		{http.MethodPost, "/v1/work-items/" + itemID + "/feedback"},
		{http.MethodPost, "/v1/artifacts"},
		{http.MethodGet, "/v1/artifacts/" + itemID},
		{http.MethodGet, "/v1/people"},
		{http.MethodGet, "/v1/access-requests"},
	} {
		response := h.do(denied.method, denied.path, nil, bearer(readOnly))
		assert.Equal(t, http.StatusForbidden, response.status, "%s %s", denied.method, denied.path)
		assert.Equal(t, "forbidden", response.body["code"])
	}

	// workflows.start passes authorization and reaches request validation.
	starter := h.personalKey(operator.ID, identity.ActionWorkflowsStart)
	started := h.do(http.MethodPost, "/v1/work-items", map[string]any{}, bearer(starter))
	assert.Equal(t, http.StatusBadRequest, started.status, "missing Idempotency-Key is a validation error after authorization")

	// An unknown bearer value is invalid and never falls back to the cookie.
	headers := operatorSession.headers()
	headers["Authorization"] = "Bearer cp-unknown"
	response := h.do(http.MethodGet, "/v1/work-items", nil, headers)
	assert.Equal(t, http.StatusUnauthorized, response.status)
	assert.Equal(t, "invalid_credential", response.body["code"])

	// Consequence-capable work commands and every security mutation refuse a
	// bearer credential, even a fully privileged one beside a valid cookie.
	h.reauthenticate(admin, "admin-long-password")
	privileged := h.personalKey(admin.user.ID, identity.CatalogueActions(identity.CredentialKindPersonal)...)
	unsafe := admin.unsafeHeaders()
	unsafe["Authorization"] = "Bearer " + privileged
	for _, path := range []string{
		"/v1/work-blockers/" + itemID + "/responses",
		"/v1/work-items/" + itemID + "/revisions",
		"/v1/access-requests/" + itemID + "/approve",
		"/v1/access-requests/" + itemID + "/decline",
		"/v1/people/" + operator.ID + "/role",
		"/v1/sessions/revoke-others",
		"/v1/auth/sign-out",
	} {
		response := h.do(http.MethodPost, path, map[string]any{}, unsafe)
		assert.Equal(t, http.StatusUnauthorized, response.status, path)
		assert.Equal(t, "bearer_not_accepted", response.body["code"], path)
	}

	// A plain API client (no browser Origin) gets the same bearer refusal.
	for _, path := range []string{
		"/v1/work-blockers/" + itemID + "/responses",
		"/v1/people/" + operator.ID + "/role",
	} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer "+privileged)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		h.router.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code, path)
		assert.Contains(t, recorder.Body.String(), "bearer_not_accepted", path)
	}

	// Cookie-only work commands are reachable by a session with CSRF.
	noCSRF := h.do(http.MethodPost, "/v1/work-blockers/"+itemID+"/responses", map[string]any{"outcome": "x"}, operatorSession.headers())
	assert.Equal(t, http.StatusForbidden, noCSRF.status)
	assert.Equal(t, "csrf", noCSRF.body["code"])
	withCSRF := h.do(http.MethodPost, "/v1/work-blockers/"+itemID+"/responses", map[string]any{"outcome": "x"}, operatorSession.unsafeHeaders())
	assert.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, withCSRF.status)

	// A session's unsafe action routes need CSRF too.
	sessionStart := h.do(http.MethodPost, "/v1/work-items", map[string]any{}, operatorSession.headers())
	assert.Equal(t, http.StatusForbidden, sessionStart.status)
	assert.Equal(t, "csrf", sessionStart.body["code"])

	// Artifact deletion is a personal current-Admin API action only.
	deleted := h.do(http.MethodDelete, "/v1/artifacts/"+itemID, nil, admin.unsafeHeaders())
	assert.Equal(t, http.StatusUnauthorized, deleted.status)
	assert.Equal(t, "api_credential_required", deleted.body["code"])
	deleter := h.personalKey(admin.user.ID, identity.ActionArtifactsRead, identity.ActionArtifactsDelete)
	deleted = h.do(http.MethodDelete, "/v1/artifacts/"+itemID, nil, bearer(deleter))
	assert.Equal(t, http.StatusNotFound, deleted.status, "authorized; the artifact simply does not exist")
	deleted = h.do(http.MethodDelete, "/v1/artifacts/"+itemID, nil, bearer(readOnly))
	assert.Equal(t, http.StatusForbidden, deleted.status)
}

// TestAuthorityIsCurrentOnTheNextRequest proves a disabled user, a demoted
// Admin, a revoked session, and a suspended credential lose authority on the
// very next request.
func TestAuthorityIsCurrentOnTheNextRequest(t *testing.T) {
	h := newAuthAPIHarness(t)
	ctx := context.Background()
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	second := h.activeUser("second@example.com", "admin", "second-long-password")
	secondSession := h.signIn("second@example.com", "second-long-password", second)
	operator := h.activeUser("operator@example.com", "operator", "operator-long-password")
	operatorSession := h.signIn("operator@example.com", "operator-long-password", operator)

	operatorKey := h.personalKey(operator.ID, identity.ActionWorkRead)
	secondPeopleKey := h.personalKey(second.ID, identity.ActionPeopleRead)
	bearer := func(key string) map[string]string { return map[string]string{"Authorization": "Bearer " + key} }

	require.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/work-items", nil, bearer(operatorKey)).status)
	require.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/work-items", nil, operatorSession.headers()).status)
	require.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/people", nil, secondSession.headers()).status)
	require.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/people", nil, bearer(secondPeopleKey)).status)

	// Disabling the Operator suspends their key and revokes their session.
	_, _, err := h.store.DisablePerson(ctx, admin.user.ID, operator.ID, h.now)
	require.NoError(t, err)
	response := h.do(http.MethodGet, "/v1/work-items", nil, bearer(operatorKey))
	assert.Equal(t, http.StatusUnauthorized, response.status)
	assert.Equal(t, "credential_not_eligible", response.body["code"])
	assert.Equal(t, http.StatusUnauthorized, h.do(http.MethodGet, "/v1/work-items", nil, operatorSession.headers()).status)

	// Re-enable never resumes the suspended key.
	_, err = h.store.EnablePerson(ctx, admin.user.ID, operator.ID, h.now)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, h.do(http.MethodGet, "/v1/work-items", nil, bearer(operatorKey)).status)

	// Demoting an Admin revokes the session and suspends Admin-only keys.
	_, _, err = h.store.ChangePersonRole(ctx, admin.user.ID, second.ID, identity.RoleOperator, h.now)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, h.do(http.MethodGet, "/v1/people", nil, secondSession.headers()).status)
	assert.NotEqual(t, http.StatusOK, h.do(http.MethodGet, "/v1/people", nil, bearer(secondPeopleKey)).status)

	// A revoked session is refused on its next request.
	adminKeyless := h.do(http.MethodGet, "/v1/work-items", nil, admin.headers())
	require.Equal(t, http.StatusOK, adminKeyless.status)
	_, err = h.store.RevokeAllSessionsForIdentity(ctx, admin.user.ID, h.now, "revoked")
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, h.do(http.MethodGet, "/v1/work-items", nil, admin.headers()).status)
}

// TestWorkStartRecordsThreePrincipalsAndClipsValue proves POST /v1/work-items
// persists the initiating human, request actor, and authorization evidence from
// the authenticated boundary only, and that value data is visible only to a
// principal holding the Admin-only value.read action.
func TestWorkStartRecordsThreePrincipalsAndClipsValue(t *testing.T) {
	h := newAuthAPIHarness(t)
	ctx := context.Background()
	seedManualValueWorkflow(t, ctx, h.pool)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	operator := h.activeUser("operator@example.com", "operator", "operator-long-password")
	operatorSession := h.signIn("operator@example.com", "operator-long-password", operator)

	start := func(headers map[string]string, idempotency string) authHTTPResponse {
		headers["Idempotency-Key"] = idempotency
		return h.do(http.MethodPost, "/v1/work-items", map[string]any{
			"workflowKey": "walter/manual", "title": "Quotation — ACME", "source": map[string]any{"m": idempotency},
			"sourcePresentation": map[string]any{"kind": "email", "title": "Quote", "evidence": []any{}},
		}, headers)
	}

	// Browser start: the signed-in human is both principals.
	browser := start(operatorSession.unsafeHeaders(), "browser-1")
	require.Equal(t, http.StatusCreated, browser.status, "%v", browser.body)
	browserID, _ := browser.body["id"].(string)
	assertPrincipals(t, h.pool, browserID, operator.ID, operator.ID, "browser")
	_, valueVisible := browser.body["estimatedValue"]
	assert.False(t, valueVisible || browser.body["valueConfigured"] == true, "an Operator never receives value data")

	// API start: the credential owner and actor come from the credential, not
	// from anything the caller supplies.
	key := h.personalKey(operator.ID, identity.ActionWorkflowsStart, identity.ActionWorkRead)
	apiHeaders := map[string]string{"Authorization": "Bearer " + key}
	api := start(apiHeaders, "api-1")
	require.Equal(t, http.StatusCreated, api.status, "%v", api.body)
	apiID, _ := api.body["id"].(string)
	assertPrincipals(t, h.pool, apiID, operator.ID, operator.ID, "api")
	var keyID *string
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT authorization_api_key_id::text FROM work.work_items WHERE id = $1`, apiID).Scan(&keyID))
	require.NotNil(t, keyID)

	// Value data: clipped for Operators and keys without value.read, visible to
	// an Admin session and a personal Admin key holding value.read.
	for name, headers := range map[string]map[string]string{
		"operator session": operatorSession.headers(),
		"work.read key":    {"Authorization": "Bearer " + key},
	} {
		item := h.do(http.MethodGet, "/v1/work-items/"+browserID, nil, headers)
		require.Equal(t, http.StatusOK, item.status, name)
		assert.Equal(t, false, item.body["valueConfigured"], name)
		assert.Nil(t, item.body["valueModel"], name)
		dashboard := h.do(http.MethodGet, "/v1/work-dashboard", nil, headers)
		value, _ := dashboard.body["value"].(map[string]any)
		assert.Equal(t, false, value["configured"], name)
	}
	reporter := h.personalKey(admin.user.ID, identity.ActionWorkRead, identity.ActionValueRead)
	for name, headers := range map[string]map[string]string{
		"admin session":  admin.headers(),
		"value.read key": {"Authorization": "Bearer " + reporter},
	} {
		item := h.do(http.MethodGet, "/v1/work-items/"+browserID, nil, headers)
		require.Equal(t, http.StatusOK, item.status, name)
		assert.Equal(t, true, item.body["valueConfigured"], name)
		dashboard := h.do(http.MethodGet, "/v1/work-dashboard", nil, headers)
		value, _ := dashboard.body["value"].(map[string]any)
		assert.Equal(t, true, value["configured"], name)
	}
	listed := h.list("/v1/work-items", operatorSession.headers())
	require.NotEmpty(t, listed)
	for _, item := range listed {
		assert.Equal(t, false, item["valueConfigured"])
	}

	// Value timeline events: omitted from the timeline and the work-event
	// stream without value.read, delivered with it; non-value events and
	// resume cursors are unaffected.
	_, err := h.pool.Exec(ctx, `
		INSERT INTO work.timeline_events (work_item_id, code, params) VALUES
			($1, 'value_credited', '{"estimated":true,"currency":"EUR"}'),
			($1, 'value_deducted', '{"estimated":true,"currency":"EUR"}'),
			($1, 'feedback_recorded', '{}')`, browserID)
	require.NoError(t, err)
	readOnly := h.personalKey(operator.ID, identity.ActionWorkRead)
	for name, headers := range map[string]map[string]string{
		"operator session": operatorSession.headers(),
		"work.read key":    {"Authorization": "Bearer " + readOnly},
	} {
		codes := timelineCodes(h.list("/v1/work-items/"+browserID+"/timeline", headers))
		assert.NotContains(t, codes, "value_credited", name)
		assert.NotContains(t, codes, "value_deducted", name)
		assert.Contains(t, codes, "feedback_recorded", name)
		// A one-event page skips value events instead of returning empty.
		first := h.list("/v1/work-items/"+browserID+"/timeline?limit=1", headers)
		require.Len(t, first, 1, name)
		cursor := int64(first[0]["cursor"].(float64))
		var rest []string
		for {
			page := h.list(fmt.Sprintf("/v1/work-items/%s/timeline?limit=1&after=%d", browserID, cursor), headers)
			if len(page) == 0 {
				break
			}
			rest = append(rest, timelineCodes(page)...)
			cursor = int64(page[0]["cursor"].(float64))
		}
		assert.Contains(t, rest, "feedback_recorded", name)
		assert.NotContains(t, rest, "value_credited", name)
		stream := h.streamWorkEvents(headers)
		assert.NotContains(t, stream, "value_credited", name)
		assert.NotContains(t, stream, "value_deducted", name)
		assert.Contains(t, stream, "event: feedback_recorded", name)
	}
	for name, headers := range map[string]map[string]string{
		"admin session":  admin.headers(),
		"value.read key": {"Authorization": "Bearer " + reporter},
	} {
		codes := timelineCodes(h.list("/v1/work-items/"+browserID+"/timeline", headers))
		assert.Contains(t, codes, "value_credited", name)
		assert.Contains(t, codes, "value_deducted", name)
		assert.Contains(t, h.streamWorkEvents(headers), "event: value_credited", name)
	}
}

func timelineCodes(events []map[string]any) []string {
	codes := make([]string, 0, len(events))
	for _, event := range events {
		code, _ := event["code"].(string)
		codes = append(codes, code)
	}
	return codes
}

// streamWorkEvents reads the work-event SSE stream until its first poll has
// been flushed and returns the raw body.
func (h *authAPIHarness) streamWorkEvents(headers map[string]string) string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/v1/work-events", nil).WithContext(ctx)
	request.Header.Set("Origin", authTestOrigin)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	require.Equal(h.t, http.StatusOK, recorder.Code, recorder.Body.String())
	return recorder.Body.String()
}

func assertPrincipals(t *testing.T, pool *pgxpool.Pool, itemID, human, actor, source string) {
	t.Helper()
	var gotHuman, gotActor, gotSource string
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT initiating_human_identity_id::text, request_actor_identity_id::text, authorization_source
		FROM work.work_items WHERE id = $1`, itemID).Scan(&gotHuman, &gotActor, &gotSource))
	assert.Equal(t, human, gotHuman)
	assert.Equal(t, actor, gotActor)
	assert.Equal(t, source, gotSource)
}

// seedManualValueWorkflow registers a Ready manual_api workflow whose work is
// value-configured, so customer projections carry value data to clip.
func seedManualValueWorkflow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var scopeID, poolID string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO identity.identities(key,kind,source,display_name)VALUES('workflow:default/walter','workflow','local','Walter')RETURNING id`).Scan(&scopeID))
	_, err := pool.Exec(ctx, `INSERT INTO catalog.backends(key,name,namespace,kind,model,service_url,image,deployed,healthy)VALUES('default/backend','backend','default','vLLM','model/base','http://model','image',true,true)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO catalog.models(key,namespace,model_id,display_name,context_length,backend_ref,default_params,reasoning_config,available)VALUES('default/model-one','default','model-one','Model One',32768,'backend','{}','{}',true)`)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO toolgateway.pools(key,name,spiffe_id_prefix)VALUES('default/pool','pool','spiffe://iterabase.local/pools/pool/')RETURNING id`).Scan(&poolID))
	_, err = workstore.NewStore(pool).CreateValueModel(ctx, workstore.ValueModelInput{
		Ref: "quotation-value", Version: "1", Currency: "EUR", BaselineSeconds: 1200, LoadedHourlyCost: "30.00",
		Assumptions: json.RawMessage(`{}`), Explanation: json.RawMessage(`{"en":"20 minutes"}`),
	})
	require.NoError(t, err)

	spec := workflow.CanonicalSpec{
		Key: "walter/manual", ScopeIdentityKey: "workflow:default/walter", PoolRef: "pool",
		DefaultModelRef: "model-one", ValueModelRef: "quotation-value", Source: workflow.CanonicalSource{Type: "manual_api"},
		Graph: workflow.CanonicalGraph{EntryNode: "process", MaxTransitions: 20,
			Nodes: []workflow.CanonicalNode{{
				Key: "process", Label: workflow.CanonicalLocalizedText{EN: "Processing", PT: "A processar"},
				Kind: workflow.NodeAgentTask, Prompt: "Process", Outcomes: []string{"completed"},
				OutputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
				ResultPresentation: &workflow.CanonicalResultPresentation{Outcomes: []workflow.CanonicalResultOutcomePresentation{{
					Outcome: "completed", Summary: workflow.CanonicalLocalizedText{EN: "Done", PT: "Feito"},
				}}},
			}},
			TerminalOutcomes: []workflow.CanonicalTerminalOutcome{{Node: "process", Outcome: "completed"}}},
		Presentation: workflow.CanonicalPresentation{WorkflowTitle: "Quotation", PersonaName: "Marco", Locale: "en"},
	}
	specJSON, err := json.Marshal(spec)
	require.NoError(t, err)
	presentation, err := json.Marshal(spec.Presentation)
	require.NoError(t, err)
	def, err := workflow.NewStore(pool).RegisterDefinition(ctx, workflow.Definition{
		Key: "walter/manual", Version: "1", Digest: "sha256:manual-v1", SpecJSON: specJSON,
		ValidationStatus: workflow.ValidationValid, ScopeIdentityID: scopeID, SourceType: "manual_api",
		PoolKey: "default/pool", Presentation: presentation,
	})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO toolgateway.workflow_pool_bindings(workflow_definition_key,pool_id,permitted_tools)VALUES($1,$2,'[]')`,
		workflow.DefinitionKey(def.Key, def.Version), poolID)
	require.NoError(t, err)
}
