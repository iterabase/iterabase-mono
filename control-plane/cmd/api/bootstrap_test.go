package main

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/logging"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
)

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what was
// written.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	defer func() { os.Stdout = old }()

	runErr := fn()
	require.NoError(t, w.Close())
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(out), runErr
}

// applyAuthConfig sets the always-on browser-authentication prerequisites.
func applyAuthConfig(cfg *config.Config) {
	cfg.Auth.PublicOrigin = "https://app.example.com"
	cfg.Auth.Bootstrap.AdminEmail = "admin@example.com"
	cfg.Auth.Bootstrap.AdminLocale = "en"
	cfg.Auth.Email.Host = "smtp.example.com"
	cfg.Auth.Email.Port = 587
	cfg.Auth.Email.Mode = "starttls"
	cfg.Auth.Email.From = "auth@example.com"
}

func bootstrapConfig(connStr string) *config.Config {
	cfg := &config.Config{Database: config.DatabaseConfig{URL: connStr, MaxOpenConns: 5, MaxIdleConns: 2}}
	applyAuthConfig(cfg)
	return cfg
}

func queuedSetupEmails(t *testing.T, store *identity.Store, user identity.LocalUser) []identity.AuthEmailIntent {
	t.Helper()
	intents, err := store.ClaimAuthEmails(context.Background(), "bootstrap-test", 10, 0, user.UpdatedAt)
	require.NoError(t, err)
	return intents
}

// TestBootstrapFirstAdmin proves first boot makes the overlay-named Admin a
// setup_pending Admin with queued setup mail, never prints a credential, is a
// strict no-op on restart, and has no legacy key issuance path.
func TestBootstrapFirstAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	pool, connStr := testutil.NewPostgres(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	logger, _ := logging.New("error", "json")
	cfg := bootstrapConfig(connStr)

	out, err := captureStdout(t, func() error {
		return runBootstrap(cfg, logger, []string{"--admin-email", "Admin@Example.com", "--admin-locale", "pt"})
	})
	require.NoError(t, err)
	assert.NotContains(t, out, "cp-", "bootstrap must never print an API key")
	assert.NotContains(t, out, "token=", "bootstrap must never print a setup token")
	assert.Contains(t, out, "created")

	admin, err := store.FindLocalUserByEmail(ctx, "admin@example.com")
	require.NoError(t, err)
	assert.Equal(t, identity.LocalUserSetupPending, admin.Status)
	assert.Equal(t, "admin", admin.Role)
	assert.Equal(t, "pt", admin.Locale)
	intents := queuedSetupEmails(t, store, admin)
	require.Len(t, intents, 1)
	assert.Equal(t, identity.AuthLinkSetupPassword, intents[0].Purpose)

	// Restart is a strict no-op.
	out, err = captureStdout(t, func() error { return runBootstrap(cfg, logger, nil) })
	require.NoError(t, err)
	assert.Contains(t, out, "noop")

	// There is no legacy key bootstrap to ask for.
	assert.Error(t, runBootstrap(cfg, logger, []string{"--service-account", "agent"}))
	assert.Error(t, runBootstrap(cfg, logger, []string{"--reset"}))

	// Bootstrap cannot run without the always-on authentication prerequisites.
	broken := bootstrapConfig(connStr)
	broken.Auth.Email.Mode = "plain"
	assert.Error(t, runBootstrap(broken, logger, nil))
}

// TestBootstrapConvertsExistingInstallationAdmin proves the OPO1 path
// (DES-HOR-582-03): an installation with legacy passwordless users and no
// marker converts the named existing human to a setup_pending Admin on first
// boot, without inventing a second account.
func TestBootstrapConvertsExistingInstallationAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	pool, connStr := testutil.NewPostgres(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	logger, _ := logging.New("error", "json")

	existing, err := store.UpsertLocalUser(ctx, "admin@example.com", "admin@example.com", "operator")
	require.NoError(t, err)
	_, err = store.UpsertLocalUser(ctx, "colleague@example.com", "colleague@example.com", "operator")
	require.NoError(t, err)

	out, err := captureStdout(t, func() error { return runBootstrap(bootstrapConfig(connStr), logger, nil) })
	require.NoError(t, err)
	assert.Contains(t, out, "created")

	admin, err := store.FindLocalUserByEmail(ctx, "admin@example.com")
	require.NoError(t, err)
	assert.Equal(t, existing.ID, admin.ID, "the existing human is converted, not duplicated")
	assert.Equal(t, "admin", admin.Role)
	assert.Equal(t, identity.LocalUserSetupPending, admin.Status)
	require.Len(t, queuedSetupEmails(t, store, admin), 1)

	colleague, err := store.FindLocalUserByEmail(ctx, "colleague@example.com")
	require.NoError(t, err)
	assert.Equal(t, "operator", colleague.Role, "other humans are untouched")
}

// TestBootstrapIsKeyedOnlyOnTheMarker proves DES-HOR-582-03: with no marker,
// first boot makes the overlay-named human a setup_pending Admin even when
// another active Admin exists, and leaves that Admin untouched.
func TestBootstrapIsKeyedOnlyOnTheMarker(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	pool, connStr := testutil.NewPostgres(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	logger, _ := logging.New("error", "json")

	owner, err := store.UpsertLocalUser(ctx, "owner@example.com", "owner@example.com", "admin")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE identity.local_users SET status = 'active', password_hash = 'x' WHERE identity_id = $1`, owner.ID)
	require.NoError(t, err)

	out, err := captureStdout(t, func() error { return runBootstrap(bootstrapConfig(connStr), logger, nil) })
	require.NoError(t, err)
	assert.Contains(t, out, "created")

	named, err := store.FindLocalUserByEmail(ctx, "admin@example.com")
	require.NoError(t, err)
	assert.Equal(t, "admin", named.Role)
	assert.Equal(t, identity.LocalUserSetupPending, named.Status)
	require.Len(t, queuedSetupEmails(t, store, named), 1)
	kept, err := store.FindLocalUserByEmail(ctx, "owner@example.com")
	require.NoError(t, err)
	assert.Equal(t, identity.LocalUserActive, kept.Status)

	var markers int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM identity.bootstrap_marker`).Scan(&markers))
	assert.Equal(t, 1, markers)
}

// TestBootstrapRecovery proves recovery converts the named human only when no
// active Admin exists and leaves no printed secret.
func TestBootstrapRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	pool, connStr := testutil.NewPostgres(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	logger, _ := logging.New("error", "json")
	cfg := bootstrapConfig(connStr)

	_, err := store.BootstrapAdmin(ctx, identity.BootstrapOptions{AdminEmail: "admin@example.com", AdminLocale: "en"})
	require.NoError(t, err)

	// While an active Admin exists, recovery is refused.
	_, err = pool.Exec(ctx, `UPDATE identity.local_users SET status = 'active'`)
	require.NoError(t, err)
	_, err = store.BootstrapAdmin(ctx, identity.BootstrapOptions{
		AdminEmail: "admin@example.com", AdminLocale: "en", Recover: true,
	})
	assert.ErrorIs(t, err, identity.ErrRecoveryNotPermitted)

	// With no active Admin the recovery converts the named human.
	_, err = pool.Exec(ctx, `UPDATE identity.local_users SET status = 'disabled'`)
	require.NoError(t, err)

	out, err := captureStdout(t, func() error {
		return runBootstrap(cfg, logger, []string{"--recover-admin"})
	})
	require.NoError(t, err)
	assert.Contains(t, out, "recovered")
	assert.NotContains(t, out, "cp-")

	recovered, err := store.FindLocalUserByEmail(ctx, "admin@example.com")
	require.NoError(t, err)
	assert.Equal(t, identity.LocalUserSetupPending, recovered.Status)
	assert.Equal(t, "admin", recovered.Role)
}
