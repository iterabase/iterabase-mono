package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Sentinel errors for V2 credential authority.
var (
	// ErrCredentialNotEligible is returned when a syntactically valid credential
	// is currently ineligible (revoked, expired, suspended, or owned by an
	// inactive/non-Admin owner). It intentionally does not distinguish the
	// reason to the caller; Reason() exposes it for audit.
	ErrCredentialNotEligible = errors.New("identity: credential not eligible")
	// ErrCredentialInsufficient is returned when the credential is eligible but
	// its immutable actions do not cover the required action.
	ErrCredentialInsufficient = errors.New("identity: credential action missing")
	// ErrInvalidCredentialAction is returned when an action is outside the fixed
	// catalogue or outside the credential kind's boundary.
	ErrInvalidCredentialAction = errors.New("identity: invalid credential action")
	// ErrOwnerNotEligible is returned when an automation owner is not a current
	// active Admin, or a personal owner is not an active Operator/Admin.
	ErrOwnerNotEligible = errors.New("identity: credential owner not eligible")
)

// credentialLastUseCoalesce bounds last-used writes so the request path does not
// write on every call (architecture 7.7).
const credentialLastUseCoalesce = 5 * time.Minute

// Credential is one live row of identity.effective_api_credentials: the complete
// current authority of a bearer request. It never carries the raw key.
type Credential struct {
	ID               string
	FamilyID         string
	Version          int
	Kind             string // personal | automation
	Status           string
	ExpiresAt        time.Time
	Actions          []string
	OwnerIdentityID  string
	OwnerStatus      string
	OwnerRole        string
	ActorIdentityID  string
	ActorKind        string
	ActorRole        string
	RateRPM          int
	RateTPM          int
	Epoch            string
	Eligible         bool
	SuspensionReason string
}

//nolint:gosec // Column list, not a credential literal.
const credentialColumns = `api_key_id, credential_family_id, credential_version, key_type,
	credential_status, expires_at, actions, owner_identity_id, owner_status, owner_role,
	actor_identity_id, actor_kind, actor_role, rate_rpm, rate_tpm, credential_epoch,
	eligible, COALESCE(suspension_reason, '')`

func scanCredential(row pgx.Row) (Credential, error) {
	var c Credential
	err := row.Scan(&c.ID, &c.FamilyID, &c.Version, &c.Kind,
		&c.Status, &c.ExpiresAt, &c.Actions, &c.OwnerIdentityID, &c.OwnerStatus, &c.OwnerRole,
		&c.ActorIdentityID, &c.ActorKind, &c.ActorRole, &c.RateRPM, &c.RateTPM, &c.Epoch,
		&c.Eligible, &c.SuspensionReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	return c, err
}

// ResolveCredential resolves a bearer value to its current authority. It performs
// one indexed hash lookup against the live projection (never a cached or stale
// snapshot) so revocation, expiry, disablement, and demotion take effect on the
// next request. An ineligible credential returns ErrCredentialNotEligible with
// Credential carrying the audit reason.
func (s *Store) ResolveCredential(ctx context.Context, fullKey string, now time.Time) (Credential, Identity, Identity, error) {
	hash := HashAPIKey(fullKey)
	cred, err := scanCredential(s.pool.QueryRow(ctx, `
		SELECT `+credentialColumns+`
		FROM identity.effective_api_credentials WHERE key_hash = $1`, hash))
	if errors.Is(err, ErrNotFound) {
		return Credential{}, Identity{}, Identity{}, ErrInvalidAPIKey
	}
	if err != nil {
		return Credential{}, Identity{}, Identity{}, fmt.Errorf("resolve credential: %w", err)
	}

	owner, err := s.identityOrMissing(ctx, cred.OwnerIdentityID)
	if err != nil {
		return Credential{}, Identity{}, Identity{}, err
	}
	actor, err := s.identityOrMissing(ctx, cred.ActorIdentityID)
	if err != nil {
		return Credential{}, Identity{}, Identity{}, err
	}
	if !cred.Eligible {
		return cred, owner, actor, ErrCredentialNotEligible
	}
	s.touchCredentialLastUse(ctx, cred.ID, now)
	return cred, owner, actor, nil
}

// AuthorizeAction reports whether the resolved credential's immutable actions
// cover the required action, including prerequisites.
func (c Credential) AuthorizeAction(required string) error {
	if !c.Eligible {
		return ErrCredentialNotEligible
	}
	if !ActionsSatisfied(c.Actions, required) {
		return ErrCredentialInsufficient
	}
	return nil
}

// AdminOnlyActions returns the personal actions gated on a current Admin. Used
// by role transitions to clip credentials whose owner loses Admin.
func AdminOnlyActions() []string {
	var out []string
	for _, action := range CatalogueActions(CredentialKindPersonal) {
		if ActionAdminOnly(action) {
			out = append(out, action)
		}
	}
	return out
}

func (s *Store) identityOrMissing(ctx context.Context, id string) (Identity, error) {
	ident, err := s.GetIdentityByID(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Identity{ID: id}, nil
	}
	return ident, err
}

// touchCredentialLastUse stamps last_used_at at most once per coalescing window.
// A write failure never changes an already-decided authorization.
func (s *Store) touchCredentialLastUse(ctx context.Context, id string, now time.Time) {
	_, _ = s.pool.Exec(ctx, `
		UPDATE identity.api_keys SET last_used_at = $2
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < $2 - $3::interval)`,
		id, now.UTC(), credentialLastUseCoalesce.String())
}

// CreateCredentialParams describes a new V2 credential version.
type CreateCredentialParams struct {
	Kind          string
	OwnerIdentity string
	ActorIdentity string
	Name          string
	Purpose       string
	Actions       []string
	RateRPM       int
	RateTPM       int
	ExpiresAt     time.Time
	CreatedBy     string
	Now           time.Time
}

// CreateCredentialVersion issues a new credential version under one locked
// lifecycle transaction: it re-checks the owner's *current* account/role, which
// is what makes an active-Admin automation owner an invariant rather than a
// snapshot. The raw secret is returned once; only its hash is persisted.
//
//nolint:gocyclo // Owner/actor/action/rate/expiry validation is intentionally one fail-closed gate.
func (s *Store) CreateCredentialVersion(ctx context.Context, params CreateCredentialParams) (string, Credential, error) {
	if params.Kind != CredentialKindPersonal && params.Kind != CredentialKindAutomation {
		return "", Credential{}, ErrInvalidCredentialAction
	}
	if params.Kind == CredentialKindPersonal && params.OwnerIdentity != params.ActorIdentity {
		return "", Credential{}, fmt.Errorf("identity: personal credential owner must equal its actor")
	}
	if params.Kind == CredentialKindAutomation && params.OwnerIdentity == params.ActorIdentity {
		return "", Credential{}, fmt.Errorf("identity: automation credential actor must differ from its owner")
	}
	if params.RateRPM <= 0 || params.RateTPM <= 0 {
		return "", Credential{}, fmt.Errorf("identity: credential rate policy is mandatory")
	}
	if params.ExpiresAt.IsZero() || !params.ExpiresAt.After(params.Now) {
		return "", Credential{}, fmt.Errorf("identity: credential expiry is mandatory")
	}

	full, prefix, hash, err := GenerateAPIKey()
	if err != nil {
		return "", Credential{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", Credential{}, fmt.Errorf("begin credential create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	owner, err := lockLocalUserTx(ctx, tx, params.OwnerIdentity)
	if err != nil {
		return "", Credential{}, err
	}
	if err := validateOwnerEligibility(params.Kind, owner); err != nil {
		return "", Credential{}, err
	}
	if err := ValidateActions(params.Kind, params.Actions, NormalizeRole(owner.Role)); err != nil {
		return "", Credential{}, fmt.Errorf("%w: %v", ErrInvalidCredentialAction, err)
	}

	var id, familyID string
	err = tx.QueryRow(ctx, `
		INSERT INTO identity.api_keys
			(identity_id, key_hash, prefix, name, purpose, expires_at,
			 key_type, owner_user_identity_id, actor_identity_id, actions,
			 rate_rpm, rate_tpm, credential_family_id, credential_version,
			 created_by_identity_id, credential_epoch, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			gen_random_uuid(), 1, $13, 'v2', 'active')
		RETURNING id, credential_family_id`,
		params.ActorIdentity, hash, prefix, params.Name, params.Purpose, params.ExpiresAt.UTC(),
		params.Kind, params.OwnerIdentity, params.ActorIdentity, params.Actions,
		params.RateRPM, params.RateTPM, nullString(params.CreatedBy)).
		Scan(&id, &familyID)
	if err != nil {
		return "", Credential{}, fmt.Errorf("insert credential: %w", err)
	}

	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:                     EventCredentialCreated,
		Outcome:                   OutcomeSuccess,
		InitiatingHumanIdentityID: params.OwnerIdentity,
		RequestActorIdentityID:    params.ActorIdentity,
		SubjectIdentityID:         params.OwnerIdentity,
		APIKeyID:                  id,
		KeyOwnerIdentityID:        params.OwnerIdentity,
		CredentialKind:            params.Kind,
		Detail: map[string]any{
			"actions": params.Actions,
			"rateRPM": params.RateRPM,
			"rateTPM": params.RateTPM,
		},
		CreatedAt: params.Now,
	}); err != nil {
		return "", Credential{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", Credential{}, fmt.Errorf("commit credential create: %w", err)
	}

	cred, err := s.credentialByID(ctx, id)
	if err != nil {
		return "", Credential{}, err
	}
	return full, cred, nil
}

// credentialByID reads one live projection row by credential id.
func (s *Store) credentialByID(ctx context.Context, id string) (Credential, error) {
	return scanCredential(s.pool.QueryRow(ctx, `
		SELECT `+credentialColumns+`
		FROM identity.effective_api_credentials WHERE api_key_id = $1`, id))
}

// lockLocalUserTx locks the canonical local-user row for a lifecycle mutation,
// so concurrent role/account/credential changes serialize on the account.
func lockLocalUserTx(ctx context.Context, tx pgx.Tx, identityID string) (LocalUser, error) {
	lu, err := scanLocalUser(tx.QueryRow(ctx, `
		SELECT `+localUserColumns+`
		FROM identity.local_users lu
		JOIN identity.identities i ON i.id = lu.identity_id
		WHERE lu.identity_id = $1
		FOR UPDATE OF lu`, identityID))
	if errors.Is(err, ErrNotFound) {
		return LocalUser{}, ErrNotFound
	}
	return lu, err
}

// validateOwnerEligibility enforces the owner prerequisite at creation time:
// personal issuance requires an active Operator/Admin; automation requires an
// active Admin.
func validateOwnerEligibility(kind string, owner LocalUser) error {
	if owner.Status != LocalUserActive {
		return ErrOwnerNotEligible
	}
	role := NormalizeRole(owner.Role)
	switch kind {
	case CredentialKindPersonal:
		if role != RoleAdmin && role != RoleOperator {
			return ErrOwnerNotEligible
		}
	case CredentialKindAutomation:
		if role != RoleAdmin {
			return ErrOwnerNotEligible
		}
	default:
		return ErrOwnerNotEligible
	}
	return nil
}

// RecordOwnershipTransfer appends permanent ownership history. It is the only
// writer of identity.api_key_ownership_history and never updates or deletes a
// prior row. Automation transfers are the sole caller (architecture 7.8, 5.4).
func (s *Store) RecordOwnershipTransfer(
	ctx context.Context,
	tx pgx.Tx,
	familyID, credentialID, priorOwner, newOwner, transferredBy, reason, correlationID string,
	now time.Time,
) error {
	if priorOwner == newOwner {
		return fmt.Errorf("identity: ownership transfer must change the owner")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO identity.api_key_ownership_history
			(credential_family_id, credential_id, prior_owner_identity_id,
			 new_owner_identity_id, transferred_by_identity_id, reason, correlation_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		familyID, credentialID, priorOwner, newOwner, transferredBy,
		nullString(reason), nullString(correlationID), now.UTC())
	if err != nil {
		return fmt.Errorf("insert ownership history: %w", err)
	}
	return nil
}
