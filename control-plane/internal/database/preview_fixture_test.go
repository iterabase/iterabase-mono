package database_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
)

// TestPreviewInferenceAuthorizationFixture proves the CI-generated scope fits
// the real, fully migrated database, and cleanup removes all live authority.
// This is the named .github/preview <-> durable workload-authorization contract,
// not another handwritten approximation of the production schema.
func TestPreviewInferenceAuthorizationFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
	pool, _ := testutil.NewPostgres(t)
	output, err := exec.CommandContext(ctx, "python3", "-c", `
import json,sys
sys.path.insert(0, "../../../.github/scripts")
import preview_inference
print(json.dumps(preview_inference.fixture("fixture's-model")))
`).Output()
	require.NoError(t, err)
	var fixture map[string]string
	require.NoError(t, json.Unmarshal(output, &fixture))
	_, err = pool.Exec(ctx, fixture["seed"])
	require.NoError(t, err)

	var worker, state, model, kind, prefix string
	var generation int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT ta.worker_id, ta.fencing_generation, t.state, t.model, i.kind, p.spiffe_id_prefix
		FROM runtime.turn_assignments ta
		JOIN runtime.turns t ON t.id=ta.turn_id AND t.run_id=ta.run_id
		JOIN runtime.workflow_runs wr ON wr.id=ta.run_id
		JOIN runtime.run_pool_assignments a ON a.run_id=wr.id AND a.pool_id=ta.pool_id
		JOIN toolgateway.pools p ON p.id=a.pool_id AND p.deleted_at IS NULL
		JOIN identity.identities i ON i.id=wr.scope_identity_id AND i.id=ta.scope_identity_id
		WHERE t.id=$1 AND ta.run_id=$2 AND ta.state='active'`, fixture["turn"], fixture["run"]).Scan(
		&worker, &generation, &state, &model, &kind, &prefix))
	assert.Equal(t, fixture["name"], worker)
	assert.Equal(t, 1, generation)
	assert.Equal(t, "running", state)
	assert.Equal(t, "fixture's-model", model)
	assert.Equal(t, "workflow", kind)
	assert.Contains(t, fixture["certificate"], prefix+"workers/"+worker)

	_, err = pool.Exec(ctx, fixture["cleanup"])
	require.NoError(t, err)
	// Idempotency matters: explicit revocation and the EXIT trap both clean up.
	_, err = pool.Exec(ctx, fixture["cleanup"])
	require.NoError(t, err)
	for table, binding := range map[string][2]string{
		"runtime.workflow_runs":    {"id", fixture["run"]},
		"runtime.turns":            {"id", fixture["turn"]},
		"runtime.turn_assignments": {"turn_id", fixture["turn"]},
		"toolgateway.pools":        {"name", fixture["name"]},
		"identity.identities":      {"key", "workflow:iterabase-system/" + fixture["name"]},
	} {
		var count int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE "+binding[0]+"::text=$1", binding[1]).Scan(&count))
		assert.Zero(t, count, "%s has no leftover fixture authority", table)
	}
}
