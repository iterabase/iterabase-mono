package e2e_test

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// exerciseIdentityAPIStage proves the deployed V2-only authority (HOR-582):
// the overlay-named first Admin completes the emailed setup link and signs in;
// another person joins through the access-request flow; bootstrap printed no
// credential; the retired legacy routes are gone; every customer route accepts
// exactly its credential class and action; and disablement, session
// revocation, and demotion take effect on the very next request.
func exerciseIdentityAPIStage(t *testing.T, state *deployedState) {
	t.Helper()
	state.ensurePrincipals(t)

	ctx, cancel := context.WithTimeout(state.ctx, 30*time.Second)
	pod, err := state.assertBootstrapPrintedNoCredential(ctx, nil)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first-boot bootstrap printed no credential on %s", pod.safeSummary())

	assertRetiredLegacyRoutes(t, state)
	assertDeployedActionMatrix(t, state)
	assertNextRequestRevocation(t, state)
}

func assertRetiredLegacyRoutes(t *testing.T, state *deployedState) {
	t.Helper()
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/.well-known/jwks.json"},
		{http.MethodPost, "/v1/token"},
		{http.MethodGet, "/v1/users"},
		{http.MethodPost, "/v1/users"},
		{http.MethodGet, "/v1/api-keys"},
		{http.MethodPost, "/v1/api-keys"},
		{http.MethodGet, "/v1/permissions/identities/" + state.operator.identityID},
		{http.MethodPost, "/v1/value-models"},
	} {
		for _, key := range []string{"", state.adminKey} {
			status, body := state.request(t, route.method, route.path, key, nil, state.admin.headers(nil))
			if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
				t.Fatalf("retired route %s %s answered %d: %s", route.method, route.path, status, safeResponse(body))
			}
		}
	}
}

func assertDeployedActionMatrix(t *testing.T, state *deployedState) {
	t.Helper()
	expect := func(name string, status, want int, body []byte) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: HTTP %d want %d: %s", name, status, want, safeResponse(body))
		}
	}
	expectCode := func(name string, status, want int, body []byte, code string) {
		t.Helper()
		expect(name, status, want, body)
		var payload struct {
			Code string `json:"code"`
		}
		mustDecode(t, body, &payload)
		if payload.Code != code {
			t.Fatalf("%s: code=%q want %q", name, payload.Code, code)
		}
	}

	// Unauthenticated and unknown credentials are refused; an unknown bearer
	// never falls back to a valid cookie.
	status, body := state.request(t, http.MethodGet, "/v1/work-items", "", nil, nil)
	expectCode("anonymous work read", status, http.StatusUnauthorized, body, "session_expired")
	status, body = state.request(t, http.MethodGet, "/v1/work-items", "cp-NOTAREALCREDENTIAL0000000000000", nil, state.operator.headers(nil))
	expectCode("unknown bearer beside a cookie", status, http.StatusUnauthorized, body, "invalid_credential")

	// Sessions reach their role's actions; Operators never reach People or value.
	status, body = state.request(t, http.MethodGet, "/v1/work-items", "", nil, state.operator.headers(nil))
	expect("Operator session work read", status, http.StatusOK, body)
	status, body = state.request(t, http.MethodGet, "/v1/people", "", nil, state.operator.headers(nil))
	expectCode("Operator session People read", status, http.StatusForbidden, body, "forbidden")
	status, body = state.request(t, http.MethodGet, "/v1/people", "", nil, state.admin.headers(nil))
	expect("Admin session People read", status, http.StatusOK, body)
	status, body = state.request(t, http.MethodGet, "/v1/work-dashboard", "", nil, state.operator.headers(nil))
	expect("Operator dashboard", status, http.StatusOK, body)
	var dashboard struct {
		Value struct {
			Configured bool  `json:"configured"`
			Totals     []any `json:"totals"`
		} `json:"value"`
	}
	mustDecode(t, body, &dashboard)
	if dashboard.Value.Configured || len(dashboard.Value.Totals) > 0 {
		t.Fatal("an Operator dashboard carried value data")
	}

	// Personal credentials hold exactly their immutable actions.
	status, body = state.request(t, http.MethodGet, "/v1/work-items", state.workKey, nil, nil)
	expect("work key read", status, http.StatusOK, body)
	status, body = state.request(t, http.MethodGet, "/v1/people", "", nil, map[string]string{"Authorization": "Bearer " + state.workKey})
	expectCode("work key People read", status, http.StatusForbidden, body, "forbidden")
	status, body = state.request(t, http.MethodDelete, "/v1/artifacts/00000000-0000-0000-0000-000000000000", state.workKey, nil, nil)
	expectCode("work key artifact delete", status, http.StatusForbidden, body, "forbidden")
	status, body = state.request(t, http.MethodGet, "/v1/people", state.adminKey, nil, nil)
	expect("Admin key People read", status, http.StatusOK, body)
	status, body = state.request(t, http.MethodPost, "/v1/work-items", state.adminKey, []byte(`{}`), map[string]string{"Content-Type": "application/json"})
	expectCode("Admin reporting key cannot start work", status, http.StatusForbidden, body, "forbidden")

	// A bearer credential can never approve access, change roles or status,
	// revoke sessions, respond to blockers, revise work, or sign out — even a
	// valid Admin key beside a valid recently authenticated Admin cookie.
	status, body = state.sessionJSON(t, state.admin, http.MethodPost, "/v1/auth/reauthenticate", map[string]string{"password": state.admin.password})
	expect("Admin reauthentication", status, http.StatusOK, body)
	bearerBesideCookie := state.admin.headers(map[string]string{"Content-Type": "application/json"})
	for _, path := range []string{
		"/v1/people/" + state.operator.identityID + "/role",
		"/v1/people/" + state.operator.identityID + "/disable",
		"/v1/people/" + state.operator.identityID + "/sessions/revoke",
		"/v1/access-requests/00000000-0000-0000-0000-000000000000/approve",
		"/v1/work-blockers/00000000-0000-0000-0000-000000000000/responses",
		"/v1/work-items/00000000-0000-0000-0000-000000000000/revisions",
		"/v1/sessions/revoke-others",
	} {
		status, body = state.request(t, http.MethodPost, path, state.adminKey, []byte(`{"role":"admin"}`), bearerBesideCookie)
		expectCode("bearer on "+path, status, http.StatusUnauthorized, body, "bearer_not_accepted")
	}
	status, body = state.request(t, http.MethodPost, "/v1/work-blockers/00000000-0000-0000-0000-000000000000/responses",
		state.workKey, []byte(`{}`), map[string]string{"Content-Type": "application/json"})
	expectCode("work key blocker response", status, http.StatusUnauthorized, body, "bearer_not_accepted")

	// An Operator cannot mutate People through API manipulation.
	status, body = state.sessionJSON(t, state.operator, http.MethodPost, "/v1/people/"+state.admin.identityID+"/role", map[string]string{"role": "operator"})
	expectCode("Operator People mutation", status, http.StatusForbidden, body, "forbidden")
	// Unsafe cookie requests require the session-bound CSRF proof.
	noCSRF := state.operator.headers(map[string]string{"Content-Type": "application/json"})
	delete(noCSRF, "X-CSRF-Token")
	status, body = state.request(t, http.MethodPost, "/v1/work-items", "", []byte(`{}`), noCSRF)
	expectCode("cookie start without CSRF", status, http.StatusForbidden, body, "csrf")
}

func assertNextRequestRevocation(t *testing.T, state *deployedState) {
	t.Helper()
	revocable := state.onboard(t, "revocable@e2e.iterabase.test", "operator", "e2e-revocable-long-password")
	revocableKey := state.seedPersonalCredential(t, revocable.identityID, "work.read")
	demoted := state.onboard(t, "demoted@e2e.iterabase.test", "admin", "e2e-demoted-long-password")
	demotedKey := state.seedPersonalCredential(t, demoted.identityID, "people.read")

	read := func(key string, session *webSession) int {
		headers := map[string]string{}
		if session != nil {
			headers = session.headers(nil)
		}
		status, _ := state.request(t, http.MethodGet, "/v1/work-items", key, nil, headers)
		return status
	}
	if read(revocableKey, nil) != http.StatusOK || read("", &revocable) != http.StatusOK {
		t.Fatal("onboarded Operator credentials did not authorize before revocation")
	}
	admin := func(path string, value any) {
		t.Helper()
		status, body := state.sessionJSON(t, state.admin, http.MethodPost, "/v1/auth/reauthenticate", map[string]string{"password": state.admin.password})
		requireStatus(t, status, http.StatusOK, body)
		status, body = state.sessionJSON(t, state.admin, http.MethodPost, path, value)
		requireStatus(t, status, http.StatusOK, body)
	}

	// Session revocation: the next cookie request is refused; the key is not.
	admin("/v1/people/"+revocable.identityID+"/sessions/revoke", map[string]any{})
	if got := read("", &revocable); got != http.StatusUnauthorized {
		t.Fatalf("revoked session answered %d on its next request", got)
	}
	if got := read(revocableKey, nil); got != http.StatusOK {
		t.Fatalf("session revocation changed the API credential: %d", got)
	}

	// Disablement suspends the person's credential on the next request.
	admin("/v1/people/"+revocable.identityID+"/disable", map[string]any{})
	if got := read(revocableKey, nil); got != http.StatusUnauthorized {
		t.Fatalf("disabled user's credential answered %d on its next request", got)
	}

	// Demoting an Admin ends their session and their Admin-only key.
	status, body := state.request(t, http.MethodGet, "/v1/people", demotedKey, nil, nil)
	requireStatus(t, status, http.StatusOK, body)
	admin("/v1/people/"+demoted.identityID+"/role", map[string]string{"role": "operator"})
	status, body = state.request(t, http.MethodGet, "/v1/people", demotedKey, nil, nil)
	if status == http.StatusOK {
		t.Fatalf("demoted Admin's People key still authorized: %s", safeResponse(body))
	}
	status, body = state.request(t, http.MethodGet, "/v1/people", "", nil, demoted.headers(nil))
	requireStatus(t, status, http.StatusUnauthorized, body)
}
