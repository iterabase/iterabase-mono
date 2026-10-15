package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaults(t *testing.T) {
	cfg := defaults()
	assert.Equal(t, 8080, cfg.Server.Port)
	assert.Equal(t, "info", cfg.Logging.Level)
	assert.Equal(t, "json", cfg.Logging.Format)
}

func TestLoad_EnvOnly(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("ADMIN_API_KEY", "admin-secret")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, "postgres://localhost:5432/test", cfg.Database.URL)
	assert.Equal(t, "admin-secret", cfg.Auth.AdminKey)
}

func TestLoad_YAMLFile(t *testing.T) {
	content := `
server:
  port: 9090
database:
  url: "${DATABASE_URL}"
  max_open_conns: 50
logging:
  level: debug
  format: text
`
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 9090, cfg.Server.Port)
	assert.Equal(t, 50, cfg.Database.MaxOpenConns)
	assert.Equal(t, "debug", cfg.Logging.Level)
	assert.Equal(t, "text", cfg.Logging.Format)
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	content := `
database:
  url: "postgres://yaml-host:5432/test"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	// Env vars should override YAML values.
	t.Setenv("DATABASE_URL", "postgres://env-host:5432/test")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "postgres://env-host:5432/test", cfg.Database.URL)
}

func TestLoad_MissingRequired(t *testing.T) {
	// No DATABASE_URL set.
	t.Setenv("DATABASE_URL", "")

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database.url")
}

func TestLoad_PortEnvOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("PORT", "3000")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, 3000, cfg.Server.Port)
}

func TestLoad_CentralEnvOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("CENTRAL_URL", "https://central.iterabase.com")
	t.Setenv("CENTRAL_CLIENT_CERT_FILE", "/etc/iterabase/central/tls.crt")
	t.Setenv("CENTRAL_CLIENT_KEY_FILE", "/etc/iterabase/central/tls.key")
	t.Setenv("CENTRAL_CA_FILE", "/etc/iterabase/central/ca.crt")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, CentralConfig{
		URL: "https://central.iterabase.com", ClientCertFile: "/etc/iterabase/central/tls.crt",
		ClientKeyFile: "/etc/iterabase/central/tls.key", CAFile: "/etc/iterabase/central/ca.crt",
	}, cfg.Central)
}

func TestLoad_CentralRequiresCertificateFiles(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("CENTRAL_URL", "https://central.iterabase.com")
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "central.client_cert_file")
}

func TestLoad_MetricsDedicatedPort(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("METRICS_ENABLED", "true")
	t.Setenv("METRICS_PORT", "9090")
	cfg, err := Load("")
	require.NoError(t, err)
	assert.True(t, cfg.Metrics.Enabled)
	assert.Equal(t, 9090, cfg.Metrics.Port)

	t.Setenv("METRICS_PORT", "8080")
	_, err = Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metrics.port must differ")
}

func TestDefaults_Workload(t *testing.T) {
	cfg := defaults()
	assert.False(t, cfg.Workload.Enabled) // disabled by default
	assert.Equal(t, 8443, cfg.Workload.Port)
	assert.Equal(t, "iterabase.local", cfg.Workload.TrustDomain)
}

func TestLoad_WorkloadEnabledRequiresCerts(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("WORKLOAD_ENABLED", "true")
	// No cert paths -> validation error.
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workload.server_cert_file")
}

func TestLoad_WorkloadEnvOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("WORKLOAD_ENABLED", "true")
	t.Setenv("WORKLOAD_PORT", "9443")
	t.Setenv("WORKLOAD_TRUST_DOMAIN", "cluster.example")
	t.Setenv("WORKLOAD_SERVER_CERT_FILE", "/certs/srv.crt")
	t.Setenv("WORKLOAD_SERVER_KEY_FILE", "/certs/srv.key")
	t.Setenv("WORKLOAD_CLIENT_CA_FILE", "/certs/ca.crt")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.True(t, cfg.Workload.Enabled)
	assert.Equal(t, 9443, cfg.Workload.Port)
	assert.Equal(t, "cluster.example", cfg.Workload.TrustDomain)
	assert.Equal(t, "/certs/ca.crt", cfg.Workload.ClientCAFile)
}

func TestLoad_WorkloadPortMustDiffer(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("WORKLOAD_ENABLED", "true")
	t.Setenv("WORKLOAD_PORT", "8080") // same as server.port
	t.Setenv("WORKLOAD_SERVER_CERT_FILE", "/certs/srv.crt")
	t.Setenv("WORKLOAD_SERVER_KEY_FILE", "/certs/srv.key")
	t.Setenv("WORKLOAD_CLIENT_CA_FILE", "/certs/ca.crt")

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must differ from server.port")
}
