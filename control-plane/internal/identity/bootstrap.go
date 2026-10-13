package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Bootstrap sentinel errors.
var (
	// ErrBootstrapInconsistent is returned when marker and account state
	// contradict each other; bootstrap fails closed instead of guessing.
	ErrBootstrapInconsistent = errors.New("identity: inconsistent bootstrap state")
	// ErrRecoveryNotPermitted is returned when recovery is attempted while an
	// active Admin still exists.
	ErrRecoveryNotPermitted = errors.New("identity: recovery not permitted while an active Admin exists")
)

// Bootstrap result states.
const (
	BootstrapCreated   = "created"
	BootstrapNoop      = "noop"
	BootstrapRecovered = "recovered"
)

// BootstrapOptions configures one locked bootstrap or recovery run.
type BootstrapOptions struct {
	AdminEmail  string
	AdminLocale string
	Recover     bool
	Now         time.Time
}

// BootstrapResult reports what the locked run did. It never contains a secret.
type BootstrapResult struct {
	State      string
	AdminEmail string
	IdentityID string
}

// BootstrapAdmin implements the locked, RBAC-less first-Admin bootstrap and the
// cluster-operator recovery path (DES-HOR-451-11). It never creates or returns
// an API key or raw setup token.
//
//nolint:gocyclo // explicit fail-closed state matrix across fresh/restart/recovery and inconsistent markers.
func (s *Store) BootstrapAdmin(ctx context.Context, opts BootstrapOptions) (BootstrapResult, error) {
	delivery, normalized, ok := CanonicalEmail(opts.AdminEmail)
	if !ok {
		return BootstrapResult{}, fmt.Errorf("invalid bootstrap admin email %q", opts.AdminEmail)
	}
	locale := opts.AdminLocale
	if locale == "" {
		locale = "en"
	}
	if locale != "en" && locale != "pt" {
		return BootstrapResult{}, fmt.Errorf("invalid bootstrap admin locale %q", opts.AdminLocale)
	}
	now := opts.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("begin bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize every bootstrap/recovery run across replicas.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('iterabase.identity.bootstrap'))`); err != nil {
		return BootstrapResult{}, fmt.Errorf("lock bootstrap: %w", err)
	}

	markerEmail, hasMarker, userCount, err := readBootstrapStateTx(ctx, tx)
	if err != nil {
		return BootstrapResult{}, err
	}

	if opts.Recover {
		result, err := s.recoverAdminTx(ctx, tx, delivery, normalized, locale, now)
		if err != nil {
			return BootstrapResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return BootstrapResult{}, fmt.Errorf("commit recovery: %w", err)
		}
		return result, nil
	}

	switch {
	case hasMarker && userCount == 0:
		return BootstrapResult{}, ErrBootstrapInconsistent
	case hasMarker:
		// Consistent restart. Strict no-op, no secret output.
		return BootstrapResult{State: BootstrapNoop, AdminEmail: markerEmail}, nil
	}

	result, err := firstBootBootstrapTx(ctx, tx, delivery, normalized, locale, now)
	if err != nil {
		return BootstrapResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BootstrapResult{}, fmt.Errorf("commit bootstrap: %w", err)
	}
	return result, nil
}

// readBootstrapStateTx returns the marker email (empty when absent), whether a
// marker exists, and the local-user count under the bootstrap lock.
func readBootstrapStateTx(ctx context.Context, tx pgx.Tx) (string, bool, int, error) {
	var markerEmail string
	markerErr := tx.QueryRow(ctx, `SELECT admin_email FROM identity.bootstrap_marker WHERE id`).Scan(&markerEmail)
	hasMarker := markerErr == nil
	if markerErr != nil && !errors.Is(markerErr, pgx.ErrNoRows) {
		return "", false, 0, fmt.Errorf("read bootstrap marker: %w", markerErr)
	}
	var userCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity.local_users`).Scan(&userCount); err != nil {
		return "", false, 0, fmt.Errorf("count local users: %w", err)
	}
	return markerEmail, hasMarker, userCount, nil
}

// firstBootBootstrapTx runs once per installation, keyed only on the bootstrap
// marker (DES-HOR-582-03): it creates the overlay-named Admin, or converts an
// existing local user with that email, to a `setup_pending` Admin, queues
// normal setup email, and records the marker, auditing the outcome atomically.
func firstBootBootstrapTx(ctx context.Context, tx pgx.Tx, delivery, normalized, locale string, now time.Time) (BootstrapResult, error) {
	activeAdmins, err := countActiveAdminsTx(ctx, tx)
	if err != nil {
		return BootstrapResult{}, err
	}
	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:     EventBootstrapAttempted,
		Outcome:   OutcomeSuccess,
		Detail:    map[string]any{"activeAdmins": activeAdmins},
		CreatedAt: now,
	}); err != nil {
		return BootstrapResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity.bootstrap_marker (id, admin_email, bootstrapped_at)
		VALUES (true, $1, $2)`, delivery, now); err != nil {
		return BootstrapResult{}, fmt.Errorf("record bootstrap marker: %w", err)
	}
	identityID, err := setupPendingAdminTx(ctx, tx, delivery, normalized, locale, now)
	if err != nil {
		return BootstrapResult{}, err
	}
	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:             EventBootstrapCompleted,
		Outcome:           OutcomeSuccess,
		SubjectIdentityID: identityID,
		Detail:            map[string]any{"role": "admin"},
		CreatedAt:         now,
	}); err != nil {
		return BootstrapResult{}, err
	}
	return BootstrapResult{State: BootstrapCreated, AdminEmail: delivery, IdentityID: identityID}, nil
}

func countActiveAdminsTx(ctx context.Context, tx pgx.Tx) (int, error) {
	var activeAdmins int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM identity.local_users WHERE role = 'admin' AND status = 'active'`).
		Scan(&activeAdmins); err != nil {
		return 0, fmt.Errorf("count active admins: %w", err)
	}
	return activeAdmins, nil
}

//nolint:gocyclo // explicit recovery guards and credential invalidation.
func (s *Store) recoverAdminTx(ctx context.Context, tx pgx.Tx, delivery, normalized, locale string, now time.Time) (BootstrapResult, error) {
	activeAdmins, err := countActiveAdminsTx(ctx, tx)
	if err != nil {
		return BootstrapResult{}, err
	}
	if activeAdmins > 0 {
		_ = AppendSecurityEventTx(ctx, tx, SecurityEvent{
			Event:     EventRecoverAdminDenied,
			Outcome:   OutcomeDenied,
			Reason:    "active_admin_exists",
			CreatedAt: now,
		})
		return BootstrapResult{}, ErrRecoveryNotPermitted
	}

	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:     EventRecoverAdminAttempted,
		Outcome:   OutcomeSuccess,
		Detail:    map[string]any{"state": "no_active_admin"},
		CreatedAt: now,
	}); err != nil {
		return BootstrapResult{}, err
	}

	identityID, err := setupPendingAdminTx(ctx, tx, delivery, normalized, locale, now)
	if err != nil {
		return BootstrapResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity.bootstrap_marker (id, admin_email, bootstrapped_at)
		VALUES (true, $1, $2)
		ON CONFLICT (id) DO UPDATE SET admin_email = EXCLUDED.admin_email, bootstrapped_at = EXCLUDED.bootstrapped_at`,
		delivery, now); err != nil {
		return BootstrapResult{}, fmt.Errorf("record recovery marker: %w", err)
	}
	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:             EventRecoverAdminCompleted,
		Outcome:           OutcomeSuccess,
		SubjectIdentityID: identityID,
		Detail:            map[string]any{"role": "admin"},
		CreatedAt:         now,
	}); err != nil {
		return BootstrapResult{}, err
	}
	return BootstrapResult{State: BootstrapRecovered, AdminEmail: delivery, IdentityID: identityID}, nil
}

// setupPendingAdminTx makes the named human a `setup_pending` Admin: it
// converts an existing local user with that canonical email or creates one,
// clears any password, revokes every browser session and API credential the
// human holds or owns, invalidates outstanding setup/reset links, and queues
// normal setup email. It never creates or returns a credential.
func setupPendingAdminTx(ctx context.Context, tx pgx.Tx, delivery, normalized, locale string, now time.Time) (string, error) {
	user, err := scanLocalUser(tx.QueryRow(ctx, `
		SELECT `+localUserColumns+`
		FROM identity.local_users lu
		JOIN identity.identities i ON i.id = lu.identity_id
		WHERE lu.email_normalized = $1 AND i.deleted_at IS NULL
		FOR UPDATE OF lu`, normalized))
	if errors.Is(err, ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
		user = LocalUser{}
	} else if err != nil {
		return "", err
	}

	identityID := user.ID
	if identityID == "" {
		if err := tx.QueryRow(ctx, `
			INSERT INTO identity.identities (key, kind, source, display_name)
			VALUES ($1, 'user', 'local', $2)
			ON CONFLICT (key) DO UPDATE SET deleted_at = NULL, updated_at = now()
			RETURNING id`, normalized, delivery).Scan(&identityID); err != nil {
			return "", fmt.Errorf("create bootstrap identity: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity.local_users (
			identity_id, email, email_normalized, display_name, locale, role, status)
		VALUES ($1, $2, $3, $4, $5, 'admin', 'setup_pending')
		ON CONFLICT (identity_id) DO UPDATE SET
			locale = EXCLUDED.locale, role = 'admin', status = 'setup_pending',
			password_hash = NULL, password_changed_at = NULL,
			role_changed_at = $6, account_changed_at = $6, updated_at = $6`,
		identityID, delivery, normalized, delivery, locale, now); err != nil {
		return "", fmt.Errorf("set up bootstrap admin: %w", err)
	}

	// Revoke prior authority: browser sessions and every credential the human
	// acts as or owns. Raw passwords and links are already gone.
	if _, err := tx.Exec(ctx, `
		UPDATE identity.browser_sessions
		SET terminated_at = $2, termination_reason = 'account_revoked'
		WHERE identity_id = $1 AND terminated_at IS NULL`, identityID, now); err != nil {
		return "", fmt.Errorf("revoke bootstrap sessions: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE identity.api_keys
		SET revoked_at = COALESCE(revoked_at, $2), status = 'revoked',
		    revocation_reason = COALESCE(revocation_reason, 'admin_bootstrap')
		WHERE (identity_id = $1 OR owner_user_identity_id = $1) AND status <> 'revoked'`, identityID, now); err != nil {
		return "", fmt.Errorf("revoke bootstrap credentials: %w", err)
	}
	if err := InvalidateAuthLinksTx(ctx, tx, AuthLinkSetupPassword, "", identityID, now, "recovery"); err != nil {
		return "", err
	}
	if err := InvalidateAuthLinksTx(ctx, tx, AuthLinkResetPassword, "", identityID, now, "recovery"); err != nil {
		return "", err
	}
	if _, err := QueueAuthEmailTx(ctx, tx, AuthEmailIntent{
		Purpose:             AuthLinkSetupPassword,
		RecipientEmail:      delivery,
		RecipientIdentityID: identityID,
		Locale:              locale,
	}); err != nil {
		return "", err
	}
	return identityID, nil
}
