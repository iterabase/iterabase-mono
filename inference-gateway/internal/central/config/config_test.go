package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDoc is a valid configuration document shared by tests.
const TestDoc = `public_host: central.test
models:
  deepseek-v4-flash:
    base_url: https://provider.test/v1
    provider_model: deepseek-v4-flash
    currency: USD
    input_per_million: "0.50"
    cached_input_per_million: "0.13"
    output_per_million: "1.50"
    effective_from: "2026-10-14"
tiers:
  starter: {workflow_limit: 2, monthly_credits: 5000}
  growth: {workflow_limit: 6, monthly_credits: 20000}
  scale: {workflow_limit: 15, monthly_credits: 60000}
trial: {tier: starter, days_basis: 30}
limits:
  provider: {rpm: 600, tpm: 2000000}
  installation: {rpm: 120, tpm: 400000}
`

func TestParse_Valid(t *testing.T) {
	cfg, err := Parse([]byte(TestDoc))
	require.NoError(t, err)
	assert.Equal(t, "central.test", cfg.PublicHost)
	assert.Len(t, cfg.PriceVersion, 64)
	assert.Equal(t, 5000, cfg.Tiers["starter"].MonthlyCredits)
	assert.Equal(t, ":8443", cfg.ListenAddr)

	other, err := Parse([]byte(strings.Replace(TestDoc, `"1.50"`, `"1.60"`, 1)))
	require.NoError(t, err)
	assert.NotEqual(t, cfg.PriceVersion, other.PriceVersion, "any price change is a new price version")
}

func TestParse_Rejects(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"unknown field":  func(s string) string { return s + "surprise: 1\n" },
		"http base url":  func(s string) string { return strings.Replace(s, "https://provider", "http://provider", 1) },
		"bad currency":   func(s string) string { return strings.Replace(s, "currency: USD", "currency: GBP", 1) },
		"float price":    func(s string) string { return strings.Replace(s, `"0.50"`, `"abc"`, 1) },
		"negative price": func(s string) string { return strings.Replace(s, `"0.50"`, `"-1"`, 1) },
		"missing tier": func(s string) string {
			return strings.Replace(s, "  scale: {workflow_limit: 15, monthly_credits: 60000}\n", "", 1)
		},
		"bad trial tier":  func(s string) string { return strings.Replace(s, "tier: starter", "tier: gold", 1) },
		"zero days basis": func(s string) string { return strings.Replace(s, "days_basis: 30", "days_basis: 0", 1) },
		"bad date":        func(s string) string { return strings.Replace(s, `"2026-10-14"`, `"14/10/2026"`, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(mutate(TestDoc)))
			assert.Error(t, err)
		})
	}
}

func TestLoad_Environment(t *testing.T) {
	t.Setenv("CENTRAL_CONFIG", TestDoc)
	t.Setenv("CENTRAL_PUBLIC_HOST", "central.iterabase.com")
	t.Setenv("CENTRAL_DATABASE_HOST", "db.internal")
	t.Setenv("CENTRAL_DATABASE_PORT", "5432")
	t.Setenv("CENTRAL_DATABASE_NAME", "central")
	t.Setenv("CENTRAL_DATABASE_USER", "central")
	t.Setenv("CENTRAL_DATABASE_PASSWORD", "p@ss/word")
	t.Setenv("CENTRAL_DATABASE_SSLMODE", "require")
	t.Setenv("CENTRAL_OPERATORS", `[{"role_arn":"arn:aws:iam::111111111111:role/forge","scope":"admin"}]`)
	t.Setenv("CENTRAL_PROVIDER_KEY", "sk-test")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, "central.iterabase.com", cfg.PublicHost)
	assert.Equal(t, "postgres://central:p%40ss%2Fword@db.internal:5432/central?sslmode=require", cfg.DatabaseURL)
	assert.Equal(t, []Operator{{RoleARN: "arn:aws:iam::111111111111:role/forge", Scope: ScopeAdmin}}, cfg.Operators)
	assert.Equal(t, "sk-test", cfg.ProviderKey)
}

func TestLoad_RejectsBadOperators(t *testing.T) {
	t.Setenv("CENTRAL_CONFIG", TestDoc)
	t.Setenv("CENTRAL_DATABASE_URL", "postgres://x/y")
	t.Setenv("CENTRAL_OPERATORS", `[{"role_arn":"arn:aws:iam::1:role/x","scope":"root"}]`)
	_, err := Load("")
	assert.Error(t, err)
}

func TestLoad_RequiresDatabase(t *testing.T) {
	t.Setenv("CENTRAL_CONFIG", TestDoc)
	_, err := Load("")
	assert.ErrorContains(t, err, "CENTRAL_DATABASE")
}
