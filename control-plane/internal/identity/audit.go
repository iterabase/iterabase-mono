package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Security event names (architecture 11.1). Event names are stable identifiers;
// customer copy never renders them directly.
//
//nolint:gosec // Stable event identifiers, not credential values.
const (
	EventAccessRequestCreated       = "access_request_created"
	EventAccessRequestVerified      = "access_request_verified"
	EventAccessRequestExpired       = "access_request_expired"
	EventAccessRequestApproved      = "access_request_approved"
	EventAccessRequestDeclined      = "access_request_declined"
	EventAccessRequestPurged        = "access_request_purged"
	EventAuthEmailQueued            = "auth_email_queued"
	EventAuthEmailSent              = "auth_email_sent"
	EventAuthEmailOutcomeUnknown    = "auth_email_outcome_unknown"
	EventAuthEmailFailed            = "auth_email_failed"
	EventAuthEmailLeaseExpired      = "auth_email_lease_expired"
	EventLoginSucceeded             = "login_succeeded"
	EventLoginFailed                = "login_failed"
	EventLoginThrottled             = "login_throttled"
	EventLogout                     = "logout"
	EventSessionRevoked             = "session_revoked"
	EventSessionExpired             = "session_expired"
	EventReauthenticationSucceeded  = "reauthentication_succeeded"
	EventReauthenticationFailed     = "reauthentication_failed"
	EventPasswordResetRequested     = "password_reset_requested"
	EventPasswordResetCompleted     = "password_reset_completed"
	EventPasswordResetSuperseded    = "password_reset_superseded"
	EventSetupCompleted             = "setup_completed"
	EventSetupResendRequested       = "setup_resend_requested"
	EventProfileUpdated             = "profile_updated"
	EventBootstrapAttempted         = "bootstrap_attempted"
	EventBootstrapCompleted         = "bootstrap_completed"
	EventBootstrapDenied            = "bootstrap_denied"
	EventRecoverAdminAttempted      = "recover_admin_attempted"
	EventRecoverAdminCompleted      = "recover_admin_completed"
	EventRecoverAdminDenied         = "recover_admin_denied"
	EventLastAdminMutationDenied    = "last_admin_mutation_denied"
	EventAuthThrottled              = "auth_throttled"
	EventSessionResolvedIneligible  = "session_resolved_ineligible"
	EventRoleChanged                = "role_changed"
	EventAccountDisabled            = "account_disabled"
	EventAccountEnabled             = "account_enabled"
	EventAccountSessionsRevoked     = "account_sessions_revoked"
	EventCredentialCreated          = "api_key_created"
	EventCredentialRotated          = "api_key_rotated"
	EventCredentialRevoked          = "api_key_revoked"
	EventCredentialSuspended        = "api_key_suspended"
	EventCredentialExpired          = "api_key_expired"
	EventCredentialOwnerTransferred = "api_key_owner_transferred"
	EventAuthorityPreflight         = "authority_preflight"
	EventAuthorityCutover           = "authority_cutover"
	EventAuthorityVerified          = "authority_verified"
	EventAuthorityCleanup           = "authority_cleanup"
)

// Security event outcomes.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeDenied  = "denied"
	OutcomeUnknown = "unknown"
)

// Credential kind snapshots.
const (
	CredentialBrowser = "browser_session"
	CredentialAPIKey  = "api_key"
)

// SecurityEvent is one append-only core audit row. All identity fields are
// optional; Detail must stay bounded and secret-free.
type SecurityEvent struct {
	Event                     string
	Outcome                   string
	Reason                    string
	InitiatingHumanIdentityID string
	RequestActorIdentityID    string
	ActorRole                 string
	SubjectIdentityID         string
	AccessRequestID           string
	BrowserSessionID          string
	APIKeyID                  string
	KeyOwnerIdentityID        string
	CredentialKind            string
	RequestID                 string
	CorrelationID             string
	Detail                    map[string]any
	Network                   *SecurityEventNetwork
	CreatedAt                 time.Time
}

// SecurityEventNetwork is the independently purgeable network evidence for one
// core event. Raw IP is server-only security data.
type SecurityEventNetwork struct {
	SourceIP        string
	LocationCountry string
	LocationRegion  string
}

// AppendSecurityEvent inserts one standalone event in its own transaction.
// Used for failed authentication and throttling evidence, which must be
// recordable without a successful security mutation.
func (s *Store) AppendSecurityEvent(ctx context.Context, ev SecurityEvent) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin security event: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := appendSecurityEventTx(ctx, tx, ev); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AppendSecurityEventTx participates in the caller's transaction so a
// successful security mutation and its audit commit atomically (fail closed).
func AppendSecurityEventTx(ctx context.Context, tx pgx.Tx, ev SecurityEvent) error {
	return appendSecurityEventTx(ctx, tx, ev)
}

func appendSecurityEventTx(ctx context.Context, tx pgx.Tx, ev SecurityEvent) error {
	if ev.Outcome == "" {
		ev.Outcome = OutcomeSuccess
	}
	detail := ev.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	rawDetail, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encoding security event detail: %w", err)
	}
	createdAt := ev.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO identity.security_events (
			event, outcome, reason, initiating_human_identity_id, request_actor_identity_id,
			actor_role, subject_identity_id, access_request_id, browser_session_id,
			api_key_id, key_owner_identity_id, credential_kind, request_id, correlation_id,
			detail, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING id`,
		ev.Event, ev.Outcome, nullString(ev.Reason), nullString(ev.InitiatingHumanIdentityID),
		nullString(ev.RequestActorIdentityID), nullString(ev.ActorRole), nullString(ev.SubjectIdentityID),
		nullString(ev.AccessRequestID), nullString(ev.BrowserSessionID), nullString(ev.APIKeyID),
		nullString(ev.KeyOwnerIdentityID), nullString(ev.CredentialKind), nullString(ev.RequestID),
		nullString(ev.CorrelationID), rawDetail, createdAt).Scan(&id)
	if err != nil {
		return fmt.Errorf("insert security event: %w", err)
	}

	if ev.Network != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO identity.security_event_network (security_event_id, source_ip, location_country, location_region, created_at)
			VALUES ($1, NULLIF($2,'')::inet, $3, $4, $5)`,
			id, ev.Network.SourceIP, nullString(ev.Network.LocationCountry),
			nullString(ev.Network.LocationRegion), createdAt); err != nil {
			return fmt.Errorf("insert security event network: %w", err)
		}
	}
	return nil
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// PurgeSecurityEvents deletes core audit rows older than the approved 180-day
// retention. Security evidence is otherwise append-only.
func (s *Store) PurgeSecurityEvents(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM identity.security_events WHERE created_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("purge security events: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PurgeSecurityEventNetwork deletes independently purgeable network evidence
// older than the approved 30-day retention.
func (s *Store) PurgeSecurityEventNetwork(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM identity.security_event_network WHERE created_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("purge security network evidence: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PurgeAuthEmailState removes terminal outbox intents and burned one-time links
// older than the bounded retention window. No raw token is ever stored.
func (s *Store) PurgeAuthEmailState(ctx context.Context, before time.Time) (int64, error) {
	tokens, err := s.pool.Exec(ctx, `
		DELETE FROM identity.auth_link_tokens
		WHERE (consumed_at IS NOT NULL OR invalidated_at IS NOT NULL OR expires_at < $1)
		  AND created_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("purge auth links: %w", err)
	}
	outbox, err := s.pool.Exec(ctx, `
		DELETE FROM identity.auth_email_outbox
		WHERE state IN ('accepted', 'outcome_unknown', 'failed')
		  AND terminal_at IS NOT NULL AND terminal_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("purge auth email outbox: %w", err)
	}
	return tokens.RowsAffected() + outbox.RowsAffected(), nil
}
