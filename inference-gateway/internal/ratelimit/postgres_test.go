package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/testpg"
)

// counterSchema mirrors the control-plane's ratelimit.counters table.
const counterSchema = `
CREATE SCHEMA ratelimit;
CREATE TABLE ratelimit.counters (
    identity_id  text NOT NULL,
    window_start timestamptz NOT NULL,
    requests     integer NOT NULL DEFAULT 0,
    tokens       bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (identity_id, window_start)
);`

func newTestLimiter(t *testing.T) (*PostgresLimiter, *time.Time) {
	t.Helper()
	db := testpg.Start(t)
	_, err := db.Pool.Exec(context.Background(), counterSchema)
	require.NoError(t, err)
	now := time.Date(2026, 10, 14, 12, 0, 10, 0, time.UTC)
	l := NewPostgresLimiter(db.Pool, Installation)
	l.now = func() time.Time { return now }
	return l, &now
}

func TestPostgresLimiter(t *testing.T) {
	l, now := newTestLimiter(t)
	ctx := context.Background()

	t.Run("rpm allows up to the limit then denies", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			res, err := l.CheckRPM(ctx, "id-a", 3)
			require.NoError(t, err)
			assert.True(t, res.Allowed, "request %d", i+1)
			assert.Equal(t, 2-i, res.Remaining)
		}
		res, err := l.CheckRPM(ctx, "id-a", 3)
		require.NoError(t, err)
		assert.False(t, res.Allowed)
		assert.Equal(t, time.Date(2026, 10, 14, 12, 1, 0, 0, time.UTC), res.ResetAt)
	})

	t.Run("keys are independent", func(t *testing.T) {
		res, err := l.CheckRPM(ctx, "id-b", 3)
		require.NoError(t, err)
		assert.True(t, res.Allowed)
	})

	t.Run("unlimited still counts", func(t *testing.T) {
		res, err := l.CheckRPM(ctx, "id-c", 0)
		require.NoError(t, err)
		assert.True(t, res.Allowed)
	})

	t.Run("tpm checks without counting and increments add up", func(t *testing.T) {
		require.NoError(t, l.IncrementTPM(ctx, "id-d", 600))
		require.NoError(t, l.IncrementTPM(ctx, "id-d", 0))
		res, err := l.CheckTPM(ctx, "id-d", 1000)
		require.NoError(t, err)
		assert.True(t, res.Allowed)
		assert.Equal(t, 400, res.Remaining)
		require.NoError(t, l.IncrementTPM(ctx, "id-d", 400))
		res, err = l.CheckTPM(ctx, "id-d", 1000)
		require.NoError(t, err)
		assert.False(t, res.Allowed)
	})

	t.Run("admit refuses atomically without consuming other scopes", func(t *testing.T) {
		requests := func(key string) int {
			var n int
			err := l.pool.QueryRow(ctx, `SELECT COALESCE(sum(requests), 0) FROM ratelimit.counters WHERE identity_id = $1`, key).Scan(&n)
			require.NoError(t, err)
			return n
		}
		provider := Check{Key: "provider:m", RPM: 3}
		res, err := l.Admit(ctx, provider, Check{Key: "inst:acme", RPM: 1})
		require.NoError(t, err)
		assert.True(t, res.Allowed)
		for i := 0; i < 2; i++ {
			res, err = l.Admit(ctx, provider, Check{Key: "inst:acme", RPM: 1})
			require.NoError(t, err)
			assert.False(t, res.Allowed, "acme is over its own limit")
		}
		assert.Equal(t, 1, requests("provider:m"), "acme's refused requests left the provider count untouched")
		res, err = l.Admit(ctx, provider, Check{Key: "inst:beta", RPM: 1})
		require.NoError(t, err)
		assert.True(t, res.Allowed, "beta still gets the provider's remaining quota")

		require.NoError(t, l.IncrementTPM(ctx, "inst:tpm", 500))
		res, err = l.Admit(ctx, provider, Check{Key: "inst:tpm", RPM: 10, TPM: 500})
		require.NoError(t, err)
		assert.False(t, res.Allowed, "over its token limit")
		assert.Equal(t, 2, requests("provider:m"), "a token refusal counts no request anywhere")
		assert.Equal(t, 0, requests("inst:tpm"))
	})

	t.Run("a new window starts fresh and prune drops old windows", func(t *testing.T) {
		*now = now.Add(11 * time.Minute)
		res, err := l.CheckRPM(ctx, "id-a", 3)
		require.NoError(t, err)
		assert.True(t, res.Allowed)
		require.NoError(t, l.Prune(ctx))
		var n int
		require.NoError(t, l.pool.QueryRow(ctx, `SELECT count(*) FROM ratelimit.counters`).Scan(&n))
		assert.Equal(t, 1, n, "only the current window remains")
	})
}

func TestTPMBatcher_FlushesAndFinalFlush(t *testing.T) {
	l, _ := newTestLimiter(t)
	ctx := context.Background()

	batcher := NewTPMBatcher(l, "batched", 50*time.Millisecond)
	batcher.Start()
	for i := 0; i < 20; i++ {
		batcher.Add(10)
	}
	time.Sleep(150 * time.Millisecond)
	batcher.Add(300)
	batcher.Stop(ctx)

	res, err := l.CheckTPM(ctx, "batched", 1000)
	require.NoError(t, err)
	assert.Equal(t, 500, res.Remaining, "200 batched + 300 flushed on stop")
}
