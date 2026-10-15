// Package testpg starts a throwaway PostgreSQL for integration tests
// (testcontainers; requires Docker). It is imported only from _test files.
package testpg

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the pinned Postgres image used by every integration test.
const Image = "postgres:16-alpine@sha256:cf78e76683b9ca8c5733cbbdce6c9262b45b6767934dd0a95e671f9a0fc20685"

// DB is a running test database.
type DB struct {
	Container testcontainers.Container
	Pool      *pgxpool.Pool
	URL       string
}

// Start runs a fresh Postgres, skipping the test in -short mode. The container
// and pool are cleaned up with the test.
func Start(t *testing.T) *DB {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
	c, err := postgres.Run(ctx, Image,
		postgres.WithDatabase("gw"), postgres.WithUsername("t"), postgres.WithPassword("t"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return &DB{Container: c, Pool: pool, URL: url}
}
