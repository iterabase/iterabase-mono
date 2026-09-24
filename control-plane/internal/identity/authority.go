package identity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Sentinel errors for the V2 authority epoch.
var (
	// ErrAuthorityCutoverBlocked is returned when preflight finds blockers. The
	// report carries the bounded, operator-safe reason list.
	ErrAuthorityCutoverBlocked = errors.New("identity: authority cutover blocked")
	// ErrAuthorityNotV2 is returned by V2-only surfaces while the installation
	// is still pre-epoch.
	ErrAuthorityNotV2 = errors.New("identity: V2 authority epoch not active")
)

// authorityLockKey is the advisory-lock key serializing epoch transitions.
const authorityLockKey int64 = 0x484F5234_3534 // "HOR454"

// AuthorityState is the durable singleton epoch record (architecture 7.11).
type AuthorityState struct {
	Epoch              string
	SourceFingerprint  string
	PreflightAt        *time.Time
	CutoverID          string
	CutoverAt          *time.Time
	CutoverOperator    string
	CutoverRelease     string
	VerifiedAt         *time.Time
	VerificationResult map[string]any
}

// AuthorityState reads the live epoch record.
func (s *Store) AuthorityState(ctx context.Context) (AuthorityState, error) {
	var st AuthorityState
	var fingerprint, cutoverID, operator, release *string
	var preflightAt, cutoverAt, verifiedAt *time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT epoch, source_fingerprint, preflight_at, cutover_id, cutover_at,
		       cutover_operator, cutover_release, verified_at
		FROM identity.authority_state WHERE id`).
		Scan(&st.Epoch, &fingerprint, &preflightAt, &cutoverID, &cutoverAt,
			&operator, &release, &verifiedAt); err != nil {
		return AuthorityState{}, fmt.Errorf("read authority state: %w", err)
	}
	st.SourceFingerprint = derefString(fingerprint)
	st.CutoverID = derefString(cutoverID)
	st.CutoverOperator = derefString(operator)
	st.CutoverRelease = derefString(release)
	st.PreflightAt = preflightAt
	st.CutoverAt = cutoverAt
	st.VerifiedAt = verifiedAt
	return st, nil
}

// AuthorityEpoch returns the current epoch and latches a permanently observed
// `v2` in memory. The epoch is irreversible (DES-HOR-451-12), so latching it
// can never serve a stale legacy decision once V2 has been observed; a legacy
// observation is never latched and is re-read on every call.
func (s *Store) AuthorityEpoch(ctx context.Context) (string, error) {
	s.epochMu.RLock()
	latched := s.epochV2
	s.epochMu.RUnlock()
	if latched {
		return AuthorityEpochV2, nil
	}
	st, err := s.AuthorityState(ctx)
	if err != nil {
		return "", err
	}
	if st.Epoch == AuthorityEpochV2 {
		s.epochMu.Lock()
		s.epochV2 = true
		s.epochMu.Unlock()
	}
	return st.Epoch, nil
}

// RequireAuthorityV2 fails closed on every V2 customer-authority surface while
// the installation is still pre-epoch.
func (s *Store) RequireAuthorityV2(ctx context.Context) error {
	epoch, err := s.AuthorityEpoch(ctx)
	if err != nil {
		return err
	}
	if epoch != AuthorityEpochV2 {
		return ErrAuthorityNotV2
	}
	return nil
}

// PreflightBlocker is one bounded, operator-safe cutover blocker.
type PreflightBlocker struct {
	Code    string
	Message string
	Count   int64
}

// PreflightReport is the deterministic cutover preflight outcome.
type PreflightReport struct {
	Blockers []PreflightBlocker
	Ready    bool
	Checked  map[string]int64
}

// LegacyCredentialMapping is the operator-supplied disposition for one legacy
// API key that must survive the epoch as an explicitly mapped credential.
type LegacyCredentialMapping struct {
	OwnerIdentityID string   `json:"ownerIdentityID"`
	ActorIdentityID string   `json:"actorIdentityID"`
	Actions         []string `json:"actions,omitempty"`
	RPM             int      `json:"rpm"`
	TPM             int      `json:"tpm"`
	ExpiresAt       string   `json:"expiresAt"`
}

// CutoverManifest is the operator-supplied, reviewed mapping input. Cutover
// blocks rather than guessing whenever a legacy key is not safely unambiguous
// (architecture 15.2).
type CutoverManifest struct {
	DefaultRPM        int                                `json:"defaultRPM"`
	DefaultTPM        int                                `json:"defaultTPM"`
	DefaultExpiryDays int                                `json:"defaultExpiryDays"`
	Credentials       map[string]LegacyCredentialMapping `json:"credentials"`
}

// CutoverOptions carries the operator identity, release, and attestations for
// an epoch transition.
type CutoverOptions struct {
	Operator          string
	Release           string
	Manifest          CutoverManifest
	BackupEvidence    string
	RehearsalEvidence string
	Now               time.Time
}

// legacyWorkActions is the exact fixed work/start subset a legacy `work` key
// maps to. It never includes an Admin, security, or inference action that the
// key could not have exercised before the epoch.
var legacyWorkActions = []string{
	ActionWorkflowsRead, ActionWorkflowsStart, ActionWorkRead,
	ActionWorkFeedbackWrite, ActionArtifactsRead, ActionArtifactsUpload,
}

// legacyGatewayActions is the only inference mapping the architecture permits
// for a legacy `gateway` key (architecture 15.2).
var legacyGatewayActions = []string{ActionInferenceModelsRead, ActionInferenceChatInvoke}

// PreflightAuthority evaluates every cutover blocker without mutating anything.
//
//nolint:gocyclo // Every architecture 15.2 blocker is enumerated in one auditable pass.
func (s *Store) PreflightAuthority(ctx context.Context, opts CutoverOptions) (PreflightReport, error) {
	report := PreflightReport{Checked: map[string]int64{}, Ready: true}
	block := func(code, message string, count int64) {
		if count <= 0 {
			return
		}
		report.Ready = false
		report.Blockers = append(report.Blockers, PreflightBlocker{Code: code, Message: message, Count: count})
	}

	// 1. No deliverable active Admin identity, and no future setup path.
	var activeAdmins, setupAdmins int64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE lu.status = 'active' AND lu.email_normalized LIKE '%_@_%'),
		       count(*) FILTER (WHERE lu.role = 'admin' AND lu.status = 'setup_pending')
		FROM identity.local_users lu
		JOIN identity.identities i ON i.id = lu.identity_id AND i.deleted_at IS NULL
		WHERE lu.role = 'admin'`).Scan(&activeAdmins, &setupAdmins); err != nil {
		return PreflightReport{}, fmt.Errorf("preflight admins: %w", err)
	}
	report.Checked["active_admins"] = activeAdmins
	block("no_active_admin", "no valid active Admin can complete the V2 epoch", boolCount(activeAdmins+setupAdmins == 0))

	// 2. Legacy keys that cannot be safely mapped.
	rows, err := s.pool.Query(ctx, `
		SELECT k.id::text, k.prefix, k.name, COALESCE(k.scope, ''), i.kind, COALESCE(i.deleted_at IS NOT NULL, false)
		FROM identity.api_keys k
		JOIN identity.identities i ON i.id = k.identity_id
		WHERE k.credential_epoch <> 'v2'
		  AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > now())
		ORDER BY k.created_at`)
	if err != nil {
		return PreflightReport{}, fmt.Errorf("preflight credentials: %w", err)
	}
	type legacyKey struct {
		prefix string
		scope  string
		kind   string
	}
	var legacy []legacyKey
	for rows.Next() {
		var id, prefix, name, scope, kind string
		var deleted bool
		if err := rows.Scan(&id, &prefix, &name, &scope, &kind, &deleted); err != nil {
			rows.Close()
			return PreflightReport{}, err
		}
		if deleted {
			block("deleted_key_identity", "an active credential is bound to a deleted identity", 1)
			continue
		}
		_ = id
		legacy = append(legacy, legacyKey{prefix: prefix, scope: scope, kind: kind})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PreflightReport{}, err
	}

	var unsupported, unmappedService, missingRates int64
	for _, key := range legacy {
		switch key.scope {
		case ScopeAdmin, ScopeToken:
			// Revoked by the cutover; never carried forward.
		case ScopeWork:
			if key.kind == "service_account" {
				if _, ok := opts.Manifest.Credentials[key.prefix]; !ok {
					unmappedService++
				}
				if opts.Manifest.DefaultRPM <= 0 || opts.Manifest.DefaultTPM <= 0 {
					missingRates++
				}
			} else if opts.Manifest.DefaultRPM <= 0 || opts.Manifest.DefaultTPM <= 0 {
				missingRates++
			}
		case ScopeGateway:
			if _, ok := opts.Manifest.Credentials[key.prefix]; !ok {
				unsupported++
			}
		default:
			unsupported++
		}
	}
	block("unmapped_legacy_key", "a legacy credential has no approved V2 mapping", unsupported)
	block("unmapped_service_key", "a legacy service credential needs an operator-supplied human owner and service actor", unmappedService)
	block("missing_rate_policy", "a mandatory credential rate policy is missing", missingRates)

	// 3. Wildcard/invalid action rows must not exist on a V2 credential.
	var wildcard int64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM identity.api_keys
		WHERE credential_epoch = 'v2' AND actions IS NOT NULL AND '*' = ANY (actions)`).Scan(&wildcard); err != nil {
		return PreflightReport{}, fmt.Errorf("preflight wildcard: %w", err)
	}
	block("wildcard_action", "a credential still carries a wildcard action", wildcard)

	// 4. Inconsistent epoch evidence.
	var epoch string
	if err := s.pool.QueryRow(ctx, `SELECT epoch FROM identity.authority_state WHERE id`).Scan(&epoch); err != nil {
		return PreflightReport{}, fmt.Errorf("preflight epoch: %w", err)
	}
	if epoch == AuthorityEpochV2 {
		var legacyRows int64
		if err := s.pool.QueryRow(ctx, `
			SELECT count(*) FROM identity.local_users WHERE role = 'user'`).Scan(&legacyRows); err != nil {
			return PreflightReport{}, fmt.Errorf("preflight legacy roles: %w", err)
		}
		block("legacy_role_after_epoch", "the epoch is V2 but legacy local roles remain", legacyRows)
	}

	// 5. Operator attestations the database cannot prove.
	if strings.TrimSpace(opts.BackupEvidence) == "" {
		report.Ready = false
		report.Blockers = append(report.Blockers, PreflightBlocker{
			Code: "missing_backup_evidence", Message: "no rehearsed backup/restore evidence was supplied", Count: 1,
		})
	}
	if strings.TrimSpace(opts.RehearsalEvidence) == "" {
		report.Ready = false
		report.Blockers = append(report.Blockers, PreflightBlocker{
			Code: "missing_rehearsal_evidence", Message: "no rehearsed migration evidence was supplied", Count: 1,
		})
	}
	if strings.TrimSpace(opts.Operator) == "" {
		report.Ready = false
		report.Blockers = append(report.Blockers, PreflightBlocker{
			Code: "missing_operator", Message: "the cutover operator identity is required", Count: 1,
		})
	}
	sort.Slice(report.Blockers, func(i, j int) bool { return report.Blockers[i].Code < report.Blockers[j].Code })
	return report, nil
}

// RecordPreflight stores the durable preflight evidence.
func (s *Store) RecordPreflight(ctx context.Context, fingerprint string, report PreflightReport, now time.Time) error {
	blockers := make([]string, 0, len(report.Blockers))
	for _, b := range report.Blockers {
		blockers = append(blockers, fmt.Sprintf("%s:%d", b.Code, b.Count))
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE identity.authority_state
		SET source_fingerprint = $1, preflight_at = $2, preflight_result = $3`,
		nullString(fingerprint), now.UTC(),
		map[string]any{"ready": report.Ready, "blockers": blockers, "checked": report.Checked})
	if err != nil {
		return fmt.Errorf("record preflight: %w", err)
	}
	return nil
}

// CutoverReport is the durable, operator-visible outcome of an epoch flip.
type CutoverReport struct {
	AlreadyV2            bool
	CutoverID            string
	RolesRewritten       int64
	CredentialsMapped    int64
	CredentialsRevoked   int64
	VerificationPassed   bool
	VerificationFailures []string
}

// CutoverAuthority performs the irreversible V2 authority epoch transition
// (architecture 15.4): one advisory-locked transaction backfills roles and
// credentials, revokes legacy wildcard/admin/token authority, replaces the
// gateway's legacy customer grants with the exact DES-HOR-451-14 union, and
// flips `identity.authority_state.epoch` to `v2`. There is no interval with two
// customer-authority writers.
//
//nolint:gocyclo // The locked epoch transaction is deliberately one sequential contract.
func (s *Store) CutoverAuthority(ctx context.Context, opts CutoverOptions) (CutoverReport, error) {
	report := CutoverReport{}
	preflight := PreflightReport{}
	if err := s.PreflightAuthorityError(ctx, opts, &preflight); err != nil {
		return CutoverReport{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CutoverReport{}, fmt.Errorf("begin cutover: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, authorityLockKey); err != nil {
		return CutoverReport{}, fmt.Errorf("acquire cutover lock: %w", err)
	}

	var epoch string
	if err := tx.QueryRow(ctx, `SELECT epoch FROM identity.authority_state WHERE id FOR UPDATE`).Scan(&epoch); err != nil {
		return CutoverReport{}, fmt.Errorf("lock authority state: %w", err)
	}
	if epoch == AuthorityEpochV2 {
		return CutoverReport{AlreadyV2: true}, nil
	}

	cutoverID := fmt.Sprintf("%s-%s", opts.Now.UTC().Format("20060102T150405Z"), AuthorityEpochV2)

	// 1. Role rewrite: only the approved pre-epoch spelling changes.
	tag, err := tx.Exec(ctx, `
		UPDATE identity.local_users
		SET role = 'operator', role_changed_at = $1
		WHERE role = 'user'`, opts.Now.UTC())
	if err != nil {
		return CutoverReport{}, fmt.Errorf("rewrite roles: %w", err)
	}
	report.RolesRewritten = tag.RowsAffected()

	if _, err := tx.Exec(ctx, `
		ALTER TABLE identity.local_users DROP CONSTRAINT IF EXISTS local_users_role_check;
		ALTER TABLE identity.local_users ADD CONSTRAINT local_users_role_check
			CHECK (role IN ('admin', 'operator'))`); err != nil {
		return CutoverReport{}, fmt.Errorf("tighten role check: %w", err)
	}

	// 2. Legacy credential disposition.
	revoked, err := tx.Exec(ctx, `
		UPDATE identity.api_keys
		SET revoked_at = $1, status = 'revoked', revocation_reason = 'legacy_authority'
		WHERE revoked_at IS NULL AND scope IN ('admin', 'token')`, opts.Now.UTC())
	if err != nil {
		return CutoverReport{}, fmt.Errorf("revoke legacy admin/token keys: %w", err)
	}
	report.CredentialsRevoked = revoked.RowsAffected()

	mapped, err := s.mapLegacyCredentialsTx(ctx, tx, opts)
	if err != nil {
		return CutoverReport{}, err
	}
	report.CredentialsMapped = mapped

	// 3. Any legacy credential that remains unresolved is revoked rather than
	// left as a live pre-epoch authority.
	unresolved, err := tx.Exec(ctx, `
		UPDATE identity.api_keys
		SET revoked_at = $1, status = 'revoked', revocation_reason = 'legacy_unmapped'
		WHERE revoked_at IS NULL AND credential_epoch <> 'v2'`, opts.Now.UTC())
	if err != nil {
		return CutoverReport{}, fmt.Errorf("revoke unresolved legacy keys: %w", err)
	}
	report.CredentialsRevoked += unresolved.RowsAffected()

	// 4. Gateway grant replacement: revoke the legacy customer lookup and the
	// schema-wide/default reads, then materialize the exact approved union.
	if err := applyGatewayAuthorityGrantsTx(ctx, tx); err != nil {
		return CutoverReport{}, err
	}

	// 5. Flip the epoch.
	if _, err := tx.Exec(ctx, `
		UPDATE identity.authority_state
		SET epoch = 'v2', source_fingerprint = $1, cutover_id = $2, cutover_at = $3,
		    cutover_operator = $4, cutover_release = $5,
		    cutover_result = $6`,
		nullString(fingerprintValue(report)), cutoverID, opts.Now.UTC(),
		nullString(opts.Operator), nullString(opts.Release),
		map[string]any{
			"rolesRewritten":     report.RolesRewritten,
			"credentialsMapped":  report.CredentialsMapped,
			"credentialsRevoked": report.CredentialsRevoked,
			"backupEvidence":     opts.BackupEvidence,
			"rehearsalEvidence":  opts.RehearsalEvidence,
		}); err != nil {
		return CutoverReport{}, fmt.Errorf("flip epoch: %w", err)
	}

	operatorIdentityID := resolveOperatorIdentityTx(ctx, tx, opts.Operator)
	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:                     EventAuthorityCutover,
		Outcome:                   OutcomeSuccess,
		InitiatingHumanIdentityID: operatorIdentityID,
		RequestActorIdentityID:    operatorIdentityID,
		CorrelationID:             cutoverID,
		Detail: map[string]any{
			"rolesRewritten":     report.RolesRewritten,
			"credentialsMapped":  report.CredentialsMapped,
			"credentialsRevoked": report.CredentialsRevoked,
		},
		CreatedAt: opts.Now,
	}); err != nil {
		return CutoverReport{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return CutoverReport{}, fmt.Errorf("commit cutover: %w", err)
	}

	s.epochMu.Lock()
	s.epochV2 = true
	s.epochMu.Unlock()

	report.CutoverID = cutoverID
	verification := s.VerifyAuthority(ctx)
	report.VerificationPassed = verification.Passed
	report.VerificationFailures = verification.Failures
	if err := s.recordVerification(ctx, verification, opts.Now); err != nil {
		return report, err
	}
	return report, nil
}

// PreflightAuthorityError runs preflight and returns ErrAuthorityCutoverBlocked
// with the report attached when any blocker exists.
func (s *Store) PreflightAuthorityError(ctx context.Context, opts CutoverOptions, report *PreflightReport) error {
	got, err := s.PreflightAuthority(ctx, opts)
	if err != nil {
		return err
	}
	*report = got
	if !got.Ready {
		codes := make([]string, 0, len(got.Blockers))
		for _, b := range got.Blockers {
			codes = append(codes, b.Code)
		}
		return fmt.Errorf("%w: %s", ErrAuthorityCutoverBlocked, strings.Join(codes, ","))
	}
	return nil
}

// mapLegacyCredentialsTx materializes the approved V2 row for every legacy
// credential whose disposition is unambiguous, preserving canonical UUIDs and
// existing key material.
//
//nolint:gocyclo // The legacy disposition table is one explicit, blocking mapping.
func (s *Store) mapLegacyCredentialsTx(ctx context.Context, tx pgx.Tx, opts CutoverOptions) (int64, error) {
	expiryDays := opts.Manifest.DefaultExpiryDays
	if expiryDays <= 0 {
		expiryDays = 90
	}
	defaultExpiry := opts.Now.UTC().Add(time.Duration(expiryDays) * 24 * time.Hour)

	rows, err := tx.Query(ctx, `
		SELECT k.id::text, k.prefix, COALESCE(k.scope, ''), k.identity_id::text, i.kind, lu.identity_id::text, lu.role, lu.status
		FROM identity.api_keys k
		JOIN identity.identities i ON i.id = k.identity_id
		LEFT JOIN identity.local_users lu ON lu.identity_id = k.identity_id
		WHERE k.revoked_at IS NULL AND k.credential_epoch <> 'v2'
		FOR UPDATE OF k`)
	if err != nil {
		return 0, fmt.Errorf("select legacy credentials: %w", err)
	}
	type row struct {
		id, prefix, scope, identityID, kind, localUserID, role, status string
	}
	var selected []row
	for rows.Next() {
		var r row
		var localUser, role, status *string
		if err := rows.Scan(&r.id, &r.prefix, &r.scope, &r.identityID, &r.kind, &localUser, &role, &status); err != nil {
			rows.Close()
			return 0, err
		}
		r.localUserID, r.role, r.status = derefString(localUser), derefString(role), derefString(status)
		selected = append(selected, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var mapped int64
	for _, r := range selected {
		kind := CredentialKindPersonal
		ownerID := r.identityID
		actorID := r.identityID
		actions := legacyWorkActions

		switch r.scope {
		case ScopeWork:
			if r.kind == "service_account" || r.localUserID == "" {
				kind = CredentialKindAutomation
				manifest, ok := opts.Manifest.Credentials[r.prefix]
				if !ok {
					continue
				}
				ownerID = manifest.OwnerIdentityID
				actorID = firstNonEmpty(manifest.ActorIdentityID, r.identityID)
			} else if NormalizeRole(r.role) == "" || r.status != LocalUserActive {
				// A work key bound to an inactive human is not attributable to an
				// eligible owner; revoke rather than widen it.
				continue
			}
		case ScopeGateway:
			kind = CredentialKindAutomation
			manifest, ok := opts.Manifest.Credentials[r.prefix]
			if !ok {
				continue
			}
			ownerID = manifest.OwnerIdentityID
			actorID = firstNonEmpty(manifest.ActorIdentityID, r.identityID)
			actions = legacyGatewayActions
		default:
			continue
		}

		if ownerID == actorID && kind == CredentialKindAutomation {
			continue
		}
		if kind == CredentialKindPersonal && ownerID != actorID {
			continue
		}

		rpm, tpm := opts.Manifest.DefaultRPM, opts.Manifest.DefaultTPM
		expiresAt := defaultExpiry
		if manifest, ok := opts.Manifest.Credentials[r.prefix]; ok {
			if manifest.RPM > 0 {
				rpm = manifest.RPM
			}
			if manifest.TPM > 0 {
				tpm = manifest.TPM
			}
			if len(manifest.Actions) > 0 {
				actions = manifest.Actions
			}
			if manifest.ExpiresAt != "" {
				parsed, err := time.Parse(time.RFC3339, manifest.ExpiresAt)
				if err != nil {
					return 0, fmt.Errorf("legacy credential %s expiry: %w", r.prefix, err)
				}
				expiresAt = parsed
			}
		}
		if rpm <= 0 || tpm <= 0 {
			return 0, fmt.Errorf("%w: missing rate policy for legacy credential %s", ErrAuthorityCutoverBlocked, r.prefix)
		}
		ownerRole := ""
		if kind == CredentialKindPersonal {
			ownerRole = NormalizeRole(r.role)
		}
		if kind == CredentialKindAutomation {
			var adminRole string
			if err := tx.QueryRow(ctx, `
				SELECT role FROM identity.local_users WHERE identity_id = $1`, ownerID).Scan(&adminRole); err != nil {
				return 0, fmt.Errorf("%w: legacy credential %s owner is not an active Admin", ErrAuthorityCutoverBlocked, r.prefix)
			}
			ownerRole = NormalizeRole(adminRole)
		}
		if err := ValidateActions(kind, actions, ownerRole); err != nil {
			return 0, fmt.Errorf("%w: legacy credential %s: %v", ErrAuthorityCutoverBlocked, r.prefix, err)
		}

		tag, err := tx.Exec(ctx, `
			UPDATE identity.api_keys
			SET key_type = $2, owner_user_identity_id = $3, actor_identity_id = $4,
			    actions = $5, rate_rpm = $6, rate_tpm = $7, expires_at = $8,
			    credential_family_id = COALESCE(credential_family_id, id),
			    credential_epoch = 'v2', status = 'active',
			    suspension_reason = NULL, scope = NULL
			WHERE id = $1 AND revoked_at IS NULL`,
			r.id, kind, ownerID, actorID, actions, rpm, tpm, expiresAt)
		if err != nil {
			return 0, fmt.Errorf("map legacy credential: %w", err)
		}
		mapped += tag.RowsAffected()
	}
	return mapped, nil
}

// AuthorityVerification is the post-cutover verification evidence.
type AuthorityVerification struct {
	Passed   bool
	Failures []string
	Checked  map[string]int64
}

// VerifyAuthority proves the post-epoch invariants hold (architecture 15.5).
func (s *Store) VerifyAuthority(ctx context.Context) AuthorityVerification {
	result := AuthorityVerification{Passed: true, Checked: map[string]int64{}}
	fail := func(message string, count int64) {
		if count > 0 {
			result.Passed = false
			result.Failures = append(result.Failures, fmt.Sprintf("%s (%d)", message, count))
		}
	}

	var epoch string
	if err := s.pool.QueryRow(ctx, `SELECT epoch FROM identity.authority_state WHERE id`).Scan(&epoch); err != nil || epoch != AuthorityEpochV2 {
		result.Passed = false
		result.Failures = append(result.Failures, "authority epoch is not v2")
		return result
	}
	result.Checked["epoch"] = 1

	var legacyRoles int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM identity.local_users WHERE role = 'user'`).Scan(&legacyRoles); err == nil {
		fail("legacy local roles remain", legacyRoles)
	}
	var liveLegacyKeys int64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM identity.api_keys
		WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now()) AND credential_epoch <> 'v2'`).Scan(&liveLegacyKeys); err == nil {
		fail("live pre-epoch credentials remain", liveLegacyKeys)
	}
	var wildcard int64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM identity.api_keys WHERE actions IS NOT NULL AND '*' = ANY (actions)`).Scan(&wildcard); err == nil {
		fail("wildcard credential actions remain", wildcard)
	}
	var liveScope int64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM identity.api_keys
		WHERE revoked_at IS NULL AND scope IS NOT NULL`).Scan(&liveScope); err == nil {
		fail("live credentials still carry a legacy scope", liveScope)
	}
	var legacyView bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('identity.active_api_keys') IS NOT NULL`).Scan(&legacyView); err == nil {
		if legacyView {
			fail("legacy gateway customer lookup remains", 1)
		}
	}
	return result
}

// resolveOperatorIdentityTx maps the operator's stated identity (an email of a
// known human or an already-canonical identity reference) to a UUID anchor so
// the cutover audit row stays attributable without inventing a principal.
func resolveOperatorIdentityTx(ctx context.Context, tx pgx.Tx, operator string) string {
	operator = strings.TrimSpace(operator)
	if operator == "" {
		return ""
	}
	if normalized, _, ok := CanonicalEmail(operator); ok {
		var id string
		if err := tx.QueryRow(ctx, `
			SELECT identity_id::text FROM identity.local_users WHERE email_normalized = $1`,
			normalized).Scan(&id); err == nil {
			return id
		}
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM identity.identities WHERE id::text = $1`, operator).Scan(&id); err == nil {
		return id
	}
	return ""
}

func (s *Store) recordVerification(ctx context.Context, v AuthorityVerification, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE identity.authority_state
		SET verified_at = $1, verification_result = $2`,
		now.UTC(), map[string]any{"passed": v.Passed, "failures": v.Failures, "checked": v.Checked})
	if err != nil {
		return fmt.Errorf("record verification: %w", err)
	}
	return nil
}

// applyGatewayAuthorityGrantsTx replaces the legacy schema-wide/default gateway
// reads with the exact DES-HOR-451-14 union. It is idempotent and conditional on
// the dedicated role existing, matching migrations 000008 and 000023.
func applyGatewayAuthorityGrantsTx(ctx context.Context, tx pgx.Tx) error {
	statements := []string{
		// The legacy gateway customer lookup retires unconditionally: it is the
		// pre-epoch authority object, not a grant, and must not survive the flip
		// even on a development database without the dedicated role.
		`DROP VIEW IF EXISTS identity.active_api_keys`,
		`DO $$
		DECLARE
			cur_user text := current_user;
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway') THEN
				RETURN;
			END IF;

			-- Schema-wide/default reads retire with the legacy customer lookup.
			EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA identity    REVOKE SELECT ON TABLES FROM gateway', cur_user);
			EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA permissions REVOKE SELECT ON TABLES FROM gateway', cur_user);
			EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA catalog     REVOKE SELECT ON TABLES FROM gateway', cur_user);

			GRANT USAGE ON SCHEMA identity, catalog, usage, permissions, toolgateway, runtime TO gateway;
			REVOKE CREATE ON SCHEMA identity, catalog, usage FROM gateway;

			-- Customer authority: bounded projection reads plus the payload-free
			-- usage ledger. Never a mutation or raw-table read.
			GRANT SELECT ON identity.inference_api_credentials, catalog.effective_api_catalog TO gateway;
			GRANT INSERT ON usage.inference_events TO gateway;

			-- Separately approved non-customer routing/workload reads.
			GRANT SELECT ON catalog.effective_catalog,
			                permissions.effective_capabilities,
			                permissions.effective_rate_limits,
			                toolgateway.pools,
			                runtime.turns,
			                runtime.workflow_runs,
			                runtime.run_pool_assignments,
			                runtime.turn_assignments
				TO gateway;
		END $$`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("apply gateway authority grants: %w", err)
		}
	}
	return nil
}

// AuthoritySourceFingerprint is a bounded, non-secret summary of the pre-epoch
// authority inputs, used to detect source drift between preflight and cutover.
func (s *Store) AuthoritySourceFingerprint(ctx context.Context) (string, error) {
	var users, admins, keys, gateways, wildcard int64
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM identity.local_users),
		       (SELECT count(*) FROM identity.local_users WHERE role = 'admin'),
		       (SELECT count(*) FROM identity.api_keys WHERE revoked_at IS NULL),
		       (SELECT count(*) FROM identity.api_keys WHERE revoked_at IS NULL AND scope = 'gateway'),
		       (SELECT count(*) FROM identity.api_keys WHERE revoked_at IS NULL AND key_type IS NOT NULL AND actions IS NOT NULL AND '*' = ANY (actions))`).
		Scan(&users, &admins, &keys, &gateways, &wildcard); err != nil {
		return "", fmt.Errorf("authority fingerprint: %w", err)
	}
	return fmt.Sprintf("users=%d;admins=%d;keys=%d;gateway_keys=%d;wildcard=%d", users, admins, keys, gateways, wildcard), nil
}

func fingerprintValue(report CutoverReport) string {
	return fmt.Sprintf("roles=%d;mapped=%d;revoked=%d", report.RolesRewritten, report.CredentialsMapped, report.CredentialsRevoked)
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func boolCount(condition bool) int64 {
	if condition {
		return 1
	}
	return 0
}
