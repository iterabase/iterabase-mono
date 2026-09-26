package identity

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateActionsIsOrderIndependent proves the creation-time validator and
// the request-path matcher agree about an identical action set regardless of
// slice order (they previously disagreed for prerequisites listed later).
func TestValidateActionsIsOrderIndependent(t *testing.T) {
	cases := map[string]struct {
		kind      string
		role      string
		a, b      []string
		wantValid bool
	}{
		"artifacts delete prerequisites": {
			kind: CredentialKindPersonal, role: RoleAdmin,
			a:         []string{ActionArtifactsRead, ActionArtifactsDelete},
			b:         []string{ActionArtifactsDelete, ActionArtifactsRead},
			wantValid: true,
		},
		"value read alternative prerequisite": {
			kind: CredentialKindPersonal, role: RoleAdmin,
			a:         []string{ActionWorkRead, ActionValueRead},
			b:         []string{ActionValueRead, ActionWorkRead},
			wantValid: true,
		},
		"value read via workflows read": {
			kind: CredentialKindPersonal, role: RoleAdmin,
			a:         []string{ActionWorkflowsRead, ActionValueRead},
			b:         []string{ActionValueRead, ActionWorkflowsRead},
			wantValid: true,
		},
		"missing prerequisite rejected in both orders": {
			kind: CredentialKindPersonal, role: RoleAdmin,
			a:         []string{ActionArtifactsDelete},
			b:         []string{ActionArtifactsDelete},
			wantValid: false,
		},
		"unknown action rejected": {
			kind: CredentialKindPersonal, role: RoleAdmin,
			a: []string{"work.*"}, b: []string{"work.*"},
			wantValid: false,
		},
		"wildcard rejected": {
			kind: CredentialKindPersonal, role: RoleAdmin,
			a: []string{"*"}, b: []string{"*"},
			wantValid: false,
		},
		"kind boundary rejected": {
			kind: CredentialKindAutomation, role: RoleAdmin,
			a: []string{ActionWorkFeedbackWrite}, b: []string{ActionWorkFeedbackWrite},
			wantValid: false,
		},
		"admin-only rejected for operator": {
			kind: CredentialKindPersonal, role: RoleOperator,
			a: []string{ActionPeopleRead}, b: []string{ActionPeopleRead},
			wantValid: false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			errA := ValidateActions(tc.kind, tc.a, tc.role)
			errB := ValidateActions(tc.kind, tc.b, tc.role)
			if tc.wantValid {
				require.NoError(t, errA)
				require.NoError(t, errB, "order must not change acceptance")
				assert.True(t, ActionsSatisfied(tc.a, tc.a[len(tc.a)-1]))
				return
			}
			require.Error(t, errA)
			require.Error(t, errB, "order must not change rejection")
		})
	}
}

// TestActionCatalogueMatchesDatabaseChecks drives the migration 000027 CHECK
// constraints from the in-process catalogue, so drift between the two layers
// cannot go unnoticed. Every action must be one the catalogue knows, every
// catalogue set must be accepted by the database, and every action outside a
// kind's catalogue must be rejected.
func TestActionCatalogueMatchesDatabaseChecks(t *testing.T) {
	h := newAuthorityHarness(t)
	ctx := context.Background()
	admin := h.localUser(t, "admin@example.com", RoleAdmin)
	serviceActor, err := h.store.UpsertServiceAccount(ctx, "actor@example.com", "Actor")
	require.NoError(t, err)

	for _, action := range append(CatalogueActions(CredentialKindPersonal), CatalogueActions(CredentialKindAutomation)...) {
		assert.True(t, KnownAction(action), "catalogue action %q must be known", action)
	}

	expiry := time.Now().UTC().Add(24 * time.Hour)
	insert := func(kind, owner, actor string, actions []string) error {
		_, err := h.store.pool.Exec(ctx, `
			INSERT INTO identity.api_keys
				(identity_id, key_hash, prefix, name, key_type, owner_user_identity_id,
				 actor_identity_id, actions, rate_rpm, rate_tpm, expires_at,
				 credential_family_id, credential_epoch)
			VALUES ($1, $2, $3, 'parity', $4, $5, $6, $7, 60, 60000, $8, gen_random_uuid(), 'v2')`,
			actor, "hash-"+kind+"-"+actions[0], "cp-"+kind+actions[0], kind, owner, actor, actions, expiry)
		return err
	}

	// The full catalogue set of each kind must be insertable.
	for _, kind := range []string{CredentialKindPersonal, CredentialKindAutomation} {
		owner, actor := admin.ID, admin.ID
		if kind == CredentialKindAutomation {
			actor = serviceActor.ID
		}
		assert.NoError(t, insert(kind, owner, actor, CatalogueActions(kind)),
			"the database must accept every %s catalogue action", kind)
	}

	// Actions outside a kind's catalogue, and unknown actions, must be rejected.
	assert.Error(t, insert(CredentialKindAutomation, admin.ID, serviceActor.ID, []string{ActionWorkFeedbackWrite}),
		"automation must reject a personal-only action")
	assert.Error(t, insert(CredentialKindPersonal, admin.ID, admin.ID, []string{ActionWorkFeedbackWrite, "work.unknown"}),
		"the database must reject an unknown action")
	assert.Error(t, insert(CredentialKindPersonal, admin.ID, admin.ID, []string{"*"}),
		"the database must reject a wildcard action")
}
