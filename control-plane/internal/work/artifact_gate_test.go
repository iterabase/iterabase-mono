package work_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
	workstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/work"
)

// TestNodeAssignmentWaitsForInputClearance proves DES-HOR-461-02: an agent
// node is not assigned while an input is being (re)checked, and a rejected
// input fails the node explicitly instead of silently dropping the file.
func TestNodeAssignmentWaitsForInputClearance(t *testing.T) {
	pool := testutil.NewPostgresPool(t)
	ctx := context.Background()
	store := workstore.NewStore(pool)
	actorID, _, _ := seedFoundation(t, ctx, pool)
	artifactID := "55555555-5555-4555-8555-555555555555"
	_, err := pool.Exec(ctx, `
		INSERT INTO artifact.artifacts
			(id,source_type,created_by_identity_id,canonical_mime,claimed_mime,display_name,size_bytes,digest,storage_state,security_state,cleared_at,registered_at)
		VALUES ($1,'upload',$2,'text/plain','text/plain','quote.txt',4,
		        'sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee','stored','cleared',now(),now())`, artifactID, actorID)
	require.NoError(t, err)
	item, _, err := store.Start(ctx, workstore.StartInput{ActorIdentityID: actorID, WorkflowKey: "walter/quotation",
		IdempotencyKey: "gate-1", Title: "Gate", Source: json.RawMessage(`{"messageId":"gate"}`),
		SourcePresentation: workstore.SourcePresentation{Kind: "manual", Title: "Gate"},
		ArtifactRefs:       []workstore.ArtifactRef{{ArtifactID: artifactID, Role: "source"}}})
	require.NoError(t, err)
	attemptID := item.CurrentAttemptID

	// A security advisory withdraws the input for re-scan: the node waits.
	_, err = pool.Exec(ctx, `UPDATE artifact.artifacts SET security_state='rescan_pending', withdrawn_at=now(),
		withdrawn_reason='security_advisory' WHERE id=$1`, artifactID)
	require.NoError(t, err)
	node, _, dispatch, err := store.PrepareNode(ctx, attemptID)
	require.NoError(t, err)
	assert.False(t, dispatch, "a node is not assigned while an input is being checked")
	assert.Equal(t, workstore.NodePending, node.State)

	// A rejecting re-scan fails the node with a stable customer reason.
	_, err = pool.Exec(ctx, `UPDATE artifact.artifacts SET security_state='rejected', security_reason='malware' WHERE id=$1`, artifactID)
	require.NoError(t, err)
	node, _, dispatch, err = store.PrepareNode(ctx, attemptID)
	require.NoError(t, err)
	assert.False(t, dispatch)
	assert.Equal(t, workstore.NodeFailed, node.State)
	var summary []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT customer_failure_summary FROM work.attempts WHERE id=$1`, attemptID).Scan(&summary))
	assert.Contains(t, string(summary), "input_artifact_rejected")
	artifacts, err := store.ListArtifacts(ctx, item.ID)
	require.NoError(t, err)
	require.Len(t, artifacts, 1)
	assert.Equal(t, "rejected", artifacts[0].State, "the rejected input stays visible as rejected")
}
