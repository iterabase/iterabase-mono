package identity

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
)

// Browser-session sentinel errors.
var (
	// ErrSessionInvalid is returned when no live session matches the presented
	// token (unknown, terminated, or malformed).
	ErrSessionInvalid = errors.New("identity: browser session invalid")
	// ErrSessionExpired is returned when a session ended by idle/absolute
	// lifetime. The row is terminated as part of resolution.
	ErrSessionExpired = errors.New("identity: browser session expired")
	// ErrSessionIneligible is returned when the account is not currently active.
	ErrSessionIneligible = errors.New("identity: account not eligible")
)

// Session termination reasons (architecture 6.4).
const (
	TerminationRevoked         = "revoked"
	TerminationIdleExpired     = "idle_expired"
	TerminationAbsoluteExpired = "absolute_expired"
	TerminationAccountRevoked  = "account_revoked"
	TerminationPasswordRevoked = "password_revoked"
	TerminationRoleRevoked     = "role_revoked"
)

// sessionActivityCoalesce is the five-minute last-activity/IP/location
// coalescing window (DES-HOR-451-07).
const sessionActivityCoalesce = 5 * time.Minute

// BrowserSession is one live (or retained) opaque browser session with its
// current account state and bounded customer-safe client metadata.
type BrowserSession struct {
	ID                string
	IdentityID        string
	Email             string
	EmailNormalized   string
	DisplayName       string
	Role              string
	Locale            string
	Status            string
	CSRFHash          string
	CreatedAt         time.Time
	LastAuthorizedAt  time.Time
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
	RecentPasswordAt  *time.Time
	TerminatedAt      *time.Time
	TerminationReason *string
	CreatedIP         string
	LastIP            string
	LocationCountry   string
	LocationRegion    string
	Browser           string
	OS                string
	Device            string
}

const browserSessionColumns = `s.id, s.identity_id, s.csrf_hash, s.created_at, s.last_authorized_at,
	s.idle_expires_at, s.absolute_expires_at, s.recent_password_at,
	s.terminated_at, s.termination_reason,
	COALESCE(lu.email, ''), COALESCE(lu.email_normalized, ''), COALESCE(lu.display_name, ''), COALESCE(lu.role, ''),
	COALESCE(lu.locale, ''), COALESCE(lu.status, ''),
	COALESCE(host(s.created_ip), ''), COALESCE(host(s.last_ip), ''),
	COALESCE(s.location_country, ''), COALESCE(s.location_region, ''),
	COALESCE(s.browser_label, ''), COALESCE(s.os_label, ''), COALESCE(s.device_label, '')`

// CreateBrowserSessionParams describes one new session. Callers must revoke any
// presented pre-login session first (login always replaces the token). When
// Audit is set, the session insert and its core audit event commit atomically
// and fail closed.
type CreateBrowserSessionParams struct {
	IdentityID  string
	CSRFHash    string
	IdleTTL     time.Duration
	AbsoluteTTL time.Duration
	Now         time.Time
	IP          net.IP
	Labels      ClientLabels
	Location    Location
	Audit       *SecurityEvent
}

// CreateBrowserSession stores a hashed opaque session token and returns the raw
// token exactly once.
func (s *Store) CreateBrowserSession(ctx context.Context, params CreateBrowserSessionParams) (string, BrowserSession, error) {
	raw, hash, err := GenerateSecret(SecretDomainSessionToken)
	if err != nil {
		return "", BrowserSession{}, err
	}
	now := params.Now.UTC()
	idle := now.Add(params.IdleTTL)
	absolute := now.Add(params.AbsoluteTTL)
	if idle.After(absolute) {
		idle = absolute
	}
	var ip any
	if params.IP != nil {
		ip = params.IP.String()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", BrowserSession{}, fmt.Errorf("begin browser session: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	session, err := scanBrowserSession(tx.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO identity.browser_sessions (
				token_hash, identity_id, csrf_hash, created_at, last_authorized_at,
				idle_expires_at, absolute_expires_at, created_ip, last_ip,
				location_country, location_region, browser_label, os_label, device_label)
			VALUES ($1,$2,$3,$4,$4,$5,$6,$7,$7,$8,$9,$10,$11,$12)
			RETURNING *
		)
		SELECT `+browserSessionColumns+`
		FROM inserted s
		LEFT JOIN identity.local_users lu ON lu.identity_id = s.identity_id`,
		hash, params.IdentityID, params.CSRFHash, now, idle, absolute, ip,
		nullString(params.Location.Country), nullString(params.Location.Region),
		nullString(params.Labels.Browser), nullString(params.Labels.OS), nullString(params.Labels.Device)))
	if err != nil {
		return "", BrowserSession{}, fmt.Errorf("create browser session: %w", err)
	}
	if params.Audit != nil {
		// The caller cannot know the generated session id, and every other
		// session event carries it, so the login evidence is linked here.
		params.Audit.BrowserSessionID = session.ID
		if err := AppendSecurityEventTx(ctx, tx, *params.Audit); err != nil {
			return "", BrowserSession{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", BrowserSession{}, fmt.Errorf("commit browser session: %w", err)
	}
	return raw, session, nil
}

// SessionNetwork is the caller-derived network evidence for a session
// authorization. It is advisory metadata only.
type SessionNetwork struct {
	IP          net.IP
	Location    Location
	HasLocation bool
}

// ResolveBrowserSession validates and, when due, coalesces session activity. It
// resolves the current account state on every call and terminates sessions that
// are expired or no longer eligible.
func (s *Store) ResolveBrowserSession(ctx context.Context, raw string, now time.Time) (BrowserSession, error) {
	return s.ResolveBrowserSessionWithNetwork(ctx, raw, now, SessionNetwork{})
}

// ResolveBrowserSessionWithNetwork additionally records the coalesced last
// IP/location evidence for the authorized request (DES-HOR-451-07).
func (s *Store) ResolveBrowserSessionWithNetwork(ctx context.Context, raw string, now time.Time, network SessionNetwork) (BrowserSession, error) {
	if raw == "" {
		return BrowserSession{}, ErrSessionInvalid
	}
	now = now.UTC()
	hash := HashSecret(SecretDomainSessionToken, raw)

	session, err := scanBrowserSession(s.pool.QueryRow(ctx, `
		SELECT `+browserSessionColumns+`
		FROM identity.browser_sessions s
		LEFT JOIN identity.local_users lu ON lu.identity_id = s.identity_id
		WHERE s.token_hash = $1`, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return BrowserSession{}, ErrSessionInvalid
	}
	if err != nil {
		return BrowserSession{}, err
	}
	if session.TerminatedAt != nil {
		return BrowserSession{}, ErrSessionInvalid
	}
	if session.Status != "" && session.Status != "active" {
		_, _ = s.RevokeSession(ctx, session.ID, now, TerminationAccountRevoked)
		return BrowserSession{}, ErrSessionIneligible
	}
	if !now.Before(session.AbsoluteExpiresAt) {
		_, _ = s.RevokeSession(ctx, session.ID, now, TerminationAbsoluteExpired)
		return BrowserSession{}, ErrSessionExpired
	}
	if !now.Before(session.IdleExpiresAt) {
		_, _ = s.RevokeSession(ctx, session.ID, now, TerminationIdleExpired)
		return BrowserSession{}, ErrSessionExpired
	}

	session = s.coalesceSessionActivity(ctx, session, now, network)
	return session, nil
}

// coalesceSessionActivity advances the coalesced activity stamp and network
// evidence at most once per five-minute window. A failed update leaves the
// resolved session unchanged so the next request retries.
func (s *Store) coalesceSessionActivity(ctx context.Context, session BrowserSession, now time.Time, network SessionNetwork) BrowserSession {
	if now.Sub(session.LastAuthorizedAt) < sessionActivityCoalesce {
		return session
	}
	idle := now.Add(session.IdleExpiresAt.Sub(session.LastAuthorizedAt))
	if idle.After(session.AbsoluteExpiresAt) {
		idle = session.AbsoluteExpiresAt
	}
	var ipValue, country, region any
	if network.IP != nil {
		ipValue = network.IP.String()
	}
	if network.HasLocation {
		country, region = network.Location.Country, network.Location.Region
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE identity.browser_sessions
		SET last_authorized_at = $2, idle_expires_at = $3,
		    last_ip = COALESCE($4, last_ip),
		    location_country = COALESCE($5, location_country),
		    location_region = COALESCE($6, location_region)
		WHERE id = $1 AND terminated_at IS NULL`,
		session.ID, now, idle, ipValue, country, region); err != nil {
		return session
	}
	session.LastAuthorizedAt = now
	session.IdleExpiresAt = idle
	if ipValue != nil {
		session.LastIP = ipValue.(string)
	}
	if network.HasLocation {
		session.LocationCountry = network.Location.Country
		session.LocationRegion = network.Location.Region
	}
	return session
}

// VerifySessionCSRF compares the presented CSRF token with the session-bound
// proof in constant time.
func (s BrowserSession) VerifyCSRF(presented string) bool {
	if presented == "" || s.CSRFHash == "" {
		return false
	}
	return SecretMatches(s.CSRFHash, SecretDomainCSRF, presented)
}

// RotateSessionCSRF issues a fresh session-bound CSRF token and returns the raw
// value exactly once.
func (s *Store) RotateSessionCSRF(ctx context.Context, sessionID string) (string, error) {
	raw, hash, err := GenerateSecret(SecretDomainCSRF)
	if err != nil {
		return "", err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE identity.browser_sessions SET csrf_hash = $2
		WHERE id = $1 AND terminated_at IS NULL`, sessionID, hash)
	if err != nil {
		return "", fmt.Errorf("rotate csrf: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrSessionInvalid
	}
	return raw, nil
}

// SetRecentPassword records bounded session-bound recent-password evidence. It
// never creates another session.
func (s *Store) SetRecentPassword(ctx context.Context, sessionID string, now time.Time) error {
	return s.SetRecentPasswordWithAudit(ctx, sessionID, now, nil)
}

// SetRecentPasswordWithAudit records recent-password evidence and, when
// supplied, its core audit event in the same transaction.
func (s *Store) SetRecentPasswordWithAudit(ctx context.Context, sessionID string, now time.Time, event *SecurityEvent) error {
	rows, err := s.mutateSessionWithAudit(ctx, `
		UPDATE identity.browser_sessions SET recent_password_at = $2
		WHERE id = $1 AND terminated_at IS NULL`,
		[]any{sessionID, now.UTC()}, fixedEvent(event))
	if err != nil {
		return fmt.Errorf("set recent password: %w", err)
	}
	if rows == 0 {
		return ErrSessionInvalid
	}
	return nil
}

// HasRecentPassword reports whether the session carries recent-password proof
// inside the approved bounded window.
func (s BrowserSession) HasRecentPassword(window time.Duration, now time.Time) bool {
	if s.RecentPasswordAt == nil {
		return false
	}
	return now.Sub(*s.RecentPasswordAt) <= window
}

// RevokeSession terminates one session idempotently. It never distinguishes a
// missing row from an already-terminated one.
func (s *Store) RevokeSession(ctx context.Context, sessionID string, now time.Time, reason string) (bool, error) {
	return s.RevokeSessionWithAudit(ctx, sessionID, now, reason, nil)
}

// RevokeSessionWithAudit terminates one session and, when the call actually
// terminated a live row, appends the core audit event in the same transaction
// (architecture 11.2: a security mutation and its evidence commit atomically).
func (s *Store) RevokeSessionWithAudit(ctx context.Context, sessionID string, now time.Time, reason string, event *SecurityEvent) (bool, error) {
	rows, err := s.mutateSessionWithAudit(ctx, `
		UPDATE identity.browser_sessions
		SET terminated_at = $2, termination_reason = $3
		WHERE id = $1 AND terminated_at IS NULL`,
		[]any{sessionID, now.UTC(), reason}, fixedEvent(event))
	if err != nil {
		return false, fmt.Errorf("revoke session: %w", err)
	}
	return rows > 0, nil
}

// RevokeOwnedSession terminates one of the caller's own sessions. A session id
// that belongs to someone else is indistinguishable from an already-ended one.
func (s *Store) RevokeOwnedSession(ctx context.Context, identityID, sessionID string, now time.Time, reason string) (bool, error) {
	return s.RevokeOwnedSessionWithAudit(ctx, identityID, sessionID, now, reason, nil)
}

// RevokeOwnedSessionWithAudit is RevokeOwnedSession with atomic audit evidence.
func (s *Store) RevokeOwnedSessionWithAudit(ctx context.Context, identityID, sessionID string, now time.Time, reason string, event *SecurityEvent) (bool, error) {
	rows, err := s.mutateSessionWithAudit(ctx, `
		UPDATE identity.browser_sessions
		SET terminated_at = $3, termination_reason = $4
		WHERE id = $1 AND identity_id = $2 AND terminated_at IS NULL`,
		[]any{sessionID, identityID, now.UTC(), reason}, fixedEvent(event))
	if err != nil {
		return false, fmt.Errorf("revoke owned session: %w", err)
	}
	return rows > 0, nil
}

// RevokeOtherSessions terminates every live session except keepID.
func (s *Store) RevokeOtherSessions(ctx context.Context, identityID, keepID string, now time.Time, reason string) (int64, error) {
	return s.RevokeOtherSessionsWithAudit(ctx, identityID, keepID, now, reason, nil)
}

// RevokeOtherSessionsWithAudit is RevokeOtherSessions with atomic audit
// evidence. The builder receives the number of revoked rows so the event can
// record the bounded count.
func (s *Store) RevokeOtherSessionsWithAudit(ctx context.Context, identityID, keepID string, now time.Time, reason string, event func(revoked int64) *SecurityEvent) (int64, error) {
	rows, err := s.mutateSessionWithAudit(ctx, `
		UPDATE identity.browser_sessions
		SET terminated_at = $3, termination_reason = $4
		WHERE identity_id = $1 AND id <> $2 AND terminated_at IS NULL`,
		[]any{identityID, keepID, now.UTC(), reason}, event)
	if err != nil {
		return 0, fmt.Errorf("revoke other sessions: %w", err)
	}
	return rows, nil
}

// RevokeAllSessionsForIdentity terminates every live session for an identity.
// Used by account disablement, role change, and password-reset completion.
func (s *Store) RevokeAllSessionsForIdentity(ctx context.Context, identityID string, now time.Time, reason string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE identity.browser_sessions
		SET terminated_at = $2, termination_reason = $3
		WHERE identity_id = $1 AND terminated_at IS NULL`, identityID, now.UTC(), reason)
	if err != nil {
		return 0, fmt.Errorf("revoke all sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RevokeSessionByToken terminates the session presented by a cookie token.
func (s *Store) RevokeSessionByToken(ctx context.Context, raw string, now time.Time, reason string) (bool, error) {
	if raw == "" {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE identity.browser_sessions
		SET terminated_at = $2, termination_reason = $3
		WHERE token_hash = $1 AND terminated_at IS NULL`,
		HashSecret(SecretDomainSessionToken, raw), now.UTC(), reason)
	if err != nil {
		return false, fmt.Errorf("revoke session by token: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// fixedEvent adapts a fixed event pointer to the builder shape used by
// mutateSessionWithAudit when the count is not part of the evidence.
func fixedEvent(event *SecurityEvent) func(int64) *SecurityEvent {
	return func(int64) *SecurityEvent { return event }
}

// mutateSessionWithAudit runs one session mutation and, when it changed at
// least one row and an event builder is supplied, appends the evidence in the
// same transaction. The builder receives the affected row count, so a
// post-mutation count can be recorded atomically. A failed audit rolls the
// mutation back.
func (s *Store) mutateSessionWithAudit(ctx context.Context, query string, args []any, event func(int64) *SecurityEvent) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin session mutation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	rows := tag.RowsAffected()
	if rows > 0 && event != nil {
		if ev := event(rows); ev != nil {
			if err := AppendSecurityEventTx(ctx, tx, *ev); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit session mutation: %w", err)
	}
	return rows, nil
}

// ListActiveSessions returns the caller's live sessions, current first.
func (s *Store) ListActiveSessions(ctx context.Context, identityID, currentID string, now time.Time) ([]BrowserSession, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+browserSessionColumns+`
		FROM identity.browser_sessions s
		LEFT JOIN identity.local_users lu ON lu.identity_id = s.identity_id
		WHERE s.identity_id = $1
		  AND s.terminated_at IS NULL
		  AND s.idle_expires_at > $3
		  AND s.absolute_expires_at > $3
		ORDER BY (s.id = NULLIF($2, '')::uuid) DESC, s.last_authorized_at DESC`,
		identityID, currentID, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	out := []BrowserSession{}
	for rows.Next() {
		session, err := scanBrowserSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

// PurgeSessionNetworkEvidence clears server-only raw IP values no later than
// 30 days after termination (DES-HOR-451-07).
func (s *Store) PurgeSessionNetworkEvidence(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE identity.browser_sessions
		SET created_ip = NULL, last_ip = NULL
		WHERE terminated_at IS NOT NULL AND terminated_at < $1
		  AND (created_ip IS NOT NULL OR last_ip IS NOT NULL)`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("purge session network evidence: %w", err)
	}
	return tag.RowsAffected(), nil
}

// scanBrowserSession scans one browserSessionColumns row.
func scanBrowserSession(row pgx.Row) (BrowserSession, error) {
	var session BrowserSession
	err := row.Scan(
		&session.ID, &session.IdentityID, &session.CSRFHash, &session.CreatedAt, &session.LastAuthorizedAt,
		&session.IdleExpiresAt, &session.AbsoluteExpiresAt, &session.RecentPasswordAt,
		&session.TerminatedAt, &session.TerminationReason,
		&session.Email, &session.EmailNormalized, &session.DisplayName, &session.Role, &session.Locale, &session.Status,
		&session.CreatedIP, &session.LastIP, &session.LocationCountry, &session.LocationRegion,
		&session.Browser, &session.OS, &session.Device,
	)
	return session, err
}
