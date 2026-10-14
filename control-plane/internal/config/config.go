// Package config loads control-plane configuration from a YAML file with
// environment-variable overrides. Mirrors the inference-gateway pattern.
//
// Only the api binary loads YAML config (it owns the database, HTTP address,
// and browser-authentication configuration). The manager binary is flag-driven
// (controller-runtime norm) and reads only DATABASE_URL from the environment;
// both share internal/logging.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level control-plane configuration.
type Config struct {
	API      APIConfig      `yaml:"api"`
	Gateway  GatewayConfig  `yaml:"gateway"`
	Dispatch DispatchConfig `yaml:"dispatch"`
	Metrics  MetricsConfig  `yaml:"metrics"`
	Database DatabaseConfig `yaml:"database"`
	Logging  LoggingConfig  `yaml:"logging"`
	Artifact ArtifactConfig `yaml:"artifact"`
	Auth     AuthConfig     `yaml:"auth"`
}

// APIConfig configures the HTTP API server (cmd/api).
//
// TLS is opt-in: when both TLSCertFile + TLSKeyFile are set the api serves
// HTTPS (cert-manager leaf, internal issuer); when both are unset it serves
// plain HTTP (kind/E2E/dev). Exactly one set is rejected by ValidateServe.
type APIConfig struct {
	Addr        string `yaml:"addr"`
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`
}

// GatewayConfig configures the tool-gateway gRPC server (cmd/gateway, HOR-392).
// mTLS is REQUIRED (the workload boundary, not the dev HTTP API): the server
// cert + key serve HTTP/2, and ClientCAFile is the SPIFFE/workload-identity CA
// bundle used to verify runner/supervisor/workflow-step caller certs.
type GatewayConfig struct {
	Addr         string `yaml:"addr"`
	TLSCertFile  string `yaml:"tls_cert_file"`
	TLSKeyFile   string `yaml:"tls_key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
	TrustDomain  string `yaml:"trust_domain"`
	// KubeNamespace scopes Secret-read RBAC for credential-slot resolution
	// (ARCH-008). The gateway reads only named K8s Secrets in this namespace.
	KubeNamespace   string                 `yaml:"kube_namespace"`
	InlineLimit     int                    `yaml:"inline_limit"`
	ApprovedRunners []ApprovedRunnerConfig `yaml:"approved_runners"`
}

// ApprovedRunnerConfig is one exact, deployment-owned runner identity. Empty
// namespace lists and wildcards are rejected: registration remains fail-closed.
type ApprovedRunnerConfig struct {
	Namespace             string   `yaml:"namespace"`
	RunnerID              string   `yaml:"runner_id"`
	SpiffeID              string   `yaml:"spiffe_id"`
	AllowedToolNamespaces []string `yaml:"allowed_tool_namespaces"`
}

// DispatchConfig configures the dispatch Work gRPC server (cmd/dispatch,
// HOR-249): the warm-worker bidi stream + one-credit dispatch + worker
// fencing. mTLS is REQUIRED: the server cert + key serve HTTP/2, and
// ClientCAFile is the SPIFFE/workload-identity CA bundle that verifies warm
// worker (supervisor) caller certs.
type MetricsConfig struct {
	// Addr is an optional plaintext, metrics-only in-cluster listener. Empty
	// disables the listener. It never shares the customer or workload mTLS
	// serving boundary.
	Addr string `yaml:"addr"`
}

type DispatchConfig struct {
	Addr         string `yaml:"addr"`
	TLSCertFile  string `yaml:"tls_cert_file"`
	TLSKeyFile   string `yaml:"tls_key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
	TrustDomain  string `yaml:"trust_domain"`

	// DefaultModelID + DefaultModelAPI are the configured default model
	// permission captured on every AssignTurn (HOR-249 active-assignment
	// context). Per-workflow model selection is HOR-252; v1 dispatch MUST NOT
	// emit an empty model permission — the child/inference bridge cannot
	// execute a valid model request without it.
	DefaultModelID  string `yaml:"default_model_id"`
	DefaultModelAPI string `yaml:"default_model_api"`
}

// DatabaseConfig configures the Postgres connection pool.
type DatabaseConfig struct {
	URL          string `yaml:"url"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	MaxIdleConns int    `yaml:"max_idle_conns"`
}

// LoggingConfig configures the shared slog logger.
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// ArtifactConfig configures the shared Postgres + MinIO artifact domain
// (docs/architecture/v2-artifact-processing.md, DES-HOR-460-02). Each
// process receives only the zone credentials it needs: api and gateway hold
// the quarantine write-only ingest key and the cleared read-only serve key;
// only the coordinator holds the lifecycle key. None is ever mounted into
// supervisors, runners, sandbox children, or processor pods.
type ArtifactConfig struct {
	Enabled              bool   `yaml:"enabled"`
	Endpoint             string `yaml:"endpoint"`
	Bucket               string `yaml:"bucket"`
	Secure               bool   `yaml:"secure"`
	IngestAccessKey      string `yaml:"ingest_access_key"`
	IngestSecretKey      string `yaml:"ingest_secret_key"`
	ServeAccessKey       string `yaml:"serve_access_key"`
	ServeSecretKey       string `yaml:"serve_secret_key"`
	LifecycleAccessKey   string `yaml:"lifecycle_access_key"`
	LifecycleSecretKey   string `yaml:"lifecycle_secret_key"`
	DefaultRetention     string `yaml:"default_retention"` // empty = indefinite
	QuarantineMaxBytes   int64  `yaml:"quarantine_max_bytes"`
	MaxConcurrentUploads int    `yaml:"max_concurrent_uploads"`
	// Coordinator settings (cmd/artifact-coordinator only).
	ScannerURL         string `yaml:"scanner_url"`
	ScannerSPIFFEID    string `yaml:"scanner_spiffe_id"`
	ScannerCAFile      string `yaml:"scanner_ca_file"`
	ClientCertFile     string `yaml:"client_cert_file"`
	ClientKeyFile      string `yaml:"client_key_file"`
	ScanConcurrency    int    `yaml:"scan_concurrency"`
	SweepInterval      string `yaml:"sweep_interval"`
	CoordinatorAddress string `yaml:"coordinator_address"` // health/readiness listener
}

// AuthConfig configures the V2 browser-authentication journey
// (docs/architecture/v2-authentication-authority.md). Sign-in is always on: a
// valid public HTTPS origin, verified-TLS SMTP, and the first-Admin email are
// required and the API fails closed without them.
type AuthConfig struct {
	PublicOrigin    string              `yaml:"public_origin"`
	Session         AuthSessionConfig   `yaml:"session"`
	TrustedProxies  []string            `yaml:"trusted_proxies"`
	ForwardedHeader string              `yaml:"forwarded_header"`
	GeoIPDatabase   string              `yaml:"geoip_database"`
	Email           AuthEmailConfig     `yaml:"email"`
	Bootstrap       AuthBootstrapConfig `yaml:"bootstrap"`
}

// AuthSessionConfig may only tighten the approved session bounds
// (12 h idle / 30 d absolute / 15 min recent password proof).
type AuthSessionConfig struct {
	IdleTTL       string `yaml:"idle_ttl"`
	AbsoluteTTL   string `yaml:"absolute_ttl"`
	RecentAuthTTL string `yaml:"recent_auth_ttl"`
}

// AuthEmailConfig is the transactional authentication-email transport. Both
// modes require verified TLS; cleartext is rejected at startup. Secrets are
// supplied through the environment/Secret, not the customer surface.
type AuthEmailConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Mode     string `yaml:"mode"` // starttls | tls
	From     string `yaml:"from"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// CAFile optionally names a PEM bundle trusted for the SMTP server
	// certificate in addition to the system roots (a private relay). It never
	// disables verification.
	CAFile string `yaml:"ca_file"`
}

// AuthBootstrapConfig names the first Admin, set by the deployment overlay. It carries no secret: the
// first Admin completes normal email setup and no credential is ever printed.
type AuthBootstrapConfig struct {
	AdminEmail  string `yaml:"admin_email"`
	AdminLocale string `yaml:"admin_locale"`
}

// Load reads configuration from a YAML file (if path is non-empty), expands
// environment variables in the file, applies env overrides, and validates.
// If path is empty, only defaults and env vars are used.
//
// Validation enforces only fields common to every subcommand (database.url +
// field formats). Serve-specific requirements (api.addr, auth.*) are checked by
// the caller (runServe), so migrate/bootstrap need only the DB.
func Load(path string) (*Config, error) {
	cfg := defaults()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config file: %w", err)
		}
		expanded := os.ExpandEnv(string(data))
		if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
			return nil, fmt.Errorf("parsing config file: %w", err)
		}
	}

	applyEnvOverrides(cfg)

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

// DatabaseFromEnv builds a DatabaseConfig from environment variables for
// binaries that do not load YAML (the manager). DATABASE_URL is required.
func DatabaseFromEnv() (DatabaseConfig, error) {
	cfg := DatabaseConfig{
		URL:          os.Getenv("DATABASE_URL"),
		MaxOpenConns: 25,
		MaxIdleConns: 10,
	}
	if cfg.URL == "" {
		return cfg, fmt.Errorf("DATABASE_URL is required")
	}
	return cfg, nil
}

func defaults() *Config {
	return &Config{
		API:      APIConfig{Addr: ":8080"},
		Gateway:  GatewayConfig{Addr: ":8090", TrustDomain: "iterabase.local"},
		Dispatch: DispatchConfig{Addr: ":8091", TrustDomain: "iterabase.local"},
		Database: DatabaseConfig{MaxOpenConns: 25, MaxIdleConns: 10},
		Logging:  LoggingConfig{Level: "info", Format: "json"},
		Artifact: ArtifactConfig{Bucket: "iterabase-artifacts", SweepInterval: "30s", ScanConcurrency: 1, CoordinatorAddress: ":8092"},
		Auth:     AuthConfig{ForwardedHeader: "X-Forwarded-For", Bootstrap: AuthBootstrapConfig{AdminLocale: "en"}},
	}
}

// applyEnvOverrides lets critical values be set entirely via environment
// variables, taking precedence over the YAML file.
//
//nolint:gocyclo // a flat list of env-var checks; complexity is inherent.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		cfg.Database.URL = v
	}
	if v := os.Getenv("API_ADDR"); v != "" {
		cfg.API.Addr = v
	}
	if v := os.Getenv("TLS_CERT_FILE"); v != "" {
		cfg.API.TLSCertFile = v
	}
	if v := os.Getenv("TLS_KEY_FILE"); v != "" {
		cfg.API.TLSKeyFile = v
	}
	if v := os.Getenv("METRICS_ADDR"); v != "" {
		cfg.Metrics.Addr = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("LOG_FORMAT"); v != "" {
		cfg.Logging.Format = v
	}
	if v := os.Getenv("GATEWAY_ADDR"); v != "" {
		cfg.Gateway.Addr = v
	}
	if v := os.Getenv("GATEWAY_TLS_CERT_FILE"); v != "" {
		cfg.Gateway.TLSCertFile = v
	}
	if v := os.Getenv("GATEWAY_TLS_KEY_FILE"); v != "" {
		cfg.Gateway.TLSKeyFile = v
	}
	if v := os.Getenv("GATEWAY_CLIENT_CA_FILE"); v != "" {
		cfg.Gateway.ClientCAFile = v
	}
	if v := os.Getenv("GATEWAY_TRUST_DOMAIN"); v != "" {
		cfg.Gateway.TrustDomain = v
	}
	if v := os.Getenv("GATEWAY_KUBE_NAMESPACE"); v != "" {
		cfg.Gateway.KubeNamespace = v
	}
	if v := os.Getenv("DISPATCH_ADDR"); v != "" {
		cfg.Dispatch.Addr = v
	}
	if v := os.Getenv("DISPATCH_TLS_CERT_FILE"); v != "" {
		cfg.Dispatch.TLSCertFile = v
	}
	if v := os.Getenv("DISPATCH_TLS_KEY_FILE"); v != "" {
		cfg.Dispatch.TLSKeyFile = v
	}
	if v := os.Getenv("DISPATCH_CLIENT_CA_FILE"); v != "" {
		cfg.Dispatch.ClientCAFile = v
	}
	if v := os.Getenv("DISPATCH_TRUST_DOMAIN"); v != "" {
		cfg.Dispatch.TrustDomain = v
	}
	if v := os.Getenv("DISPATCH_DEFAULT_MODEL_ID"); v != "" {
		cfg.Dispatch.DefaultModelID = v
	}
	if v := os.Getenv("DISPATCH_DEFAULT_MODEL_API"); v != "" {
		cfg.Dispatch.DefaultModelAPI = v
	}
	if v := os.Getenv("ARTIFACT_ENABLED"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			cfg.Artifact.Enabled = parsed
		}
	}
	if v := os.Getenv("ARTIFACT_SECURE"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			cfg.Artifact.Secure = parsed
		}
	}
	for env, dst := range map[string]*string{
		"ARTIFACT_ENDPOINT":             &cfg.Artifact.Endpoint,
		"ARTIFACT_BUCKET":               &cfg.Artifact.Bucket,
		"ARTIFACT_INGEST_ACCESS_KEY":    &cfg.Artifact.IngestAccessKey,
		"ARTIFACT_INGEST_SECRET_KEY":    &cfg.Artifact.IngestSecretKey,
		"ARTIFACT_SERVE_ACCESS_KEY":     &cfg.Artifact.ServeAccessKey,
		"ARTIFACT_SERVE_SECRET_KEY":     &cfg.Artifact.ServeSecretKey,
		"ARTIFACT_LIFECYCLE_ACCESS_KEY": &cfg.Artifact.LifecycleAccessKey,
		"ARTIFACT_LIFECYCLE_SECRET_KEY": &cfg.Artifact.LifecycleSecretKey,
		"ARTIFACT_DEFAULT_RETENTION":    &cfg.Artifact.DefaultRetention,
		"ARTIFACT_SCANNER_URL":          &cfg.Artifact.ScannerURL,
		"ARTIFACT_SCANNER_SPIFFE_ID":    &cfg.Artifact.ScannerSPIFFEID,
		"ARTIFACT_SCANNER_CA_FILE":      &cfg.Artifact.ScannerCAFile,
		"ARTIFACT_CLIENT_CERT_FILE":     &cfg.Artifact.ClientCertFile,
		"ARTIFACT_CLIENT_KEY_FILE":      &cfg.Artifact.ClientKeyFile,
		"ARTIFACT_SWEEP_INTERVAL":       &cfg.Artifact.SweepInterval,
		"ARTIFACT_COORDINATOR_ADDR":     &cfg.Artifact.CoordinatorAddress,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	for env, dst := range map[string]*int64{
		"ARTIFACT_QUARANTINE_MAX_BYTES": &cfg.Artifact.QuarantineMaxBytes,
	} {
		if v := os.Getenv(env); v != "" {
			if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
				*dst = parsed
			}
		}
	}
	for env, dst := range map[string]*int{
		"ARTIFACT_MAX_CONCURRENT_UPLOADS": &cfg.Artifact.MaxConcurrentUploads,
		"ARTIFACT_SCAN_CONCURRENCY":       &cfg.Artifact.ScanConcurrency,
	} {
		if v := os.Getenv(env); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil {
				*dst = parsed
			}
		}
	}
	if v := os.Getenv("AUTH_PUBLIC_ORIGIN"); v != "" {
		cfg.Auth.PublicOrigin = v
	}
	if v := os.Getenv("AUTH_SESSION_IDLE_TTL"); v != "" {
		cfg.Auth.Session.IdleTTL = v
	}
	if v := os.Getenv("AUTH_SESSION_ABSOLUTE_TTL"); v != "" {
		cfg.Auth.Session.AbsoluteTTL = v
	}
	if v := os.Getenv("AUTH_RECENT_AUTH_TTL"); v != "" {
		cfg.Auth.Session.RecentAuthTTL = v
	}
	if v := os.Getenv("AUTH_TRUSTED_PROXIES"); v != "" {
		cfg.Auth.TrustedProxies = strings.Split(v, ",")
	}
	if v := os.Getenv("AUTH_FORWARDED_HEADER"); v != "" {
		cfg.Auth.ForwardedHeader = v
	}
	if v := os.Getenv("AUTH_GEOIP_DATABASE"); v != "" {
		cfg.Auth.GeoIPDatabase = v
	}
	if v := os.Getenv("AUTH_SMTP_HOST"); v != "" {
		cfg.Auth.Email.Host = v
	}
	if v := os.Getenv("AUTH_SMTP_PORT"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			cfg.Auth.Email.Port = parsed
		}
	}
	if v := os.Getenv("AUTH_SMTP_MODE"); v != "" {
		cfg.Auth.Email.Mode = v
	}
	if v := os.Getenv("AUTH_SMTP_FROM"); v != "" {
		cfg.Auth.Email.From = v
	}
	if v := os.Getenv("AUTH_SMTP_USERNAME"); v != "" {
		cfg.Auth.Email.Username = v
	}
	if v := os.Getenv("AUTH_SMTP_PASSWORD"); v != "" {
		cfg.Auth.Email.Password = v
	}
	if v := os.Getenv("AUTH_SMTP_CA_FILE"); v != "" {
		cfg.Auth.Email.CAFile = v
	}
	if v := os.Getenv("AUTH_BOOTSTRAP_ADMIN_EMAIL"); v != "" {
		cfg.Auth.Bootstrap.AdminEmail = v
	}
	if v := os.Getenv("AUTH_BOOTSTRAP_ADMIN_LOCALE"); v != "" {
		cfg.Auth.Bootstrap.AdminLocale = v
	}
}

func validate(cfg *Config) error {
	if cfg.Database.URL == "" {
		return fmt.Errorf("database.url (or DATABASE_URL) is required")
	}

	for name, value := range map[string]string{
		"artifact.default_retention": cfg.Artifact.DefaultRetention,
		"artifact.sweep_interval":    cfg.Artifact.SweepInterval,
	} {
		if value != "" {
			if d, err := time.ParseDuration(value); err != nil || d <= 0 {
				return fmt.Errorf("%s must be a positive Go duration", name)
			}
		}
	}
	if cfg.Artifact.QuarantineMaxBytes < 0 || cfg.Artifact.MaxConcurrentUploads < 0 {
		return fmt.Errorf("artifact.quarantine_max_bytes and artifact.max_concurrent_uploads cannot be negative")
	}

	return nil
}

// ValidateServe checks serve-specific requirements that config.Load does not
// enforce (so migrate/bootstrap can run without them). It is called by runServe.
func ValidateServe(cfg *Config) error {
	if cfg.API.Addr == "" {
		return fmt.Errorf("api.addr (or API_ADDR) is required for serve")
	}
	// TLS is opt-in: HTTPS when both cert+key are set, plain HTTP when neither.
	// Exactly one set is a misconfig — fail loud rather than guess.
	if (cfg.API.TLSCertFile == "") != (cfg.API.TLSKeyFile == "") {
		return fmt.Errorf("api.tls_cert_file and api.tls_key_file must both be set (HTTPS) or both unset (HTTP)")
	}
	return ValidateAuthServe(cfg)
}

// AuthPublicOrigin parses and normalizes the configured public browser origin.
// An enabled authentication surface without a valid exact HTTPS origin fails
// closed rather than guessing.
func AuthPublicOrigin(cfg *Config) (string, error) {
	if strings.TrimSpace(cfg.Auth.PublicOrigin) == "" {
		return "", fmt.Errorf("auth.public_origin (or AUTH_PUBLIC_ORIGIN) is required")
	}
	parsed, err := url.Parse(cfg.Auth.PublicOrigin)
	if err != nil {
		return "", fmt.Errorf("auth.public_origin is not a valid URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("auth.public_origin must be an absolute https origin")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("auth.public_origin must be an origin with a host and no userinfo, query, or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("auth.public_origin must not include a path")
	}
	return "https://" + parsed.Host, nil
}

// AuthSessionTTLs returns the configured session bounds, defaulting to the
// approved maximums and refusing any value that would weaken them.
func AuthSessionTTLs(cfg *Config) (idle, absolute, recent time.Duration, err error) {
	idle, absolute, recent = 12*time.Hour, 30*24*time.Hour, 15*time.Minute
	if cfg.Auth.Session.IdleTTL != "" {
		idle, err = parseBoundedDuration("auth.session.idle_ttl", cfg.Auth.Session.IdleTTL, 12*time.Hour)
		if err != nil {
			return 0, 0, 0, err
		}
	}
	if cfg.Auth.Session.AbsoluteTTL != "" {
		absolute, err = parseBoundedDuration("auth.session.absolute_ttl", cfg.Auth.Session.AbsoluteTTL, 30*24*time.Hour)
		if err != nil {
			return 0, 0, 0, err
		}
	}
	if cfg.Auth.Session.RecentAuthTTL != "" {
		recent, err = parseBoundedDuration("auth.session.recent_auth_ttl", cfg.Auth.Session.RecentAuthTTL, 15*time.Minute)
		if err != nil {
			return 0, 0, 0, err
		}
	}
	if absolute < idle {
		return 0, 0, 0, fmt.Errorf("auth.session.absolute_ttl must not be shorter than auth.session.idle_ttl")
	}
	return idle, absolute, recent, nil
}

func parseBoundedDuration(field, value string, max time.Duration) (time.Duration, error) {
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid duration: %w", field, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive", field)
	}
	if parsed > max {
		return 0, fmt.Errorf("%s must not exceed %s", field, max)
	}
	return parsed, nil
}

// AuthTrustedProxyNets parses the configured trusted-proxy CIDRs.
func AuthTrustedProxyNets(cfg *Config) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, raw := range cfg.Auth.TrustedProxies {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("auth.trusted_proxies entry %q is not a CIDR: %w", raw, err)
		}
		out = append(out, network)
	}
	return out, nil
}

// ValidateAuthServe validates the browser-authentication prerequisites. They
// are always required: there is no unauthenticated customer mode.
func ValidateAuthServe(cfg *Config) error {
	if _, err := AuthPublicOrigin(cfg); err != nil {
		return err
	}
	if _, _, _, err := AuthSessionTTLs(cfg); err != nil {
		return err
	}
	if _, err := AuthTrustedProxyNets(cfg); err != nil {
		return err
	}
	if err := validateAuthEmail(cfg.Auth.Email); err != nil {
		return err
	}
	if cfg.Auth.Bootstrap.AdminEmail == "" {
		return fmt.Errorf("auth.bootstrap.admin_email is required")
	}
	if cfg.Auth.Bootstrap.AdminLocale != "en" && cfg.Auth.Bootstrap.AdminLocale != "pt" {
		return fmt.Errorf("auth.bootstrap.admin_locale must be en or pt")
	}
	return nil
}

func validateAuthEmail(email AuthEmailConfig) error {
	if email.Host == "" || email.From == "" {
		return fmt.Errorf("auth.email.host and auth.email.from are required")
	}
	if email.Port <= 0 || email.Port > 65535 {
		return fmt.Errorf("auth.email.port must be 1-65535")
	}
	if email.Mode != "starttls" && email.Mode != "tls" {
		return fmt.Errorf("auth.email.mode must be starttls or tls (verified TLS is required)")
	}
	if strings.ContainsAny(email.Host+email.From, "\r\n") {
		return fmt.Errorf("auth.email.host and auth.email.from must not contain line breaks")
	}
	if (email.Username == "") != (email.Password == "") {
		return fmt.Errorf("auth.email.username and auth.email.password must be set together")
	}
	return nil
}

// ValidateGatewayServe checks gateway-serve requirements (cmd/gateway). mTLS is
// required: server cert + key + the client CA bundle that verifies workload
// identities. KubeNamespace scopes Secret-read RBAC (ARCH-008).
// ValidateArtifactServe checks the api/gateway artifact backend. Both
// serving processes fail closed without both zone credentials, and refuse the
// lifecycle credential so a serving defect can never read quarantine.
func ValidateArtifactServe(cfg *Config) error {
	a := cfg.Artifact
	if !a.Enabled {
		return nil
	}
	if a.Endpoint == "" || a.Bucket == "" {
		return fmt.Errorf("artifact endpoint and bucket are required")
	}
	if a.IngestAccessKey == "" || a.IngestSecretKey == "" || a.ServeAccessKey == "" || a.ServeSecretKey == "" {
		return fmt.Errorf("artifact ingest and serve credentials are required")
	}
	if a.LifecycleAccessKey != "" || a.LifecycleSecretKey != "" {
		return fmt.Errorf("the artifact lifecycle credential is coordinator-only and must not be configured here")
	}
	return validateArtifactDurations(a)
}

// ValidateArtifactCoordinator checks the coordinator: lifecycle credential
// plus the exact mTLS scanner identity.
func ValidateArtifactCoordinator(cfg *Config) error {
	a := cfg.Artifact
	if a.Endpoint == "" || a.Bucket == "" || a.LifecycleAccessKey == "" || a.LifecycleSecretKey == "" {
		return fmt.Errorf("artifact endpoint, bucket, and lifecycle credential are required")
	}
	if a.ScannerURL == "" || a.ScannerSPIFFEID == "" || a.ScannerCAFile == "" || a.ClientCertFile == "" || a.ClientKeyFile == "" {
		return fmt.Errorf("artifact scanner_url, scanner_spiffe_id, scanner_ca_file, client_cert_file, and client_key_file are required")
	}
	if a.ScanConcurrency < 1 || a.ScanConcurrency > 8 {
		return fmt.Errorf("artifact scan_concurrency must be between 1 and 8")
	}
	return validateArtifactDurations(a)
}

func validateArtifactDurations(a ArtifactConfig) error {
	if a.DefaultRetention != "" {
		if d, err := time.ParseDuration(a.DefaultRetention); err != nil || d <= 0 {
			return fmt.Errorf("artifact default_retention must be a positive duration")
		}
	}
	if d, err := time.ParseDuration(a.SweepInterval); err != nil || d <= 0 {
		return fmt.Errorf("artifact sweep_interval must be a positive duration")
	}
	return nil
}

// ArtifactDurations parses the already-validated duration strings.
func ArtifactDurations(cfg *Config) (defaultRetention, sweepInterval time.Duration) {
	defaultRetention, _ = time.ParseDuration(cfg.Artifact.DefaultRetention)
	sweepInterval, _ = time.ParseDuration(cfg.Artifact.SweepInterval)
	return
}

//nolint:gocyclo // fail-closed validation enumerates each runner identity field.
func ValidateGatewayServe(cfg *Config) error {
	g := cfg.Gateway
	if g.Addr == "" {
		return fmt.Errorf("gateway.addr (or GATEWAY_ADDR) is required for gateway serve")
	}
	if g.TLSCertFile == "" || g.TLSKeyFile == "" {
		return fmt.Errorf("gateway.tls_cert_file + gateway.tls_key_file are required (mTLS is mandatory for the gateway)")
	}
	if g.ClientCAFile == "" {
		return fmt.Errorf("gateway.client_ca_file is required (mTLS client verification)")
	}
	if g.KubeNamespace == "" {
		return fmt.Errorf("gateway.kube_namespace is required (Secret-read scope for credential resolution)")
	}
	seen := make(map[string]struct{}, len(g.ApprovedRunners))
	for i, r := range g.ApprovedRunners {
		if r.Namespace == "" || r.RunnerID == "" || r.SpiffeID == "" || len(r.AllowedToolNamespaces) == 0 {
			return fmt.Errorf("gateway.approved_runners[%d] requires namespace, runner_id, spiffe_id, and allowed_tool_namespaces", i)
		}
		if _, ok := seen[r.SpiffeID]; ok {
			return fmt.Errorf("gateway.approved_runners[%d] duplicates spiffe_id %q", i, r.SpiffeID)
		}
		seen[r.SpiffeID] = struct{}{}
		for _, ns := range r.AllowedToolNamespaces {
			if ns == "" || ns == "*" || strings.ContainsAny(ns, "/ ") {
				return fmt.Errorf("gateway.approved_runners[%d] has invalid allowed namespace %q", i, ns)
			}
		}
	}
	return nil
}

// ValidateDispatchServe checks dispatch-serve requirements (cmd/dispatch). mTLS
// is required: server cert + key + the client CA bundle that verifies warm
// worker (supervisor) caller certs (ARCH-010).
func ValidateDispatchServe(cfg *Config) error {
	d := cfg.Dispatch
	if d.Addr == "" {
		return fmt.Errorf("dispatch.addr (or DISPATCH_ADDR) is required for dispatch serve")
	}
	if d.TLSCertFile == "" || d.TLSKeyFile == "" {
		return fmt.Errorf("dispatch.tls_cert_file + dispatch.tls_key_file are required (mTLS is mandatory for the Work server)")
	}
	if d.ClientCAFile == "" {
		return fmt.Errorf("dispatch.client_ca_file is required (mTLS client verification)")
	}
	if d.DefaultModelID == "" {
		return fmt.Errorf("dispatch.default_model_id (or DISPATCH_DEFAULT_MODEL_ID) is required: dispatch must not emit an empty model permission (HOR-249)")
	}
	if d.DefaultModelAPI == "" {
		return fmt.Errorf("dispatch.default_model_api (or DISPATCH_DEFAULT_MODEL_API) is required")
	}
	return nil
}
