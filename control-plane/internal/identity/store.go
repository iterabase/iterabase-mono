// Package identity implements the control-plane identity service: the Postgres
// store for identities, local accounts, browser sessions, and V2 API
// credentials. It is shared by the manager (workflow identities) and the api
// (HTTP endpoints + bootstrap).
package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors.
var (
	// ErrNotFound is returned when no identity/binding matches.
	ErrNotFound = errors.New("identity: not found")
	// ErrInvalidAPIKey is returned when a bearer value matches no V2 credential.
	ErrInvalidAPIKey = errors.New("identity: invalid api key")
)

// Identity is a row from identity.identities.
type Identity struct {
	ID          string
	Key         string
	Kind        string // user | group | service_account | workflow
	Source      string // local | external | external-auto
	DisplayName string
}

// LocalUser is an identity with a local_users satellite row. Email is the
// original validated delivery address; EmailNormalized is the canonical unique
// key used for lookup.
type LocalUser struct {
	Identity
	Email                   string
	EmailNormalized         string
	DisplayName             string
	Role                    string // admin | operator (legacy `user` normalizes to operator)
	Locale                  string // en | pt
	Status                  string // setup_pending | active | disabled
	PasswordHash            string
	PasswordChangedAt       *time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
	ApprovedAccessRequestID string
}

// Store reads and writes the identity schema via a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a pool for identity operations.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// UpsertIdentity inserts an identity keyed by `key`, or — if the row exists
// (including a soft-deleted one) — revives it and updates kind/source/display.
// This is the reconciler's primary write on add/update and the foundation of
// revive-on-recreate (HOR-242 Q9).
func (s *Store) UpsertIdentity(ctx context.Context, key, kind, source, displayName string) (Identity, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO identity.identities (key, kind, source, display_name)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (key) DO UPDATE
			SET kind         = EXCLUDED.kind,
			    source       = EXCLUDED.source,
			    display_name = EXCLUDED.display_name,
			    deleted_at   = NULL,
			    updated_at   = now()
		RETURNING id, key, kind, source, display_name`,
		key, kind, source, displayName)

	return scanIdentity(row)
}

// SoftDeleteIdentityByKey marks the identity inactive (deleted_at). Used by the
// workflow reconciler on CR deletion: the row is kept for usage/history while
// its workload capability is withdrawn (HOR-242 Q9).
func (s *Store) SoftDeleteIdentityByKey(ctx context.Context, key string) error {
	// Zero rows is fine: the identity was already deleted or never existed.
	if _, err := s.pool.Exec(ctx, `
		UPDATE identity.identities SET deleted_at = now()
		WHERE key = $1 AND deleted_at IS NULL`, key); err != nil {
		return fmt.Errorf("soft delete: %w", err)
	}
	return nil
}

// GetIdentityByID fetches an active identity by its UUID.
func (s *Store) GetIdentityByID(ctx context.Context, id string) (Identity, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, key, kind, source, display_name
		FROM identity.identities WHERE id = $1 AND deleted_at IS NULL`, id)

	ident, err := scanIdentity(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrNotFound
	}
	return ident, err
}

// UpsertLocalUser creates or revives a local-user identity keyed by `key`
// (canonical normalized email), and upserts its `setup_pending` local_users
// satellite row. Idempotent; it never sets a password or creates a session.
func (s *Store) UpsertLocalUser(ctx context.Context, key, email, role string) (LocalUser, error) {
	delivery, normalized, ok := CanonicalEmail(email)
	if !ok {
		return LocalUser{}, fmt.Errorf("invalid email %q", email)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LocalUser{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var identID string
	err = tx.QueryRow(ctx, `
		INSERT INTO identity.identities (key, kind, source, display_name)
		VALUES ($1, 'user', 'local', $2)
		ON CONFLICT (key) DO UPDATE
			SET deleted_at   = NULL,
			    display_name = EXCLUDED.display_name,
			    updated_at   = now()
		RETURNING id`, normalized, delivery).Scan(&identID)
	if err != nil {
		return LocalUser{}, fmt.Errorf("upsert identity: %w", err)
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO identity.local_users (identity_id, email, email_normalized, display_name, role, status)
		VALUES ($1, $2, $3, $4, $5, 'setup_pending')
		ON CONFLICT (identity_id) DO UPDATE
			SET email = EXCLUDED.email, email_normalized = EXCLUDED.email_normalized,
			    role = EXCLUDED.role, updated_at = now()
		RETURNING identity_id`,
		identID, delivery, normalized, delivery, role).Scan(&identID)
	if err != nil {
		return LocalUser{}, fmt.Errorf("upsert local_user: %w", err)
	}

	// Backfill the rest of the identity fields for a complete LocalUser.
	ident, err := s.identityInTx(ctx, tx, identID)
	if err != nil {
		return LocalUser{}, err
	}

	lu, err := scanLocalUser(tx.QueryRow(ctx, `
		SELECT `+localUserColumns+`
		FROM identity.local_users lu
		JOIN identity.identities i ON i.id = lu.identity_id
		WHERE lu.identity_id = $1`, identID))
	if err != nil {
		return LocalUser{}, err
	}
	lu.Identity = ident

	if err := tx.Commit(ctx); err != nil {
		return LocalUser{}, fmt.Errorf("commit: %w", err)
	}
	return lu, nil
}

// GetLocalUser returns the local user for an identity ID.
func (s *Store) GetLocalUser(ctx context.Context, identityID string) (LocalUser, error) {
	lu, err := scanLocalUser(s.pool.QueryRow(ctx, `
		SELECT `+localUserColumns+`
		FROM identity.local_users lu
		JOIN identity.identities i ON i.id = lu.identity_id
		WHERE lu.identity_id = $1 AND i.deleted_at IS NULL`, identityID))
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalUser{}, ErrNotFound
	}
	return lu, err
}

// scanIdentity scans a 5-column identity row.
func scanIdentity(row pgx.Row) (Identity, error) {
	var i Identity
	err := row.Scan(&i.ID, &i.Key, &i.Kind, &i.Source, &i.DisplayName)
	return i, err
}

// identityInTx fetches an identity within an existing transaction.
func (s *Store) identityInTx(ctx context.Context, tx pgx.Tx, id string) (Identity, error) {
	return scanIdentity(tx.QueryRow(ctx, `
		SELECT id, key, kind, source, display_name
		FROM identity.identities WHERE id = $1`, id))
}
