package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/config"
)

func TestLoad_EnvOnly(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/cp")

	cfg, err := config.Load("")
	require.NoError(t, err)
	assert.Equal(t, "postgres://localhost:5432/cp", cfg.Database.URL)
	assert.Equal(t, ":8080", cfg.API.Addr)
	assert.Equal(t, "info", cfg.Logging.Level)
	assert.Equal(t, "json", cfg.Logging.Format)
	assert.Equal(t, 25, cfg.Database.MaxOpenConns)
	assert.Equal(t, 10, cfg.Database.MaxIdleConns)
	assert.Equal(t, "X-Forwarded-For", cfg.Auth.ForwardedHeader)
	assert.Equal(t, "en", cfg.Auth.Bootstrap.AdminLocale)
}

func TestLoad_YAMLFile(t *testing.T) {
	content := `
api:
  addr: ":9090"
database:
  url: "${DATABASE_URL}"
  max_open_conns: 50
logging:
  level: debug
  format: text
auth:
  public_origin: "https://app.example.com"
  email:
    host: "email-smtp.eu-west-1.amazonaws.com"
    port: 587
    mode: starttls
    from: "Iterabase <no-reply@example.com>"
    ca_file: "/etc/iterabase/smtp-ca/ca.crt"
  bootstrap:
    admin_email: "admin@example.com"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "control-plane.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	t.Setenv("DATABASE_URL", "postgres://localhost:5432/cp")

	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, ":9090", cfg.API.Addr)
	assert.Equal(t, "postgres://localhost:5432/cp", cfg.Database.URL)
	assert.Equal(t, 50, cfg.Database.MaxOpenConns)
	assert.Equal(t, "debug", cfg.Logging.Level)
	assert.Equal(t, "text", cfg.Logging.Format)
	assert.Equal(t, "https://app.example.com", cfg.Auth.PublicOrigin)
	assert.Equal(t, "email-smtp.eu-west-1.amazonaws.com", cfg.Auth.Email.Host)
	assert.Equal(t, 587, cfg.Auth.Email.Port)
	assert.Equal(t, "/etc/iterabase/smtp-ca/ca.crt", cfg.Auth.Email.CAFile)
	assert.Equal(t, "admin@example.com", cfg.Auth.Bootstrap.AdminEmail)
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	content := `
database:
  url: "postgres://yaml-host:5432/cp"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "control-plane.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	t.Setenv("DATABASE_URL", "postgres://env-host:5432/cp")

	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "postgres://env-host:5432/cp", cfg.Database.URL)
}

func TestLoad_RequiresDatabaseURL(t *testing.T) {
	// No DATABASE_URL, no YAML.
	cfg, err := config.Load("")
	_ = cfg
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database.url")
}

// TestLoad_DoesNotRequireServeFields confirms migrate can load config without
// the always-on authentication prerequisites (serve-specific requirements are
// enforced separately by ValidateServe).
func TestLoad_DoesNotRequireServeFields(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/cp")
	t.Setenv("API_ADDR", "")

	cfg, err := config.Load("")
	require.NoError(t, err)
	assert.Equal(t, ":8080", cfg.API.Addr) // default

	// Serve validation requires the browser-authentication prerequisites.
	err = config.ValidateServe(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth.public_origin")
}

// setAuthEnv supplies the always-on browser-authentication prerequisites.
func setAuthEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AUTH_PUBLIC_ORIGIN", "https://app.example.com")
	t.Setenv("AUTH_SMTP_HOST", "smtp.example.com")
	t.Setenv("AUTH_SMTP_PORT", "587")
	t.Setenv("AUTH_SMTP_MODE", "starttls")
	t.Setenv("AUTH_SMTP_FROM", "auth@example.com")
	t.Setenv("AUTH_BOOTSTRAP_ADMIN_EMAIL", "admin@example.com")
}

func TestLoad_MetricsAddressOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/cp")
	t.Setenv("METRICS_ADDR", ":9090")
	cfg, err := config.Load("")
	require.NoError(t, err)
	assert.Equal(t, ":9090", cfg.Metrics.Addr)
}

func TestValidateServe_OK(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/cp")
	setAuthEnv(t)

	cfg, err := config.Load("")
	require.NoError(t, err)
	assert.NoError(t, config.ValidateServe(cfg))
}

// TestLoad_TLSEnvOverrides confirms the TLS cert/key env vars populate
// APIConfig (HTTPS opt-in for the api serve path).
func TestLoad_TLSEnvOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/cp")
	t.Setenv("TLS_CERT_FILE", "/etc/cp/tls/tls.crt")
	t.Setenv("TLS_KEY_FILE", "/etc/cp/tls/tls.key")

	cfg, err := config.Load("")
	require.NoError(t, err)
	assert.Equal(t, "/etc/cp/tls/tls.crt", cfg.API.TLSCertFile)
	assert.Equal(t, "/etc/cp/tls/tls.key", cfg.API.TLSKeyFile)
}

// TestValidateServe_TLSBothOrNeither enforces the opt-in contract: both set
// (HTTPS) or both unset (HTTP); exactly one is a misconfig.
func TestValidateServe_TLSBothOrNeither(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/cp")
	setAuthEnv(t)
	t.Setenv("API_ADDR", ":8080")

	t.Run("both set -> HTTPS ok", func(t *testing.T) {
		t.Setenv("TLS_CERT_FILE", "/tls/tls.crt")
		t.Setenv("TLS_KEY_FILE", "/tls/tls.key")
		cfg, err := config.Load("")
		require.NoError(t, err)
		assert.NoError(t, config.ValidateServe(cfg))
	})

	t.Run("neither set -> HTTP ok", func(t *testing.T) {
		t.Setenv("TLS_CERT_FILE", "")
		t.Setenv("TLS_KEY_FILE", "")
		cfg, err := config.Load("")
		require.NoError(t, err)
		assert.NoError(t, config.ValidateServe(cfg))
	})

	t.Run("cert only -> error", func(t *testing.T) {
		t.Setenv("TLS_CERT_FILE", "/tls/tls.crt")
		t.Setenv("TLS_KEY_FILE", "")
		cfg, err := config.Load("")
		require.NoError(t, err)
		err = config.ValidateServe(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tls_cert_file")
	})

	t.Run("key only -> error", func(t *testing.T) {
		t.Setenv("TLS_CERT_FILE", "")
		t.Setenv("TLS_KEY_FILE", "/tls/tls.key")
		cfg, err := config.Load("")
		require.NoError(t, err)
		err = config.ValidateServe(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tls_key_file")
	})
}

func TestValidateGatewayServe_ApprovedRunners(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{Gateway: config.GatewayConfig{
			Addr: ":8090", TLSCertFile: "tls.crt", TLSKeyFile: "tls.key",
			ClientCAFile: "ca.crt", KubeNamespace: "iterabase-system",
			ApprovedRunners: []config.ApprovedRunnerConfig{{
				Namespace: "iterabase-system", RunnerID: "overlay-tools",
				SpiffeID:              "spiffe://iterabase.local/tool-runners/iterabase-system/overlay-tools",
				AllowedToolNamespaces: []string{"platform"},
			}},
		}}
	}
	t.Run("exact identity accepted", func(t *testing.T) { assert.NoError(t, config.ValidateGatewayServe(base())) })
	t.Run("wildcard rejected", func(t *testing.T) {
		cfg := base()
		cfg.Gateway.ApprovedRunners[0].AllowedToolNamespaces = []string{"*"}
		require.ErrorContains(t, config.ValidateGatewayServe(cfg), "invalid allowed namespace")
	})
	t.Run("duplicate identity rejected", func(t *testing.T) {
		cfg := base()
		cfg.Gateway.ApprovedRunners = append(cfg.Gateway.ApprovedRunners, cfg.Gateway.ApprovedRunners[0])
		require.ErrorContains(t, config.ValidateGatewayServe(cfg), "duplicates spiffe_id")
	})
}

func TestDatabaseFromEnv(t *testing.T) {
	t.Run("requires url", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "")
		_, err := config.DatabaseFromEnv()
		require.Error(t, err)
	})
	t.Run("ok", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://localhost:5432/cp")
		cfg, err := config.DatabaseFromEnv()
		require.NoError(t, err)
		assert.Equal(t, "postgres://localhost:5432/cp", cfg.URL)
		assert.Equal(t, 25, cfg.MaxOpenConns)
	})
}
