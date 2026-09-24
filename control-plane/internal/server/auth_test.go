package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/server"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
)

const authTestOrigin = "https://app.example.com"

type authAPIHarness struct {
	t      *testing.T
	router http.Handler
	store  *identity.Store
	pool   *pgxpool.Pool
	now    time.Time
}

func newAuthAPIHarness(t *testing.T) *authAPIHarness {
	t.Helper()
	pool := testutil.NewPostgresPool(t)
	store := identity.NewStore(pool)
	harness := &authAPIHarness{
		t:     t,
		store: store,
		pool:  pool,
		now:   time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC),
	}
	harness.router = server.New(server.Services{
		Pool:  pool,
		Store: store,
		Auth: &server.AuthServices{
			Enabled:      true,
			PublicOrigin: authTestOrigin,
			Store:        store,
			Location:     identity.NoLocation{},
			Now:          func() time.Time { return harness.now },
		},
	})
	return harness
}

type authHTTPResponse struct {
	status  int
	body    map[string]any
	cookies []*http.Cookie
	header  http.Header
}

func (h *authAPIHarness) do(method, path string, body any, headers map[string]string) authHTTPResponse {
	h.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(h.t, err)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Origin", authTestOrigin)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)

	response := authHTTPResponse{
		status:  recorder.Code,
		cookies: recorder.Result().Cookies(),
		header:  recorder.Header(),
		body:    map[string]any{},
	}
	if raw := recorder.Body.Bytes(); len(raw) > 0 {
		_ = json.Unmarshal(raw, &response.body)
	}
	return response
}

// list issues a GET and decodes the JSON array body.
func (h *authAPIHarness) list(path string, headers map[string]string) []map[string]any {
	h.t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Origin", authTestOrigin)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	require.Equal(h.t, http.StatusOK, recorder.Code, "GET %s returned %d: %s", path, recorder.Code, recorder.Body.String())
	var out []map[string]any
	require.NoError(h.t, json.Unmarshal(recorder.Body.Bytes(), &out))
	return out
}

func (h *authAPIHarness) cookieFor(name string, response authHTTPResponse) *http.Cookie {
	h.t.Helper()
	for _, cookie := range response.cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func (h *authAPIHarness) sessionCookieHeader(response authHTTPResponse) map[string]string {
	h.t.Helper()
	cookie := h.cookieFor(server.DefaultSessionCookie, response)
	require.NotNil(h.t, cookie, "response must set the browser session cookie")
	return map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}
}

// link leases the pending intent for a recipient/purpose and returns the raw
// one-time token exactly as the email worker would issue it.
func (h *authAPIHarness) link(recipient, purpose string) string {
	h.t.Helper()
	ctx := context.Background()
	intents, err := h.store.ClaimAuthEmails(ctx, "test-"+recipient, 20, time.Minute, h.now)
	require.NoError(h.t, err)
	for _, intent := range intents {
		if intent.RecipientEmail == recipient && intent.Purpose == purpose {
			_, raw, err := h.store.CreateAuthLinkForIntent(ctx, intent.OutboxID, h.now)
			require.NoError(h.t, err)
			return raw
		}
	}
	h.t.Fatalf("no pending %s intent for %s", purpose, recipient)
	return ""
}

type signedIn struct {
	cookie *http.Cookie
	csrf   string
	user   identity.LocalUser
}

// provisionAdmin bootstraps the first Admin through normal setup and signs in.
func (h *authAPIHarness) provisionAdmin(email, password string) signedIn {
	h.t.Helper()
	ctx := context.Background()
	_, err := h.store.BootstrapAdmin(ctx, identity.BootstrapOptions{AdminEmail: email, AdminLocale: "en", Now: h.now})
	require.NoError(h.t, err)
	setupToken := h.link(email, identity.AuthLinkSetupPassword)
	_, err = h.store.CompleteSetup(ctx, setupToken, "Admin", "en", password, h.now)
	require.NoError(h.t, err)
	user, err := h.store.FindLocalUserByEmail(ctx, email)
	require.NoError(h.t, err)
	return h.signIn(email, password, user)
}

func (h *authAPIHarness) signIn(email, password string, user identity.LocalUser) signedIn {
	h.t.Helper()
	response := h.do(http.MethodPost, "/v1/auth/sign-in", map[string]any{"email": email, "password": password}, nil)
	require.Equal(h.t, http.StatusOK, response.status, "sign-in failed: %v", response.body)
	cookie := h.cookieFor(server.DefaultSessionCookie, response)
	require.NotNil(h.t, cookie)
	csrf, _ := response.body["csrfToken"].(string)
	require.NotEmpty(h.t, csrf)
	return signedIn{cookie: cookie, csrf: csrf, user: user}
}

// reauthenticate satisfies the recent-password requirement for a session.
func (h *authAPIHarness) reauthenticate(session signedIn, password string) {
	h.t.Helper()
	response := h.do(http.MethodPost, "/v1/auth/reauthenticate", map[string]any{"password": password}, session.unsafeHeaders())
	require.Equal(h.t, http.StatusOK, response.status, "reauthentication failed: %v", response.body)
}

func (s signedIn) headers() map[string]string {
	return map[string]string{"Cookie": s.cookie.Name + "=" + s.cookie.Value}
}

func (s signedIn) unsafeHeaders() map[string]string {
	return map[string]string{
		"Cookie":       s.cookie.Name + "=" + s.cookie.Value,
		"X-CSRF-Token": s.csrf,
	}
}

func (h *authAPIHarness) activeUser(email, role, password string) identity.LocalUser {
	h.t.Helper()
	ctx := context.Background()
	user, err := h.store.UpsertLocalUser(ctx, email, email, role)
	require.NoError(h.t, err)
	// Activate through the normal setup path.
	_, err = h.store.QueueAuthEmail(ctx, identity.AuthEmailIntent{
		Purpose:             identity.AuthLinkSetupPassword,
		RecipientEmail:      email,
		RecipientIdentityID: user.ID,
		Locale:              "en",
	})
	require.NoError(h.t, err)
	setupToken := h.link(email, identity.AuthLinkSetupPassword)
	_, err = h.store.CompleteSetup(ctx, setupToken, "Person", "en", password, h.now)
	require.NoError(h.t, err)
	updated, err := h.store.FindLocalUserByEmail(ctx, email)
	require.NoError(h.t, err)
	return updated
}

func TestAuthRoutesFailSafeWhenDisabled(t *testing.T) {
	router := server.New(server.Services{})
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/sign-in", bytes.NewReader([]byte(`{"email":"a@b.com","password":"x"}`)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", authTestOrigin)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "auth_unavailable")

	// Public health remains independent of the browser surface.
	health := httptest.NewRecorder()
	router.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, health.Code)
}

func TestRequestAccessValidationAndOrigin(t *testing.T) {
	h := newAuthAPIHarness(t)

	invalid := h.do(http.MethodPost, "/v1/auth/request-access", map[string]any{"email": "not-an-email", "locale": "en"}, nil)
	assert.Equal(t, http.StatusBadRequest, invalid.status)
	assert.Equal(t, "invalid_email", invalid.body["code"])

	// A browser request without the configured origin is refused.
	noOrigin := httptest.NewRequest(http.MethodPost, "/v1/auth/request-access", bytes.NewReader([]byte(`{"email":"ada@example.com","locale":"en"}`)))
	noOrigin.Header.Set("Content-Type", "application/json")
	noOriginRecorder := httptest.NewRecorder()
	h.router.ServeHTTP(noOriginRecorder, noOrigin)
	assert.Equal(t, http.StatusForbidden, noOriginRecorder.Code)

	accepted := h.do(http.MethodPost, "/v1/auth/request-access", map[string]any{"email": "Ada@Example.com", "locale": "en"}, nil)
	assert.Equal(t, http.StatusAccepted, accepted.status)
	assert.Equal(t, "accepted", accepted.body["status"])

	// A duplicate request is generically accepted without a second intent.
	duplicate := h.do(http.MethodPost, "/v1/auth/request-access", map[string]any{"email": "ada@example.com", "locale": "en"}, nil)
	assert.Equal(t, http.StatusAccepted, duplicate.status)
}

func TestAccessRequestJourneyWithAdminApprovalAndSessions(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")

	// Request access and verify the email.
	require.Equal(t, http.StatusAccepted,
		h.do(http.MethodPost, "/v1/auth/request-access", map[string]any{"email": "ada@example.com", "locale": "en"}, nil).status)
	verifyToken := h.link("ada@example.com", identity.AuthLinkVerifyAccess)
	verified := h.do(http.MethodPost, "/v1/auth/verify", map[string]any{"token": verifyToken}, nil)
	require.Equal(t, http.StatusOK, verified.status, "%v", verified.body)
	assert.Equal(t, "waiting", verified.body["state"])

	// An Operator (once provisioned) cannot see People data.
	operator := h.activeUser("operator@example.com", "operator", "operator-long-password")
	operatorSession := h.signIn("operator@example.com", "operator-long-password", operator)
	forbidden := h.do(http.MethodGet, "/v1/access-requests", nil, operatorSession.headers())
	assert.Equal(t, http.StatusForbidden, forbidden.status)
	assert.NotContains(t, forbidden.body, "email")

	// Admin lists verified pending requests.
	list := h.list("/v1/access-requests", admin.headers())
	require.Len(t, list, 1)
	requestID, _ := list[0]["id"].(string)
	require.NotEmpty(t, requestID)

	// Approval requires recent-password evidence.
	needsReauth := h.do(http.MethodPost, "/v1/access-requests/"+requestID+"/approve", map[string]any{"role": "operator"}, admin.unsafeHeaders())
	assert.Equal(t, http.StatusForbidden, needsReauth.status)
	assert.Equal(t, "recent_auth_required", needsReauth.body["code"])

	badReauth := h.do(http.MethodPost, "/v1/auth/reauthenticate", map[string]any{"password": "wrong-password"}, admin.unsafeHeaders())
	assert.Equal(t, http.StatusUnauthorized, badReauth.status)
	assert.Equal(t, "invalid_credentials", badReauth.body["code"])

	reauth := h.do(http.MethodPost, "/v1/auth/reauthenticate", map[string]any{"password": "admin-long-password"}, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, reauth.status, "%v", reauth.body)
	assert.NotEmpty(t, reauth.body["recentPasswordAt"])

	approved := h.do(http.MethodPost, "/v1/access-requests/"+requestID+"/approve", map[string]any{"role": "operator"}, admin.unsafeHeaders())
	require.Equal(t, http.StatusOK, approved.status, "%v", approved.body)
	assert.Equal(t, "setup_pending", approved.body["state"])

	// First-time setup creates no session; sign-in follows explicitly.
	setupToken := h.link("ada@example.com", identity.AuthLinkSetupPassword)
	policy := h.do(http.MethodPost, "/v1/auth/setup", map[string]any{
		"token": setupToken, "displayName": "Ada", "locale": "en", "password": "password12345",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, policy.status)
	assert.Equal(t, "password_policy", policy.body["code"])

	completed := h.do(http.MethodPost, "/v1/auth/setup", map[string]any{
		"token": setupToken, "displayName": "Ada", "locale": "pt", "password": "ada-long-password",
	}, nil)
	require.Equal(t, http.StatusOK, completed.status, "%v", completed.body)
	assert.Nil(t, h.cookieFor(server.DefaultSessionCookie, completed), "setup must not create a session")

	adaUser, err := h.store.FindLocalUserByEmail(context.Background(), "ada@example.com")
	require.NoError(t, err)
	ada := h.signIn("ada@example.com", "ada-long-password", adaUser)
	assertSessionCookieContract(t, ada.cookie)

	// Profile reflects the current role and locale, and rotates the session CSRF
	// proof so the caller always has a usable one.
	profileRecorder := h.do(http.MethodGet, "/v1/profile", nil, ada.headers())
	require.Equal(t, http.StatusOK, profileRecorder.status)
	assert.Equal(t, "operator", profileRecorder.body["profile"].(map[string]any)["role"])
	assert.Equal(t, "pt", profileRecorder.body["profile"].(map[string]any)["locale"])
	assert.Equal(t, "no-store", profileRecorder.header.Get("Cache-Control"))
	refreshed, _ := profileRecorder.body["csrfToken"].(string)
	require.NotEmpty(t, refreshed)
	ada.csrf = refreshed

	// Sessions are caller-only and privacy-safe.
	sessions := h.list("/v1/sessions", ada.headers())
	require.Len(t, sessions, 1)
	assert.Equal(t, true, sessions[0]["current"])
	assert.NotContains(t, sessions[0], "ip")
	assert.NotContains(t, sessions[0], "userAgent")

	// Sign-out prevents the next authenticated request.
	signOut := h.do(http.MethodPost, "/v1/auth/sign-out", nil, ada.unsafeHeaders())
	assert.Equal(t, http.StatusNoContent, signOut.status)
	afterSignOut := h.do(http.MethodGet, "/v1/profile", nil, ada.headers())
	assert.Equal(t, http.StatusUnauthorized, afterSignOut.status)
	assert.Equal(t, "session_expired", afterSignOut.body["code"])
}

func TestBearerIsRejectedOnBrowserSecurityRoutes(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")

	// A valid bearer key alongside a valid cookie is still refused.
	user, err := h.store.FindLocalUserByEmail(context.Background(), "admin@example.com")
	require.NoError(t, err)
	_, key, err := h.store.CreateAPIKey(context.Background(), user.ID, "machine", identity.ScopeAdmin, nil)
	require.NoError(t, err)

	headers := admin.headers()
	headers["Authorization"] = "Bearer " + key.Prefix
	response := h.do(http.MethodGet, "/v1/profile", nil, headers)
	assert.Equal(t, http.StatusUnauthorized, response.status)
	assert.Equal(t, "bearer_not_accepted", response.body["code"])

	unsafe := admin.unsafeHeaders()
	unsafe["Authorization"] = "Bearer not-a-real-key"
	response = h.do(http.MethodPost, "/v1/auth/sign-out", nil, unsafe)
	assert.Equal(t, http.StatusUnauthorized, response.status)
	assert.Equal(t, "bearer_not_accepted", response.body["code"])
}

func TestCSRFOriginAndThrottleContract(t *testing.T) {
	h := newAuthAPIHarness(t)
	user := h.activeUser("ada@example.com", "operator", "ada-long-password")
	session := h.signIn("ada@example.com", "ada-long-password", user)

	// Missing CSRF header is refused.
	missingCSRF := h.do(http.MethodPost, "/v1/auth/sign-out", nil, session.headers())
	assert.Equal(t, http.StatusForbidden, missingCSRF.status)
	assert.Equal(t, "csrf", missingCSRF.body["code"])

	// Wrong CSRF header is refused.
	wrongHeaders := session.headers()
	wrongHeaders["X-CSRF-Token"] = "not-the-token"
	wrong := h.do(http.MethodPost, "/v1/auth/sign-out", nil, wrongHeaders)
	assert.Equal(t, http.StatusForbidden, wrong.status)

	// A foreign origin is refused even with a valid session.
	foreign := h.do(http.MethodPost, "/v1/auth/sign-out", nil, session.unsafeHeaders())
	assert.Equal(t, http.StatusNoContent, foreign.status)

	request := httptest.NewRequest(http.MethodPost, "/v1/auth/sign-out", nil)
	request.Header.Set("Origin", "https://evil.example.com")
	request.Header.Set("Cookie", session.cookie.Name+"="+session.cookie.Value)
	request.Header.Set("X-CSRF-Token", session.csrf)
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusForbidden, recorder.Code)

	// Sign-in failures are generic and eventually throttled.
	errorCode := ""
	for i := 0; i < 10; i++ {
		response := h.do(http.MethodPost, "/v1/auth/sign-in", map[string]any{"email": "ada@example.com", "password": "wrong-password"}, nil)
		errorCode, _ = response.body["code"].(string)
		if response.status == http.StatusTooManyRequests {
			break
		}
		assert.Equal(t, http.StatusUnauthorized, response.status)
		assert.Equal(t, "invalid_credentials", errorCode)
	}
	assert.Equal(t, "throttled", errorCode)
}

func TestForgotAndResetOverHTTP(t *testing.T) {
	h := newAuthAPIHarness(t)
	user := h.activeUser("ada@example.com", "operator", "ada-long-password")
	session := h.signIn("ada@example.com", "ada-long-password", user)

	unknown := h.do(http.MethodPost, "/v1/auth/password/forgot", map[string]any{"email": "nobody@example.com"}, nil)
	assert.Equal(t, http.StatusAccepted, unknown.status)
	known := h.do(http.MethodPost, "/v1/auth/password/forgot", map[string]any{"email": "ada@example.com"}, nil)
	assert.Equal(t, http.StatusAccepted, known.status)

	resetToken := h.link("ada@example.com", identity.AuthLinkResetPassword)
	reset := h.do(http.MethodPost, "/v1/auth/password/reset", map[string]any{"token": resetToken, "password": "replacement-long-password"}, nil)
	require.Equal(t, http.StatusOK, reset.status, "%v", reset.body)
	assert.Equal(t, "reset_complete", reset.body["status"])

	// Every browser session was revoked by the reset.
	stale := h.do(http.MethodGet, "/v1/profile", nil, session.headers())
	assert.Equal(t, http.StatusUnauthorized, stale.status)

	// The new password works and the old one does not.
	assert.Equal(t, http.StatusUnauthorized, h.do(http.MethodPost, "/v1/auth/sign-in", map[string]any{
		"email": "ada@example.com", "password": "ada-long-password",
	}, nil).status)
	fresh := h.do(http.MethodPost, "/v1/auth/sign-in", map[string]any{
		"email": "ada@example.com", "password": "replacement-long-password",
	}, nil)
	require.Equal(t, http.StatusOK, fresh.status, "%v", fresh.body)
	assert.NotNil(t, h.cookieFor(server.DefaultSessionCookie, fresh))
}

func TestProfileUpdateValidation(t *testing.T) {
	h := newAuthAPIHarness(t)
	user := h.activeUser("ada@example.com", "operator", "ada-long-password")
	session := h.signIn("ada@example.com", "ada-long-password", user)

	empty := h.do(http.MethodPatch, "/v1/profile", map[string]any{"displayName": "   "}, session.unsafeHeaders())
	assert.Equal(t, http.StatusBadRequest, empty.status)
	assert.Equal(t, "display_name", empty.body["code"])

	updated := h.do(http.MethodPatch, "/v1/profile", map[string]any{"displayName": "Ada Lovelace", "locale": "pt"}, session.unsafeHeaders())
	require.Equal(t, http.StatusOK, updated.status, "%v", updated.body)
	profile := updated.body["profile"].(map[string]any)
	assert.Equal(t, "Ada Lovelace", profile["displayName"])
	assert.Equal(t, "pt", profile["locale"])
}

func TestSetupLinkStatesOverHTTP(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")

	require.Equal(t, http.StatusAccepted,
		h.do(http.MethodPost, "/v1/auth/request-access", map[string]any{"email": "ada@example.com", "locale": "en"}, nil).status)
	verifyToken := h.link("ada@example.com", identity.AuthLinkVerifyAccess)
	require.Equal(t, http.StatusOK, h.do(http.MethodPost, "/v1/auth/verify", map[string]any{"token": verifyToken}, nil).status)

	request := httptest.NewRequest(http.MethodGet, "/v1/access-requests", nil)
	request.Header.Set("Origin", authTestOrigin)
	request.Header.Set("Cookie", admin.cookie.Name+"="+admin.cookie.Value)
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	var list []map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &list))
	require.Len(t, list, 1)
	h.reauthenticate(admin, "admin-long-password")
	require.Equal(t, http.StatusOK, h.do(http.MethodPost, "/v1/access-requests/"+list[0]["id"].(string)+"/approve",
		map[string]any{"role": "operator"}, admin.unsafeHeaders()).status)

	setupToken := h.link("ada@example.com", identity.AuthLinkSetupPassword)
	require.Equal(t, http.StatusOK, h.do(http.MethodPost, "/v1/auth/setup", map[string]any{
		"token": setupToken, "displayName": "Ada", "locale": "en", "password": "ada-long-password",
	}, nil).status)

	// Reused links are bounded terminal states, and resend is generic.
	reused := h.do(http.MethodPost, "/v1/auth/setup", map[string]any{
		"token": setupToken, "displayName": "Ada", "locale": "en", "password": "other-long-password",
	}, nil)
	assert.Equal(t, http.StatusGone, reused.status)
	assert.Equal(t, "reused", reused.body["code"])

	unknown := h.do(http.MethodPost, "/v1/auth/setup/resend", map[string]any{"token": "malformed-token"}, nil)
	assert.Equal(t, http.StatusAccepted, unknown.status)
}

func TestDeclineIsTerminalOverHTTP(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")

	require.Equal(t, http.StatusAccepted,
		h.do(http.MethodPost, "/v1/auth/request-access", map[string]any{"email": "ada@example.com", "locale": "en"}, nil).status)
	verifyToken := h.link("ada@example.com", identity.AuthLinkVerifyAccess)
	require.Equal(t, http.StatusOK, h.do(http.MethodPost, "/v1/auth/verify", map[string]any{"token": verifyToken}, nil).status)

	request := httptest.NewRequest(http.MethodGet, "/v1/access-requests", nil)
	request.Header.Set("Origin", authTestOrigin)
	request.Header.Set("Cookie", admin.cookie.Name+"="+admin.cookie.Value)
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	var list []map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &list))
	require.Len(t, list, 1)

	h.reauthenticate(admin, "admin-long-password")
	require.Equal(t, http.StatusOK, h.do(http.MethodPost, "/v1/access-requests/"+list[0]["id"].(string)+"/decline",
		nil, admin.unsafeHeaders()).status)

	status := h.do(http.MethodPost, "/v1/auth/request-status", map[string]any{"token": verifyToken}, nil)
	require.Equal(t, http.StatusOK, status.status)
	assert.Equal(t, "declined", status.body["state"])

	// A terminal request permits a new request.
	assert.Equal(t, http.StatusAccepted,
		h.do(http.MethodPost, "/v1/auth/request-access", map[string]any{"email": "ada@example.com", "locale": "en"}, nil).status)
}

func assertSessionCookieContract(t *testing.T, cookie *http.Cookie) {
	t.Helper()
	require.NotNil(t, cookie)
	assert.Equal(t, server.DefaultSessionCookie, cookie.Name)
	assert.True(t, cookie.Secure)
	assert.True(t, cookie.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	assert.Equal(t, "/", cookie.Path)
	assert.Empty(t, cookie.Domain)
}

func TestAuthSessionBootstrapProbe(t *testing.T) {
	h := newAuthAPIHarness(t)

	// Anonymous: 200 with enabled + unauthenticated so the app renders sign-in
	// without a failed network response.
	anonymous := h.do(http.MethodGet, "/v1/auth/session", nil, nil)
	require.Equal(t, http.StatusOK, anonymous.status)
	assert.Equal(t, true, anonymous.body["enabled"])
	assert.NotEqual(t, true, anonymous.body["authenticated"])
	assert.Equal(t, "no-store", anonymous.header.Get("Cache-Control"))

	// Authenticated: the same endpoint returns the profile and a fresh CSRF
	// proof for the session shell.
	user := h.activeUser("ada@example.com", "operator", "ada-long-password")
	session := h.signIn("ada@example.com", "ada-long-password", user)
	authenticated := h.do(http.MethodGet, "/v1/auth/session", nil, session.headers())
	require.Equal(t, http.StatusOK, authenticated.status)
	assert.Equal(t, true, authenticated.body["enabled"])
	assert.Equal(t, true, authenticated.body["authenticated"])
	profile := authenticated.body["profile"].(map[string]any)
	assert.Equal(t, "ada@example.com", profile["email"])
	refreshedCSRF, _ := authenticated.body["csrfToken"].(string)
	require.NotEmpty(t, refreshedCSRF)

	// An expired/revoked cookie is anonymous again, not an error.
	session.csrf = refreshedCSRF
	signOut := h.do(http.MethodPost, "/v1/auth/sign-out", nil, session.unsafeHeaders())
	require.Equal(t, http.StatusNoContent, signOut.status)
	after := h.do(http.MethodGet, "/v1/auth/session", nil, session.headers())
	require.Equal(t, http.StatusOK, after.status)
	assert.NotEqual(t, true, after.body["authenticated"])

	// Bearer authentication is still refused on browser-only routes.
	bearer := h.do(http.MethodGet, "/v1/auth/session", nil, map[string]string{"Authorization": "Bearer not-a-key"})
	assert.Equal(t, http.StatusUnauthorized, bearer.status)
	assert.Equal(t, "bearer_not_accepted", bearer.body["code"])
}

func TestAuthSessionProbeDisabledSurface(t *testing.T) {
	router := server.New(server.Services{})
	request := httptest.NewRequest(http.MethodGet, "/v1/auth/session", nil)
	request.Header.Set("Origin", authTestOrigin)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, false, body["enabled"])
}

func TestSetupContextEndpointDisclosesBoundedContext(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")

	require.Equal(t, http.StatusAccepted,
		h.do(http.MethodPost, "/v1/auth/request-access", map[string]any{"email": "ada@example.com", "locale": "en"}, nil).status)
	verifyToken := h.link("ada@example.com", identity.AuthLinkVerifyAccess)
	require.Equal(t, http.StatusOK, h.do(http.MethodPost, "/v1/auth/verify", map[string]any{"token": verifyToken}, nil).status)
	list := h.list("/v1/access-requests", admin.headers())
	require.Len(t, list, 1)
	h.reauthenticate(admin, "admin-long-password")
	require.Equal(t, http.StatusOK, h.do(http.MethodPost, "/v1/access-requests/"+list[0]["id"].(string)+"/approve",
		map[string]any{"role": "admin"}, admin.unsafeHeaders()).status)

	setupToken := h.link("ada@example.com", identity.AuthLinkSetupPassword)
	context := h.do(http.MethodPost, "/v1/auth/setup/context", map[string]any{"token": setupToken}, nil)
	require.Equal(t, http.StatusOK, context.status, "%v", context.body)
	assert.Equal(t, "ada@example.com", context.body["email"])
	assert.Equal(t, "admin", context.body["role"])

	// The host is the only holder of the link secret, so an unknown token is a
	// bounded invalid state rather than an enumeration oracle.
	unknown := h.do(http.MethodPost, "/v1/auth/setup/context", map[string]any{"token": "not-a-token"}, nil)
	assert.Equal(t, http.StatusBadRequest, unknown.status)
	assert.Equal(t, "invalid", unknown.body["code"])

	require.Equal(t, http.StatusOK, h.do(http.MethodPost, "/v1/auth/setup", map[string]any{
		"token": setupToken, "displayName": "Ada", "locale": "en", "password": "ada-long-password",
	}, nil).status)
	reused := h.do(http.MethodPost, "/v1/auth/setup/context", map[string]any{"token": setupToken}, nil)
	assert.Equal(t, http.StatusGone, reused.status)
	assert.Equal(t, "reused", reused.body["code"])
}

func TestProfileConflictOverHTTP(t *testing.T) {
	h := newAuthAPIHarness(t)
	user := h.activeUser("ada@example.com", "operator", "ada-long-password")
	session := h.signIn("ada@example.com", "ada-long-password", user)

	// The authenticated shell receives a fresh CSRF proof and the row version.
	probe := h.do(http.MethodGet, "/v1/auth/session", nil, session.headers())
	require.Equal(t, http.StatusOK, probe.status)
	updatedAt, _ := probe.body["profile"].(map[string]any)["updatedAt"].(string)
	require.NotEmpty(t, updatedAt)
	csrf, _ := probe.body["csrfToken"].(string)
	session.csrf = csrf

	first := h.do(http.MethodPatch, "/v1/profile", map[string]any{
		"displayName": "Ada One", "locale": "en", "expectedUpdatedAt": updatedAt,
	}, session.unsafeHeaders())
	require.Equal(t, http.StatusOK, first.status, "%v", first.body)

	// The same stale version can no longer overwrite the newer row.
	stale := h.do(http.MethodPatch, "/v1/profile", map[string]any{
		"displayName": "Ada Stale", "locale": "en", "expectedUpdatedAt": updatedAt,
	}, session.unsafeHeaders())
	assert.Equal(t, http.StatusConflict, stale.status)
	assert.Equal(t, "profile_conflict", stale.body["code"])
}
