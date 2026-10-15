package artifact_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	artifactstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/admission/admissiontest"
)

// TestTransientlyFailedAdvisoryRescanCanBeRetried proves section 7's
// retryable_failed -> pending recovery for a security-advisory re-scan: the
// stored original is retained, so after three transient failures a person's
// retry creates one new attributable trigger over that same original, the
// artifact stays withdrawn until the verdict, and a clean verdict
// re-registers it.
func TestTransientlyFailedAdvisoryRescanCanBeRetried(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.admit("advisory-retry.txt", "text/plain", admissiontest.TXT())
	h.drain()
	require.True(t, h.get(a.ID).Cleared())
	_, err := h.svc.RetrySecurity(ctx, a.ID, h.actorID, "")
	require.ErrorIs(t, err, artifactstore.ErrNotRetryable, "a cleared artifact has nothing to retry")

	_, err = h.store.WithdrawForRescan(ctx, a.ID, "security_advisory")
	require.NoError(t, err)
	h.engine.setDown(true)
	for attempt := 1; attempt <= 3; attempt++ {
		worked, err := h.coord.ProcessOne(ctx)
		require.NoError(t, err)
		require.True(t, worked, "attempt %d", attempt)
		_, err = h.pool.Exec(ctx, `UPDATE artifact.security_attempts SET not_before=now() WHERE state='queued'`)
		require.NoError(t, err)
	}
	got := h.get(a.ID)
	require.Equal(t, artifactstore.SecurityRetryableFailed, got.SecurityState)
	assert.Equal(t, artifactstore.StorageStored, got.StorageState, "the advisory keeps the stored original")
	assert.Equal(t, artifactstore.StatusSecurityCheckFailed, got.Status())
	worked, err := h.coord.ProcessOne(ctx)
	require.NoError(t, err)
	assert.False(t, worked, "there is no automatic fourth attempt")

	h.engine.setDown(false)
	retried, err := h.svc.RetrySecurity(ctx, a.ID, h.actorID, "")
	require.NoError(t, err)
	assert.Equal(t, artifactstore.SecurityPending, retried.SecurityState)
	assert.NotNil(t, retried.WithdrawnAt, "withdrawal stays in place until a successful verdict")
	assert.False(t, h.listed(a.ID))
	_, _, err = h.svc.Open(ctx, a.ID)
	assert.ErrorIs(t, err, artifactstore.ErrNotAvailable)
	var actor, kind string
	require.NoError(t, h.pool.QueryRow(ctx, `
		SELECT trigger_actor_identity_id::text, trigger_kind FROM artifact.security_attempts
		WHERE artifact_id=$1 AND state='queued'`, a.ID).Scan(&actor, &kind))
	assert.Equal(t, h.actorID, actor, "the retry is attributable")
	assert.Equal(t, "manual_retry", kind)

	h.drain()
	assert.True(t, h.get(a.ID).Cleared(), "a clean verdict re-registers the same original")
	assert.True(t, h.listed(a.ID))
	var triggers int
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(DISTINCT trigger_id) FROM artifact.security_attempts WHERE artifact_id=$1`, a.ID).Scan(&triggers))
	assert.Equal(t, 3, triggers, "initial check, advisory re-scan, one manual retry")
}
