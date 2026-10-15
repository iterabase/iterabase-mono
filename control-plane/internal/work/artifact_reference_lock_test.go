package work_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
	workstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/work"
)

func seedClearedArtifact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, actorID string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO artifact.artifacts
			(id,source_type,created_by_identity_id,canonical_mime,claimed_mime,display_name,size_bytes,digest,storage_state,security_state,cleared_at,registered_at)
		VALUES ($1,'upload',$2,'text/plain','text/plain','quote.txt',4,
		        'sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee','stored','cleared',now(),now())`, id, actorID)
	require.NoError(t, err)
}

func startWithSource(ctx context.Context, store *workstore.Store, actorID, key, artifactID string) (workstore.WorkItem, error) {
	item, _, err := store.Start(ctx, workstore.StartInput{ActorIdentityID: actorID, WorkflowKey: "walter/quotation",
		IdempotencyKey: key, Title: "Lock", Source: json.RawMessage(`{"messageId":"` + key + `"}`),
		SourcePresentation: workstore.SourcePresentation{Kind: "manual", Title: "Lock"},
		ArtifactRefs:       []workstore.ArtifactRef{{ArtifactID: artifactID, Role: "source"}}})
	return item, err
}

// TestSourceReferenceSerializesWithDeletion reproduces the section 16 race:
// deletion holds the artifact row lock and enters `deleting` while a work
// start that already saw the artifact as cleared tries to link it. The start
// must wait for the lock, re-check clearance, and refuse; it must never link
// bytes whose purge deletion has already authorized.
func TestSourceReferenceSerializesWithDeletion(t *testing.T) {
	pool := testutil.NewPostgresPool(t)
	ctx := context.Background()
	store := workstore.NewStore(pool)
	actorID, _, _ := seedFoundation(t, ctx, pool)
	artifactID := "66666666-6666-4666-8666-666666666666"
	seedClearedArtifact(t, ctx, pool, artifactID, actorID)

	deletion, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = deletion.Rollback(ctx) }()
	_, err = deletion.Exec(ctx, `SELECT 1 FROM artifact.artifacts WHERE id=$1 FOR UPDATE`, artifactID)
	require.NoError(t, err)
	_, err = deletion.Exec(ctx, `UPDATE artifact.artifacts SET storage_state='deleting',deletion_reason='admin_explicit',
		deletion_started_at=now(),withdrawn_at=now(),withdrawn_reason='admin_explicit' WHERE id=$1`, artifactID)
	require.NoError(t, err)

	result := make(chan error, 1)
	go func() {
		_, err := startWithSource(ctx, store, actorID, "lock-race", artifactID)
		result <- err
	}()
	// The start is blocked on the artifact lock, not finished.
	require.Eventually(t, func() bool {
		var waiting bool
		_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type='Lock' AND datname=current_database())`).Scan(&waiting)
		return waiting
	}, 10*time.Second, 20*time.Millisecond, "work start never waited on the deletion lock")
	select {
	case err := <-result:
		t.Fatalf("work start finished before deletion committed: %v", err)
	default:
	}
	require.NoError(t, deletion.Commit(ctx))

	select {
	case err := <-result:
		require.ErrorIs(t, err, workstore.ErrInvalidInput)
	case <-time.After(10 * time.Second):
		t.Fatal("work start did not finish after deletion committed")
	}
	var links int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM work.artifact_links WHERE artifact_id=$1`, artifactID).Scan(&links))
	assert.Zero(t, links, "no reference may be acquired against an artifact entering deletion")
}

// TestArtifactLinkTriggerRefusesDeletingArtifacts covers every link boundary,
// including those that do not require clearance (outputs, propagation):
// the database refuses a link to an artifact being deleted, but keeps
// accepting a link to a security-rejected one so consumers fail closed.
func TestArtifactLinkTriggerRefusesDeletingArtifacts(t *testing.T) {
	pool := testutil.NewPostgresPool(t)
	ctx := context.Background()
	store := workstore.NewStore(pool)
	actorID, _, _ := seedFoundation(t, ctx, pool)
	sourceID := "77777777-7777-4777-8777-777777777777"
	seedClearedArtifact(t, ctx, pool, sourceID, actorID)
	item, err := startWithSource(ctx, store, actorID, "trigger", sourceID)
	require.NoError(t, err)

	link := func(artifactID string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO work.artifact_links (artifact_id,work_item_id,attempt_id,role,metadata)
			VALUES ($1,$2,$3,'output','{}'::jsonb)`, artifactID, item.ID, item.CurrentAttemptID)
		return err
	}
	deleting := "88888888-8888-4888-8888-888888888888"
	seedClearedArtifact(t, ctx, pool, deleting, actorID)
	_, err = pool.Exec(ctx, `UPDATE artifact.artifacts SET storage_state='deleting',deletion_reason='retention_expired',
		deletion_started_at=now(),withdrawn_at=now(),withdrawn_reason='retention_expired' WHERE id=$1`, deleting)
	require.NoError(t, err)
	err = link(deleting)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "want the serialization trigger's refusal, got %v", err)
	assert.Equal(t, "55000", pgErr.Code)

	rejected := "99999999-9999-4999-8999-999999999999"
	seedClearedArtifact(t, ctx, pool, rejected, actorID)
	_, err = pool.Exec(ctx, `UPDATE artifact.artifacts SET security_state='rescan_pending',withdrawn_at=now(),withdrawn_reason='advisory' WHERE id=$1`, rejected)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE artifact.artifacts SET security_state='rejected',storage_state='deleting',
		deletion_reason='security_rejected',deletion_started_at=now() WHERE id=$1`, rejected)
	require.NoError(t, err)
	require.NoError(t, link(rejected), "a rejected artifact may still be referenced; the input gate fails closed")
}

// TestNodeKeepsWaitingThroughEveryRescanPhase proves DES-HOR-461-02 for a
// security-advisory re-scan: leasing it (scanning), a scheduled retry
// (pending), and exhausted transient attempts (retryable_failed) all keep the
// node waiting. Only a terminal verdict fails it.
func TestNodeKeepsWaitingThroughEveryRescanPhase(t *testing.T) {
	pool := testutil.NewPostgresPool(t)
	ctx := context.Background()
	store := workstore.NewStore(pool)
	actorID, _, _ := seedFoundation(t, ctx, pool)
	artifactID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	seedClearedArtifact(t, ctx, pool, artifactID, actorID)
	item, err := startWithSource(ctx, store, actorID, "rescan-phases", artifactID)
	require.NoError(t, err)

	for _, phase := range []string{"rescan_pending", "scanning", "pending", "scanning", "retryable_failed", "pending"} {
		extra := ""
		if phase == "rescan_pending" {
			extra = ",withdrawn_at=now(),withdrawn_reason='security_advisory'"
		}
		_, err := pool.Exec(ctx, `UPDATE artifact.artifacts SET security_state=$2`+extra+` WHERE id=$1`, artifactID, phase)
		require.NoError(t, err, phase)
		node, _, dispatch, err := store.PrepareNode(ctx, item.CurrentAttemptID)
		require.NoError(t, err, phase)
		assert.False(t, dispatch, "%s: not assigned while the input is being re-checked", phase)
		assert.Equal(t, workstore.NodePending, node.State, "%s: the node waits instead of failing", phase)
	}
}
