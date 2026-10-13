package identity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
)

// newTestStore returns a Store backed by a fresh migrated Postgres.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(testutil.NewPostgresPool(t))
}

func TestStore_UpsertIdentityAndRevive(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Create.
	id1, err := store.UpsertIdentity(ctx, "workflow:default/invoices", KindWorkflow, SourceLocal, "Invoices")
	require.NoError(t, err)
	assert.Equal(t, "workflow:default/invoices", id1.Key)

	// Soft delete (CR deletion path).
	require.NoError(t, store.SoftDeleteIdentityByKey(ctx, "workflow:default/invoices"))
	_, err = store.GetIdentityByID(ctx, id1.ID)
	assert.ErrorIs(t, err, ErrNotFound)

	// Re-create -> revives the same row (same UUID).
	id2, err := store.UpsertIdentity(ctx, "workflow:default/invoices", KindWorkflow, SourceLocal, "Invoices Wong")
	require.NoError(t, err)
	assert.Equal(t, id1.ID, id2.ID, "revive should reuse the existing identity row")
	assert.Equal(t, "Invoices Wong", id2.DisplayName)
}

func TestStore_UpsertLocalUserIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	u1, err := store.UpsertLocalUser(ctx, "admin@local", "admin@local", "admin")
	require.NoError(t, err)
	assert.Equal(t, "admin", u1.Role)
	assert.Equal(t, "admin@local", u1.Email)

	// Second call (idempotent) returns the same identity.
	u2, err := store.UpsertLocalUser(ctx, "admin@local", "admin@local", "admin")
	require.NoError(t, err)
	assert.Equal(t, u1.ID, u2.ID)

	assert.Equal(t, LocalUserSetupPending, u2.Status, "an upserted account still completes email setup")
}
