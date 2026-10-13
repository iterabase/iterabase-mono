package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Local-user account states (architecture 6.2).
const (
	LocalUserSetupPending = "setup_pending"
	LocalUserActive       = "active"
	LocalUserDisabled     = "disabled"
)

// ErrProfileConflict is returned when a profile update is based on a stale
// version of the account row.
var ErrProfileConflict = errors.New("identity: profile changed since it was loaded")

const localUserColumns = `lu.identity_id, lu.email, lu.email_normalized, lu.display_name,
	lu.role, COALESCE(lu.locale, ''), lu.status, COALESCE(lu.password_hash, ''), lu.password_changed_at,
	lu.created_at, lu.updated_at, COALESCE(lu.approved_access_request_id::text, ''),
	i.key, i.kind, i.source, i.display_name`

func scanLocalUser(row pgx.Row) (LocalUser, error) {
	var lu LocalUser
	err := row.Scan(&lu.ID, &lu.Email, &lu.EmailNormalized, &lu.DisplayName,
		&lu.Role, &lu.Locale, &lu.Status, &lu.PasswordHash, &lu.PasswordChangedAt,
		&lu.CreatedAt, &lu.UpdatedAt, &lu.ApprovedAccessRequestID,
		&lu.Key, &lu.Kind, &lu.Source, &lu.Identity.DisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalUser{}, ErrNotFound
	}
	return lu, err
}

// FindLocalUserByEmail resolves the canonical account for sign-in, reset, and
// setup. It is deliberately not enumeration-resistant; callers must return
// generic responses.
func (s *Store) FindLocalUserByEmail(ctx context.Context, normalized string) (LocalUser, error) {
	lu, err := scanLocalUser(s.pool.QueryRow(ctx, `
		SELECT `+localUserColumns+`
		FROM identity.local_users lu
		JOIN identity.identities i ON i.id = lu.identity_id
		WHERE lu.email_normalized = $1 AND i.deleted_at IS NULL`, normalized))
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalUser{}, ErrNotFound
	}
	return lu, err
}

// UpdateLocalUserProfile updates the editable display name and locale. An empty
// locale means "follow the organisation default" (DES-HOR-670-02).
func (s *Store) UpdateLocalUserProfile(ctx context.Context, identityID, displayName, locale string, now time.Time) (LocalUser, error) {
	return s.UpdateLocalUserProfileVersioned(ctx, identityID, displayName, locale, nil, now)
}

// UpdateLocalUserProfileVersioned updates the profile only when the caller's
// expected row version still matches, so a stale tab cannot silently overwrite
// a newer change (COV-PROFILE-001 "Prof. conflict"). A nil expected version
// skips the precondition.
func (s *Store) UpdateLocalUserProfileVersioned(ctx context.Context, identityID, displayName, locale string, expected *time.Time, now time.Time) (LocalUser, error) {
	if locale != "" && !ValidLocale(locale) {
		return LocalUser{}, ErrInvalidLocale
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LocalUser{}, fmt.Errorf("begin profile update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var tag pgconn.CommandTag
	if expected != nil {
		tag, err = tx.Exec(ctx, `
			UPDATE identity.local_users
			SET display_name = $2, locale = NULLIF($3, '')
			WHERE identity_id = $1 AND updated_at = $4`,
			identityID, displayName, locale, expected.UTC())
	} else {
		tag, err = tx.Exec(ctx, `
			UPDATE identity.local_users
			SET display_name = $2, locale = NULLIF($3, '')
			WHERE identity_id = $1`, identityID, displayName, locale)
	}
	if err != nil {
		return LocalUser{}, fmt.Errorf("update local user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if expected != nil {
			return LocalUser{}, ErrProfileConflict
		}
		return LocalUser{}, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE identity.identities SET display_name = $2 WHERE id = $1`, identityID, displayName); err != nil {
		return LocalUser{}, fmt.Errorf("update identity: %w", err)
	}
	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:             EventProfileUpdated,
		Outcome:           OutcomeSuccess,
		SubjectIdentityID: identityID,
		CredentialKind:    CredentialBrowser,
		Detail:            map[string]any{"locale": locale},
		CreatedAt:         now,
	}); err != nil {
		return LocalUser{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LocalUser{}, fmt.Errorf("commit profile update: %w", err)
	}
	return s.GetLocalUser(ctx, identityID)
}

// CountActiveAdmins reports the number of active Admins. Product mutations must
// never leave this at zero (DES-HOR-451-11).
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM identity.local_users
		WHERE role = 'admin' AND status = 'active'`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count active admins: %w", err)
	}
	return count, nil
}

// activateLocalUserTx completes first-time setup inside the caller's
// transaction: it stores the password hash and activates the account without
// creating a session.
func activateLocalUserTx(ctx context.Context, tx pgx.Tx, identityID, passwordHash string, displayName, locale string, now time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE identity.local_users
		SET password_hash = $2, password_changed_at = $3, status = 'active',
		    display_name = $4, locale = $5
		WHERE identity_id = $1 AND status = 'setup_pending'`,
		identityID, passwordHash, now.UTC(), displayName, locale)
	if err != nil {
		return fmt.Errorf("activate local user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE identity.identities SET display_name = $2, deleted_at = NULL, updated_at = now()
		WHERE id = $1`, identityID, displayName); err != nil {
		return fmt.Errorf("update identity display: %w", err)
	}
	return nil
}

// setLocalUserPasswordTx replaces the password hash atomically inside the
// caller's transaction (password-reset completion).
func setLocalUserPasswordTx(ctx context.Context, tx pgx.Tx, identityID, passwordHash string, now time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE identity.local_users
		SET password_hash = $2, password_changed_at = $3
		WHERE identity_id = $1 AND status = 'active'`,
		identityID, passwordHash, now.UTC())
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
