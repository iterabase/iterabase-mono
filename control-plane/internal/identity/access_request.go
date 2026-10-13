package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Access-request states (architecture 6.1).
const (
	AccessRequestVerificationPending = "verification_pending"
	AccessRequestApprovalPending     = "approval_pending"
	AccessRequestApproved            = "approved"
	AccessRequestDeclined            = "declined"
	AccessRequestExpired             = "expired"
)

// AccessRequestRetention is the approved terminal-request personal-data purge
// deadline.
const AccessRequestRetention = 180 * 24 * time.Hour

// Access-request sentinel errors.
var (
	// ErrAccessRequestNotFound is returned when no request matches.
	ErrAccessRequestNotFound = errors.New("identity: access request not found")
	// ErrAccountExists is returned when approval would collide with an existing
	// canonical account.
	ErrAccountExists = errors.New("identity: account already exists for email")
	// ErrRequestStateChanged is returned when a concurrent review already moved
	// the request out of approval_pending.
	ErrRequestStateChanged = errors.New("identity: access request state changed")
)

// AccessRequest is the durable, non-secret access-request projection.
type AccessRequest struct {
	ID                 string
	Email              string
	EmailNormalized    string
	Locale             string
	State              string
	TerminalReason     string
	VerifiedAt         *time.Time
	ReviewedBy         string
	ReviewedAt         *time.Time
	ApprovedIdentityID string
	ApprovedRole       string
	PurgeAt            time.Time
	CreatedAt          time.Time
}

const accessRequestColumns = `id, email, email_normalized, locale, state,
	COALESCE(terminal_reason, ''), verified_at,
	COALESCE(reviewed_by_identity_id::text, ''), reviewed_at,
	COALESCE(approved_identity_id::text, ''), COALESCE(approved_role, ''),
	purge_at, created_at`

func scanAccessRequest(row pgx.Row) (AccessRequest, error) {
	var request AccessRequest
	err := row.Scan(&request.ID, &request.Email, &request.EmailNormalized, &request.Locale, &request.State,
		&request.TerminalReason, &request.VerifiedAt, &request.ReviewedBy, &request.ReviewedAt,
		&request.ApprovedIdentityID, &request.ApprovedRole, &request.PurgeAt, &request.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessRequest{}, ErrAccessRequestNotFound
	}
	return request, err
}

// CreateOrResendAccessRequest records a generic request for a canonical email,
// or resends verification for an existing non-terminal request. It never
// reveals which branch ran; the caller returns the same accepted response.
func (s *Store) CreateOrResendAccessRequest(ctx context.Context, delivery, normalized, locale string, now time.Time) (AccessRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AccessRequest{}, fmt.Errorf("begin access request: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	existing, err := scanAccessRequest(tx.QueryRow(ctx, `
		SELECT `+accessRequestColumns+`
		FROM identity.access_requests
		WHERE email_normalized = $1 AND state IN ('verification_pending', 'approval_pending')
		FOR UPDATE`, normalized))
	resent := err == nil
	if err != nil && !errors.Is(err, ErrAccessRequestNotFound) {
		return AccessRequest{}, err
	}

	var request AccessRequest
	switch {
	case resent && existing.State == AccessRequestApprovalPending:
		// Already verified and waiting for review: no new mail, same outcome.
		return existing, nil
	case resent:
		if err := InvalidateAuthLinksTx(ctx, tx, AuthLinkVerifyAccess, existing.ID, "", now, "resend"); err != nil {
			return AccessRequest{}, err
		}
		if _, err := QueueAuthEmailTx(ctx, tx, AuthEmailIntent{
			Purpose:         AuthLinkVerifyAccess,
			RecipientEmail:  existing.Email,
			AccessRequestID: existing.ID,
			Locale:          existing.Locale,
		}); err != nil {
			return AccessRequest{}, err
		}
		request = existing
	default:
		request, err = scanAccessRequest(tx.QueryRow(ctx, `
			INSERT INTO identity.access_requests (email, email_normalized, locale, state, purge_at)
			VALUES ($1, $2, $3, 'verification_pending', $4)
			RETURNING `+accessRequestColumns,
			delivery, normalized, locale, now.UTC().Add(AccessRequestRetention)))
		if err != nil {
			return AccessRequest{}, fmt.Errorf("insert access request: %w", err)
		}
		if _, err := QueueAuthEmailTx(ctx, tx, AuthEmailIntent{
			Purpose:         AuthLinkVerifyAccess,
			RecipientEmail:  delivery,
			AccessRequestID: request.ID,
			Locale:          locale,
		}); err != nil {
			return AccessRequest{}, err
		}
	}

	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:           EventAccessRequestCreated,
		Outcome:         OutcomeSuccess,
		AccessRequestID: request.ID,
		CredentialKind:  "",
		Detail:          map[string]any{"resend": resent},
		CreatedAt:       now,
	}); err != nil {
		return AccessRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AccessRequest{}, fmt.Errorf("commit access request: %w", err)
	}
	return request, nil
}

// VerifyAccessRequest consumingly verifies a verification link. It is
// idempotent for a token that this browser has already consumed, and it moves
// the request to approval_pending exactly once.
func (s *Store) VerifyAccessRequest(ctx context.Context, raw string, now time.Time) (AccessRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AccessRequest{}, fmt.Errorf("begin verify: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	token, err := lockAuthLinkTx(ctx, tx, raw)
	if err != nil {
		return AccessRequest{}, err
	}
	if token.Purpose != AuthLinkVerifyAccess {
		return AccessRequest{}, ErrAuthLinkInvalid
	}
	if token.InvalidatedAt != nil {
		return AccessRequest{}, ErrAuthLinkSuperseded
	}
	request, err := scanAccessRequest(tx.QueryRow(ctx, `
		SELECT `+accessRequestColumns+`
		FROM identity.access_requests WHERE id = $1
		FOR UPDATE`, token.AccessRequestID))
	if err != nil {
		return AccessRequest{}, err
	}
	if token.ConsumedAt != nil {
		return request, nil
	}
	if !now.UTC().Before(token.ExpiresAt) {
		return AccessRequest{}, ErrAuthLinkExpired
	}
	if request.State == AccessRequestVerificationPending {
		verified, err := scanAccessRequest(tx.QueryRow(ctx, `
			UPDATE identity.access_requests
			SET state = 'approval_pending', verified_at = $2
			WHERE id = $1 AND state = 'verification_pending'
			RETURNING `+accessRequestColumns, request.ID, now.UTC()))
		if err != nil {
			return AccessRequest{}, fmt.Errorf("advance access request: %w", err)
		}
		request = verified
		if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
			Event:           EventAccessRequestVerified,
			Outcome:         OutcomeSuccess,
			AccessRequestID: request.ID,
			CreatedAt:       now,
		}); err != nil {
			return AccessRequest{}, err
		}
	}
	if err := ConsumeAuthLinkTx(ctx, tx, token.ID, now); err != nil {
		return AccessRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AccessRequest{}, fmt.Errorf("commit verify: %w", err)
	}
	return request, nil
}

// AccessRequestByToken resolves the request linked to a verification token,
// including consumed tokens, so the browser that holds the secret can render
// the waiting/terminal state after a reload.
func (s *Store) AccessRequestByToken(ctx context.Context, raw string) (AccessRequest, error) {
	token, err := scanAuthLink(s.pool.QueryRow(ctx, `
		SELECT id, purpose, access_request_id, identity_id, outbox_id, generation, expires_at, consumed_at, invalidated_at
		FROM identity.auth_link_tokens WHERE token_hash = $1`,
		HashSecret(SecretDomainAuthLink, raw)))
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessRequest{}, ErrAuthLinkInvalid
	}
	if err != nil {
		return AccessRequest{}, fmt.Errorf("read auth link: %w", err)
	}
	if token.Purpose != AuthLinkVerifyAccess {
		return AccessRequest{}, ErrAuthLinkInvalid
	}
	if token.InvalidatedAt != nil {
		return AccessRequest{}, ErrAuthLinkSuperseded
	}
	return scanAccessRequest(s.pool.QueryRow(ctx, `
		SELECT `+accessRequestColumns+`
		FROM identity.access_requests WHERE id = $1`, token.AccessRequestID))
}

// ListPendingAccessRequests returns verified requests awaiting Admin review.
func (s *Store) ListPendingAccessRequests(ctx context.Context) ([]AccessRequest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+accessRequestColumns+`
		FROM identity.access_requests
		WHERE state = 'approval_pending'
		ORDER BY verified_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list pending requests: %w", err)
	}
	defer rows.Close()

	out := []AccessRequest{}
	for rows.Next() {
		request, err := scanAccessRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, request)
	}
	return out, rows.Err()
}

// ApproveAccessRequest atomically creates the canonical human as setup_pending
// with the reviewed role, or re-approves a migrated passwordless setup_pending
// human under its existing identity, and queues first-time setup mail. No session is
// created and no credential is printed.
func (s *Store) ApproveAccessRequest(ctx context.Context, requestID, reviewerID, role string, now time.Time) (LocalUser, error) {
	if role != "operator" && role != "admin" {
		return LocalUser{}, fmt.Errorf("invalid approved role %q", role)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LocalUser{}, fmt.Errorf("begin approve: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	request, err := scanAccessRequest(tx.QueryRow(ctx, `
		SELECT `+accessRequestColumns+`
		FROM identity.access_requests WHERE id = $1
		FOR UPDATE`, requestID))
	if err != nil {
		return LocalUser{}, err
	}
	if request.State != AccessRequestApprovalPending {
		return LocalUser{}, ErrRequestStateChanged
	}

	identityID, reapproved, err := approvedAccountTx(ctx, tx, request, reviewerID, role, now)
	if err != nil {
		return LocalUser{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE identity.access_requests
		SET state = 'approved', terminal_reason = 'approved',
		    approved_identity_id = $2, approved_role = $3,
		    reviewed_by_identity_id = $4, reviewed_at = $5, purge_at = $6
		WHERE id = $1`, request.ID, identityID, role, reviewerID, now.UTC(),
		now.UTC().Add(AccessRequestRetention)); err != nil {
		return LocalUser{}, fmt.Errorf("approve access request: %w", err)
	}
	if err := InvalidateAuthLinksTx(ctx, tx, AuthLinkVerifyAccess, request.ID, "", now, "approved"); err != nil {
		return LocalUser{}, err
	}
	if _, err := QueueAuthEmailTx(ctx, tx, AuthEmailIntent{
		Purpose:             AuthLinkSetupPassword,
		RecipientEmail:      request.Email,
		RecipientIdentityID: identityID,
		Locale:              request.Locale,
	}); err != nil {
		return LocalUser{}, err
	}
	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:                  EventAccessRequestApproved,
		Outcome:                OutcomeSuccess,
		RequestActorIdentityID: reviewerID,
		SubjectIdentityID:      identityID,
		AccessRequestID:        request.ID,
		CredentialKind:         CredentialBrowser,
		Detail:                 map[string]any{"role": role, "existingAccount": reapproved},
		CreatedAt:              now,
	}); err != nil {
		return LocalUser{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LocalUser{}, fmt.Errorf("commit approve: %w", err)
	}
	user, err := s.GetLocalUser(ctx, identityID)
	if err != nil {
		return LocalUser{}, err
	}
	return user, nil
}

// approvedAccountTx creates the approved human as setup_pending, or re-approves
// a migrated passwordless setup_pending human under its existing identity, and
// reports which happened.
func approvedAccountTx(ctx context.Context, tx pgx.Tx, request AccessRequest, reviewerID, role string, now time.Time) (string, bool, error) {
	identityID, err := approvableExistingUserTx(ctx, tx, request.EmailNormalized)
	if err != nil {
		return "", false, err
	}
	reapproved := identityID != ""
	if reapproved {
		// A migrated human (DES-HOR-582-03) keeps its canonical identity and
		// history; the reviewed role and a fresh setup link replace any
		// outstanding one.
		if _, err := tx.Exec(ctx, `
			UPDATE identity.local_users
			SET role = $2, locale = $3, approved_access_request_id = $4,
			    role_changed_at = $5, role_changed_by = $6,
			    account_changed_at = $5, account_changed_by = $6, updated_at = $5
			WHERE identity_id = $1`,
			identityID, role, request.Locale, request.ID, now.UTC(), reviewerID); err != nil {
			return "", false, fmt.Errorf("re-approve local user: %w", err)
		}
		if err := InvalidateAuthLinksTx(ctx, tx, AuthLinkSetupPassword, "", identityID, now, "superseded"); err != nil {
			return "", false, err
		}
	} else {
		if err := tx.QueryRow(ctx, `
			INSERT INTO identity.identities (key, kind, source, display_name)
			VALUES ($1, 'user', 'local', $2)
			ON CONFLICT (key) DO UPDATE
				SET deleted_at = NULL, updated_at = now()
			RETURNING id`, request.EmailNormalized, request.Email).Scan(&identityID); err != nil {
			return "", false, fmt.Errorf("create identity: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO identity.local_users (
				identity_id, email, email_normalized, display_name, locale, role, status, approved_access_request_id)
			VALUES ($1, $2, $3, $4, $5, $6, 'setup_pending', $7)`,
			identityID, request.Email, request.EmailNormalized, request.Email, request.Locale, role, request.ID); err != nil {
			return "", false, fmt.Errorf("create local user: %w", err)
		}
	}
	return identityID, reapproved, nil
}

// approvableExistingUserTx returns the identity of an existing account that an
// approval may take over, or "" when no account exists. Only a passwordless
// `setup_pending` account (a human migrated into V2 who never completed setup)
// is approvable; any other existing account blocks approval.
func approvableExistingUserTx(ctx context.Context, tx pgx.Tx, normalized string) (string, error) {
	var (
		existing    string
		status      string
		hasPassword bool
	)
	err := tx.QueryRow(ctx, `
		SELECT identity_id, status, password_hash IS NOT NULL
		FROM identity.local_users WHERE email_normalized = $1
		FOR UPDATE`, normalized).Scan(&existing, &status, &hasPassword)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("check existing account: %w", err)
	}
	if status != LocalUserSetupPending || hasPassword {
		return "", ErrAccountExists
	}
	return existing, nil
}

// DeclineAccessRequest terminates a verified request without creating a person.
func (s *Store) DeclineAccessRequest(ctx context.Context, requestID, reviewerID string, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin decline: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	request, err := scanAccessRequest(tx.QueryRow(ctx, `
		SELECT `+accessRequestColumns+`
		FROM identity.access_requests WHERE id = $1
		FOR UPDATE`, requestID))
	if err != nil {
		return err
	}
	if request.State != AccessRequestApprovalPending {
		return ErrRequestStateChanged
	}
	if _, err := tx.Exec(ctx, `
		UPDATE identity.access_requests
		SET state = 'declined', terminal_reason = 'declined',
		    reviewed_by_identity_id = $2, reviewed_at = $3, purge_at = $4
		WHERE id = $1`, request.ID, reviewerID, now.UTC(), now.UTC().Add(AccessRequestRetention)); err != nil {
		return fmt.Errorf("decline access request: %w", err)
	}
	if err := InvalidateAuthLinksTx(ctx, tx, AuthLinkVerifyAccess, request.ID, "", now, "declined"); err != nil {
		return err
	}
	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:                  EventAccessRequestDeclined,
		Outcome:                OutcomeSuccess,
		RequestActorIdentityID: reviewerID,
		AccessRequestID:        request.ID,
		CreatedAt:              now,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ExpireAccessRequests expires verification-pending requests whose link
// lifetime has fully elapsed without a valid token. Expiry and its audit are
// one statement so evidence cannot diverge from state.
func (s *Store) ExpireAccessRequests(ctx context.Context, now time.Time) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx, `
		WITH expired AS (
			UPDATE identity.access_requests r
			SET state = 'expired', terminal_reason = 'verification_expired', purge_at = $1
			WHERE r.state = 'verification_pending'
			  AND r.created_at + make_interval(secs => $2) <= $3
			  AND NOT EXISTS (
			      SELECT 1 FROM identity.auth_link_tokens t
			      WHERE t.access_request_id = r.id AND t.purpose = 'verify_access'
			        AND t.consumed_at IS NULL AND t.invalidated_at IS NULL AND t.expires_at > $3)
			RETURNING r.id
		), audited AS (
			INSERT INTO identity.security_events (event, outcome, access_request_id, detail, created_at)
			SELECT 'access_request_expired', 'success', id, '{"reason":"verification_expired"}'::jsonb, $3
			FROM expired
			RETURNING access_request_id
		)
		SELECT count(*) FROM expired`,
		now.UTC().Add(AccessRequestRetention), VerifyAccessLinkTTL.Seconds(), now.UTC()).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("expire access requests: %w", err)
	}
	return count, nil
}

// PurgeAccessRequests deletes terminal requests whose 180-day retention has
// elapsed, keeping only secret-free audit evidence.
func (s *Store) PurgeAccessRequests(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin purge: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE identity.local_users lu
		SET approved_access_request_id = NULL
		FROM identity.access_requests r
		WHERE lu.approved_access_request_id = r.id
		  AND r.purge_at <= $1
		  AND r.state IN ('approved', 'declined', 'expired')`, now.UTC()); err != nil {
		return 0, fmt.Errorf("unlink approved requests: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		WITH purged AS (
			SELECT id FROM identity.access_requests
			WHERE purge_at <= $1 AND state IN ('approved', 'declined', 'expired')
		), audited AS (
			INSERT INTO identity.security_events (event, outcome, detail, created_at)
			SELECT 'access_request_purged', 'success', '{}'::jsonb, $1 FROM purged
			RETURNING id
		)
		SELECT count(*) FROM audited`, now.UTC()); err != nil {
		return 0, fmt.Errorf("audit purged requests: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		DELETE FROM identity.access_requests
		WHERE purge_at <= $1 AND state IN ('approved', 'declined', 'expired')`, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("purge access requests: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit purge: %w", err)
	}
	return tag.RowsAffected(), nil
}
