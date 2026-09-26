// Package testutil provides shared test helpers for control-plane integration
// tests (Postgres via testcontainers). It is a non-test package so it can be
// imported by _test packages in internal/identity, internal/controller, and
// internal/server.
package testutil

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/database"
)

// NewPostgres starts a fresh pgvector Postgres container, applies all
// migrations, and returns a ready connection pool plus its connection string
// (useful when a subcommand must connect itself, e.g. bootstrap). It skips in
// -short mode. Requires Docker.
func NewPostgres(t *testing.T) (*pgxpool.Pool, string) {
	return newPostgres(t, nil)
}

// NewPostgresWithRoles starts the same fresh container but creates the given
// PostgreSQL roles between container startup and migration, reproducing a
// production installation whose dedicated roles exist before `migrate up` (and
// therefore before the conditional grant migrations run). It skips in -short
// mode. Requires Docker.
func NewPostgresWithRoles(t *testing.T, roles ...string) (*pgxpool.Pool, string) {
	t.Helper()
	return newPostgres(t, roles)
}

func newPostgres(t *testing.T, roles []string) (*pgxpool.Pool, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	pgC, err := postgres.Run(ctx, "pgvector/pgvector:pg16@sha256:ccc6e83d6e35e931dc7c5def2022729d5a6c370318d099181995567ff1fb4d6b",
		postgres.WithDatabase("controlplane"),
		postgres.WithUsername("cp"),
		postgres.WithPassword("cp"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })

	connStr, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool := waitForPool(t, ctx, connStr)
	t.Cleanup(pool.Close)

	for _, role := range roles {
		_, err := pool.Exec(ctx, `CREATE ROLE `+quoteIdentifier(role))
		require.NoError(t, err)
	}

	require.NoError(t, database.MigrateUp(connStr))
	return pool, connStr
}

// quoteIdentifier renders a bare identifier for the test-only CREATE ROLE
// statements. Callers pass literals, never user input.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// NewPostgresPool is a convenience wrapper returning only the pool.
func NewPostgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := NewPostgres(t)
	return pool
}

// waitForPool retries connecting until the database accepts connections.
func waitForPool(t *testing.T, ctx context.Context, connStr string) *pgxpool.Pool {
	t.Helper()
	var lastErr error
	for range 30 {
		pool, err := pgxpool.New(ctx, connStr)
		if err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = pool.Ping(pingCtx)
			cancel()
			if err == nil {
				return pool
			}
			pool.Close()
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	require.NoError(t, lastErr)
	return nil
}
