package identity

import (
	"context"
	"net"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
)

// fakeSender records rendered authentication mail and returns a scripted
// outcome. It never touches the network.
type fakeSender struct {
	mu       sync.Mutex
	messages []AuthEmailMessage
	outcome  AuthEmailSendResult
}

func (f *fakeSender) Send(_ context.Context, message AuthEmailMessage) AuthEmailSendResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, message)
	if f.outcome.Outcome != "" {
		return f.outcome
	}
	return AuthEmailSendResult{Outcome: SendOutcomeAccepted}
}

func (f *fakeSender) setOutcome(outcome AuthEmailSendResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcome = outcome
}

func (f *fakeSender) tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	pattern := regexp.MustCompile(`token=([A-Za-z0-9_-]+)`)
	var out []string
	for _, message := range f.messages {
		if match := pattern.FindStringSubmatch(message.Text); len(match) == 2 {
			out = append(out, match[1])
		}
	}
	return out
}

func (f *fakeSender) lastToken() string {
	tokens := f.tokens()
	if len(tokens) == 0 {
		return ""
	}
	return tokens[len(tokens)-1]
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}

type authHarness struct {
	store  *Store
	sender *fakeSender
	worker *AuthEmailWorker
	now    time.Time
}

func newAuthHarness(t *testing.T) *authHarness {
	t.Helper()
	harness := &authHarness{
		store:  NewStore(testutil.NewPostgresPool(t)),
		sender: &fakeSender{},
		now:    time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC),
	}
	harness.worker = &AuthEmailWorker{
		Store:  harness.store,
		Sender: harness.sender,
		Origin: "https://app.example.com",
		Config: AuthEmailWorkerConfig{Owner: "test-worker", BatchSize: 10, LeaseTTL: time.Minute, MaxAttempts: 3},
		Now:    func() time.Time { return harness.now },
	}
	return harness
}

func (h *authHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *authHarness) deliver(t *testing.T) {
	t.Helper()
	require.NoError(t, h.worker.RunOnce(context.Background()))
}

func (h *authHarness) createAdmin(t *testing.T, email string) LocalUser {
	t.Helper()
	user, err := h.store.UpsertLocalUser(context.Background(), email, email, "admin")
	require.NoError(t, err)
	return user
}

func TestAccessRequestJourney(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	reviewer := h.createAdmin(t, "reviewer@example.com")

	request, err := h.store.CreateOrResendAccessRequest(ctx, "Ada@Example.com", "ada@example.com", "en", h.now)
	require.NoError(t, err)
	assert.Equal(t, AccessRequestVerificationPending, request.State)
	assert.Equal(t, "Ada@Example.com", request.Email)
	assert.False(t, request.PurgeAt.Before(h.now.Add(179*24*time.Hour)), "terminal purge deadline is retained")

	// The API never stores a raw token: the intent only becomes a link when the
	// sender leases it.
	var tokenRows int
	require.NoError(t, h.store.pool.QueryRow(ctx, `SELECT count(*) FROM identity.auth_link_tokens`).Scan(&tokenRows))
	assert.Zero(t, tokenRows)

	h.deliver(t)
	require.Equal(t, 1, h.sender.count())
	verifyToken := h.sender.lastToken()
	require.NotEmpty(t, verifyToken)

	// Verification is consuming and idempotent for the same browser secret.
	verified, err := h.store.VerifyAccessRequest(ctx, verifyToken, h.now)
	require.NoError(t, err)
	assert.Equal(t, AccessRequestApprovalPending, verified.State)
	require.NotNil(t, verified.VerifiedAt)

	again, err := h.store.VerifyAccessRequest(ctx, verifyToken, h.now)
	require.NoError(t, err)
	assert.Equal(t, AccessRequestApprovalPending, again.State)

	// No account exists until an Admin approves.
	_, err = h.store.FindLocalUserByEmail(ctx, "ada@example.com")
	assert.ErrorIs(t, err, ErrNotFound)

	// An unverified request is not Admin-actionable.
	pending, err := h.store.ListPendingAccessRequests(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, request.ID, pending[0].ID)

	// Duplicate/resend after verification is generic and sends no new mail.
	_, err = h.store.CreateOrResendAccessRequest(ctx, "Ada@Example.com", "ada@example.com", "en", h.now)
	require.NoError(t, err)
	h.deliver(t)
	assert.Equal(t, 1, h.sender.count())

	user, err := h.store.ApproveAccessRequest(ctx, request.ID, reviewer.ID, "admin", h.now)
	require.NoError(t, err)
	assert.Equal(t, LocalUserSetupPending, user.Status)
	assert.Equal(t, "admin", user.Role)
	assert.Equal(t, "ada@example.com", user.EmailNormalized)

	h.deliver(t)
	require.Equal(t, 2, h.sender.count())
	setupToken := h.sender.lastToken()

	// Setup completion stores the password and creates no session.
	activated, err := h.store.CompleteSetup(ctx, setupToken, "Ada Lovelace", "en", "correct-horse-battery", h.now)
	require.NoError(t, err)
	assert.Equal(t, LocalUserActive, activated.Status)
	assert.Equal(t, "Ada Lovelace", activated.DisplayName)
	assert.True(t, VerifyPassword(activated.PasswordHash, "correct-horse-battery"))

	var sessions int
	require.NoError(t, h.store.pool.QueryRow(ctx, `SELECT count(*) FROM identity.browser_sessions`).Scan(&sessions))
	assert.Zero(t, sessions, "setup completion must not create a browser session")

	// Reused setup links are refused without replaying the transition.
	_, err = h.store.CompleteSetup(ctx, setupToken, "Ada", "en", "another-long-password", h.now)
	assert.ErrorIs(t, err, ErrAuthLinkConsumed)

	var events int
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT count(*) FROM identity.security_events
		WHERE event IN ('access_request_created','access_request_verified','access_request_approved','setup_completed')`).Scan(&events))
	assert.GreaterOrEqual(t, events, 4)
}

func TestAccessRequestExpiredSupersededAndDeferred(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()

	created, err := h.store.CreateOrResendAccessRequest(ctx, "expired@example.com", "expired@example.com", "pt", h.now)
	require.NoError(t, err)
	// access_requests.created_at is stamped by the database clock rather than
	// the harness clock. Align to that durable instant before issuing the link
	// so the 24 h expiry advance below is deterministic whenever the suite runs.
	if created.CreatedAt.After(h.now) {
		h.now = created.CreatedAt.UTC()
	}
	h.deliver(t)
	expiredToken := h.sender.lastToken()

	// The link lifetime is measured from issuance, and the request lifetime
	// from the durable created_at; 25 h crosses both 24 h bounds.
	h.advance(25 * time.Hour)
	_, err = h.store.VerifyAccessRequest(ctx, expiredToken, h.now)
	assert.ErrorIs(t, err, ErrAuthLinkExpired)

	expired, err := h.store.ExpireAccessRequests(ctx, h.now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), expired)
	request, err := h.store.AccessRequestByToken(ctx, expiredToken)
	require.NoError(t, err)
	assert.Equal(t, AccessRequestExpired, request.State)

	// A terminal request permits a new one, and a resend supersedes the prior
	// link while the request is still pending verification.
	_, err = h.store.CreateOrResendAccessRequest(ctx, "second@example.com", "second@example.com", "en", h.now)
	require.NoError(t, err)
	h.deliver(t)
	firstToken := h.sender.lastToken()
	_, err = h.store.CreateOrResendAccessRequest(ctx, "second@example.com", "second@example.com", "en", h.now)
	require.NoError(t, err)
	h.deliver(t)
	secondToken := h.sender.lastToken()
	require.NotEqual(t, firstToken, secondToken)

	_, err = h.store.VerifyAccessRequest(ctx, firstToken, h.now)
	assert.ErrorIs(t, err, ErrAuthLinkSuperseded)
	verified, err := h.store.VerifyAccessRequest(ctx, secondToken, h.now)
	require.NoError(t, err)
	assert.Equal(t, AccessRequestApprovalPending, verified.State)
}

func TestAccessRequestApprovalGuards(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	reviewer := h.createAdmin(t, "reviewer@example.com")

	_, err := h.store.CreateOrResendAccessRequest(ctx, "dup@example.com", "dup@example.com", "en", h.now)
	require.NoError(t, err)
	h.deliver(t)
	_, err = h.store.VerifyAccessRequest(ctx, h.sender.lastToken(), h.now)
	require.NoError(t, err)
	pending, err := h.store.ListPendingAccessRequests(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	// An existing canonical account blocks approval instead of duplicating it.
	_, err = h.store.UpsertLocalUser(ctx, "dup@example.com", "dup@example.com", "operator")
	require.NoError(t, err)
	_, err = h.store.ApproveAccessRequest(ctx, pending[0].ID, reviewer.ID, "admin", h.now)
	assert.ErrorIs(t, err, ErrAccountExists)

	// Decline is terminal and does not create a person.
	require.NoError(t, h.store.DeclineAccessRequest(ctx, pending[0].ID, reviewer.ID, h.now))
	_, err = h.store.ApproveAccessRequest(ctx, pending[0].ID, reviewer.ID, "admin", h.now)
	assert.ErrorIs(t, err, ErrRequestStateChanged)
	_, err = h.store.ApproveAccessRequest(ctx, "00000000-0000-0000-0000-000000000000", reviewer.ID, "admin", h.now)
	assert.ErrorIs(t, err, ErrAccessRequestNotFound)
}

func TestPasswordResetSemantics(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()

	// Unknown and ineligible accounts are generic no-ops with no mail.
	queued, err := h.store.RequestPasswordReset(ctx, "unknown@example.com", h.now)
	require.NoError(t, err)
	assert.False(t, queued)

	user, err := h.store.UpsertLocalUser(ctx, "ada@example.com", "ada@example.com", "operator")
	require.NoError(t, err)
	// Setup-complete the account so it is eligible.
	token := mustSetupToken(t, h.store, user.ID, h.now)
	_, err = h.store.CompleteSetup(ctx, token, "Ada", "en", "correct-horse-battery", h.now)
	require.NoError(t, err)
	active, err := h.store.FindLocalUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)

	ip := net.ParseIP("203.0.113.7")
	_, _, err = h.store.CreateBrowserSession(ctx, CreateBrowserSessionParams{
		IdentityID: active.ID, CSRFHash: HashSecret(SecretDomainCSRF, "csrf"),
		IdleTTL: 12 * time.Hour, AbsoluteTTL: 30 * 24 * time.Hour, Now: h.now, IP: ip,
		Labels: ClientLabels{Browser: BrowserChrome, OS: OSMacOS, Device: DeviceDesktop},
	})
	require.NoError(t, err)
	keyFull, _, err := h.store.CreateAPIKey(ctx, active.ID, "automation", ScopeWork, nil)
	require.NoError(t, err)

	queued, err = h.store.RequestPasswordReset(ctx, "ada@example.com", h.now)
	require.NoError(t, err)
	assert.True(t, queued)

	// A reset request changes neither password nor sessions.
	unchanged, err := h.store.FindLocalUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)
	assert.Equal(t, active.PasswordHash, unchanged.PasswordHash)
	sessions, err := h.store.ListActiveSessions(ctx, active.ID, "", h.now)
	require.NoError(t, err)
	assert.Len(t, sessions, 1)

	h.deliver(t)
	resetToken := h.sender.lastToken()
	require.NotEmpty(t, resetToken)

	// Policy is enforced before any state change.
	err = h.store.CompletePasswordReset(ctx, resetToken, "password12345", h.now)
	assert.ErrorIs(t, err, ErrPasswordPolicy)

	require.NoError(t, h.store.CompletePasswordReset(ctx, resetToken, "another-long-password", h.now))
	updated, err := h.store.FindLocalUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)
	assert.False(t, VerifyPassword(updated.PasswordHash, "correct-horse-battery"))
	assert.True(t, VerifyPassword(updated.PasswordHash, "another-long-password"))

	// Successful reset revokes every browser session and no API key.
	sessions, err = h.store.ListActiveSessions(ctx, active.ID, "", h.now)
	require.NoError(t, err)
	assert.Empty(t, sessions)
	_, _, err = h.store.ValidateAPIKey(ctx, keyFull)
	assert.NoError(t, err, "password reset must not implicitly revoke API keys")

	// The reset link is one-time.
	err = h.store.CompletePasswordReset(ctx, resetToken, "third-long-password-x", h.now)
	assert.ErrorIs(t, err, ErrAuthLinkConsumed)
}

func mustSetupToken(t *testing.T, store *Store, identityID string, now time.Time) string {
	t.Helper()
	ctx := context.Background()
	outboxID, err := queueIntentForTest(ctx, store, AuthEmailIntent{
		Purpose:             AuthLinkSetupPassword,
		RecipientEmail:      "ada@example.com",
		RecipientIdentityID: identityID,
		Locale:              "en",
	})
	require.NoError(t, err)
	return mustTokenForOutbox(t, store, outboxID, now)
}

func queueIntentForTest(ctx context.Context, store *Store, intent AuthEmailIntent) (string, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, err := QueueAuthEmailTx(ctx, tx, intent)
	if err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

func mustTokenForOutbox(t *testing.T, store *Store, outboxID string, now time.Time) string {
	t.Helper()
	ctx := context.Background()
	claimed, err := store.ClaimAuthEmails(ctx, "test", 1, time.Minute, now)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	_, raw, err := store.CreateAuthLinkForIntent(ctx, claimed[0].OutboxID, now)
	require.NoError(t, err)
	return raw
}

func TestBrowserSessionLifecycle(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	user, err := h.store.UpsertLocalUser(ctx, "ada@example.com", "ada@example.com", "operator")
	require.NoError(t, err)
	token := mustSetupToken(t, h.store, user.ID, h.now)
	_, err = h.store.CompleteSetup(ctx, token, "Ada", "en", "correct-horse-battery", h.now)
	require.NoError(t, err)
	active, err := h.store.FindLocalUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)

	csrfHash := HashSecret(SecretDomainCSRF, "csrf-secret")
	raw, session, err := h.store.CreateBrowserSession(ctx, CreateBrowserSessionParams{
		IdentityID: active.ID, CSRFHash: csrfHash, IdleTTL: 12 * time.Hour, AbsoluteTTL: 30 * 24 * time.Hour,
		Now: h.now, IP: net.ParseIP("198.51.100.4"),
		Labels:   ClientLabels{Browser: BrowserFirefox, OS: OSLinux, Device: DeviceDesktop},
		Location: Location{Country: "PT", Region: "11"},
	})
	require.NoError(t, err)
	assert.True(t, session.VerifyCSRF("csrf-secret"))
	assert.False(t, session.VerifyCSRF("other-secret"))
	assert.False(t, session.VerifyCSRF(""))

	resolved, err := h.store.ResolveBrowserSession(ctx, raw, h.now)
	require.NoError(t, err)
	assert.Equal(t, "operator", resolved.Role)
	assert.Equal(t, active.Status, resolved.Status)
	assert.Equal(t, BrowserFirefox, resolved.Browser)
	assert.Equal(t, "PT", resolved.LocationCountry)

	// CSRF rotation invalidates the previous proof.
	rotated, err := h.store.RotateSessionCSRF(ctx, session.ID)
	require.NoError(t, err)
	resolved, err = h.store.ResolveBrowserSession(ctx, raw, h.now)
	require.NoError(t, err)
	assert.True(t, resolved.VerifyCSRF(rotated))
	assert.False(t, resolved.VerifyCSRF("csrf-secret"))

	// Recent-password evidence is bounded and session-bound.
	require.NoError(t, h.store.SetRecentPassword(ctx, session.ID, h.now))
	resolved, err = h.store.ResolveBrowserSession(ctx, raw, h.now)
	require.NoError(t, err)
	assert.True(t, resolved.HasRecentPassword(15*time.Minute, h.now))
	assert.False(t, resolved.HasRecentPassword(15*time.Minute, h.now.Add(16*time.Minute)))

	// A second session lets the caller revoke only the others.
	secondRaw, second, err := h.store.CreateBrowserSession(ctx, CreateBrowserSessionParams{
		IdentityID: active.ID, CSRFHash: csrfHash, IdleTTL: 12 * time.Hour, AbsoluteTTL: 30 * 24 * time.Hour, Now: h.now,
	})
	require.NoError(t, err)
	listed, err := h.store.ListActiveSessions(ctx, active.ID, session.ID, h.now)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	assert.Equal(t, session.ID, listed[0].ID, "current session is listed first")

	revoked, err := h.store.RevokeOtherSessions(ctx, active.ID, session.ID, h.now, TerminationRevoked)
	require.NoError(t, err)
	assert.Equal(t, int64(1), revoked)
	_, err = h.store.ResolveBrowserSession(ctx, secondRaw, h.now)
	assert.ErrorIs(t, err, ErrSessionInvalid)
	_ = second

	// Own-session revocation never matches another identity.
	other, err := h.store.UpsertLocalUser(ctx, "other@example.com", "other@example.com", "admin")
	require.NoError(t, err)
	owned, err := h.store.RevokeOwnedSession(ctx, other.ID, session.ID, h.now, TerminationRevoked)
	require.NoError(t, err)
	assert.False(t, owned)
	stillLive, err := h.store.ResolveBrowserSession(ctx, raw, h.now)
	require.NoError(t, err)
	assert.Equal(t, session.ID, stillLive.ID)

	// Idle expiry terminates the row and denies the next request.
	h.advance(13 * time.Hour)
	_, err = h.store.ResolveBrowserSession(ctx, raw, h.now)
	assert.ErrorIs(t, err, ErrSessionExpired)
	var reason string
	require.NoError(t, h.store.pool.QueryRow(ctx, `SELECT termination_reason FROM identity.browser_sessions WHERE id = $1`, session.ID).Scan(&reason))
	assert.Equal(t, TerminationIdleExpired, reason)

	// Account disablement terminates a live session with account_revoked.
	thirdRaw, _, err := h.store.CreateBrowserSession(ctx, CreateBrowserSessionParams{
		IdentityID: active.ID, CSRFHash: csrfHash, IdleTTL: 12 * time.Hour, AbsoluteTTL: 30 * 24 * time.Hour, Now: h.now,
	})
	require.NoError(t, err)
	_, err = h.store.pool.Exec(ctx, `UPDATE identity.local_users SET status = 'disabled' WHERE identity_id = $1`, active.ID)
	require.NoError(t, err)
	_, err = h.store.ResolveBrowserSession(ctx, thirdRaw, h.now)
	assert.ErrorIs(t, err, ErrSessionIneligible)
	require.NoError(t, h.store.pool.QueryRow(ctx, `SELECT termination_reason FROM identity.browser_sessions WHERE token_hash = $1`,
		HashSecret(SecretDomainSessionToken, thirdRaw)).Scan(&reason))
	assert.Equal(t, TerminationAccountRevoked, reason)
}

func TestSessionAbsoluteExpiryAndNetworkPurge(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	user, err := h.store.UpsertLocalUser(ctx, "ada@example.com", "ada@example.com", "operator")
	require.NoError(t, err)
	token := mustSetupToken(t, h.store, user.ID, h.now)
	_, err = h.store.CompleteSetup(ctx, token, "Ada", "en", "correct-horse-battery", h.now)
	require.NoError(t, err)
	active, err := h.store.FindLocalUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)

	raw, _, err := h.store.CreateBrowserSession(ctx, CreateBrowserSessionParams{
		IdentityID: active.ID, CSRFHash: "h", IdleTTL: 12 * time.Hour, AbsoluteTTL: 30 * 24 * time.Hour,
		Now: h.now, IP: net.ParseIP("198.51.100.9"),
	})
	require.NoError(t, err)

	// Keep the session active past its idle window; the absolute bound wins.
	var expiryErr error
	for i := 0; i < 70; i++ {
		h.advance(11 * time.Hour)
		if _, err := h.store.ResolveBrowserSession(ctx, raw, h.now); err != nil {
			expiryErr = err
			break
		}
	}
	require.Error(t, expiryErr)
	assert.ErrorIs(t, expiryErr, ErrSessionExpired)
	var reason string
	require.NoError(t, h.store.pool.QueryRow(ctx, `SELECT termination_reason FROM identity.browser_sessions WHERE identity_id = $1`, active.ID).Scan(&reason))
	assert.Equal(t, TerminationAbsoluteExpired, reason)

	// Raw network evidence is purged no later than 30 days after termination.
	h.advance(31 * 24 * time.Hour)
	purged, err := h.store.PurgeSessionNetworkEvidence(ctx, h.now.Add(-30*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), purged)
	var createdIP, lastIP *string
	require.NoError(t, h.store.pool.QueryRow(ctx, `SELECT host(created_ip), host(last_ip) FROM identity.browser_sessions WHERE identity_id = $1`, active.ID).Scan(&createdIP, &lastIP))
	assert.Nil(t, createdIP)
	assert.Nil(t, lastIP)
}

func TestThrottlePersistsBlocksAndClears(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	rule := ThrottleRule{Scope: "test_scope", Limit: 3, Window: time.Hour, Block: time.Hour}
	subject := HashSubject("subject")

	state, err := h.store.RecordAuthAttempt(ctx, rule, subject, h.now)
	require.NoError(t, err)
	assert.False(t, state.Blocked)
	for i := 0; i < 2; i++ {
		state, err = h.store.RecordAuthAttempt(ctx, rule, subject, h.now)
		require.NoError(t, err)
	}
	assert.True(t, state.Blocked, "third attempt crosses the limit")
	assert.Positive(t, state.RetryAfter)

	status, err := h.store.ThrottleStatus(ctx, rule, subject, h.now)
	require.NoError(t, err)
	assert.True(t, status.Blocked)

	// The block expires and the window resets.
	h.advance(2 * time.Hour)
	status, err = h.store.ThrottleStatus(ctx, rule, subject, h.now)
	require.NoError(t, err)
	assert.False(t, status.Blocked)

	state, err = h.store.RecordAuthAttempt(ctx, rule, subject, h.now)
	require.NoError(t, err)
	assert.False(t, state.Blocked)

	require.NoError(t, h.store.ClearAuthThrottle(ctx, rule.Scope, subject))
	status, err = h.store.ThrottleStatus(ctx, rule, subject, h.now)
	require.NoError(t, err)
	assert.False(t, status.Blocked)

	// Pruning removes only untracked, unblocked counters.
	_, err = h.store.RecordAuthAttempt(ctx, rule, HashSubject("other"), h.now)
	require.NoError(t, err)
	pruned, err := h.store.PruneAuthThrottle(ctx, h.now.Add(48*time.Hour), h.now)
	require.NoError(t, err)
	assert.Positive(t, pruned)
}

func TestAuthEmailOutboxOutcomes(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()

	_, err := h.store.CreateOrResendAccessRequest(ctx, "ada@example.com", "ada@example.com", "en", h.now)
	require.NoError(t, err)

	// A definite pre-acceptance failure returns the intent to pending.
	h.sender.setOutcome(AuthEmailSendResult{Outcome: SendOutcomeRetryable, ErrorClass: "connect"})
	h.deliver(t)
	var state string
	require.NoError(t, h.store.pool.QueryRow(ctx, `SELECT state FROM identity.auth_email_outbox`).Scan(&state))
	assert.Equal(t, AuthEmailPending, state)

	// The next attempt supersedes the unusable link and succeeds.
	h.sender.setOutcome(AuthEmailSendResult{})
	h.deliver(t)
	require.NoError(t, h.store.pool.QueryRow(ctx, `SELECT state FROM identity.auth_email_outbox`).Scan(&state))
	assert.Equal(t, AuthEmailAccepted, state)
	token := h.sender.lastToken()
	_, err = h.store.VerifyAccessRequest(ctx, token, h.now)
	require.NoError(t, err)

	// Ambiguous acceptance is explicit and never silently retried.
	_, err = h.store.CreateOrResendAccessRequest(ctx, "unknown@example.com", "unknown@example.com", "en", h.now)
	require.NoError(t, err)
	h.sender.setOutcome(AuthEmailSendResult{Outcome: SendOutcomeUnknown, ErrorClass: "write"})
	h.deliver(t)
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT state FROM identity.auth_email_outbox WHERE recipient_email = 'unknown@example.com'`).Scan(&state))
	assert.Equal(t, AuthEmailOutcomeUnknown, state)

	// An overdue lease becomes outcome_unknown instead of a duplicate send.
	_, err = h.store.CreateOrResendAccessRequest(ctx, "lease@example.com", "lease@example.com", "en", h.now)
	require.NoError(t, err)
	_, err = h.store.ClaimAuthEmails(ctx, "worker-a", 1, -time.Minute, h.now)
	require.NoError(t, err)
	h.advance(time.Minute)
	h.deliver(t)
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT state FROM identity.auth_email_outbox WHERE recipient_email = 'lease@example.com'`).Scan(&state))
	assert.Equal(t, AuthEmailOutcomeUnknown, state)
}

func TestBootstrapAndRecovery(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()

	result, err := h.store.BootstrapAdmin(ctx, BootstrapOptions{AdminEmail: "Admin@Example.com", AdminLocale: "pt", Now: h.now})
	require.NoError(t, err)
	assert.Equal(t, BootstrapCreated, result.State)
	assert.Equal(t, "Admin@example.com", result.AdminEmail)

	admin, err := h.store.FindLocalUserByEmail(ctx, "admin@example.com")
	require.NoError(t, err)
	assert.Equal(t, "admin", admin.Role)
	assert.Equal(t, LocalUserSetupPending, admin.Status)
	assert.Equal(t, "pt", admin.Locale)

	var marker string
	require.NoError(t, h.store.pool.QueryRow(ctx, `SELECT admin_email FROM identity.bootstrap_marker`).Scan(&marker))
	assert.Equal(t, "Admin@example.com", marker)

	// Restart is a strict no-op and never prints a credential.
	again, err := h.store.BootstrapAdmin(ctx, BootstrapOptions{AdminEmail: "Admin@Example.com", AdminLocale: "pt", Now: h.now.Add(time.Hour)})
	require.NoError(t, err)
	assert.Equal(t, BootstrapNoop, again.State)

	// Inconsistent marker/account state fails closed.
	_, err = h.store.pool.Exec(ctx, `DELETE FROM identity.local_users`)
	require.NoError(t, err)
	_, err = h.store.BootstrapAdmin(ctx, BootstrapOptions{AdminEmail: "Admin@Example.com", Now: h.now})
	assert.ErrorIs(t, err, ErrBootstrapInconsistent)
	_, err = h.store.pool.Exec(ctx, `DELETE FROM identity.bootstrap_marker`)
	require.NoError(t, err)

	// Recovery is refused while an active Admin exists.
	activeAdmin, err := h.store.UpsertLocalUser(ctx, "active@example.com", "active@example.com", "admin")
	require.NoError(t, err)
	_, err = h.store.pool.Exec(ctx, `UPDATE identity.local_users SET status = 'active' WHERE identity_id = $1`, activeAdmin.ID)
	require.NoError(t, err)
	_, err = h.store.BootstrapAdmin(ctx, BootstrapOptions{AdminEmail: "active@example.com", Recover: true, Now: h.now})
	assert.ErrorIs(t, err, ErrRecoveryNotPermitted)

	// With no active Admin the recovery converts the named human, clears the
	// password, revokes sessions and keys, and queues normal setup mail.
	_, err = h.store.pool.Exec(ctx, `UPDATE identity.local_users SET status = 'disabled'`)
	require.NoError(t, err)
	_, _, err = h.store.CreateAPIKey(ctx, activeAdmin.ID, "legacy", ScopeWork, nil)
	require.NoError(t, err)
	_, _, err = h.store.CreateBrowserSession(ctx, CreateBrowserSessionParams{
		IdentityID: activeAdmin.ID, CSRFHash: "h", IdleTTL: time.Hour, AbsoluteTTL: 24 * time.Hour, Now: h.now,
	})
	require.NoError(t, err)

	recovered, err := h.store.BootstrapAdmin(ctx, BootstrapOptions{AdminEmail: "active@example.com", Recover: true, Now: h.now})
	require.NoError(t, err)
	assert.Equal(t, BootstrapRecovered, recovered.State)
	recoveredUser, err := h.store.FindLocalUserByEmail(ctx, "active@example.com")
	require.NoError(t, err)
	assert.Equal(t, LocalUserSetupPending, recoveredUser.Status)
	assert.Empty(t, recoveredUser.PasswordHash)
	sessions, err := h.store.ListActiveSessions(ctx, activeAdmin.ID, "", h.now)
	require.NoError(t, err)
	assert.Empty(t, sessions)

	// The recovery queued exactly one setup intent and delivery produces a link.
	h.deliver(t)
	setupToken := h.sender.lastToken()
	require.NotEmpty(t, setupToken)
	activated, err := h.store.CompleteSetup(ctx, setupToken, "Recovered Admin", "en", "recovered-long-password", h.now)
	require.NoError(t, err)
	assert.Equal(t, LocalUserActive, activated.Status)
	assert.Equal(t, "admin", activated.Role)
}

func TestSessionResolveTouchesCoalescedActivity(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	user, err := h.store.UpsertLocalUser(ctx, "ada@example.com", "ada@example.com", "operator")
	require.NoError(t, err)
	token := mustSetupToken(t, h.store, user.ID, h.now)
	_, err = h.store.CompleteSetup(ctx, token, "Ada", "en", "correct-horse-battery", h.now)
	require.NoError(t, err)
	active, err := h.store.FindLocalUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)

	raw, _, err := h.store.CreateBrowserSession(ctx, CreateBrowserSessionParams{
		IdentityID: active.ID, CSRFHash: "h", IdleTTL: 12 * time.Hour, AbsoluteTTL: 30 * 24 * time.Hour, Now: h.now,
	})
	require.NoError(t, err)

	// Inside the coalescing window nothing changes.
	_, err = h.store.ResolveBrowserSession(ctx, raw, h.now.Add(time.Minute))
	require.NoError(t, err)
	var lastAuthorized time.Time
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT last_authorized_at FROM identity.browser_sessions WHERE identity_id = $1`, active.ID).Scan(&lastAuthorized))
	assert.WithinDuration(t, h.now, lastAuthorized, time.Second)

	// Past the window the idle bound advances.
	_, err = h.store.ResolveBrowserSession(ctx, raw, h.now.Add(sessionActivityCoalesce+time.Minute))
	require.NoError(t, err)
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT last_authorized_at FROM identity.browser_sessions WHERE identity_id = $1`, active.ID).Scan(&lastAuthorized))
	assert.WithinDuration(t, h.now.Add(sessionActivityCoalesce+time.Minute), lastAuthorized, time.Second)
}

func TestSecurityAuditNeverCarriesRawSecrets(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	password := "correct-horse-battery"
	reviewer := h.createAdmin(t, "reviewer@example.com")

	_, err := h.store.CreateOrResendAccessRequest(ctx, "ada@example.com", "ada@example.com", "en", h.now)
	require.NoError(t, err)
	h.deliver(t)
	verifyToken := h.sender.lastToken()
	_, err = h.store.VerifyAccessRequest(ctx, verifyToken, h.now)
	require.NoError(t, err)
	pending, err := h.store.ListPendingAccessRequests(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	_, err = h.store.ApproveAccessRequest(ctx, pending[0].ID, reviewer.ID, "operator", h.now)
	require.NoError(t, err)
	h.deliver(t)
	setupToken := h.sender.lastToken()
	_, err = h.store.CompleteSetup(ctx, setupToken, "Ada", "en", password, h.now)
	require.NoError(t, err)

	// Sign in and revoke so session/login evidence also exists.
	active, err := h.store.FindLocalUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)
	csrfHash := HashSecret(SecretDomainCSRF, "csrf")
	raw, session, err := h.store.CreateBrowserSession(ctx, CreateBrowserSessionParams{
		IdentityID: active.ID, CSRFHash: csrfHash, IdleTTL: time.Hour, AbsoluteTTL: 24 * time.Hour, Now: h.now,
	})
	require.NoError(t, err)
	_, err = h.store.ResolveBrowserSession(ctx, raw, h.now)
	require.NoError(t, err)
	_, err = h.store.RevokeSession(ctx, session.ID, h.now, TerminationRevoked)
	require.NoError(t, err)

	_, err = h.store.RequestPasswordReset(ctx, "ada@example.com", h.now)
	require.NoError(t, err)
	h.deliver(t)
	resetToken := h.sender.lastToken()
	require.NoError(t, h.store.CompletePasswordReset(ctx, resetToken, "another-long-password", h.now))

	rows, err := h.store.pool.Query(ctx, `SELECT event, COALESCE(reason, ''), detail::text FROM identity.security_events`)
	require.NoError(t, err)
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var event, reason, detail string
		require.NoError(t, rows.Scan(&event, &reason, &detail))
		seen++
		for _, secret := range []string{password, verifyToken, setupToken, resetToken, raw, "csrf"} {
			assert.NotContains(t, detail, secret, "event %s detail leaked a secret", event)
			assert.NotContains(t, reason, secret, "event %s reason leaked a secret", event)
		}
	}
	require.NoError(t, rows.Err())
	assert.Greater(t, seen, 5, "expected the journey to append security evidence")

	// Raw one-time and session material is never persisted: only
	// domain-separated hashes exist.
	for name, rawSecret := range map[string]string{"auth link": verifyToken, "session": raw} {
		var stored int
		if name == "auth link" {
			require.NoError(t, h.store.pool.QueryRow(ctx, `
				SELECT count(*) FROM identity.auth_link_tokens WHERE token_hash = $1`, rawSecret).Scan(&stored))
		} else {
			require.NoError(t, h.store.pool.QueryRow(ctx, `
				SELECT count(*) FROM identity.browser_sessions WHERE token_hash = $1`, rawSecret).Scan(&stored))
		}
		assert.Zero(t, stored, "%s raw value must not be persisted", name)
	}
}

func TestSetupContextBoundedDisclosure(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	user, err := h.store.UpsertLocalUser(ctx, "ada@example.com", "ada@example.com", "operator")
	require.NoError(t, err)
	token := mustSetupToken(t, h.store, user.ID, h.now)

	context, err := h.store.SetupContext(ctx, token, h.now)
	require.NoError(t, err)
	assert.Equal(t, "ada@example.com", context.Email)
	assert.Equal(t, "operator", context.Role)

	_, err = h.store.SetupContext(ctx, "not-a-token", h.now)
	assert.ErrorIs(t, err, ErrAuthLinkInvalid)

	_, err = h.store.CompleteSetup(ctx, token, "Ada", "en", "correct-horse-battery", h.now)
	require.NoError(t, err)
	_, err = h.store.SetupContext(ctx, token, h.now)
	assert.ErrorIs(t, err, ErrAuthLinkConsumed)

	// A disabled account cannot disclose its context even with a live link.
	other, err := h.store.UpsertLocalUser(ctx, "bob@example.com", "bob@example.com", "operator")
	require.NoError(t, err)
	otherToken := mustSetupToken(t, h.store, other.ID, h.now)
	_, err = h.store.pool.Exec(ctx, `UPDATE identity.local_users SET status = 'disabled' WHERE identity_id = $1`, other.ID)
	require.NoError(t, err)
	_, err = h.store.SetupContext(ctx, otherToken, h.now)
	assert.ErrorIs(t, err, ErrAccountNotEligible)
}

// TestSetupContextExpiryFollowsTheRequestClock pins the HOR-610 contract: setup
// link expiry follows the caller's clock, so the suite can advance past the
// lifetime instead of depending on when it runs. A wall-clock read in this path
// would ignore the advance and return the context, failing this assertion.
func TestSetupContextExpiryFollowsTheRequestClock(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	user, err := h.store.UpsertLocalUser(ctx, "expiry@example.com", "expiry@example.com", "operator")
	require.NoError(t, err)
	token := mustSetupToken(t, h.store, user.ID, h.now)

	context, err := h.store.SetupContext(ctx, token, h.now)
	require.NoError(t, err)
	assert.Equal(t, "expiry@example.com", context.Email)

	h.advance(SetupPasswordLinkTTL + time.Minute)
	_, err = h.store.SetupContext(ctx, token, h.now)
	assert.ErrorIs(t, err, ErrAuthLinkExpired)
}

func TestSessionNetworkCoalescingAndAuditAtomicity(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	user, err := h.store.UpsertLocalUser(ctx, "ada@example.com", "ada@example.com", "operator")
	require.NoError(t, err)
	setupToken := mustSetupToken(t, h.store, user.ID, h.now)
	_, err = h.store.CompleteSetup(ctx, setupToken, "Ada", "en", "correct-horse-battery", h.now)
	require.NoError(t, err)
	active, err := h.store.FindLocalUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)

	raw, session, err := h.store.CreateBrowserSession(ctx, CreateBrowserSessionParams{
		IdentityID: active.ID, CSRFHash: "h", IdleTTL: 12 * time.Hour, AbsoluteTTL: 30 * 24 * time.Hour,
		Now: h.now, IP: net.ParseIP("198.51.100.10"),
		Location: Location{Country: "PT", Region: "11"},
	})
	require.NoError(t, err)

	// Inside the coalescing window the network evidence is unchanged.
	_, err = h.store.ResolveBrowserSessionWithNetwork(ctx, raw, h.now.Add(time.Minute), SessionNetwork{
		IP: net.ParseIP("203.0.113.99"), Location: Location{Country: "US", Region: "CA"}, HasLocation: true,
	})
	require.NoError(t, err)
	resolved, err := h.store.ResolveBrowserSession(ctx, raw, h.now.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, "PT", resolved.LocationCountry)
	assert.Equal(t, "198.51.100.10", resolved.CreatedIP)

	// Past the window the coalesced IP/location advance with the activity stamp.
	_, err = h.store.ResolveBrowserSessionWithNetwork(ctx, raw, h.now.Add(sessionActivityCoalesce+time.Minute), SessionNetwork{
		IP: net.ParseIP("203.0.113.99"), Location: Location{Country: "US", Region: "CA"}, HasLocation: true,
	})
	require.NoError(t, err)
	var lastIP, country, region string
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT host(last_ip), location_country, location_region FROM identity.browser_sessions WHERE id = $1`,
		session.ID).Scan(&lastIP, &country, &region))
	assert.Equal(t, "203.0.113.99", lastIP)
	assert.Equal(t, "US", country)
	assert.Equal(t, "CA", region)

	// The audit-carrying revocation commits evidence with the mutation.
	event := SecurityEvent{
		Event: EventSessionRevoked, Outcome: OutcomeSuccess, SubjectIdentityID: active.ID,
		BrowserSessionID: session.ID, CredentialKind: CredentialBrowser, CreatedAt: h.now,
	}
	revoked, err := h.store.RevokeSessionWithAudit(ctx, session.ID, h.now, TerminationRevoked, &event)
	require.NoError(t, err)
	assert.True(t, revoked)
	var events int
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT count(*) FROM identity.security_events
		WHERE event = 'session_revoked' AND browser_session_id = $1`, session.ID).Scan(&events))
	assert.Equal(t, 1, events)
}

func TestSessionAuditLinkageAndRevokeOthersCount(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	user, err := h.store.UpsertLocalUser(ctx, "ada@example.com", "ada@example.com", "operator")
	require.NoError(t, err)
	setupToken := mustSetupToken(t, h.store, user.ID, h.now)
	_, err = h.store.CompleteSetup(ctx, setupToken, "Ada", "en", "correct-horse-battery", h.now)
	require.NoError(t, err)
	active, err := h.store.FindLocalUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)

	// The login event is built before the row exists, so the store must link it.
	login := SecurityEvent{
		Event: EventLoginSucceeded, Outcome: OutcomeSuccess, SubjectIdentityID: active.ID,
		CredentialKind: CredentialBrowser, CreatedAt: h.now,
	}
	raw, session, err := h.store.CreateBrowserSession(ctx, CreateBrowserSessionParams{
		IdentityID: active.ID, CSRFHash: "h", IdleTTL: 12 * time.Hour, AbsoluteTTL: 30 * 24 * time.Hour,
		Now: h.now, Audit: &login,
	})
	require.NoError(t, err)
	require.NotEmpty(t, raw)

	var linked *string
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT browser_session_id::text FROM identity.security_events
		WHERE event = 'login_succeeded'`).Scan(&linked))
	require.NotNil(t, linked)
	assert.Equal(t, session.ID, *linked)

	// Revoke-others records the exact bounded count with the surviving session.
	_, _, err = h.store.CreateBrowserSession(ctx, CreateBrowserSessionParams{
		IdentityID: active.ID, CSRFHash: "h2", IdleTTL: 12 * time.Hour, AbsoluteTTL: 30 * 24 * time.Hour, Now: h.now,
	})
	require.NoError(t, err)
	_, _, err = h.store.CreateBrowserSession(ctx, CreateBrowserSessionParams{
		IdentityID: active.ID, CSRFHash: "h3", IdleTTL: 12 * time.Hour, AbsoluteTTL: 30 * 24 * time.Hour, Now: h.now,
	})
	require.NoError(t, err)

	revoked, err := h.store.RevokeOtherSessionsWithAudit(ctx, active.ID, session.ID, h.now, TerminationRevoked,
		func(revoked int64) *SecurityEvent {
			return &SecurityEvent{
				Event: EventSessionRevoked, Outcome: OutcomeSuccess, SubjectIdentityID: active.ID,
				BrowserSessionID: session.ID, CredentialKind: CredentialBrowser,
				Detail: map[string]any{"others": revoked}, CreatedAt: h.now,
			}
		})
	require.NoError(t, err)
	assert.Equal(t, int64(2), revoked)

	var eventSession string
	var others string
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT browser_session_id::text, detail->>'others' FROM identity.security_events
		WHERE event = 'session_revoked' ORDER BY created_at DESC LIMIT 1`).Scan(&eventSession, &others))
	assert.Equal(t, session.ID, eventSession)
	assert.Equal(t, "2", others)
}
