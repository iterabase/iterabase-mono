package identity

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
)

// authorityHarness seeds a V2 authority fixture over a migrated database.
type authorityHarness struct {
	store *Store
	now   time.Time
}

// newAuthorityHarness anchors the harness clock to the wall clock. The live
// credential projection evaluates expiry against the database clock, so a
// hardcoded harness date would eventually make every fixture credential look
// expired.
func newAuthorityHarness(t *testing.T) *authorityHarness {
	t.Helper()
	return &authorityHarness{
		store: NewStore(testutil.NewPostgresPool(t)),
		now:   time.Now().UTC().Truncate(time.Second),
	}
}

// localUser creates an active account with the requested role.
func (h *authorityHarness) localUser(t *testing.T, email, role string) LocalUser {
	t.Helper()
	user, err := h.store.UpsertLocalUser(context.Background(), email, email, role)
	require.NoError(t, err)
	_, err = h.store.pool.Exec(context.Background(), `
		UPDATE identity.local_users
		SET status = 'active', password_hash = 'x', password_changed_at = now()
		WHERE identity_id = $1`, user.ID)
	require.NoError(t, err)
	return user
}

// reviewerOptions builds cutover options carrying the reviewed source
// fingerprint, which preflight and the locked cutover both require.
func (h *authorityHarness) reviewedOptions(t *testing.T, opts CutoverOptions) CutoverOptions {
	t.Helper()
	fingerprint, err := h.store.AuthoritySourceFingerprint(context.Background())
	require.NoError(t, err)
	opts.ExpectedFingerprint = fingerprint
	return opts
}

func (h *authorityHarness) credential(t *testing.T, params CreateCredentialParams) (string, Credential) {
	t.Helper()
	params.Now = h.now
	full, credential, err := h.store.CreateCredentialVersion(context.Background(), params)
	require.NoError(t, err)
	return full, credential
}

func TestCredentialAuthorityClipsOnOwnerTransitions(t *testing.T) {
	h := newAuthorityHarness(t)
	ctx := context.Background()

	admin := h.localUser(t, "admin@example.com", RoleAdmin)
	operator := h.localUser(t, "operator@example.com", RoleOperator)
	serviceActor, err := h.store.UpsertServiceAccount(ctx, "automation-actor@example.com", "Automation Actor")
	require.NoError(t, err)

	// An operator-only personal credential keeps working across nothing.
	operatorKey, _ := h.credential(t, CreateCredentialParams{
		Kind: CredentialKindPersonal, OwnerIdentity: operator.ID, ActorIdentity: operator.ID,
		Name: "observe", Actions: []string{ActionWorkflowsRead, ActionWorkRead},
		RateRPM: 60, RateTPM: 60000, ExpiresAt: h.now.Add(24 * time.Hour),
	})
	credential, _, _, err := h.store.ResolveCredential(ctx, operatorKey, h.now)
	require.NoError(t, err)
	assert.Equal(t, CredentialKindPersonal, credential.Kind)
	require.NoError(t, credential.AuthorizeAction(ActionWorkRead))
	assert.ErrorIs(t, credential.AuthorizeAction(ActionPeopleRead), ErrCredentialInsufficient)
	assert.ErrorIs(t, credential.AuthorizeAction(ActionValueRead), ErrCredentialInsufficient)

	// An Admin-only personal credential created by the Admin.
	adminKey, _ := h.credential(t, CreateCredentialParams{
		Kind: CredentialKindPersonal, OwnerIdentity: admin.ID, ActorIdentity: admin.ID,
		Name: "reporting", Actions: []string{ActionWorkflowsRead, ActionWorkRead, ActionValueRead},
		RateRPM: 60, RateTPM: 60000, ExpiresAt: h.now.Add(24 * time.Hour),
	})
	adminCredential, _, _, err := h.store.ResolveCredential(ctx, adminKey, h.now)
	require.NoError(t, err)
	require.NoError(t, adminCredential.AuthorizeAction(ActionValueRead))

	// An automation credential owned by the Admin with a distinct service actor.
	automationKey, _ := h.credential(t, CreateCredentialParams{
		Kind: CredentialKindAutomation, OwnerIdentity: admin.ID, ActorIdentity: serviceActor.ID,
		Name: "nightly", Actions: []string{ActionWorkflowsRead, ActionWorkflowsStart},
		RateRPM: 30, RateTPM: 30000, ExpiresAt: h.now.Add(48 * time.Hour),
	})
	automation, owner, actor, err := h.store.ResolveCredential(ctx, automationKey, h.now)
	require.NoError(t, err)
	assert.Equal(t, admin.ID, owner.ID, "automation keeps its accountable human owner")
	assert.Equal(t, serviceActor.ID, actor.ID, "automation actor is the distinct service identity")
	assert.Equal(t, admin.ID, automation.OwnerIdentityID)
	assert.NotEqual(t, automation.OwnerIdentityID, automation.ActorIdentityID)

	// A second active Admin keeps the demotion below legal.
	second := h.localUser(t, "second-admin@example.com", RoleAdmin)

	// Demotion clips the Admin-only personal credential and every owned
	// automation credential, while the operator-only personal credential of the
	// demoted... Admin has none, so the operator's own credential is untouched.
	_, _, err = h.store.ChangePersonRole(ctx, second.ID, admin.ID, RoleOperator, h.now)
	require.NoError(t, err)

	_, _, _, err = h.store.ResolveCredential(ctx, adminKey, h.now)
	assert.ErrorIs(t, err, ErrCredentialNotEligible)
	_, _, _, err = h.store.ResolveCredential(ctx, automationKey, h.now)
	assert.ErrorIs(t, err, ErrCredentialNotEligible)
	_, _, _, err = h.store.ResolveCredential(ctx, operatorKey, h.now)
	assert.NoError(t, err, "an unrelated Operator credential is unaffected")

	// Promotion never resumes the suspended credentials.
	_, _, err = h.store.ChangePersonRole(ctx, second.ID, admin.ID, RoleAdmin, h.now)
	require.NoError(t, err)
	_, _, _, err = h.store.ResolveCredential(ctx, adminKey, h.now)
	assert.ErrorIs(t, err, ErrCredentialNotEligible, "re-enable/promotion never resumes a suspended key")
	_, _, _, err = h.store.ResolveCredential(ctx, automationKey, h.now)
	assert.ErrorIs(t, err, ErrCredentialNotEligible)
}

func TestCredentialOwnerRequirementsAndCatalogue(t *testing.T) {
	h := newAuthorityHarness(t)
	ctx := context.Background()
	admin := h.localUser(t, "admin@example.com", RoleAdmin)
	operator := h.localUser(t, "operator@example.com", RoleOperator)
	serviceActor, err := h.store.UpsertServiceAccount(ctx, "actor@example.com", "Actor")
	require.NoError(t, err)

	base := CreateCredentialParams{
		Name: "attempt", RateRPM: 60, RateTPM: 60000, ExpiresAt: h.now.Add(time.Hour), Now: h.now,
	}
	// Personal owner must equal actor.
	personal := base
	personal.Kind, personal.OwnerIdentity, personal.ActorIdentity = CredentialKindPersonal, admin.ID, serviceActor.ID
	personal.Actions = []string{ActionWorkRead}
	_, _, err = h.store.CreateCredentialVersion(ctx, personal)
	assert.Error(t, err)

	// Automation actor must differ from owner.
	automation := base
	automation.Kind, automation.OwnerIdentity, automation.ActorIdentity = CredentialKindAutomation, admin.ID, admin.ID
	automation.Actions = []string{ActionWorkRead}
	_, _, err = h.store.CreateCredentialVersion(ctx, automation)
	assert.Error(t, err)

	// Automation owner must be a current active Admin.
	notAdmin := base
	notAdmin.Kind, notAdmin.OwnerIdentity, notAdmin.ActorIdentity = CredentialKindAutomation, operator.ID, serviceActor.ID
	notAdmin.Actions = []string{ActionWorkRead}
	_, _, err = h.store.CreateCredentialVersion(ctx, notAdmin)
	assert.ErrorIs(t, err, ErrOwnerNotEligible)

	// Operator personal credentials cannot hold Admin-only actions.
	adminOnly := base
	adminOnly.Kind, adminOnly.OwnerIdentity, adminOnly.ActorIdentity = CredentialKindPersonal, operator.ID, operator.ID
	adminOnly.Actions = []string{ActionPeopleRead}
	_, _, err = h.store.CreateCredentialVersion(ctx, adminOnly)
	assert.ErrorIs(t, err, ErrInvalidCredentialAction)

	// Automation cannot hold Operator-only actions.
	feedback := base
	feedback.Kind, feedback.OwnerIdentity, feedback.ActorIdentity = CredentialKindAutomation, admin.ID, serviceActor.ID
	feedback.Actions = []string{ActionWorkRead, ActionWorkFeedbackWrite}
	_, _, err = h.store.CreateCredentialVersion(ctx, feedback)
	assert.ErrorIs(t, err, ErrInvalidCredentialAction)

	// Unknown action, wildcard action, prerequisite gaps, and mandatory rate
	// policy are all rejected.
	for name, mutate := range map[string]func(*CreateCredentialParams){
		"unknown":      func(p *CreateCredentialParams) { p.Actions = []string{"work.*"} },
		"wildcard":     func(p *CreateCredentialParams) { p.Actions = []string{"*"} },
		"missing-rpm":  func(p *CreateCredentialParams) { p.RateRPM = 0 },
		"missing-tpm":  func(p *CreateCredentialParams) { p.RateTPM = 0 },
		"prerequisite": func(p *CreateCredentialParams) { p.Actions = []string{ActionArtifactsDelete} },
		"expiry":       func(p *CreateCredentialParams) { p.ExpiresAt = time.Time{} },
	} {
		params := base
		params.Kind, params.OwnerIdentity, params.ActorIdentity = CredentialKindPersonal, admin.ID, admin.ID
		params.Actions = []string{ActionWorkRead}
		mutate(&params)
		_, _, err := h.store.CreateCredentialVersion(ctx, params)
		assert.Error(t, err, "case %s must be rejected", name)
	}

	// The database enforces the same boundary as the application catalogue.
	_, err = h.store.pool.Exec(ctx, `
		INSERT INTO identity.api_keys (identity_id, key_hash, prefix, name, key_type, owner_user_identity_id, actor_identity_id, actions, rate_rpm, rate_tpm, expires_at, credential_family_id, credential_epoch)
		VALUES ($1, 'db-check', 'cp-dbcheck', 'db', 'personal', $1, $1, ARRAY['*'], 1, 1, now() + interval '1 hour', gen_random_uuid(), 'v2')`, admin.ID)
	assert.Error(t, err, "the database must reject a wildcard action")
}

func TestPeopleMutationsRevokeSessionsAndProtectLastAdmin(t *testing.T) {
	h := newAuthorityHarness(t)
	ctx := context.Background()
	first := h.localUser(t, "first@example.com", RoleAdmin)
	second := h.localUser(t, "second@example.com", RoleAdmin)
	target := h.localUser(t, "operator@example.com", RoleOperator)

	_, _, err := h.store.ChangePersonRole(ctx, first.ID, second.ID, RoleOperator, h.now)
	require.NoError(t, err)
	_, _, err = h.store.ChangePersonRole(ctx, first.ID, first.ID, RoleOperator, h.now)
	assert.ErrorIs(t, err, ErrLastAdmin, "the last active Admin cannot be demoted")
	_, _, err = h.store.DisablePerson(ctx, first.ID, first.ID, h.now)
	assert.ErrorIs(t, err, ErrLastAdmin, "the last active Admin cannot be disabled")

	// Session revocation is audited and bounded.
	_, err = h.store.pool.Exec(ctx, `
		INSERT INTO identity.browser_sessions (token_hash, identity_id, csrf_hash, idle_expires_at, absolute_expires_at)
		VALUES ('hash-target', $1, 'csrf', now() + interval '1 hour', now() + interval '1 day')`, target.ID)
	require.NoError(t, err)
	revoked, err := h.store.RevokePersonSessions(ctx, first.ID, target.ID, h.now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, revoked)
	revoked, err = h.store.RevokePersonSessions(ctx, first.ID, target.ID, h.now)
	require.NoError(t, err)
	assert.EqualValues(t, 0, revoked, "revocation is idempotent")

	// Disable then enable never resumes a credential.
	serviceActor, err := h.store.UpsertServiceAccount(ctx, "svc@example.com", "Svc")
	require.NoError(t, err)
	key, _ := h.credential(t, CreateCredentialParams{
		Kind: CredentialKindAutomation, OwnerIdentity: first.ID, ActorIdentity: serviceActor.ID,
		Name: "owned", Actions: []string{ActionWorkRead},
		RateRPM: 5, RateTPM: 5000, ExpiresAt: h.now.Add(time.Hour),
	})
	_, _, _, err = h.store.ResolveCredential(ctx, key, h.now)
	require.NoError(t, err)

	// Promote second back so disabling first is legal.
	_, _, err = h.store.ChangePersonRole(ctx, first.ID, second.ID, RoleAdmin, h.now)
	require.NoError(t, err)
	_, _, err = h.store.DisablePerson(ctx, second.ID, first.ID, h.now)
	require.NoError(t, err)
	_, _, _, err = h.store.ResolveCredential(ctx, key, h.now)
	assert.ErrorIs(t, err, ErrCredentialNotEligible)

	_, err = h.store.EnablePerson(ctx, second.ID, first.ID, h.now)
	require.NoError(t, err)
	_, _, _, err = h.store.ResolveCredential(ctx, key, h.now)
	assert.ErrorIs(t, err, ErrCredentialNotEligible, "re-enable never resumes a suspended key")
}

func TestAuthorityCutoverIsAtomicAndVerified(t *testing.T) {
	h := newAuthorityHarness(t)
	ctx := context.Background()

	// Seed a pre-epoch install: a legacy `user` role, a human work key, a
	// revocable admin key, and a legacy gateway key with a manifest.
	legacyHuman, err := h.store.UpsertLocalUser(ctx, "legacy@example.com", "legacy@example.com", "user")
	require.NoError(t, err)
	_, err = h.store.pool.Exec(ctx, `UPDATE identity.local_users SET status = 'active' WHERE identity_id = $1`, legacyHuman.ID)
	require.NoError(t, err)
	admin := h.localUser(t, "admin@example.com", RoleAdmin)

	var workPrefix, gatewayPrefix string
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		INSERT INTO identity.api_keys (identity_id, key_hash, prefix, name, scope)
		VALUES ($1, 'legacy-work', 'cp-legacywork', 'legacy-work', 'work')
		RETURNING prefix`, legacyHuman.ID).Scan(&workPrefix))
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		INSERT INTO identity.api_keys (identity_id, key_hash, prefix, name, scope)
		VALUES ($1, 'legacy-admin', 'cp-legacyadmin', 'legacy-admin', 'admin')
		RETURNING prefix`, admin.ID).Scan(&gatewayPrefix))
	serviceActor, err := h.store.UpsertServiceAccount(ctx, "gateway-svc@example.com", "Gateway Svc")
	require.NoError(t, err)
	var gatewayKeyPrefix string
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		INSERT INTO identity.api_keys (identity_id, key_hash, prefix, name, scope)
		VALUES ($1, 'legacy-gateway', 'cp-legacygateway', 'legacy-gateway', 'gateway')
		RETURNING prefix`, serviceActor.ID).Scan(&gatewayKeyPrefix))

	opts := h.reviewedOptions(t, CutoverOptions{
		Operator:          "operator@example.com",
		Release:           "test-release",
		BackupEvidence:    "rehearsal-2026-09-24",
		RehearsalEvidence: "rehearsal-2026-09-24",
		Manifest: CutoverManifest{
			DefaultRPM: 60, DefaultTPM: 60000, DefaultExpiryDays: 30,
			Credentials: map[string]LegacyCredentialMapping{
				gatewayKeyPrefix: {OwnerIdentityID: admin.ID, ActorIdentityID: serviceActor.ID, RPM: 10, TPM: 10000},
			},
		},
		Now: h.now,
	})

	report, err := h.store.PreflightAuthority(ctx, opts)
	require.NoError(t, err)
	require.True(t, report.Ready, "preflight blockers: %+v", report.Blockers)

	cutover, err := h.store.CutoverAuthority(ctx, opts)
	require.NoError(t, err)
	assert.False(t, cutover.AlreadyV2)
	assert.True(t, cutover.VerificationPassed, "verification failures: %v", cutover.VerificationFailures)
	assert.EqualValues(t, 1, cutover.RolesRewritten)
	assert.EqualValues(t, 2, cutover.CredentialsMapped, "work + gateway keys are explicitly mapped")
	assert.GreaterOrEqual(t, cutover.CredentialsRevoked, int64(1), "the legacy admin key is revoked")

	// Role rewrite and epoch.
	var role string
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT role FROM identity.local_users WHERE identity_id = $1`, legacyHuman.ID).Scan(&role))
	assert.Equal(t, RoleOperator, role)
	state, err := h.store.AuthorityState(ctx)
	require.NoError(t, err)
	assert.Equal(t, AuthorityEpochV2, state.Epoch)
	assert.NotEmpty(t, state.CutoverID)

	// The migrated work credential keeps its bytes and now resolves through the
	// V2 authority path with the fixed work action subset.
	_, _, _, err = h.store.ResolveCredential(ctx, legacyWorkKeyFull, h.now)
	assert.ErrorIs(t, err, ErrInvalidAPIKey, "the raw legacy value is not a V2 key")

	var mapped Credential
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT `+credentialColumns+` FROM identity.effective_api_credentials WHERE api_key_id = (
			SELECT id FROM identity.api_keys WHERE prefix = $1)`, workPrefix).Scan(
		&mapped.ID, &mapped.FamilyID, &mapped.Version, &mapped.Kind, &mapped.Status, &mapped.ExpiresAt,
		&mapped.Actions, &mapped.OwnerIdentityID, &mapped.OwnerStatus, &mapped.OwnerRole,
		&mapped.ActorIdentityID, &mapped.ActorKind, &mapped.ActorRole, &mapped.RateRPM, &mapped.RateTPM,
		&mapped.Epoch, &mapped.Eligible, &mapped.SuspensionReason))
	assert.True(t, mapped.Eligible)
	assert.Equal(t, CredentialKindPersonal, mapped.Kind)
	assert.Equal(t, legacyHuman.ID, mapped.OwnerIdentityID)
	assert.Equal(t, legacyHuman.ID, mapped.ActorIdentityID)
	assert.ElementsMatch(t, legacyWorkActions, mapped.Actions)
	assert.Equal(t, 60, mapped.RateRPM)

	// The gateway credential is mapped only to the two approved inference actions.
	var gatewayActions []string
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT actions FROM identity.api_keys WHERE prefix = $1`, gatewayKeyPrefix).Scan(&gatewayActions))
	assert.ElementsMatch(t, legacyGatewayActions, gatewayActions)

	// A second cutover is a strict no-op.
	again, err := h.store.CutoverAuthority(ctx, opts)
	require.NoError(t, err)
	assert.True(t, again.AlreadyV2)
}

func TestAuthorityPreflightBlocksAmbiguousMigration(t *testing.T) {
	h := newAuthorityHarness(t)
	ctx := context.Background()
	admin := h.localUser(t, "admin@example.com", RoleAdmin)
	serviceActor, err := h.store.UpsertServiceAccount(ctx, "svc@example.com", "Svc")
	require.NoError(t, err)
	_, err = h.store.pool.Exec(ctx, `
		INSERT INTO identity.api_keys (identity_id, key_hash, prefix, name, scope)
		VALUES ($1, 'legacy-gateway', 'cp-unmappedgw', 'legacy-gateway', 'gateway')`, serviceActor.ID)
	require.NoError(t, err)

	opts := h.reviewedOptions(t, CutoverOptions{
		Operator: "operator", BackupEvidence: "b", RehearsalEvidence: "r", Now: h.now,
		Manifest: CutoverManifest{DefaultRPM: 60, DefaultTPM: 60000},
	})
	report, err := h.store.PreflightAuthority(ctx, opts)
	require.NoError(t, err)
	assert.False(t, report.Ready)
	codes := make([]string, 0, len(report.Blockers))
	for _, blocker := range report.Blockers {
		codes = append(codes, blocker.Code)
	}
	assert.Contains(t, codes, "unmapped_legacy_key")

	_, err = h.store.CutoverAuthority(ctx, opts)
	assert.ErrorIs(t, err, ErrAuthorityCutoverBlocked)
	state, stateErr := h.store.AuthorityState(ctx)
	require.NoError(t, stateErr)
	assert.Equal(t, AuthorityEpochLegacy, state.Epoch, "a blocked cutover leaves no partial epoch")
	assert.ErrorIs(t, h.store.RequireAuthorityV2(ctx), ErrAuthorityNotV2)
	assert.NotEmpty(t, admin.ID)
}

// legacyWorkKeyFull documents that legacy raw values never resolve after the
// epoch: only their explicitly mapped hash row survives.
const legacyWorkKeyFull = "cp-legacywork-raw-value"

// TestAuthorityRefusesReviewedSourceDrift proves the cutover re-derives the
// reviewed source fingerprint under the advisory lock and refuses to apply a
// plan to changed pre-epoch authority (architecture 15.2/15.4).
func TestAuthorityRefusesReviewedSourceDrift(t *testing.T) {
	h := newAuthorityHarness(t)
	ctx := context.Background()
	admin := h.localUser(t, "admin@example.com", RoleAdmin)

	opts := h.reviewedOptions(t, CutoverOptions{
		Operator: "operator@example.com", BackupEvidence: "b", RehearsalEvidence: "r",
		Manifest: CutoverManifest{DefaultRPM: 60, DefaultTPM: 60000}, Now: h.now,
	})

	// Pre-epoch authority changes after the operator reviewed the plan: a new
	// human account joins the installation. The service-account identity created
	// above is not itself counted by the fingerprint (it holds no credential and
	// no local account), so the drift must be observable.
	h.localUser(t, "late-arrival@example.com", RoleOperator)

	report, err := h.store.PreflightAuthority(ctx, opts)
	require.NoError(t, err)
	require.False(t, report.Ready)
	assert.Contains(t, blockerCodes(report), "source_fingerprint_drift")

	_, err = h.store.CutoverAuthority(ctx, opts)
	assert.ErrorIs(t, err, ErrAuthorityCutoverBlocked)

	state, stateErr := h.store.AuthorityState(ctx)
	require.NoError(t, stateErr)
	assert.Equal(t, AuthorityEpochLegacy, state.Epoch, "drift must not flip the epoch")

	// A preflight that names the current state succeeds.
	refreshed := h.reviewedOptions(t, opts)
	cutover, err := h.store.CutoverAuthority(ctx, refreshed)
	require.NoError(t, err)
	assert.True(t, cutover.VerificationPassed, "failures: %v", cutover.VerificationFailures)
	_ = admin
}

// TestAuthorityRecordsPreflightEvidence proves the reviewed fingerprint and the
// preflight/verification evidence are observable through the store rather than
// only with raw SQL.
func TestAuthorityRecordsPreflightEvidence(t *testing.T) {
	h := newAuthorityHarness(t)
	ctx := context.Background()
	h.localUser(t, "admin@example.com", RoleAdmin)

	opts := h.reviewedOptions(t, CutoverOptions{
		Operator: "admin@example.com", Release: "test", BackupEvidence: "b", RehearsalEvidence: "r",
		Manifest: CutoverManifest{DefaultRPM: 60, DefaultTPM: 60000}, Now: h.now,
	})
	_, err := h.store.CutoverAuthority(ctx, opts)
	require.NoError(t, err)

	state, err := h.store.AuthorityState(ctx)
	require.NoError(t, err)
	assert.Equal(t, opts.ExpectedFingerprint, state.SourceFingerprint,
		"source_fingerprint must carry the reviewed pre-epoch fingerprint, not a result summary")
	assert.NotNil(t, state.PreflightAt)
	assert.NotNil(t, state.PreflightResult)
	assert.NotNil(t, state.CutoverResult)
	assert.NotNil(t, state.VerificationResult)
	assert.Equal(t, true, state.VerificationResult["passed"])
}

// TestAuthorityNeverResurrectsExpiredLegacyKey proves an expired-but-unrevoked
// legacy credential is never remapped as an active V2 credential with a fresh
// expiry; it is revoked instead (architecture 15.2, no widening).
func TestAuthorityNeverResurrectsExpiredLegacyKey(t *testing.T) {
	h := newAuthorityHarness(t)
	ctx := context.Background()
	h.localUser(t, "admin@example.com", RoleAdmin)
	human := h.localUser(t, "human@example.com", RoleOperator)

	var prefix string
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		INSERT INTO identity.api_keys (identity_id, key_hash, prefix, name, scope, expires_at)
		VALUES ($1, 'legacy-expired', 'cp-expired', 'legacy-expired', 'work', now() - interval '1 hour')
		RETURNING prefix`, human.ID).Scan(&prefix))

	opts := h.reviewedOptions(t, CutoverOptions{
		Operator: "operator@example.com", BackupEvidence: "b", RehearsalEvidence: "r",
		Manifest: CutoverManifest{DefaultRPM: 60, DefaultTPM: 60000, DefaultExpiryDays: 30}, Now: h.now,
	})
	report, err := h.store.PreflightAuthority(ctx, opts)
	require.NoError(t, err)
	require.True(t, report.Ready, "blockers: %v", report.Blockers)
	assert.EqualValues(t, 1, report.Checked["expired_legacy_credentials"])
	assert.Contains(t, report.Fingerprint, "keys=1")

	cutover, err := h.store.CutoverAuthority(ctx, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 0, cutover.CredentialsMapped, "an expired credential must never be remapped")
	assert.GreaterOrEqual(t, cutover.CredentialsRevoked, int64(1))

	var epoch string
	var revoked bool
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT credential_epoch, revoked_at IS NOT NULL FROM identity.api_keys WHERE prefix = $1`,
		prefix).Scan(&epoch, &revoked))
	assert.Equal(t, "legacy", epoch)
	assert.True(t, revoked, "the expired legacy credential is revoked, not revived")
}

// TestAuthorityRejectsWideningActionOverride proves a manifest override may only
// narrow the scope's approved subset (architecture 15.2: block rather than
// widen).
func TestAuthorityRejectsWideningActionOverride(t *testing.T) {
	h := newAuthorityHarness(t)
	ctx := context.Background()
	admin := h.localUser(t, "admin@example.com", RoleAdmin)
	serviceActor, err := h.store.UpsertServiceAccount(ctx, "gw@example.com", "GW")
	require.NoError(t, err)

	var gatewayPrefix string
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		INSERT INTO identity.api_keys (identity_id, key_hash, prefix, name, scope)
		VALUES ($1, 'legacy-gw', 'cp-gw', 'legacy-gw', 'gateway')
		RETURNING prefix`, serviceActor.ID).Scan(&gatewayPrefix))

	// A gateway key must never receive a work action, even from the Admin owner.
	opts := h.reviewedOptions(t, CutoverOptions{
		Operator: "operator@example.com", BackupEvidence: "b", RehearsalEvidence: "r",
		Manifest: CutoverManifest{
			DefaultRPM: 60, DefaultTPM: 60000,
			Credentials: map[string]LegacyCredentialMapping{
				gatewayPrefix: {
					OwnerIdentityID: admin.ID, ActorIdentityID: serviceActor.ID,
					Actions: []string{ActionInferenceModelsRead, ActionWorkflowsStart},
					RPM:     10, TPM: 10000,
				},
			},
		},
		Now: h.now,
	})
	report, err := h.store.PreflightAuthority(ctx, opts)
	require.NoError(t, err)
	require.False(t, report.Ready)
	assert.Contains(t, blockerCodes(report), "manifest_actions_widened")

	_, err = h.store.CutoverAuthority(ctx, opts)
	assert.ErrorIs(t, err, ErrAuthorityCutoverBlocked)

	// Narrowing within the scope subset is allowed.
	narrowed := opts
	narrowed.Manifest.Credentials[gatewayPrefix] = LegacyCredentialMapping{
		OwnerIdentityID: admin.ID, ActorIdentityID: serviceActor.ID,
		Actions: []string{ActionInferenceChatInvoke}, RPM: 10, TPM: 10000,
	}
	narrowed = h.reviewedOptions(t, narrowed)
	cutover, err := h.store.CutoverAuthority(ctx, narrowed)
	require.NoError(t, err)
	require.True(t, cutover.VerificationPassed, "failures: %v", cutover.VerificationFailures)
}

// TestAuthorityBlocksIneligibleAutomationOwner proves 15.2's automation-owner
// blocker: a disabled Admin (or a demoted Operator) cannot own a remapped
// automation credential.
func TestAuthorityBlocksIneligibleAutomationOwner(t *testing.T) {
	h := newAuthorityHarness(t)
	ctx := context.Background()
	admin := h.localUser(t, "admin@example.com", RoleAdmin)
	demoted := h.localUser(t, "demoted@example.com", RoleOperator)
	serviceActor, err := h.store.UpsertServiceAccount(ctx, "gw@example.com", "GW")
	require.NoError(t, err)

	var prefix string
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		INSERT INTO identity.api_keys (identity_id, key_hash, prefix, name, scope)
		VALUES ($1, 'legacy-gw', 'cp-gw', 'legacy-gw', 'gateway')
		RETURNING prefix`, serviceActor.ID).Scan(&prefix))

	for name, owner := range map[string]string{"operator owner": demoted.ID, "disabled admin": admin.ID} {
		if name == "disabled admin" {
			_, err := h.store.pool.Exec(ctx,
				`UPDATE identity.local_users SET status = 'disabled' WHERE identity_id = $1`, admin.ID)
			require.NoError(t, err)
		}
		opts := CutoverOptions{
			Operator: "operator@example.com", BackupEvidence: "b", RehearsalEvidence: "r",
			Manifest: CutoverManifest{
				DefaultRPM: 60, DefaultTPM: 60000,
				Credentials: map[string]LegacyCredentialMapping{
					prefix: {OwnerIdentityID: owner, ActorIdentityID: serviceActor.ID, RPM: 10, TPM: 10000},
				},
			},
			Now: h.now,
		}
		opts = h.reviewedOptions(t, opts)
		report, err := h.store.PreflightAuthority(ctx, opts)
		require.NoError(t, err)
		require.False(t, report.Ready, "%s must block", name)
		assert.Contains(t, blockerCodes(report), "manifest_owner_not_active_admin", "case %s", name)
	}
}

// TestAuthorityMaterializesExactGatewayGrantUnion proves the cutover removes the
// legacy schema-wide/default gateway reads, keeps the wider identity projection
// out of the gateway's reach, and preserves every required routing/workload
// read (DES-HOR-451-14, architecture 7.12/15.4.4).
func TestAuthorityMaterializesExactGatewayGrantUnion(t *testing.T) {
	pool, _ := testutil.NewPostgresWithRoles(t, "gateway")
	h := &authorityHarness{store: NewStore(pool), now: time.Now().UTC().Truncate(time.Second)}
	ctx := context.Background()
	h.localUser(t, "admin@example.com", RoleAdmin)

	// The migration-8 default privileges really did materialize broad reads.
	var broadBefore int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'identity' AND c.relkind = 'r'
		  AND has_table_privilege('gateway', c.oid, 'SELECT')`).Scan(&broadBefore))
	require.Positive(t, broadBefore, "fixture must reproduce the legacy schema-wide grant")

	opts := h.reviewedOptions(t, CutoverOptions{
		Operator: "admin@example.com", BackupEvidence: "b", RehearsalEvidence: "r",
		Manifest: CutoverManifest{DefaultRPM: 60, DefaultTPM: 60000}, Now: h.now,
	})
	cutover, err := h.store.CutoverAuthority(ctx, opts)
	require.NoError(t, err)
	require.True(t, cutover.VerificationPassed, "failures: %v", cutover.VerificationFailures)

	// No relation outside the approved union stays readable.
	rows, err := pool.Query(ctx, `
		SELECT format('%I.%I', n.nspname, c.relname)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN ('identity', 'permissions', 'catalog', 'toolgateway', 'runtime', 'usage')
		  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		  AND has_table_privilege('gateway', c.oid, 'SELECT')
		ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()
	var readable []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		readable = append(readable, name)
	}
	require.NoError(t, rows.Err())
	assert.ElementsMatch(t, approvedGatewayReadObjects, readable,
		"the gateway must hold exactly the approved read union")

	// The wider action payload and every identity/security table are unreachable.
	for _, denied := range []string{
		"identity.effective_api_credentials",
		"identity.browser_sessions",
		"identity.security_events",
		"identity.authority_state",
		"identity.api_key_ownership_history",
	} {
		var has bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT has_table_privilege('gateway', $1, 'SELECT')`, denied).Scan(&has))
		assert.False(t, has, "gateway must not read %s", denied)
	}
	// The payload-free usage ledger stays insert-only.
	var canInsert, canSelect bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT has_table_privilege('gateway', 'usage.inference_events', 'INSERT'),
		        has_table_privilege('gateway', 'usage.inference_events', 'SELECT')`).Scan(&canInsert, &canSelect))
	assert.True(t, canInsert)
	assert.False(t, canSelect, "the usage ledger is append-only for the gateway")
}

func blockerCodes(report PreflightReport) []string {
	codes := make([]string, 0, len(report.Blockers))
	for _, blocker := range report.Blockers {
		codes = append(codes, blocker.Code)
	}
	return codes
}
