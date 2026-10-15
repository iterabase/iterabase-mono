package database_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/database"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
)

// TestArtifactEpochReclassifiesLegacyRowsAsUnscanned proves the HOR-461
// epoch (architecture section 19): no legacy row is grandfathered to
// cleared; every committed v1 object re-enters the scan path as quarantine.
func TestArtifactEpochReclassifiesLegacyRowsAsUnscanned(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	pool, connStr := testutil.NewPostgres(t)
	ctx := context.Background()
	require.NoError(t, database.MigrateDown(connStr, 1))

	var creator, available, pending string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO identity.identities (key,kind,source,display_name)
		VALUES ('legacy-artifacts','user','local','Legacy') RETURNING id::text`).Scan(&creator))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO artifact.artifacts (storage_key,source_type,created_by_identity_id,mime_type,size_bytes,digest,state,available_at)
		VALUES ('artifacts/legacy-available','user_upload',$1,'application/pdf',10,
		        'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','available',now())
		RETURNING id::text`, creator).Scan(&available))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO artifact.artifacts (storage_key,source_type,created_by_identity_id,mime_type)
		VALUES ('artifacts/legacy-pending','tool_output',$1,'text/plain') RETURNING id::text`, creator).Scan(&pending))

	require.NoError(t, database.MigrateUp(connStr))

	var storage, security, source string
	var registered, cleared bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT storage_state, security_state, source_type, registered_at IS NOT NULL, artifact.is_cleared(id)
		FROM artifact.artifacts WHERE id=$1`, available).Scan(&storage, &security, &source, &registered, &cleared))
	assert.Equal(t, []string{"quarantined", "pending", "upload"}, []string{storage, security, source})
	assert.False(t, registered, "no legacy row is registered in the shared library")
	assert.False(t, cleared, "no legacy row is grandfathered to cleared")
	var zone, key, attemptState string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT o.zone, o.storage_key, sa.state FROM artifact.objects o
		JOIN artifact.security_attempts sa ON sa.input_object_id=o.id
		WHERE o.artifact_id=$1`, available).Scan(&zone, &key, &attemptState))
	assert.Equal(t, []string{"quarantine", "artifacts/legacy-available", "queued"}, []string{zone, key, attemptState},
		"the legacy object is quarantine input to a queued scan")

	require.NoError(t, pool.QueryRow(ctx, `SELECT storage_state, source_type FROM artifact.artifacts WHERE id=$1`, pending).
		Scan(&storage, &source))
	assert.Equal(t, []string{"uploading", "tool"}, []string{storage, source}, "an uncommitted v1 upload is swept as incomplete")

	var stateColumn bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema='artifact' AND table_name='artifacts' AND column_name='state')`).Scan(&stateColumn))
	assert.False(t, stateColumn, "older binaries cannot read a v1 availability state")
}
