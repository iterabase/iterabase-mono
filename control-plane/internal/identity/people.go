package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Sentinel errors for People administration.
var (
	// ErrLastAdmin is returned when a mutation would leave zero active Admins.
	ErrLastAdmin = errors.New("identity: last active admin")
	// ErrInvalidRole is returned for any role outside exactly admin|operator.
	ErrInvalidRole = errors.New("identity: invalid role")
	// ErrPersonStateChanged is returned when a mutation targets a person whose
	// account state no longer matches the requested transition.
	ErrPersonStateChanged = errors.New("identity: person state changed")
)

// Person is the bounded Admin People projection (architecture 9.3: People reads
// are a current-Admin surface; no session, network, or credential secret detail
// is exposed).
type Person struct {
	IdentityID       string
	Email            string
	DisplayName      string
	Role             string
	Locale           string
	Status           string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	RoleChangedAt    *time.Time
	AccountChangedAt *time.Time
}

// PersonConsequences records the bounded, non-secret outcome of a People
// mutation so the caller can state what actually happened.
type PersonConsequences struct {
	SessionsRevoked      int64
	CredentialsSuspended int64
}

const personColumns = `lu.identity_id, lu.email, lu.display_name, lu.role, lu.locale,
	lu.status, lu.created_at, lu.updated_at, lu.role_changed_at, lu.account_changed_at`

func scanPerson(row pgx.Row) (Person, error) {
	var p Person
	err := row.Scan(&p.IdentityID, &p.Email, &p.DisplayName, &p.Role, &p.Locale,
		&p.Status, &p.CreatedAt, &p.UpdatedAt, &p.RoleChangedAt, &p.AccountChangedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Person{}, ErrNotFound
	}
	p.Role = NormalizeRole(p.Role)
	return p, err
}

// ListPeople returns every non-purged local account (active and disabled), so
// the People directory reflects current server authority rather than UI state.
func (s *Store) ListPeople(ctx context.Context) ([]Person, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+personColumns+`
		FROM identity.local_users lu
		JOIN identity.identities i ON i.id = lu.identity_id
		WHERE i.deleted_at IS NULL
		ORDER BY (lu.role = 'admin') DESC, lu.email_normalized`)
	if err != nil {
		return nil, fmt.Errorf("list people: %w", err)
	}
	defer rows.Close()

	var out []Person
	for rows.Next() {
		p, err := scanPerson(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPerson returns one person for the People detail surface.
func (s *Store) GetPerson(ctx context.Context, identityID string) (Person, error) {
	return scanPerson(s.pool.QueryRow(ctx, `
		SELECT `+personColumns+`
		FROM identity.local_users lu
		JOIN identity.identities i ON i.id = lu.identity_id
		WHERE lu.identity_id = $1 AND i.deleted_at IS NULL`, identityID))
}

// ChangePersonRole applies an audited, current-authority role change. It revokes
// every browser session of the target (the new role must be established by a
// fresh sign-in), refuses to leave zero active Admins, and clips credentials
// exactly as architecture section 10 requires:
//
//   - Operator -> Admin: sessions revoked; existing credentials unchanged; no
//     Admin-only action is added to any credential.
//   - Admin -> Operator: sessions revoked; personal credentials holding an
//     Admin-only action are suspended; every owned automation credential is
//     suspended until an active Admin explicitly reissues it.
//
// Promotion and re-enablement never resume or widen a credential.
func (s *Store) ChangePersonRole(ctx context.Context, actorID, targetID, role string, now time.Time) (Person, PersonConsequences, error) {
	if role != RoleAdmin && role != RoleOperator {
		return Person{}, PersonConsequences{}, ErrInvalidRole
	}
	var cons PersonConsequences
	err := s.mutatePerson(ctx, actorID, targetID, now, EventRoleChanged, func(tx pgx.Tx, target LocalUser) (map[string]any, error) {
		current := NormalizeRole(target.Role)
		if current == role {
			return nil, nil
		}
		demoting := current == RoleAdmin && role == RoleOperator
		if demoting {
			if err := requireAnotherActiveAdminTx(ctx, tx, targetID); err != nil {
				return nil, err
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE identity.local_users
			SET role = $2, role_changed_at = $3, role_changed_by = $4
			WHERE identity_id = $1`, targetID, role, now.UTC(), actorID); err != nil {
			return nil, fmt.Errorf("update role: %w", err)
		}

		revoked, err := revokeAllSessionsTx(ctx, tx, targetID, now, TerminationRoleRevoked)
		if err != nil {
			return nil, err
		}
		cons.SessionsRevoked = revoked

		var clipped int64
		if demoting {
			adminOnly, err := suspendAdminOnlyCredentialsTx(ctx, tx, targetID)
			if err != nil {
				return nil, err
			}
			automation, err := suspendOwnedAutomationTx(ctx, tx, targetID)
			if err != nil {
				return nil, err
			}
			clipped = adminOnly + automation
		}
		cons.CredentialsSuspended = clipped
		return map[string]any{
			"fromRole":             current,
			"toRole":               role,
			"sessionsRevoked":      revoked,
			"credentialsSuspended": clipped,
		}, nil
	})
	if err != nil {
		return Person{}, PersonConsequences{}, err
	}
	// The mutation is already durable, so a failed re-read is a real error rather
	// than an empty person: reporting a zero-value success would misstate what
	// authority the caller now holds.
	person, err := s.GetPerson(ctx, targetID)
	if err != nil {
		return Person{}, cons, fmt.Errorf("read person after role change: %w", err)
	}
	return person, cons, nil
}

// DisablePerson disables an account: sessions are revoked and every owned
// credential is suspended. Re-enablement never resumes them.
func (s *Store) DisablePerson(ctx context.Context, actorID, targetID string, now time.Time) (Person, PersonConsequences, error) {
	var cons PersonConsequences
	err := s.mutatePerson(ctx, actorID, targetID, now, EventAccountDisabled, func(tx pgx.Tx, target LocalUser) (map[string]any, error) {
		if target.Status == LocalUserDisabled {
			return nil, nil
		}
		if NormalizeRole(target.Role) == RoleAdmin {
			if err := requireAnotherActiveAdminTx(ctx, tx, targetID); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE identity.local_users
			SET status = 'disabled', account_changed_at = $2, account_changed_by = $3
			WHERE identity_id = $1`, targetID, now.UTC(), actorID); err != nil {
			return nil, fmt.Errorf("disable account: %w", err)
		}
		revoked, err := revokeAllSessionsTx(ctx, tx, targetID, now, TerminationAccountRevoked)
		if err != nil {
			return nil, err
		}
		suspended, err := suspendAllCredentialsTx(ctx, tx, targetID)
		if err != nil {
			return nil, err
		}
		cons.SessionsRevoked = revoked
		cons.CredentialsSuspended = suspended
		return map[string]any{
			"sessionsRevoked":      revoked,
			"credentialsSuspended": suspended,
		}, nil
	})
	if err != nil {
		return Person{}, PersonConsequences{}, err
	}
	// The mutation is already durable, so a failed re-read is a real error rather
	// than an empty person: reporting a zero-value success would misstate what
	// authority the caller now holds.
	person, err := s.GetPerson(ctx, targetID)
	if err != nil {
		return Person{}, cons, fmt.Errorf("read person after disablement: %w", err)
	}
	return person, cons, nil
}

// EnablePerson restores sign-in/review eligibility. It creates no session and
// resumes no suspended or expired credential.
func (s *Store) EnablePerson(ctx context.Context, actorID, targetID string, now time.Time) (Person, error) {
	err := s.mutatePerson(ctx, actorID, targetID, now, EventAccountEnabled, func(tx pgx.Tx, target LocalUser) (map[string]any, error) {
		if target.Status == LocalUserActive {
			return nil, nil
		}
		if target.Status != LocalUserDisabled {
			return nil, ErrPersonStateChanged
		}
		if _, err := tx.Exec(ctx, `
			UPDATE identity.local_users
			SET status = 'active', account_changed_at = $2, account_changed_by = $3
			WHERE identity_id = $1`, targetID, now.UTC(), actorID); err != nil {
			return nil, fmt.Errorf("enable account: %w", err)
		}
		return map[string]any{"resumed_credentials": 0}, nil
	})
	if err != nil {
		return Person{}, err
	}
	return s.GetPerson(ctx, targetID)
}

// RevokePersonSessions revokes every active browser session of another person.
// It exposes no device/location/network detail and reverses no already
// authorized effect; the next authorization is denied.
func (s *Store) RevokePersonSessions(ctx context.Context, actorID, targetID string, now time.Time) (int64, error) {
	var revoked int64
	err := s.mutatePerson(ctx, actorID, targetID, now, EventAccountSessionsRevoked, func(tx pgx.Tx, _ LocalUser) (map[string]any, error) {
		count, err := revokeAllSessionsTx(ctx, tx, targetID, now, TerminationRevoked)
		if err != nil {
			return nil, err
		}
		revoked = count
		if count == 0 {
			// Idempotent: nothing was live, so no mutation evidence is invented.
			return nil, nil
		}
		return map[string]any{"sessionsRevoked": count}, nil
	})
	return revoked, err
}

// mutatePerson runs one audited People mutation under a locked transaction: the
// actor and the target account rows are locked, the current server-side account
// state is re-read (never trusted from the request), the mutation runs, and the
// core audit event commits atomically with it. A failed audit rolls the
// mutation back (architecture 11.2). A no-op commits without inventing a
// mutation event.
func (s *Store) mutatePerson(
	ctx context.Context,
	actorID, targetID string,
	now time.Time,
	event string,
	mutate func(tx pgx.Tx, target LocalUser) (map[string]any, error),
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin people mutation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := lockLocalUserTx(ctx, tx, actorID); err != nil {
		return err
	}
	target, err := lockLocalUserTx(ctx, tx, targetID)
	if err != nil {
		return err
	}

	detail, err := mutate(tx, target)
	if err != nil {
		if errors.Is(err, ErrLastAdmin) {
			// A denied last-Admin mutation is durable evidence even though the
			// mutation itself never happened (separate, successful audit row in
			// the same transaction as the rollback of nothing).
			if auditErr := AppendSecurityEventTx(ctx, tx, SecurityEvent{
				Event:                     EventLastAdminMutationDenied,
				Outcome:                   OutcomeDenied,
				Reason:                    "last_active_admin",
				InitiatingHumanIdentityID: actorID,
				RequestActorIdentityID:    actorID,
				SubjectIdentityID:         targetID,
				CredentialKind:            CredentialBrowser,
				CreatedAt:                 now,
			}); auditErr != nil {
				return auditErr
			}
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return fmt.Errorf("commit last-admin denial: %w", commitErr)
			}
		}
		return err
	}
	if detail == nil {
		return tx.Commit(ctx)
	}

	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:                     event,
		Outcome:                   OutcomeSuccess,
		InitiatingHumanIdentityID: actorID,
		RequestActorIdentityID:    actorID,
		SubjectIdentityID:         targetID,
		CredentialKind:            CredentialBrowser,
		Detail:                    detail,
		CreatedAt:                 now,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit people mutation: %w", err)
	}
	return nil
}

// requireAnotherActiveAdminTx fails when targetID is the only active Admin, so
// a product mutation can never leave zero active Admins (DES-HOR-451-11).
func requireAnotherActiveAdminTx(ctx context.Context, tx pgx.Tx, targetID string) error {
	var others int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM identity.local_users
		WHERE role = 'admin' AND status = 'active' AND identity_id <> $1`, targetID).Scan(&others); err != nil {
		return fmt.Errorf("count other active admins: %w", err)
	}
	if others == 0 {
		return ErrLastAdmin
	}
	return nil
}

// revokeAllSessionsTx terminates every live session inside the caller's
// transaction.
func revokeAllSessionsTx(ctx context.Context, tx pgx.Tx, identityID string, now time.Time, reason string) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE identity.browser_sessions
		SET terminated_at = $2, termination_reason = $3
		WHERE identity_id = $1 AND terminated_at IS NULL`, identityID, now.UTC(), reason)
	if err != nil {
		return 0, fmt.Errorf("revoke sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// suspendAllCredentialsTx suspends every live credential owned by a human.
func suspendAllCredentialsTx(ctx context.Context, tx pgx.Tx, ownerID string) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE identity.api_keys
		SET status = 'suspended', suspension_reason = 'owner_inactive'
		WHERE owner_user_identity_id = $1
		  AND credential_epoch = 'v2'
		  AND status IN ('active', 'retiring')`, ownerID)
	if err != nil {
		return 0, fmt.Errorf("suspend owned credentials: %w", err)
	}
	return tag.RowsAffected(), nil
}

// suspendAdminOnlyCredentialsTx suspends personal credentials that hold an
// Admin-only action, which is the exact clipping rule for Admin -> Operator.
// Operator-only personal credentials keep working.
func suspendAdminOnlyCredentialsTx(ctx context.Context, tx pgx.Tx, ownerID string) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE identity.api_keys
		SET status = 'suspended', suspension_reason = 'owner_role_changed'
		WHERE owner_user_identity_id = $1
		  AND key_type = 'personal'
		  AND credential_epoch = 'v2'
		  AND status IN ('active', 'retiring')
		  AND actions && $2::text[]`, ownerID, AdminOnlyActions())
	if err != nil {
		return 0, fmt.Errorf("suspend admin-only credentials: %w", err)
	}
	return tag.RowsAffected(), nil
}

// suspendOwnedAutomationTx suspends every automation credential owned by a
// demoted Admin: an automation credential is authorized only while its owner is
// a current active Admin.
func suspendOwnedAutomationTx(ctx context.Context, tx pgx.Tx, ownerID string) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE identity.api_keys
		SET status = 'suspended', suspension_reason = 'owner_role_changed'
		WHERE owner_user_identity_id = $1
		  AND key_type = 'automation'
		  AND credential_epoch = 'v2'
		  AND status IN ('active', 'retiring')`, ownerID)
	if err != nil {
		return 0, fmt.Errorf("suspend owned automation: %w", err)
	}
	return tag.RowsAffected(), nil
}
