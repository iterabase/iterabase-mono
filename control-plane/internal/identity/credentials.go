package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrAccountNotEligible is returned when an account may not complete a
// credential flow (disabled or not in the expected state).
var ErrAccountNotEligible = errors.New("identity: account not eligible")

// CompleteSetup consumes a valid first-time setup link, stores the password,
// and activates the account. It creates no browser session.
func (s *Store) CompleteSetup(ctx context.Context, raw, displayName, locale, password string, now time.Time) (LocalUser, error) {
	if locale != "en" && locale != "pt" {
		return LocalUser{}, fmt.Errorf("unsupported locale %q", locale)
	}
	displayName = normalizeDisplayName(displayName)
	passwordHash, err := HashPassword(password)
	if err != nil {
		return LocalUser{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LocalUser{}, fmt.Errorf("begin setup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	token, err := claimAuthLinkTx(ctx, tx, raw, AuthLinkSetupPassword, now)
	if err != nil {
		return LocalUser{}, err
	}
	user, err := localUserInTx(ctx, tx, token.IdentityID)
	if err != nil {
		return LocalUser{}, err
	}
	switch user.Status {
	case LocalUserSetupPending:
	case LocalUserDisabled:
		return LocalUser{}, ErrAccountNotEligible
	default:
		return LocalUser{}, ErrAuthLinkConsumed
	}

	if err := activateLocalUserTx(ctx, tx, user.ID, passwordHash, displayName, locale, now); err != nil {
		return LocalUser{}, err
	}
	if err := ConsumeAuthLinkTx(ctx, tx, token.ID, now); err != nil {
		return LocalUser{}, err
	}
	if err := InvalidateAuthLinksTx(ctx, tx, AuthLinkSetupPassword, "", user.ID, now, "completed"); err != nil {
		return LocalUser{}, err
	}
	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:             EventSetupCompleted,
		Outcome:           OutcomeSuccess,
		SubjectIdentityID: user.ID,
		CredentialKind:    CredentialBrowser,
		Detail:            map[string]any{"locale": locale},
		CreatedAt:         now,
	}); err != nil {
		return LocalUser{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LocalUser{}, fmt.Errorf("commit setup: %w", err)
	}
	return s.GetLocalUser(ctx, user.ID)
}

// RequestPasswordReset queues reset mail for an eligible active account. It
// never changes the password, sessions, or API keys and never reports whether
// the account exists.
func (s *Store) RequestPasswordReset(ctx context.Context, normalized string, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin reset request: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	user, err := scanLocalUser(tx.QueryRow(ctx, `
		SELECT `+localUserColumns+`
		FROM identity.local_users lu
		JOIN identity.identities i ON i.id = lu.identity_id
		WHERE lu.email_normalized = $1 AND i.deleted_at IS NULL
		FOR UPDATE OF lu`, normalized))
	if errors.Is(err, ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if user.Status != LocalUserActive || user.PasswordHash == "" {
		return false, nil
	}

	if err := InvalidateAuthLinksTx(ctx, tx, AuthLinkResetPassword, "", user.ID, now, "resend"); err != nil {
		return false, err
	}
	if _, err := QueueAuthEmailTx(ctx, tx, AuthEmailIntent{
		Purpose:             AuthLinkResetPassword,
		RecipientEmail:      user.Email,
		RecipientIdentityID: user.ID,
		Locale:              user.Locale,
	}); err != nil {
		return false, err
	}
	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:             EventPasswordResetRequested,
		Outcome:           OutcomeSuccess,
		SubjectIdentityID: user.ID,
		CredentialKind:    CredentialBrowser,
		CreatedAt:         now,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit reset request: %w", err)
	}
	return true, nil
}

// CompletePasswordReset consumes a valid reset link, replaces the password,
// invalidates every other reset link, and revokes all browser sessions. It does
// not revoke API keys and creates no session.
func (s *Store) CompletePasswordReset(ctx context.Context, raw, password string, now time.Time) error {
	passwordHash, err := HashPassword(password)
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin reset: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	token, err := claimAuthLinkTx(ctx, tx, raw, AuthLinkResetPassword, now)
	if err != nil {
		return err
	}
	user, err := localUserInTx(ctx, tx, token.IdentityID)
	if err != nil {
		return err
	}
	if user.Status != LocalUserActive || user.PasswordHash == "" {
		return ErrAccountNotEligible
	}
	if err := setLocalUserPasswordTx(ctx, tx, user.ID, passwordHash, now); err != nil {
		return err
	}
	if err := ConsumeAuthLinkTx(ctx, tx, token.ID, now); err != nil {
		return err
	}
	if err := InvalidateAuthLinksTx(ctx, tx, AuthLinkResetPassword, "", user.ID, now, "completed"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE identity.browser_sessions
		SET terminated_at = $2, termination_reason = 'password_revoked'
		WHERE identity_id = $1 AND terminated_at IS NULL`, user.ID, now.UTC()); err != nil {
		return fmt.Errorf("revoke sessions on reset: %w", err)
	}
	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:             EventPasswordResetCompleted,
		Outcome:           OutcomeSuccess,
		SubjectIdentityID: user.ID,
		CredentialKind:    CredentialBrowser,
		CreatedAt:         now,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ResendSetupInstructions rotates and re-queues first-time setup mail for the
// account behind a setup link. A malformed or unknown token is a no-op with the
// same generic outcome.
func (s *Store) ResendSetupInstructions(ctx context.Context, raw string, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin setup resend: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	token, err := scanAuthLink(tx.QueryRow(ctx, `
		SELECT id, purpose, access_request_id, identity_id, outbox_id, generation, expires_at, consumed_at, invalidated_at
		FROM identity.auth_link_tokens WHERE token_hash = $1
		FOR UPDATE`, HashSecret(SecretDomainAuthLink, raw)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if token.Purpose != AuthLinkSetupPassword || token.IdentityID == "" {
		return false, nil
	}
	user, err := localUserInTx(ctx, tx, token.IdentityID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if user.Status != LocalUserSetupPending {
		return false, nil
	}
	if err := InvalidateAuthLinksTx(ctx, tx, AuthLinkSetupPassword, "", user.ID, now, "resend"); err != nil {
		return false, err
	}
	if _, err := QueueAuthEmailTx(ctx, tx, AuthEmailIntent{
		Purpose:             AuthLinkSetupPassword,
		RecipientEmail:      user.Email,
		RecipientIdentityID: user.ID,
		Locale:              user.Locale,
	}); err != nil {
		return false, err
	}
	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:             EventSetupResendRequested,
		Outcome:           OutcomeSuccess,
		SubjectIdentityID: user.ID,
		CredentialKind:    CredentialBrowser,
		CreatedAt:         now,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit setup resend: %w", err)
	}
	return true, nil
}

// claimAuthLinkTx locks a one-time link and validates purpose, lifetime, and
// single-use state. Exactly one consumer can pass this check under row locking.
func claimAuthLinkTx(ctx context.Context, tx pgx.Tx, raw, purpose string, now time.Time) (AuthLinkToken, error) {
	token, err := lockAuthLinkTx(ctx, tx, raw)
	if err != nil {
		return AuthLinkToken{}, err
	}
	switch {
	case token.Purpose != purpose:
		return AuthLinkToken{}, ErrAuthLinkInvalid
	case token.InvalidatedAt != nil:
		return AuthLinkToken{}, ErrAuthLinkSuperseded
	case token.ConsumedAt != nil:
		return AuthLinkToken{}, ErrAuthLinkConsumed
	case !now.UTC().Before(token.ExpiresAt):
		return AuthLinkToken{}, ErrAuthLinkExpired
	}
	return token, nil
}

// SetupContext is the bounded read-only context a valid first-time setup link
// may disclose to its holder: the verified work email and the approved role.
type SetupContext struct {
	Email string
	Role  string
}

// SetupContext resolves the bounded read-only context for a valid setup link.
// Any other token state returns the matching bounded link error.
//
// Expiry is decided against the caller's request clock, like every other
// auth-link read path (ClaimAuthLinkToken, VerifyAccessRequest). Reading the
// wall clock here made the frozen-clock integration harness expire setup links
// as soon as real time passed the harness date (HOR-610).
func (s *Store) SetupContext(ctx context.Context, raw string, now time.Time) (SetupContext, error) {
	if raw == "" {
		return SetupContext{}, ErrAuthLinkInvalid
	}
	var purpose, email, role, status string
	var expiresAt time.Time
	var consumedAt, invalidatedAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT t.purpose, t.expires_at, t.consumed_at, t.invalidated_at,
		       COALESCE(lu.email, ''), COALESCE(lu.role, ''), COALESCE(lu.status, '')
		FROM identity.auth_link_tokens t
		LEFT JOIN identity.local_users lu ON lu.identity_id = t.identity_id
		WHERE t.token_hash = $1`, HashSecret(SecretDomainAuthLink, raw)).
		Scan(&purpose, &expiresAt, &consumedAt, &invalidatedAt, &email, &role, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return SetupContext{}, ErrAuthLinkInvalid
	}
	if err != nil {
		return SetupContext{}, fmt.Errorf("read setup context: %w", err)
	}
	switch {
	case purpose != AuthLinkSetupPassword:
		return SetupContext{}, ErrAuthLinkInvalid
	case invalidatedAt != nil:
		return SetupContext{}, ErrAuthLinkSuperseded
	case consumedAt != nil:
		return SetupContext{}, ErrAuthLinkConsumed
	case !now.UTC().Before(expiresAt):
		return SetupContext{}, ErrAuthLinkExpired
	case status != LocalUserSetupPending:
		return SetupContext{}, ErrAccountNotEligible
	}
	return SetupContext{Email: email, Role: NormalizeRole(role)}, nil
}

func localUserInTx(ctx context.Context, tx pgx.Tx, identityID string) (LocalUser, error) {
	user, err := scanLocalUser(tx.QueryRow(ctx, `
		SELECT `+localUserColumns+`
		FROM identity.local_users lu
		JOIN identity.identities i ON i.id = lu.identity_id
		WHERE lu.identity_id = $1 AND i.deleted_at IS NULL
		FOR UPDATE OF lu`, identityID))
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalUser{}, ErrNotFound
	}
	return user, err
}

func normalizeDisplayName(displayName string) string {
	return boundRunes(displayName, 200)
}

func boundRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) > max {
		return string(runes[:max])
	}
	return value
}
