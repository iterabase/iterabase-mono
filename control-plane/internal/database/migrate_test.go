package database_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/database"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
)

// TestMigrations applies the schema scaffold against a real pgvector Postgres
// container, asserts the four schemas + vector extension exist, then rolls
// them back. Requires Docker.
func TestMigrations(t *testing.T) {
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

	// The default log-based wait can report ready before the server fully
	// accepts external connections, so poll until a real connection succeeds.
	pool := waitForPool(t, ctx, connStr)
	t.Cleanup(pool.Close)

	// The bundled Postgres chart creates this dedicated role before the
	// control-plane migration init container runs. Reproduce that ordering so
	// migration 23 exercises its production grant path.
	_, err = pool.Exec(ctx, `CREATE ROLE gateway NOLOGIN`)
	require.NoError(t, err)

	// Up: schemas + pgvector should exist.
	require.NoError(t, database.MigrateUp(connStr))

	for _, schema := range []string{"identity", "permissions", "usage", "ai_data"} {
		var exists bool
		err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", schema,
		).Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, "schema %q should exist after MigrateUp", schema)
	}

	var extExists bool
	err = pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')",
	).Scan(&extExists)
	require.NoError(t, err)
	assert.True(t, extExists, "pgvector extension should be installed after MigrateUp")

	// HOR-242: identity tables should exist after MigrateUp.
	for _, table := range []string{"identities", "external_mappings", "local_users", "api_keys"} {
		var exists bool
		err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'identity' AND tablename = $1)", table,
		).Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, "identity.%s should exist after MigrateUp", table)
	}

	// HOR-243: permissions table + view should exist after MigrateUp.
	var policyTableExists bool
	err = pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'permissions' AND tablename = 'policies')",
	).Scan(&policyTableExists)
	require.NoError(t, err)
	assert.True(t, policyTableExists, "permissions.policies should exist after MigrateUp")

	var viewExists bool
	err = pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_views WHERE schemaname = 'permissions' AND viewname = 'effective_capabilities')",
	).Scan(&viewExists)
	require.NoError(t, err)
	assert.True(t, viewExists, "permissions.effective_capabilities view should exist after MigrateUp")

	// DES-HOR-545-01: per-pool gates are created by the first pool observation;
	// a fresh install has no misleading installation-global capacity row.
	var workspaceGateCount int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM runtime.workspace_capacity_state`).Scan(&workspaceGateCount))
	assert.Zero(t, workspaceGateCount)
	var storageAuthorizedNullable, storageAuthorizedDefault string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT is_nullable, column_default FROM information_schema.columns
		WHERE table_schema='runtime' AND table_name='workspace_capacity_state' AND column_name='storage_authorized'`).Scan(
		&storageAuthorizedNullable, &storageAuthorizedDefault))
	assert.Equal(t, "NO", storageAuthorizedNullable)
	assert.Equal(t, "false", storageAuthorizedDefault, "fresh pools must start unauthorized for dispatch")

	// HOR-489: fresh installs grant only the durable workload-authorization
	// reads. A migration-22 OPO1 database already has this exact ACL, so moving
	// down migrations 28, 27, 26, 25, 24, and 23 intentionally preserves it and
	// reapplying them must be an idempotent metadata advance with no manual role
	// update.
	assertGatewayWorkloadPrivileges(t, ctx, pool)
	require.NoError(t, database.MigrateDown(connStr, 6))
	var migrationVersion int
	require.NoError(t, pool.QueryRow(ctx, `SELECT version FROM schema_migrations`).Scan(&migrationVersion))
	assert.Equal(t, 22, migrationVersion)
	assertGatewayWorkloadPrivileges(t, ctx, pool)
	require.NoError(t, database.MigrateUp(connStr))
	require.NoError(t, pool.QueryRow(ctx, `SELECT version FROM schema_migrations`).Scan(&migrationVersion))
	assert.Equal(t, 28, migrationVersion)
	assertGatewayWorkloadPrivileges(t, ctx, pool)

	// HOR-254 must migrate an existing gateway ledger without requiring an
	// unavailable customer-safe summary backfill. Roll back migrations 28 and
	// 27 (HOR-454), 26, 25, 24, migration 23, the three HOR-396 migrations, plus
	// HOR-425, HOR-397, HOR-399, and HOR-254; seed a pre-existing write
	// descriptor/invocation; and apply all thirteen again.
	require.NoError(t, database.MigrateDown(connStr, 13))
	_, err = pool.Exec(ctx, `
		INSERT INTO toolgateway.tool_versions
		    (name,version,digest,description,input_schema,effect_class,credential_slots,artifact_capabilities,timeout_ms)
		VALUES ('legacy.write','1.0.0','sha256:legacy-write','Legacy write','{}','non_idempotent_write','[]','{}',1000);
		INSERT INTO toolgateway.runner_registrations
		    (runner_id,spiffe_id,namespace,tool_name,tool_version,tool_digest,fencing_generation)
		VALUES ('legacy-runner','spiffe://iterabase.local/tool-runners/legacy/runner','legacy','legacy.write','1.0.0','sha256:legacy-write',1);
		INSERT INTO toolgateway.invocations
		    (attempt_id,caller_scope,caller_scope_id,tool_call_id,tool_name,tool_version_digest,idempotency_key,effect_class)
		VALUES ('legacy-attempt','turn','legacy-turn','legacy-call','legacy.write','sha256:legacy-write','legacy-key','non_idempotent_write')`)
	require.NoError(t, err)
	require.NoError(t, database.MigrateUp(connStr))

	var legacySummaryMissing bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT consequence_summary IS NULL FROM toolgateway.invocations
		WHERE tool_call_id='legacy-call'`).Scan(&legacySummaryMissing))
	assert.True(t, legacySummaryMissing, "historical write invocation should survive without fabricated summary")

	_, err = pool.Exec(ctx, `
		INSERT INTO toolgateway.invocations
		    (attempt_id,caller_scope,caller_scope_id,tool_call_id,tool_name,tool_version_digest,idempotency_key,effect_class)
		VALUES ('new-attempt','turn','new-turn','new-call','legacy.write','sha256:legacy-write','new-key','non_idempotent_write')`)
	assert.Error(t, err, "new write invocations must include a consequence summary")

	var legacyVersionAvailable bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM toolgateway.available_tool_versions WHERE name='legacy.write')`).Scan(&legacyVersionAvailable))
	assert.False(t, legacyVersionAvailable, "legacy write descriptor without a template must not enter new snapshots")

	// HOR-254 graph/work domain exists after migration.
	for _, table := range []string{"work_items", "attempts", "blockers", "feedback", "timeline_events", "value_models", "value_ledger", "artifact_links"} {
		var exists bool
		err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'work' AND tablename = $1)", table,
		).Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, "work.%s should exist after MigrateUp", table)
	}
	var nodeExecutions bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname='runtime' AND tablename='node_executions')").Scan(&nodeExecutions))
	assert.True(t, nodeExecutions)
	var artifacts bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname='artifact' AND tablename='artifacts')").Scan(&artifacts))
	assert.True(t, artifacts)
	for table, column := range map[string]string{"work_items": "source_presentation", "attempts": "presentation_snapshot", "blockers": "response_presentation"} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema='work' AND table_name=$1 AND column_name=$2)`, table, column).Scan(&exists))
		assert.True(t, exists, "work.%s.%s should exist after HOR-396 migration", table, column)
	}
	for _, column := range []string{"business_label", "result_presentation"} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema='runtime' AND table_name='node_executions' AND column_name=$1)`, column).Scan(&exists))
		assert.True(t, exists, "runtime.node_executions.%s should exist after HOR-396 migration", column)
	}

	// HOR-425: the workflow source_type contract admits the manual_api source
	// (ARCH-026) in both the definitions and trigger_bindings tables.
	for _, constraint := range []struct{ table, name string }{
		{"workflow.definitions", "definitions_source_type_check"},
		{"workflow.trigger_bindings", "trigger_bindings_source_type_check"},
	} {
		var def string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conrelid=$1::regclass AND conname=$2`, constraint.table, constraint.name).Scan(&def))
		assert.Contains(t, def, "'manual_api'", "%s should admit manual_api", constraint.name)
	}

	// Down: schemas should be gone.
	require.NoError(t, database.MigrateDown(connStr, 0))

	for _, schema := range []string{"identity", "permissions", "usage", "ai_data", "work", "artifact"} {
		var exists bool
		err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", schema,
		).Scan(&exists)
		require.NoError(t, err)
		assert.False(t, exists, "schema %q should be dropped after MigrateDown", schema)
	}
}

func assertGatewayWorkloadPrivileges(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, schema := range []string{"toolgateway", "runtime"} {
		var allowed bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT has_schema_privilege('gateway', $1, 'USAGE')`, schema).Scan(&allowed))
		assert.True(t, allowed, "gateway should have USAGE on %s", schema)
	}
	for _, table := range []string{
		"toolgateway.pools",
		"runtime.turns",
		"runtime.workflow_runs",
		"runtime.run_pool_assignments",
		"runtime.turn_assignments",
	} {
		var allowed bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT has_table_privilege('gateway', $1, 'SELECT')`, table).Scan(&allowed))
		assert.True(t, allowed, "gateway should have SELECT on %s", table)
		for _, privilege := range []string{"INSERT", "UPDATE", "DELETE"} {
			require.NoError(t, pool.QueryRow(ctx,
				`SELECT has_table_privilege('gateway', $1, $2)`, table, privilege).Scan(&allowed))
			assert.False(t, allowed, "gateway must not have %s on %s", privilege, table)
		}
	}
	for _, table := range []string{
		"toolgateway.credential_bindings",
		"toolgateway.invocations",
		"runtime.events",
	} {
		var allowed bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT has_table_privilege('gateway', $1, 'SELECT')`, table).Scan(&allowed))
		assert.False(t, allowed, "gateway must not have SELECT on %s", table)
	}
}

// TestIdentityConstraints exercises the identity schema against a real Postgres,
// validating CHECK constraints, unique bindings, and soft-delete behavior that
// the reconciler + identity service rely on. Requires Docker.
func TestIdentityConstraints(t *testing.T) {
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

	require.NoError(t, database.MigrateUp(connStr))

	// Insert a CR-sourced identity.
	var id string
	err = pool.QueryRow(ctx, `
		INSERT INTO identity.identities (key, kind, source, display_name)
		VALUES ('default/alice', 'user', 'external', 'Alice Wong') RETURNING id`,
	).Scan(&id)
	require.NoError(t, err)

	// Bind two external IDs.
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.external_mappings (identity_id, provider, type, external_id) VALUES
		($1, 'teams', 'user', 'aad:aaaa-1111'),
		($1, 'slack', 'user', 'U012345ABCD')`, id)
	require.NoError(t, err)

	// Duplicate binding (same provider/type/external_id) must be rejected.
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.external_mappings (identity_id, provider, type, external_id)
		VALUES ($1, 'slack', 'user', 'U012345ABCD')`, id)
	assert.Error(t, err, "duplicate external binding should violate UNIQUE constraint")

	// Invalid kind must be rejected.
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.identities (key, kind, source) VALUES ('bad', 'robot', 'local')`)
	assert.Error(t, err, "invalid kind should violate CHECK constraint")

	// Invalid api_key scope must be rejected.
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.api_keys (identity_id, key_hash, prefix, scope)
		VALUES ($1, 'hash', 'cp-xxxx', 'superuser')`, id)
	assert.Error(t, err, "invalid scope should violate CHECK constraint")

	// Soft delete: set deleted_at; identity row persists for usage/history.
	_, err = pool.Exec(ctx, `UPDATE identity.identities SET deleted_at = now() WHERE key = 'default/alice'`)
	require.NoError(t, err)
	var stillThere bool
	err = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identity.identities WHERE key = 'default/alice')`).Scan(&stillThere)
	require.NoError(t, err)
	assert.True(t, stillThere, "soft-deleted identity row should persist")

	// The updated_at trigger should fire on update.
	var updatedIsNull bool
	err = pool.QueryRow(ctx, `SELECT updated_at IS NULL FROM identity.identities WHERE key = 'default/alice'`).Scan(&updatedIsNull)
	require.NoError(t, err)
	assert.False(t, updatedIsNull, "updated_at should be set by trigger")
}

// waitForPool retries connecting until the database accepts connections, then
// returns a ready pool. Fails the test if it never becomes ready.
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
	require.NoError(t, fmt.Errorf("database not ready after 30s: %w", lastErr))
	return nil
}

// TestAuthJourneyMigrationPreservesLegacyIdentity proves migration 26 keeps
// existing canonical identities, leaves the pre-epoch `user` role untouched for
// HOR-454's authority epoch (normalized at every V2 authorization decision),
// requires email setup instead of inventing a password, and leaves explicitly
// scoped API keys usable.
func TestAuthJourneyMigrationPreservesLegacyIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	pool, connStr := testutil.NewPostgres(t)
	ctx := context.Background()

	// Step back to the pre-HOR-453 schema, seed a legacy install, then upgrade.
	// HOR-454 added migrations 27 (V2 authority expand) and 28 (three-principal
	// seam), so the pre-HOR-453 baseline is three steps down.
	require.NoError(t, database.MigrateDown(connStr, 3))
	version, dirty, err := database.MigrateVersion(connStr)
	require.NoError(t, err)
	require.False(t, dirty)
	assert.Equal(t, uint(25), version)

	var identityID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO identity.identities (key, kind, source, display_name)
		VALUES ('Legacy.User@Example.com', 'user', 'local', 'Legacy User')
		RETURNING id`).Scan(&identityID))
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.local_users (identity_id, email, role)
		VALUES ($1, 'Legacy.User@Example.com', 'user')`, identityID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.api_keys (identity_id, key_hash, prefix, name, scope)
		VALUES ($1, 'legacy-hash', 'cp-legacy', 'deployed-e2e', 'work')`, identityID)
	require.NoError(t, err)

	require.NoError(t, database.MigrateUp(connStr))

	var role, status, normalized, email string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT role, status, email_normalized, email FROM identity.local_users WHERE identity_id = $1`,
		identityID).Scan(&role, &status, &normalized, &email))
	assert.Equal(t, "user", role, "the HOR-454 epoch ticket performs the role rewrite")
	assert.Equal(t, "operator", identity.NormalizeRole(role), "V2 decisions normalize the legacy spelling")
	assert.Equal(t, "setup_pending", status, "existing humans complete email setup")

	// The widened check accepts the V2 spelling without a data rewrite.
	_, err = pool.Exec(ctx, `
		WITH ident AS (
			INSERT INTO identity.identities (key, kind, source, display_name)
			VALUES ('v2@example.com', 'user', 'local', 'V2 User')
			RETURNING id
		)
		INSERT INTO identity.local_users (identity_id, email, email_normalized, role, status)
		SELECT id, 'v2@example.com', 'v2@example.com', 'operator', 'setup_pending' FROM ident`)
	assert.NoError(t, err, "V2 write paths may store operator before the epoch")
	assert.Equal(t, "legacy.user@example.com", normalized)
	assert.Equal(t, "Legacy.User@Example.com", email, "delivery email is preserved")

	var scope string
	var revoked *time.Time
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT scope, revoked_at FROM identity.api_keys WHERE identity_id = $1`, identityID).Scan(&scope, &revoked))
	assert.Equal(t, "work", scope, "explicitly scoped API access survives the migration")
	assert.Nil(t, revoked)
}
