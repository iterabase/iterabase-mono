package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
)

// Persistent bounded throttles per browser-entry family (architecture 4.3).
var (
	ruleRequestAccessIP    = identity.ThrottleRule{Scope: "request_access_ip", Limit: 10, Window: time.Hour, Block: time.Hour}
	ruleRequestAccessEmail = identity.ThrottleRule{Scope: "request_access_email", Limit: 3, Window: time.Hour, Block: time.Hour}
	ruleVerifyIP           = identity.ThrottleRule{Scope: "verify_ip", Limit: 60, Window: time.Hour, Block: time.Hour}
	ruleSetupIP            = identity.ThrottleRule{Scope: "setup_ip", Limit: 30, Window: time.Hour, Block: time.Hour}
	ruleSignInIP           = identity.ThrottleRule{Scope: "sign_in_ip", Limit: 30, Window: 15 * time.Minute, Block: 15 * time.Minute}
	ruleSignInAccount      = identity.ThrottleRule{Scope: "sign_in_account", Limit: 8, Window: 15 * time.Minute, Block: 15 * time.Minute}
	ruleForgotIP           = identity.ThrottleRule{Scope: "forgot_ip", Limit: 10, Window: time.Hour, Block: time.Hour}
	ruleForgotAccount      = identity.ThrottleRule{Scope: "forgot_account", Limit: 3, Window: time.Hour, Block: time.Hour}
	ruleResetIP            = identity.ThrottleRule{Scope: "reset_ip", Limit: 30, Window: time.Hour, Block: time.Hour}
	ruleReauthAccount      = identity.ThrottleRule{Scope: "reauth_account", Limit: 8, Window: 15 * time.Minute, Block: 15 * time.Minute}
)

type throttleEntry struct {
	rule    identity.ThrottleRule
	subject string
}

// authErrorBody is the bounded customer-safe error contract for the browser
// journey. Codes drive UI state; messages never disclose account facts.
type authErrorBody struct {
	Error             string `json:"error"`
	Code              string `json:"code"`
	RetryAfterSeconds int    `json:"retryAfterSeconds,omitempty"`
}

func authWrite(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeJSON(w, status, v)
}

func authError(w http.ResponseWriter, status int, code, message string) {
	authWrite(w, status, authErrorBody{Error: message, Code: code})
}

func (h *Handler) authReady(w http.ResponseWriter) bool {
	if h.authCfg == nil || !h.authCfg.Enabled || h.store == nil {
		authError(w, http.StatusServiceUnavailable, "auth_unavailable",
			"Browser authentication is temporarily unavailable.")
		return false
	}
	return true
}

// registerAuthRoutes mounts the public browser journey and the cookie-session
// customer APIs. Every route here rejects bearer authentication by design.
func (h *Handler) registerAuthRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.requireOrigin)
		r.Post("/v1/auth/request-access", h.requestAccess)
		r.Post("/v1/auth/verify", h.verifyAccessRequest)
		r.Post("/v1/auth/request-status", h.accessRequestStatus)
		r.Post("/v1/auth/setup", h.completeSetup)
		r.Post("/v1/auth/setup/resend", h.resendSetup)
		r.Post("/v1/auth/setup/context", h.setupContext)
		r.Post("/v1/auth/sign-in", h.signIn)
		r.Post("/v1/auth/password/forgot", h.forgotPassword)
		r.Post("/v1/auth/password/reset", h.resetPassword)
		r.Get("/v1/auth/session", h.authSession)

		r.Group(func(r chi.Router) {
			r.Use(h.requireBrowser)
			r.Get("/v1/profile", h.getProfile)
			r.Get("/v1/sessions", h.listSessions)

			r.Group(func(r chi.Router) {
				r.Use(h.requireCSRF)
				r.Patch("/v1/profile", h.updateProfile)
				r.Post("/v1/auth/sign-out", h.signOut)
				r.Post("/v1/auth/reauthenticate", h.reauthenticate)
				r.Delete("/v1/sessions/{id}", h.revokeSession)
				r.Post("/v1/sessions/revoke-others", h.revokeOtherSessions)
			})

			r.Group(func(r chi.Router) {
				r.Use(h.requireAdmin)
				r.Get("/v1/access-requests", h.listAccessRequests)
				r.Group(func(r chi.Router) {
					r.Use(h.requireCSRF, h.requireRecentAuth)
					r.Post("/v1/access-requests/{id}/approve", h.approveAccessRequest)
					r.Post("/v1/access-requests/{id}/decline", h.declineAccessRequest)
				})
			})
		})
	})
}

// requireOrigin enforces the exact configured origin on unsafe browser
// requests. Safe methods and non-browser clients are unaffected.
func (h *Handler) requireOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A disabled surface reports its customer-safe unavailable state from
		// the handler instead of a misleading origin denial.
		if h.authCfg == nil || !h.authCfg.Enabled {
			next.ServeHTTP(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if !h.authCfg.OriginAllowed(r) {
			authError(w, http.StatusForbidden, "origin", "This request did not come from the configured application origin.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireBrowser resolves the opaque session, resolving current account state,
// and refuses any bearer-authenticated request.
func (h *Handler) requireBrowser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.authReady(w) {
			return
		}
		token, err := h.authCfg.sessionCookie(r)
		if err != nil {
			h.writeSessionError(r, w, err)
			return
		}
		session, err := h.store.ResolveBrowserSessionWithNetwork(r.Context(), token, h.authCfg.now(), h.sessionNetwork(r))
		if err != nil {
			h.writeSessionError(r, w, err)
			return
		}
		ctx := context.WithValue(r.Context(), ctxBrowserSession, session)
		ctx = context.WithValue(ctx, ctxSessionToken, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireCSRF enforces the session-bound CSRF proof on unsafe methods.
func (h *Handler) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		session, ok := sessionFromContext(r.Context())
		if !ok {
			h.writeSessionError(r, w, identity.ErrSessionInvalid)
			return
		}
		if !session.VerifyCSRF(r.Header.Get(csrfHeaderName)) {
			authError(w, http.StatusForbidden, "csrf", "The request could not be verified. Reload the page and try again.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAdmin enforces the current Admin role server-side.
func (h *Handler) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := sessionFromContext(r.Context())
		if !ok || session.Role != RoleAdmin {
			authError(w, http.StatusForbidden, "forbidden", "This area is not available with your current access.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireRecentAuth enforces bounded session-bound recent-password evidence.
func (h *Handler) requireRecentAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := sessionFromContext(r.Context())
		if !ok || !session.HasRecentPassword(h.authCfg.recentAuthTTL(), h.authCfg.now()) {
			authError(w, http.StatusForbidden, "recent_auth_required", "Enter your current password to continue.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeSessionError maps session failures to bounded states and records
// security evidence for expiry/ineligibility.
func (h *Handler) writeSessionError(r *http.Request, w http.ResponseWriter, err error) {
	if errors.Is(err, ErrBearerNotAccepted) {
		authError(w, http.StatusUnauthorized, "bearer_not_accepted",
			"This area requires a browser session; API keys are not accepted here.")
		return
	}
	h.auditSessionRefusal(r, err)
	authError(w, http.StatusUnauthorized, "session_expired", "Your session expired. Sign in again to continue.")
}

// sessionRefusalEvent maps a refused session to the one shared audit event, so
// the refusal-evidence contract cannot drift between callers.
func sessionRefusalEvent(err error, now time.Time) (identity.SecurityEvent, bool) {
	switch {
	case errors.Is(err, identity.ErrSessionExpired):
		return identity.SecurityEvent{
			Event: identity.EventSessionExpired, Outcome: identity.OutcomeDenied, Reason: "expired", CreatedAt: now,
		}, true
	case errors.Is(err, identity.ErrSessionIneligible):
		return identity.SecurityEvent{
			Event: identity.EventSessionResolvedIneligible, Outcome: identity.OutcomeDenied, Reason: "account_not_active", CreatedAt: now,
		}, true
	default:
		return identity.SecurityEvent{}, false
	}
}

// --- public journey ---

// authSession is the always-200 browser-session bootstrap probe. It lets the
// same-origin app learn whether browser authentication is enabled and whether
// the caller already holds a live session without producing failed network
// responses for the disabled/anonymous states.
func (h *Handler) authSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if h.authCfg == nil || !h.authCfg.Enabled || h.store == nil {
		writeJSON(w, http.StatusOK, authSessionResponse{Enabled: false})
		return
	}
	token, err := h.authCfg.sessionCookie(r)
	if err != nil {
		if errors.Is(err, ErrBearerNotAccepted) {
			authError(w, http.StatusUnauthorized, "bearer_not_accepted",
				"This area requires a browser session; API keys are not accepted here.")
			return
		}
		writeJSON(w, http.StatusOK, authSessionResponse{Enabled: true})
		return
	}
	session, err := h.store.ResolveBrowserSessionWithNetwork(r.Context(), token, h.authCfg.now(), h.sessionNetwork(r))
	if err != nil {
		// An expired or ineligible browser session is the anonymous state for
		// the bootstrap probe; the refusal is still audited.
		h.auditSessionRefusal(r, err)
		writeJSON(w, http.StatusOK, authSessionResponse{Enabled: true})
		return
	}
	user, err := h.store.GetLocalUser(r.Context(), session.IdentityID)
	if err != nil {
		writeJSON(w, http.StatusOK, authSessionResponse{Enabled: true})
		return
	}
	csrfRaw, err := h.store.RotateSessionCSRF(r.Context(), session.ID)
	if err != nil {
		writeJSON(w, http.StatusOK, authSessionResponse{Enabled: true})
		return
	}
	profile := profileFor(user)
	profile.RecentPasswordAt = session.RecentPasswordAt
	writeJSON(w, http.StatusOK, authSessionResponse{
		Enabled: true, Authenticated: true, Profile: &profile, CSRFToken: csrfRaw,
	})
}

func (h *Handler) requestAccess(w http.ResponseWriter, r *http.Request) {
	if !h.authReady(w) {
		return
	}
	var req struct {
		Email  string `json:"email"`
		Locale string `json:"locale"`
	}
	if err := decodeJSON(r, &req); err != nil {
		authError(w, http.StatusBadRequest, "invalid_request", "Enter your work email address.")
		return
	}
	delivery, normalized, ok := identity.CanonicalEmail(req.Email)
	if !ok {
		authError(w, http.StatusBadRequest, "invalid_email", "Enter a valid work email address.")
		return
	}
	entries := h.entryThrottles(r, normalized, ruleRequestAccessIP, ruleRequestAccessEmail)
	if h.blocked(w, r, entries) {
		return
	}
	locale := normalizeLocale(req.Locale)
	if _, err := h.store.CreateOrResendAccessRequest(r.Context(), delivery, normalized, locale, h.authCfg.now()); err != nil {
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	if _, _, err := h.record(r, entries); err != nil {
		h.logWarn("recording access-request throttle failed", err)
		authError(w, http.StatusServiceUnavailable, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	authWrite(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (h *Handler) verifyAccessRequest(w http.ResponseWriter, r *http.Request) {
	if !h.authReady(w) {
		return
	}
	if h.throttleOnly(w, r, h.entryThrottles(r, "", ruleVerifyIP)) {
		return
	}
	token, ok := h.tokenBody(w, r)
	if !ok {
		return
	}
	request, err := h.store.VerifyAccessRequest(r.Context(), token, h.authCfg.now())
	switch {
	case errors.Is(err, identity.ErrAuthLinkInvalid):
		authError(w, http.StatusBadRequest, "invalid", "This link is not valid. Request a new verification email.")
	case errors.Is(err, identity.ErrAuthLinkExpired):
		authError(w, http.StatusGone, "expired", "This link expired. Request a new verification email.")
	case errors.Is(err, identity.ErrAuthLinkSuperseded):
		authError(w, http.StatusGone, "superseded", "A newer verification email was sent. Use the newest link.")
	case err != nil:
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
	default:
		authWrite(w, http.StatusOK, accessRequestState(request))
	}
}

func (h *Handler) accessRequestStatus(w http.ResponseWriter, r *http.Request) {
	if !h.authReady(w) {
		return
	}
	token, ok := h.tokenBody(w, r)
	if !ok {
		return
	}
	request, err := h.store.AccessRequestByToken(r.Context(), token)
	switch {
	case errors.Is(err, identity.ErrAuthLinkInvalid):
		authError(w, http.StatusBadRequest, "invalid", "This link is not valid.")
	case errors.Is(err, identity.ErrAuthLinkSuperseded):
		authError(w, http.StatusGone, "superseded", "A newer email was sent. Use the newest link.")
	case err != nil:
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
	default:
		authWrite(w, http.StatusOK, accessRequestState(request))
	}
}

func accessRequestState(request identity.AccessRequest) map[string]string {
	state := "waiting"
	switch request.State {
	case identity.AccessRequestVerificationPending:
		state = "waiting_verification"
	case identity.AccessRequestApprovalPending:
		state = "waiting"
	case identity.AccessRequestApproved:
		state = "approved"
	case identity.AccessRequestDeclined:
		state = "declined"
	case identity.AccessRequestExpired:
		state = "expired"
	}
	return map[string]string{"state": state}
}

func (h *Handler) completeSetup(w http.ResponseWriter, r *http.Request) {
	if !h.authReady(w) {
		return
	}
	var req struct {
		Token       string `json:"token"`
		DisplayName string `json:"displayName"`
		Locale      string `json:"locale"`
		Password    string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		authError(w, http.StatusBadRequest, "invalid_request", "Enter the requested details.")
		return
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		authError(w, http.StatusBadRequest, "display_name", "Enter your name.")
		return
	}
	if h.blocked(w, r, h.entryThrottles(r, "", ruleSetupIP)) {
		return
	}
	_, err := h.store.CompleteSetup(r.Context(), req.Token, req.DisplayName, normalizeLocale(req.Locale), req.Password, h.authCfg.now())
	h.writeAuthLinkError(w, err)
	if err == nil {
		authWrite(w, http.StatusOK, map[string]string{"status": "setup_complete"})
	}
}

func (h *Handler) resendSetup(w http.ResponseWriter, r *http.Request) {
	if !h.authReady(w) {
		return
	}
	token, ok := h.tokenBody(w, r)
	if !ok {
		return
	}
	if h.blocked(w, r, h.entryThrottles(r, "", ruleSetupIP)) {
		return
	}
	// The response is generic whether or not an eligible account exists.
	if _, err := h.store.ResendSetupInstructions(r.Context(), token, h.authCfg.now()); err != nil {
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	authWrite(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// setupContext discloses only the bounded read-only context (verified work
// email and approved role) authorized by a valid first-time setup link.
func (h *Handler) setupContext(w http.ResponseWriter, r *http.Request) {
	if !h.authReady(w) {
		return
	}
	token, ok := h.tokenBody(w, r)
	if !ok {
		return
	}
	context, err := h.store.SetupContext(r.Context(), token, h.authCfg.now())
	h.writeAuthLinkError(w, err)
	if err == nil {
		authWrite(w, http.StatusOK, map[string]string{"email": context.Email, "role": context.Role})
	}
}

func (h *Handler) signIn(w http.ResponseWriter, r *http.Request) {
	if !h.authReady(w) {
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		authError(w, http.StatusBadRequest, "invalid_request", "Enter your email and password.")
		return
	}
	_, normalized, validEmail := identity.CanonicalEmail(req.Email)
	entries := h.entryThrottles(r, normalized, ruleSignInIP, ruleSignInAccount)
	if h.blockedWithEvent(w, r, entries, identity.EventLoginThrottled) {
		return
	}

	now := h.authCfg.now()
	user, err := h.store.FindLocalUserByEmail(r.Context(), normalized)
	if err != nil && !errors.Is(err, identity.ErrNotFound) {
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	eligible := validEmail && err == nil && user.Status == identity.LocalUserActive && user.PasswordHash != ""
	if !eligible {
		identity.DummyPasswordVerify(req.Password)
		h.failedSignIn(r, w, entries, user.ID)
		return
	}
	if !identity.VerifyPassword(user.PasswordHash, req.Password) {
		h.failedSignIn(r, w, entries, user.ID)
		return
	}

	if err := h.clearThrottle(r.Context(), entries); err != nil {
		// The credential was valid; a failed counter cleanup leaves the
		// attempt budget intact and must not turn a successful sign-in into a
		// failure.
		h.logWarn("clearing sign-in throttle failed", err)
	}
	h.finishSignIn(r, w, user, now)
}

func (h *Handler) failedSignIn(r *http.Request, w http.ResponseWriter, entries []throttleEntry, identityID string) {
	blocked, retryAfter, err := h.record(r, entries)
	h.audit(r, identity.SecurityEvent{
		Event:             identity.EventLoginFailed,
		Outcome:           identity.OutcomeFailure,
		Reason:            "invalid_credentials",
		SubjectIdentityID: identityID,
		CredentialKind:    identity.CredentialBrowser,
		CreatedAt:         h.authCfg.now(),
	})
	if err != nil {
		// The attempt could not be durably bounded, so deny rather than allow
		// an unthrottled brute-force path (architecture §14 fail-closed).
		h.logWarn("recording sign-in throttle failed", err)
		authError(w, http.StatusServiceUnavailable, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	if blocked {
		authWrite(w, http.StatusTooManyRequests, authErrorBody{
			Error:             "Too many attempts. Try again shortly.",
			Code:              "throttled",
			RetryAfterSeconds: int(retryAfter.Seconds()) + 1,
		})
		return
	}
	authError(w, http.StatusUnauthorized, "invalid_credentials", "We could not sign you in with those details.")
}

func (h *Handler) finishSignIn(r *http.Request, w http.ResponseWriter, user identity.LocalUser, now time.Time) {
	// Login always replaces any presented pre-login session token.
	if token, err := h.authCfg.sessionCookie(r); err == nil && token != "" {
		_, _ = h.store.RevokeSessionByToken(r.Context(), token, now, identity.TerminationRevoked)
	}
	csrfRaw, csrfHash, err := identity.GenerateSecret(identity.SecretDomainCSRF)
	if err != nil {
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	ip := h.authCfg.ClientIP(r)
	location, _ := h.authCfg.LocationFor(ip)
	audit := h.securityEvent(r, identity.SecurityEvent{
		Event:             identity.EventLoginSucceeded,
		Outcome:           identity.OutcomeSuccess,
		SubjectIdentityID: user.ID,
		CredentialKind:    identity.CredentialBrowser,
		Detail:            map[string]any{"role": identity.NormalizeRole(user.Role)},
		CreatedAt:         now,
	})
	raw, session, err := h.store.CreateBrowserSession(r.Context(), identity.CreateBrowserSessionParams{
		IdentityID:  user.ID,
		CSRFHash:    csrfHash,
		IdleTTL:     h.authCfg.idleTTL(),
		AbsoluteTTL: h.authCfg.absoluteTTL(),
		Now:         now,
		IP:          ip,
		Labels:      identity.ParseClientLabels(r.Header.Get("User-Agent")),
		Location:    location,
		Audit:       &audit,
	})
	if err != nil {
		// The session insert and its evidence commit atomically: a failed
		// audit leaves no session behind.
		h.logWarn("creating browser session failed", err)
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	h.authCfg.setSessionCookie(w, raw, session.AbsoluteExpiresAt)
	authWrite(w, http.StatusOK, signInResponse{
		Profile:   profileFor(user),
		CSRFToken: csrfRaw,
	})
}

func (h *Handler) forgotPassword(w http.ResponseWriter, r *http.Request) {
	if !h.authReady(w) {
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(r, &req); err != nil {
		authError(w, http.StatusBadRequest, "invalid_request", "Enter your work email address.")
		return
	}
	_, normalized, _ := identity.CanonicalEmail(req.Email)
	entries := h.entryThrottles(r, normalized, ruleForgotIP, ruleForgotAccount)
	if h.blocked(w, r, entries) {
		return
	}
	if normalized != "" {
		if _, err := h.store.RequestPasswordReset(r.Context(), normalized, h.authCfg.now()); err != nil {
			authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
			return
		}
	}
	if _, _, err := h.record(r, entries); err != nil {
		h.logWarn("recording reset-request throttle failed", err)
		authError(w, http.StatusServiceUnavailable, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	// The same generic outcome applies to unknown, disabled, pending, and
	// active accounts.
	authWrite(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	if !h.authReady(w) {
		return
	}
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		authError(w, http.StatusBadRequest, "invalid_request", "Enter the requested details.")
		return
	}
	if h.blocked(w, r, h.entryThrottles(r, "", ruleResetIP)) {
		return
	}
	err := h.store.CompletePasswordReset(r.Context(), req.Token, req.Password, h.authCfg.now())
	h.writeAuthLinkError(w, err)
	if err == nil {
		authWrite(w, http.StatusOK, map[string]string{"status": "reset_complete"})
	}
}

// auditSessionRefusal records expiry/ineligibility evidence for probes that
// intentionally return the anonymous state instead of an error.
func (h *Handler) auditSessionRefusal(r *http.Request, err error) {
	if h.store == nil {
		return
	}
	if event, ok := sessionRefusalEvent(err, h.authCfg.now()); ok {
		h.audit(r, event)
	}
}

// --- cookie session ---

func (h *Handler) getProfile(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	csrfRaw, err := h.store.RotateSessionCSRF(r.Context(), session.ID)
	if err != nil {
		h.writeSessionError(r, w, identity.ErrSessionInvalid)
		return
	}
	user, err := h.store.GetLocalUser(r.Context(), session.IdentityID)
	if err != nil {
		h.writeSessionError(r, w, identity.ErrSessionIneligible)
		return
	}
	profile := profileFor(user)
	profile.RecentPasswordAt = session.RecentPasswordAt
	authWrite(w, http.StatusOK, signInResponse{
		Profile:   profile,
		CSRFToken: csrfRaw,
	})
}

func (h *Handler) updateProfile(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	var req struct {
		DisplayName       *string    `json:"displayName"`
		Locale            *string    `json:"locale"`
		ExpectedUpdatedAt *time.Time `json:"expectedUpdatedAt,omitempty"`
	}
	if err := decodeJSON(r, &req); err != nil {
		authError(w, http.StatusBadRequest, "invalid_request", "Enter the requested details.")
		return
	}
	user, err := h.store.GetLocalUser(r.Context(), session.IdentityID)
	if err != nil {
		h.writeSessionError(r, w, identity.ErrSessionIneligible)
		return
	}
	displayName := user.DisplayName
	if req.DisplayName != nil {
		displayName = strings.TrimSpace(*req.DisplayName)
		if displayName == "" || len([]rune(displayName)) > 200 {
			authError(w, http.StatusBadRequest, "display_name", "Enter a name of up to 200 characters.")
			return
		}
	}
	locale := user.Locale
	if req.Locale != nil {
		locale = normalizeLocale(*req.Locale)
	}
	updated, err := h.store.UpdateLocalUserProfileVersioned(r.Context(), session.IdentityID, displayName, locale, req.ExpectedUpdatedAt, h.authCfg.now())
	switch {
	case errors.Is(err, identity.ErrProfileConflict):
		authError(w, http.StatusConflict, "profile_conflict",
			"Your profile changed elsewhere. Reload to see the current values.")
	case err != nil:
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
	default:
		authWrite(w, http.StatusOK, signInResponse{Profile: profileFor(updated)})
	}
}

func (h *Handler) signOut(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	now := h.authCfg.now()
	audit := h.securityEvent(r, identity.SecurityEvent{
		Event:             identity.EventLogout,
		Outcome:           identity.OutcomeSuccess,
		SubjectIdentityID: session.IdentityID,
		BrowserSessionID:  session.ID,
		CredentialKind:    identity.CredentialBrowser,
		CreatedAt:         now,
	})
	if _, err := h.store.RevokeSessionWithAudit(r.Context(), session.ID, now, identity.TerminationRevoked, &audit); err != nil {
		h.logWarn("revoking session on sign-out failed", err)
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	h.authCfg.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) reauthenticate(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		authError(w, http.StatusBadRequest, "invalid_request", "Enter your current password.")
		return
	}
	subject := identity.HashSubject("reauth:" + session.IdentityID)
	entries := []throttleEntry{{rule: ruleReauthAccount, subject: subject}}
	if h.blockedWithEvent(w, r, entries, identity.EventLoginThrottled) {
		return
	}
	user, err := h.store.FindLocalUserByEmail(r.Context(), session.EmailNormalized)
	if err != nil || user.PasswordHash == "" || !identity.VerifyPassword(user.PasswordHash, req.Password) {
		if _, _, recordErr := h.record(r, entries); recordErr != nil {
			h.logWarn("recording reauthentication throttle failed", recordErr)
			authError(w, http.StatusServiceUnavailable, "unavailable", "This is temporarily unavailable. Try again shortly.")
			return
		}
		h.audit(r, identity.SecurityEvent{
			Event:             identity.EventReauthenticationFailed,
			Outcome:           identity.OutcomeFailure,
			Reason:            "invalid_credentials",
			SubjectIdentityID: session.IdentityID,
			BrowserSessionID:  session.ID,
			CredentialKind:    identity.CredentialBrowser,
			CreatedAt:         h.authCfg.now(),
		})
		authError(w, http.StatusUnauthorized, "invalid_credentials", "We could not confirm your password.")
		return
	}
	now := h.authCfg.now()
	audit := h.securityEvent(r, identity.SecurityEvent{
		Event:             identity.EventReauthenticationSucceeded,
		Outcome:           identity.OutcomeSuccess,
		SubjectIdentityID: session.IdentityID,
		BrowserSessionID:  session.ID,
		CredentialKind:    identity.CredentialBrowser,
		CreatedAt:         now,
	})
	if err := h.store.SetRecentPasswordWithAudit(r.Context(), session.ID, now, &audit); err != nil {
		if errors.Is(err, identity.ErrSessionInvalid) {
			h.writeSessionError(r, w, identity.ErrSessionInvalid)
			return
		}
		h.logWarn("recording recent-password evidence failed", err)
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	if err := h.clearThrottle(r.Context(), entries); err != nil {
		h.logWarn("clearing reauthentication throttle failed", err)
	}
	authWrite(w, http.StatusOK, map[string]any{"recentPasswordAt": now})
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	sessions, err := h.store.ListActiveSessions(r.Context(), session.IdentityID, session.ID, h.authCfg.now())
	if err != nil {
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	out := make([]sessionResponse, 0, len(sessions))
	for _, item := range sessions {
		out = append(out, sessionResponse{
			ID:           item.ID,
			Current:      item.ID == session.ID,
			Browser:      item.Browser,
			OS:           item.OS,
			Device:       item.Device,
			Country:      item.LocationCountry,
			Region:       item.LocationRegion,
			CreatedAt:    item.CreatedAt,
			LastActiveAt: item.LastAuthorizedAt,
		})
	}
	authWrite(w, http.StatusOK, out)
}

func (h *Handler) revokeSession(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	target := chi.URLParam(r, "id")
	now := h.authCfg.now()
	audit := h.securityEvent(r, identity.SecurityEvent{
		Event:             identity.EventSessionRevoked,
		Outcome:           identity.OutcomeSuccess,
		SubjectIdentityID: session.IdentityID,
		BrowserSessionID:  target,
		CredentialKind:    identity.CredentialBrowser,
		Detail:            map[string]any{"self": target == session.ID},
		CreatedAt:         now,
	})
	if _, err := h.store.RevokeOwnedSessionWithAudit(r.Context(), session.IdentityID, target, now, identity.TerminationRevoked, &audit); err != nil {
		h.logWarn("revoking own session failed", err)
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	if target == session.ID {
		h.authCfg.clearSessionCookie(w)
	}
	// Idempotent: an already-ended or unknown own session is not an error.
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) revokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	now := h.authCfg.now()
	revoked, err := h.store.RevokeOtherSessionsWithAudit(r.Context(), session.IdentityID, session.ID, now, identity.TerminationRevoked,
		func(revoked int64) *identity.SecurityEvent {
			event := h.securityEvent(r, identity.SecurityEvent{
				Event:             identity.EventSessionRevoked,
				Outcome:           identity.OutcomeSuccess,
				SubjectIdentityID: session.IdentityID,
				BrowserSessionID:  session.ID,
				CredentialKind:    identity.CredentialBrowser,
				Detail:            map[string]any{"others": revoked},
				CreatedAt:         now,
			})
			return &event
		})
	if err != nil {
		h.logWarn("revoking other sessions failed", err)
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	authWrite(w, http.StatusOK, map[string]int64{"revoked": revoked})
}

// --- Admin approval handoff (People administration beyond this is HOR-454) ---

func (h *Handler) listAccessRequests(w http.ResponseWriter, r *http.Request) {
	requests, err := h.store.ListPendingAccessRequests(r.Context())
	if err != nil {
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return
	}
	out := make([]accessRequestResponse, 0, len(requests))
	for _, request := range requests {
		out = append(out, accessRequestResponse{
			ID:         request.ID,
			Email:      request.Email,
			VerifiedAt: request.VerifiedAt,
			CreatedAt:  request.CreatedAt,
		})
	}
	authWrite(w, http.StatusOK, out)
}

func (h *Handler) approveAccessRequest(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	var req struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &req); err != nil {
		authError(w, http.StatusBadRequest, "invalid_request", "Choose Operator or Admin.")
		return
	}
	if req.Role != RoleOperator && req.Role != RoleAdmin {
		authError(w, http.StatusBadRequest, "invalid_role", "Choose Operator or Admin.")
		return
	}
	user, err := h.store.ApproveAccessRequest(r.Context(), chi.URLParam(r, "id"), session.IdentityID, req.Role, h.authCfg.now())
	switch {
	case errors.Is(err, identity.ErrAccessRequestNotFound):
		authError(w, http.StatusNotFound, "not_found", "This request is no longer available.")
	case errors.Is(err, identity.ErrRequestStateChanged):
		authError(w, http.StatusConflict, "state_changed", "This request changed. Refresh to see the current state.")
	case errors.Is(err, identity.ErrAccountExists):
		authError(w, http.StatusConflict, "account_exists", "An account already exists for this email.")
	case err != nil:
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
	default:
		authWrite(w, http.StatusOK, map[string]string{"state": "setup_pending", "role": identity.NormalizeRole(user.Role)})
	}
}

func (h *Handler) declineAccessRequest(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	err := h.store.DeclineAccessRequest(r.Context(), chi.URLParam(r, "id"), session.IdentityID, h.authCfg.now())
	switch {
	case errors.Is(err, identity.ErrAccessRequestNotFound):
		authError(w, http.StatusNotFound, "not_found", "This request is no longer available.")
	case errors.Is(err, identity.ErrRequestStateChanged):
		authError(w, http.StatusConflict, "state_changed", "This request changed. Refresh to see the current state.")
	case err != nil:
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
	default:
		authWrite(w, http.StatusOK, map[string]string{"state": "declined"})
	}
}

// --- helpers ---

type signInResponse struct {
	Profile   profileResponse `json:"profile"`
	CSRFToken string          `json:"csrfToken,omitempty"`
}

type authSessionResponse struct {
	Enabled       bool             `json:"enabled"`
	Authenticated bool             `json:"authenticated,omitempty"`
	Profile       *profileResponse `json:"profile,omitempty"`
	CSRFToken     string           `json:"csrfToken,omitempty"`
}

type profileResponse struct {
	ID               string     `json:"id"`
	Email            string     `json:"email"`
	DisplayName      string     `json:"displayName"`
	Role             string     `json:"role"`
	Locale           string     `json:"locale"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	RecentPasswordAt *time.Time `json:"recentPasswordAt,omitempty"`
}

type sessionResponse struct {
	ID           string    `json:"id"`
	Current      bool      `json:"current"`
	Browser      string    `json:"browser,omitempty"`
	OS           string    `json:"os,omitempty"`
	Device       string    `json:"device,omitempty"`
	Country      string    `json:"country,omitempty"`
	Region       string    `json:"region,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	LastActiveAt time.Time `json:"lastActiveAt"`
}

type accessRequestResponse struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	VerifiedAt *time.Time `json:"verifiedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

func profileFor(user identity.LocalUser) profileResponse {
	return profileResponse{
		ID:          user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		Role:        identity.NormalizeRole(user.Role),
		Locale:      user.Locale,
		UpdatedAt:   user.UpdatedAt,
	}
}

func normalizeLocale(locale string) string {
	if strings.EqualFold(locale, "pt") {
		return "pt"
	}
	return "en"
}

func (h *Handler) tokenBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Token == "" {
		authError(w, http.StatusBadRequest, "invalid", "This link is not valid.")
		return "", false
	}
	return req.Token, true
}

func (h *Handler) writeAuthLinkError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, identity.ErrPasswordPolicy):
		authError(w, http.StatusBadRequest, "password_policy", passwordPolicyMessage())
	case errors.Is(err, identity.ErrAuthLinkInvalid):
		authError(w, http.StatusBadRequest, "invalid", "This link is not valid. Request a new email.")
	case errors.Is(err, identity.ErrAuthLinkExpired):
		authError(w, http.StatusGone, "expired", "This link expired. Request a new email.")
	case errors.Is(err, identity.ErrAuthLinkSuperseded):
		authError(w, http.StatusGone, "superseded", "A newer email was sent. Use the newest link.")
	case errors.Is(err, identity.ErrAuthLinkConsumed):
		authError(w, http.StatusGone, "reused", "This link was already used. Request a new email.")
	case errors.Is(err, identity.ErrAccountNotEligible):
		authError(w, http.StatusForbidden, "ineligible", "This account cannot complete this step.")
	default:
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
	}
}

func passwordPolicyMessage() string {
	return "Use 12 to 128 characters. Common passwords are rejected; there are no composition rules."
}

func (h *Handler) entryThrottles(r *http.Request, normalized string, rules ...identity.ThrottleRule) []throttleEntry {
	entries := make([]throttleEntry, 0, len(rules))
	for _, rule := range rules {
		var subject string
		if strings.HasSuffix(rule.Scope, "_email") || strings.HasSuffix(rule.Scope, "_account") {
			if normalized == "" {
				continue
			}
			subject = identity.HashSubject("email:" + normalized)
		} else {
			ip := h.authCfg.ClientIP(r)
			if ip == nil {
				continue
			}
			subject = identity.HashSubject("ip:" + ip.String())
		}
		entries = append(entries, throttleEntry{rule: rule, subject: subject})
	}
	return entries
}

// throttleOnly checks throttles without recording an attempt.
func (h *Handler) throttleOnly(w http.ResponseWriter, r *http.Request, entries []throttleEntry) bool {
	state, err := h.throttleStatus(r, entries)
	if err != nil {
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return true
	}
	if state.Blocked {
		h.writeThrottled(w, state)
		return true
	}
	return false
}

// blocked checks throttles and returns true when the request must stop.
func (h *Handler) blocked(w http.ResponseWriter, r *http.Request, entries []throttleEntry) bool {
	return h.blockedWithEvent(w, r, entries, identity.EventAuthThrottled)
}

func (h *Handler) blockedWithEvent(w http.ResponseWriter, r *http.Request, entries []throttleEntry, event string) bool {
	state, err := h.throttleStatus(r, entries)
	if err != nil {
		authError(w, http.StatusInternalServerError, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return true
	}
	if state.Blocked {
		h.audit(r, identity.SecurityEvent{
			Event:          event,
			Outcome:        identity.OutcomeDenied,
			Reason:         "throttled",
			CredentialKind: identity.CredentialBrowser,
			CreatedAt:      h.authCfg.now(),
		})
		h.writeThrottled(w, state)
		return true
	}
	return false
}

func (h *Handler) throttleStatus(r *http.Request, entries []throttleEntry) (identity.ThrottleState, error) {
	var blocked identity.ThrottleState
	for _, entry := range entries {
		state, err := h.store.ThrottleStatus(r.Context(), entry.rule, entry.subject, h.authCfg.now())
		if err != nil {
			return identity.ThrottleState{}, err
		}
		if state.Blocked && state.RetryAfter > blocked.RetryAfter {
			blocked = state
		}
	}
	return blocked, nil
}

// record increments every entry and reports whether a limit was crossed. A
// persistence failure is returned so the caller can fail closed instead of
// silently allowing an unbounded attempt budget.
func (h *Handler) record(r *http.Request, entries []throttleEntry) (bool, time.Duration, error) {
	var blocked bool
	var retryAfter time.Duration
	for _, entry := range entries {
		state, err := h.store.RecordAuthAttempt(r.Context(), entry.rule, entry.subject, h.authCfg.now())
		if err != nil {
			return false, 0, err
		}
		if state.Blocked && state.RetryAfter > retryAfter {
			blocked = true
			retryAfter = state.RetryAfter
		}
	}
	return blocked, retryAfter, nil
}

func (h *Handler) clearThrottle(ctx context.Context, entries []throttleEntry) error {
	for _, entry := range entries {
		if err := h.store.ClearAuthThrottle(ctx, entry.rule.Scope, entry.subject); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) writeThrottled(w http.ResponseWriter, state identity.ThrottleState) {
	seconds := int(state.RetryAfter.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	authWrite(w, http.StatusTooManyRequests, authErrorBody{
		Error:             "Too many attempts. Try again shortly.",
		Code:              "throttled",
		RetryAfterSeconds: seconds,
	})
}

// securityEvent completes an event with the bounded network metadata and a
// default outcome, without writing it. Callers committing a security mutation
// pass the result to the store so the mutation and its evidence are one
// transaction.
func (h *Handler) securityEvent(r *http.Request, event identity.SecurityEvent) identity.SecurityEvent {
	if event.Outcome == "" {
		event.Outcome = identity.OutcomeSuccess
	}
	ip := h.authCfg.ClientIP(r)
	if ip != nil {
		network := &identity.SecurityEventNetwork{SourceIP: ip.String()}
		if location, ok := h.authCfg.LocationFor(ip); ok {
			network.LocationCountry = location.Country
			network.LocationRegion = location.Region
		}
		event.Network = network
	}
	return event
}

// sessionNetwork is the caller-derived network evidence for session resolution.
func (h *Handler) sessionNetwork(r *http.Request) identity.SessionNetwork {
	ip := h.authCfg.ClientIP(r)
	location, hasLocation := h.authCfg.LocationFor(ip)
	return identity.SessionNetwork{IP: ip, Location: location, HasLocation: hasLocation}
}

// audit writes best-effort standalone security evidence. Failed-auth and
// throttle evidence intentionally use a separate transaction (architecture
// 11.2); persistence failures are logged, never silently discarded.
func (h *Handler) audit(r *http.Request, event identity.SecurityEvent) {
	if h.store == nil {
		return
	}
	if err := h.store.AppendSecurityEvent(r.Context(), h.securityEvent(r, event)); err != nil {
		h.logWarn("appending security evidence failed: "+event.Event, err)
	}
}

// logWarn records an operational warning through the configured logger.
func (h *Handler) logWarn(message string, err error) {
	if h.authCfg != nil && h.authCfg.Logger != nil {
		h.authCfg.Logger.Warn(message, "error", err)
	}
}
