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

// serviceActor creates a distinct service identity for automation credentials.
func (h *authorityHarness) serviceActor(t *testing.T, key string) Identity {
	t.Helper()
	actor, err := scanIdentity(h.store.pool.QueryRow(context.Background(), `
		INSERT INTO identity.identities (key, kind, source, display_name)
		VALUES ($1, 'service_account', 'local', $1)
		RETURNING id, key, kind, source, display_name`, key))
	require.NoError(t, err)
	return actor
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
	serviceActor := h.serviceActor(t, "automation-actor@example.com")

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
	serviceActor := h.serviceActor(t, "actor@example.com")

	base := CreateCredentialParams{
		Name: "attempt", RateRPM: 60, RateTPM: 60000, ExpiresAt: h.now.Add(time.Hour), Now: h.now,
	}
	// Personal owner must equal actor.
	personal := base
	personal.Kind, personal.OwnerIdentity, personal.ActorIdentity = CredentialKindPersonal, admin.ID, serviceActor.ID
	personal.Actions = []string{ActionWorkRead}
	_, _, err := h.store.CreateCredentialVersion(ctx, personal)
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
		INSERT INTO identity.api_keys (identity_id, key_hash, prefix, name, key_type, owner_user_identity_id, actor_identity_id, actions, rate_rpm, rate_tpm, expires_at, credential_family_id)
		VALUES ($1, 'db-check', 'cp-dbcheck', 'db', 'personal', $1, $1, ARRAY['*'], 1, 1, now() + interval '1 hour', gen_random_uuid())`, admin.ID)
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
	serviceActor := h.serviceActor(t, "svc@example.com")
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
