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
	for _, table := range []string{"identities", "local_users", "api_keys"} {
		var exists bool
		err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'identity' AND tablename = $1)", table,
		).Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, "identity.%s should exist after MigrateUp", table)
	}

	// HOR-582: the retired legacy authority objects are gone; the workload
	// capability view remains for the gateway.
	for _, relation := range []string{
		"identity.external_mappings", "identity.authority_state", "identity.active_api_keys", "permissions.policies",
	} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, relation).Scan(&exists))
		assert.False(t, exists, "%s must not exist after MigrateUp", relation)
	}

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
	// down migrations 31 through 23 intentionally preserves it; reapplying
	// them must be an idempotent metadata advance with no manual role update.
	assertGatewayWorkloadPrivileges(t, ctx, pool)
	require.NoError(t, database.MigrateDown(connStr, 9))
	var migrationVersion int
	require.NoError(t, pool.QueryRow(ctx, `SELECT version FROM schema_migrations`).Scan(&migrationVersion))
	assert.Equal(t, 22, migrationVersion)
	assertGatewayWorkloadPrivileges(t, ctx, pool)
	require.NoError(t, database.MigrateUp(connStr))
	require.NoError(t, pool.QueryRow(ctx, `SELECT version FROM schema_migrations`).Scan(&migrationVersion))
	assert.Equal(t, 31, migrationVersion)
	assertGatewayWorkloadPrivileges(t, ctx, pool)

	// HOR-254 must migrate an existing gateway ledger without requiring an
	// unavailable customer-safe summary backfill. Step back from 31 (HOR-582),
	// through 30 (HOR-597) and the earlier migrations to the pre-HOR-254 schema;
	// seed a historical write, then apply all sixteen again.
	require.NoError(t, database.MigrateDown(connStr, 16))
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
// validating CHECK constraints and soft-delete behavior the identity service
// relies on. Requires Docker.
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

	var id string
	err = pool.QueryRow(ctx, `
		INSERT INTO identity.identities (key, kind, source, display_name)
		VALUES ('default/alice', 'user', 'local', 'Alice Wong') RETURNING id`,
	).Scan(&id)
	require.NoError(t, err)

	// Invalid kind must be rejected.
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.identities (key, kind, source) VALUES ('bad', 'robot', 'local')`)
	assert.Error(t, err, "invalid kind should violate CHECK constraint")

	// A live key without the complete V2 credential shape is unrepresentable:
	// only fully specified V2 credentials or revoked legacy history may exist.
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.api_keys (identity_id, key_hash, prefix)
		VALUES ($1, 'hash', 'cp-xxxx')`, id)
	assert.Error(t, err, "an untyped live key should violate api_keys_v2_complete")

	// The legacy role spelling is gone.
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.local_users (identity_id, email, email_normalized, role)
		VALUES ($1, 'alice@example.com', 'alice@example.com', 'user')`, id)
	assert.Error(t, err, "role must be exactly admin or operator")

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

// TestV2OnlyAuthorityMigrationUpgradesALegacyInstallation proves the OPO1
// forward migration (DES-HOR-582-02): a pre-V2 installation with legacy roles,
// scoped keys, IdentityMapping bindings, and PermissionPolicy rows upgrades in
// one step to V2-only authority. Canonical identities and delivery emails are
// preserved, humans complete email setup instead of receiving a password, no
// legacy key can authenticate, the retired authority objects are gone, the
// broad capability is limited to workload identities, and the gateway role
// holds exactly the DES-HOR-451-14 grant union.
//
//nolint:gocyclo // One sequential upgrade proof.
func TestV2OnlyAuthorityMigrationUpgradesALegacyInstallation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	pool, connStr := testutil.NewPostgres(t)
	ctx := context.Background()

	// Step back to the pre-HOR-453 schema (migration 25) and seed a legacy
	// install shaped like OPO1. The combined tree has 29 (wait/pause),
	// 30 (backend auth_ref) and 31 (V2-only authority), so step down six.
	require.NoError(t, database.MigrateDown(connStr, 6))
	version, dirty, err := database.MigrateVersion(connStr)
	require.NoError(t, err)
	require.False(t, dirty)
	assert.Equal(t, uint(25), version)

	_, err = pool.Exec(ctx, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway') THEN CREATE ROLE gateway NOLOGIN; END IF;
	END $$`)
	require.NoError(t, err)

	var humanID, workflowID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO identity.identities (key, kind, source, display_name)
		VALUES ('Legacy.User@Example.com', 'user', 'local', 'Legacy User')
		RETURNING id`).Scan(&humanID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO identity.identities (key, kind, source, display_name)
		VALUES ('workflow:default/invoices', 'workflow', 'local', 'Invoices')
		RETURNING id`).Scan(&workflowID))
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.local_users (identity_id, email, role)
		VALUES ($1, 'Legacy.User@Example.com', 'user')`, humanID)
	require.NoError(t, err)
	for _, scope := range []string{"work", "admin", "token", "gateway"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO identity.api_keys (identity_id, key_hash, prefix, name, scope)
			VALUES ($1, $2, $3, 'legacy', $4)`, humanID, "legacy-hash-"+scope, "cp-"+scope, scope)
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.external_mappings (identity_id, provider, type, external_id)
		VALUES ($1, 'teams', 'user', 'aad:legacy')`, humanID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO permissions.policies (key, subject_kind, subject_key, rate_limits)
		VALUES ('default/legacy', 'user', 'Legacy.User@Example.com', '{"rpm": 10, "tpm": 1000}')`)
	require.NoError(t, err)

	// Work, timeline, artifact, and security history attributed to the human.
	var workItemID, artifactID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO work.work_items (workflow_key, scope_identity_id, title, start_identity_id,
			start_idempotency_key, start_payload_hash)
		VALUES ('default/invoices', $2, 'Invoice 42', $1, 'legacy-1', 'hash')
		RETURNING id`, humanID, workflowID).Scan(&workItemID))
	_, err = pool.Exec(ctx, `
		INSERT INTO work.timeline_events (work_item_id, code, actor_identity_id)
		VALUES ($1, 'feedback_recorded', $2)`, workItemID, humanID)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO artifact.artifacts (storage_key, source_type, created_by_identity_id, mime_type)
		VALUES ('legacy/invoice-42.pdf', 'user_upload', $1, 'application/pdf')
		RETURNING id`, humanID).Scan(&artifactID))
	historyBefore := legacyHistory(t, ctx, pool)

	require.NoError(t, database.MigrateUp(connStr))

	// History is untouched and still attributed to the same canonical human.
	assert.Equal(t, historyBefore, legacyHistory(t, ctx, pool), "work and artifact history is preserved")
	var starter, actor, creator string
	require.NoError(t, pool.QueryRow(ctx, `SELECT start_identity_id::text FROM work.work_items WHERE id = $1`, workItemID).Scan(&starter))
	require.NoError(t, pool.QueryRow(ctx, `SELECT actor_identity_id::text FROM work.timeline_events WHERE work_item_id = $1`, workItemID).Scan(&actor))
	require.NoError(t, pool.QueryRow(ctx, `SELECT created_by_identity_id::text FROM artifact.artifacts WHERE id = $1`, artifactID).Scan(&creator))
	assert.Equal(t, []string{humanID, humanID, humanID}, []string{starter, actor, creator})

	// Canonical identity, delivery email, and email-setup requirement survive;
	// the role is exactly operator.
	var role, status, normalized, email string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT role, status, email_normalized, email FROM identity.local_users WHERE identity_id = $1`,
		humanID).Scan(&role, &status, &normalized, &email))
	assert.Equal(t, "operator", role)
	assert.Equal(t, "setup_pending", status, "existing humans complete email setup")
	assert.Equal(t, "legacy.user@example.com", normalized)
	assert.Equal(t, "Legacy.User@Example.com", email, "delivery email is preserved")

	// Every legacy key is revoked history; none is a live credential.
	var live, revoked int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status <> 'revoked' OR revoked_at IS NULL),
		       count(*) FILTER (WHERE status = 'revoked' AND revoked_at IS NOT NULL)
		FROM identity.api_keys WHERE identity_id = $1`, humanID).Scan(&live, &revoked))
	assert.Zero(t, live)
	assert.Equal(t, 4, revoked)
	var projected int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM identity.effective_api_credentials`).Scan(&projected))
	assert.Zero(t, projected, "no legacy key enters the V2 credential projection")
	for _, column := range []string{"scope", "credential_epoch"} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema='identity' AND table_name='api_keys' AND column_name=$1)`, column).Scan(&exists))
		assert.False(t, exists, "identity.api_keys.%s is retired", column)
	}

	// Retired authority objects are gone.
	for _, relation := range []string{
		"identity.external_mappings", "identity.authority_state", "identity.active_api_keys", "permissions.policies",
	} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, relation).Scan(&exists))
		assert.False(t, exists, "%s must not exist", relation)
	}

	// The broad capability covers workload identities only; no human wildcard.
	var humanCaps, workflowCaps, limits int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE identity_id = $1), count(*) FILTER (WHERE identity_id = $2)
		FROM permissions.effective_capabilities`, humanID, workflowID).Scan(&humanCaps, &workflowCaps))
	assert.Zero(t, humanCaps)
	assert.Equal(t, 1, workflowCaps)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM permissions.effective_rate_limits`).Scan(&limits))
	assert.Zero(t, limits)

	// The gateway role holds exactly the DES-HOR-451-14 union.
	approved := []string{
		"catalog.effective_api_catalog", "catalog.effective_catalog", "identity.inference_api_credentials",
		"permissions.effective_capabilities", "permissions.effective_rate_limits",
		"runtime.run_pool_assignments", "runtime.turn_assignments", "runtime.turns", "runtime.workflow_runs",
		"toolgateway.pools",
	}
	rows, err := pool.Query(ctx, `
		SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN ('identity', 'permissions', 'catalog', 'toolgateway', 'runtime', 'usage')
		  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		  AND has_table_privilege('gateway', c.oid, 'SELECT')
		ORDER BY 1`)
	require.NoError(t, err)
	var readable []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		readable = append(readable, name)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, approved, readable)
	var canInsertUsage bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT has_table_privilege('gateway', 'usage.inference_events', 'INSERT')`).Scan(&canInsertUsage))
	assert.True(t, canInsertUsage)
}

// legacyHistory fingerprints the work, timeline, and artifact rows an
// authority migration must leave untouched (ids and attribution).
func legacyHistory(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	out := map[string]string{}
	for relation, attribution := range map[string]string{
		"work.work_items":      "start_identity_id",
		"work.timeline_events": "actor_identity_id",
		"artifact.artifacts":   "created_by_identity_id",
	} {
		var fingerprint string
		require.NoError(t, pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT count(*)::text || ':' || coalesce(md5(string_agg(id::text || '/' || coalesce(%s::text, ''), ',' ORDER BY id)), '') FROM %s`,
			attribution, relation)).Scan(&fingerprint))
		out[relation] = fingerprint
	}
	return out
}
