package gateway_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/gateway"
)

// TestBeginInvocationSerializesArtifactInputsWithDeletion reproduces the
// section 16 race at the gateway's reference boundary: deletion holds the
// input's row lock and enters `deleting` after the input was validated. The
// invocation ledger insert must wait for that lock, re-check clearance, and
// refuse without recording an active reference.
func TestBeginInvocationSerializesArtifactInputsWithDeletion(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	runID, turnID, workItemID, nodeID, creatorID := seedWorkAttempt(t, env)
	input := admitClearedInput(t, env, creatorID, workItemID, runID, nodeID, "deleted-under-the-tool")

	deletion, err := env.pgpool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = deletion.Rollback(ctx) }()
	_, err = deletion.Exec(ctx, `SELECT 1 FROM artifact.artifacts WHERE id=$1 FOR UPDATE`, input.ID)
	require.NoError(t, err)
	_, err = deletion.Exec(ctx, `UPDATE artifact.artifacts SET storage_state='deleting',deletion_reason='admin_explicit',
		deletion_started_at=now(),withdrawn_at=now(),withdrawn_reason='admin_explicit' WHERE id=$1`, input.ID)
	require.NoError(t, err)

	key := gateway.InvocationKey{AttemptID: runID, CallerScope: gateway.CallerScopeTurn, CallerScopeID: turnID,
		ToolCallID: "deleted-input", ToolVersionDigest: "sha256:echo-1"}
	refs := []byte(fmt.Sprintf(`[{"artifact_id":%q}]`, input.ID))
	result := make(chan error, 1)
	go func() {
		_, _, err := env.store.BeginInvocation(ctx, key, gateway.ToolVersion{Name: "echo", EffectClass: gateway.EffectReadOnly},
			&env.poolID, []byte(`{}`), refs, nil, time.Now().Add(time.Minute), "test")
		result <- err
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		_ = env.pgpool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type='Lock' AND datname=current_database())`).Scan(&waiting)
		return waiting
	}, 10*time.Second, 20*time.Millisecond, "invocation never waited on the deletion lock")
	require.NoError(t, deletion.Commit(ctx))

	select {
	case err := <-result:
		require.ErrorIs(t, err, gateway.ErrArtifactUnavailable)
	case <-time.After(10 * time.Second):
		t.Fatal("invocation did not finish after deletion committed")
	}
	var rows int
	require.NoError(t, env.pgpool.QueryRow(ctx, `SELECT count(*) FROM toolgateway.invocations WHERE tool_call_id='deleted-input'`).Scan(&rows))
	assert.Zero(t, rows, "no active invocation may reference an artifact entering deletion")
}
