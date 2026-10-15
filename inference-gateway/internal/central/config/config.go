// Package config loads the central gateway's configuration: the committed
// price book and policy (CENTRAL_CONFIG, raw YAML rendered by Terraform from
// infra/hosted-aws/central/central.yaml) plus deployment environment
// (DES-HOR-661-01/04; docs/architecture/v2-central-inference-gateway.md).
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"regexp"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Currencies supported for supplier prices and installation billing.
const (
	EUR = "EUR"
	USD = "USD"
)

// Operator scopes (contract: Operator API).
const (
	ScopeAdmin   = "admin"
	ScopePreview = "preview"
)

// File is the committed configuration document.
type File struct {
	PublicHost string           `yaml:"public_host"`
	Models     map[string]Model `yaml:"models"`
	Tiers      map[string]Tier  `yaml:"tiers"`
	Trial      Trial            `yaml:"trial"`
	Limits     Limits           `yaml:"limits"`
}

// Model is one managed model: where central sends it and what the supplier
// charges. Prices are decimal strings per million tokens.
type Model struct {
	BaseURL               string `yaml:"base_url"`
	ProviderModel         string `yaml:"provider_model"`
	Currency              string `yaml:"currency"`
	InputPerMillion       string `yaml:"input_per_million"`
	CachedInputPerMillion string `yaml:"cached_input_per_million"`
	OutputPerMillion      string `yaml:"output_per_million"`
	EffectiveFrom         string `yaml:"effective_from"`
}

// Tier is what a paid plan includes (DES-HOR-661-05).
type Tier struct {
	WorkflowLimit  int `yaml:"workflow_limit"`
	MonthlyCredits int `yaml:"monthly_credits"`
}

// Trial says which tier a trial is pro-rated from, and over how many days.
type Trial struct {
	Tier      string `yaml:"tier"`
	DaysBasis int    `yaml:"days_basis"`
}

// Limits are the per-minute ceilings toward the provider (DES-HOR-661-07).
type Limits struct {
	Provider     Limit `yaml:"provider"`
	Installation Limit `yaml:"installation"`
}

// Limit is one RPM/TPM pair; 0 means unlimited.
type Limit struct {
	RPM int `yaml:"rpm"`
	TPM int `yaml:"tpm"`
}

// Operator is one allow-listed operator role.
type Operator struct {
	RoleARN string `json:"role_arn"`
	Scope   string `json:"scope"`
}

// Config is the complete runtime configuration.
type Config struct {
	File
	// PriceVersion is the SHA-256 of the configuration document, stamped on
	// every usage record (DES-HOR-661-04).
	PriceVersion  string
	DatabaseURL   string
	ProviderKey   string
	Operators     []Operator
	AlertTopicARN string
	KMSKeyID      string
	Region        string
	ListenAddr    string
	HealthAddr    string
	MetricsAddr   string
	ECBURL        string
	LogLevel      string
}

// ECBFeedURL is the ECB euro reference rates feed for the last 90 days. Each
// fetch records every observation it carries, so a fresh database can lock the
// rate for any current period: a month or a trial of up to 90 days.
const ECBFeedURL = "https://www.ecb.europa.eu/stats/eurofxref/eurofxref-hist-90d.xml"

// Load reads the configuration from the environment. CENTRAL_CONFIG holds the
// YAML document; CENTRAL_CONFIG_FILE (or path) names a file instead.
func Load(path string) (*Config, error) {
	raw := []byte(os.Getenv("CENTRAL_CONFIG"))
	if len(raw) == 0 {
		if path == "" {
			path = os.Getenv("CENTRAL_CONFIG_FILE")
		}
		if path == "" {
			return nil, errors.New("CENTRAL_CONFIG (or CENTRAL_CONFIG_FILE) is required")
		}
		data, err := os.ReadFile(path) // #nosec G703 -- the operator's own config path
		if err != nil {
			return nil, fmt.Errorf("read central config: %w", err)
		}
		raw = data
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Parse parses and validates a configuration document.
func Parse(raw []byte) (*Config, error) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse central config: %w", err)
	}
	sum := sha256.Sum256(raw)
	cfg := &Config{
		File:         f,
		PriceVersion: hex.EncodeToString(sum[:]),
		ListenAddr:   ":8443",
		HealthAddr:   ":8081",
		MetricsAddr:  ":9090",
		ECBURL:       ECBFeedURL,
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)

// Validate checks the document.
//
//nolint:gocyclo // A flat list of independent field checks.
func (f *File) Validate() error {
	if len(f.Models) == 0 {
		return errors.New("central config: models is empty")
	}
	for _, name := range sortedKeys(f.Models) {
		m := f.Models[name]
		if !modelNamePattern.MatchString(name) {
			return fmt.Errorf("central config: invalid model name %q", name)
		}
		u, err := url.Parse(m.BaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("central config: model %s: base_url must be an https URL", name)
		}
		if m.ProviderModel == "" {
			return fmt.Errorf("central config: model %s: provider_model is required", name)
		}
		if m.Currency != EUR && m.Currency != USD {
			return fmt.Errorf("central config: model %s: currency must be EUR or USD", name)
		}
		for field, v := range map[string]string{
			"input_per_million": m.InputPerMillion, "cached_input_per_million": m.CachedInputPerMillion,
			"output_per_million": m.OutputPerMillion,
		} {
			r, ok := new(big.Rat).SetString(v)
			if !ok || r.Sign() < 0 {
				return fmt.Errorf("central config: model %s: %s must be a non-negative decimal string", name, field)
			}
		}
		if _, err := time.Parse(time.DateOnly, m.EffectiveFrom); err != nil {
			return fmt.Errorf("central config: model %s: effective_from must be YYYY-MM-DD", name)
		}
	}
	for _, plan := range []string{"starter", "growth", "scale"} {
		t, ok := f.Tiers[plan]
		if !ok {
			return fmt.Errorf("central config: tiers.%s is required", plan)
		}
		if t.WorkflowLimit < 1 || t.MonthlyCredits < 0 {
			return fmt.Errorf("central config: tiers.%s must have a positive workflow_limit and non-negative monthly_credits", plan)
		}
	}
	for name := range f.Tiers {
		if name != "starter" && name != "growth" && name != "scale" {
			return fmt.Errorf("central config: unknown tier %q", name)
		}
	}
	if _, ok := f.Tiers[f.Trial.Tier]; !ok {
		return errors.New("central config: trial.tier must name a tier")
	}
	if f.Trial.DaysBasis < 1 {
		return errors.New("central config: trial.days_basis must be positive")
	}
	for _, l := range []Limit{f.Limits.Provider, f.Limits.Installation} {
		if l.RPM < 0 || l.TPM < 0 {
			return errors.New("central config: limits must not be negative")
		}
	}
	return nil
}

//nolint:gocyclo // Each variable is an independent optional override.
func (c *Config) applyEnv() error {
	if v := os.Getenv("CENTRAL_PUBLIC_HOST"); v != "" {
		c.PublicHost = v
	}
	if c.PublicHost == "" {
		return errors.New("public_host (or CENTRAL_PUBLIC_HOST) is required")
	}
	c.DatabaseURL = databaseURL()
	if c.DatabaseURL == "" {
		return errors.New("CENTRAL_DATABASE_URL or CENTRAL_DATABASE_HOST/_NAME/_USER/_PASSWORD is required")
	}
	c.ProviderKey = os.Getenv("CENTRAL_PROVIDER_KEY")
	if v := os.Getenv("CENTRAL_OPERATORS"); v != "" {
		if err := json.Unmarshal([]byte(v), &c.Operators); err != nil {
			return fmt.Errorf("CENTRAL_OPERATORS: %w", err)
		}
		for _, op := range c.Operators {
			if op.Scope != ScopeAdmin && op.Scope != ScopePreview {
				return fmt.Errorf("CENTRAL_OPERATORS: unknown scope %q", op.Scope)
			}
			if op.RoleARN == "" {
				return errors.New("CENTRAL_OPERATORS: role_arn is required")
			}
		}
	}
	c.AlertTopicARN = os.Getenv("CENTRAL_ALERT_TOPIC_ARN")
	c.KMSKeyID = os.Getenv("CENTRAL_KMS_KEY_ID")
	c.Region = os.Getenv("AWS_REGION")
	c.LogLevel = os.Getenv("LOG_LEVEL")
	for env, field := range map[string]*string{
		"CENTRAL_LISTEN_ADDR": &c.ListenAddr, "CENTRAL_HEALTH_ADDR": &c.HealthAddr,
		"CENTRAL_METRICS_ADDR": &c.MetricsAddr, "CENTRAL_ECB_URL": &c.ECBURL,
	} {
		if v := os.Getenv(env); v != "" {
			*field = v
		}
	}
	return nil
}

// databaseURL composes the DSN from the ECS-injected parts, or takes
// CENTRAL_DATABASE_URL whole.
func databaseURL() string {
	if v := os.Getenv("CENTRAL_DATABASE_URL"); v != "" {
		return v
	}
	host, name, user := os.Getenv("CENTRAL_DATABASE_HOST"), os.Getenv("CENTRAL_DATABASE_NAME"), os.Getenv("CENTRAL_DATABASE_USER")
	if host == "" || name == "" || user == "" {
		return ""
	}
	port := os.Getenv("CENTRAL_DATABASE_PORT")
	if port == "" {
		port = "5432"
	}
	sslmode := os.Getenv("CENTRAL_DATABASE_SSLMODE")
	if sslmode == "" {
		sslmode = "require"
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, os.Getenv("CENTRAL_DATABASE_PASSWORD")),
		Host:     host + ":" + port,
		Path:     "/" + name,
		RawQuery: url.Values{"sslmode": {sslmode}}.Encode(),
	}
	return u.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
