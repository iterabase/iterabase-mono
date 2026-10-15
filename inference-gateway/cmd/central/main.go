// Command central is the central Iterabase inference gateway (HOR-661): it
// holds the provider keys, terminates installation mTLS, meters credits and
// owns the hosted installation registry. See
// docs/architecture/v2-central-inference-gateway.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/billing"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/config"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/fx"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/notify"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/opauth"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/pki"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/registry"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/server"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/store"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/version"
)

const usage = `Usage: central <command>

Commands:
  serve        Serve installations and operators
  migrate up   Apply the database migrations (advisory-locked)
  ca init      Create the installation CA from the KMS key, once; prints the CA PEM

Configuration comes from the environment (CENTRAL_CONFIG, CENTRAL_DATABASE_*,
CENTRAL_KMS_KEY_ID, CENTRAL_PROVIDER_KEY, CENTRAL_OPERATORS, ...).`

func main() {
	args := strings.Join(os.Args[1:], " ")
	logger := newLogger()
	var err error
	switch args {
	case "serve":
		err = runServe(logger)
	case "migrate up":
		err = runMigrate(logger)
	case "ca init":
		err = runCAInit(logger)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		logger.Error("central failed", "command", args, "error", err)
		os.Exit(1)
	}
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func connect(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("database URL: %w", err)
	}
	poolCfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: %w", err)
	}
	return pool, nil
}

func runMigrate(logger *slog.Logger) error {
	ctx := context.Background()
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	pool, err := connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		return err
	}
	logger.Info("migrations applied")
	return nil
}

func kmsSigner(ctx context.Context, cfg *config.Config) (*pki.KMSSigner, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return pki.NewKMSSigner(ctx, kms.NewFromConfig(awsCfg), cfg.KMSKeyID)
}

// runCAInit creates the CA certificate once and prints it. It is idempotent:
// an existing CA is printed (after checking it belongs to the KMS key).
func runCAInit(logger *slog.Logger) error {
	ctx := context.Background()
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	pool, err := connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		return err
	}
	signer, err := kmsSigner(ctx, cfg)
	if err != nil {
		return err
	}
	st := store.New(pool, billing.NewPolicy(cfg.File))
	existing, _, err := st.CA(ctx)
	if errors.Is(err, store.ErrNotFound) {
		ca, err := pki.NewCA(signer, time.Now())
		if err != nil {
			return err
		}
		inserted, err := st.InsertCA(ctx, string(pki.EncodeCert(ca.Raw)), cfg.KMSKeyID)
		if err != nil {
			return err
		}
		if inserted {
			logger.Info("installation CA created")
		}
		existing, _, err = st.CA(ctx)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	ca, err := pki.ParseCertPEM([]byte(existing))
	if err != nil {
		return err
	}
	if _, err := pki.NewAuthority(ca, signer); err != nil {
		return err
	}
	logger.Info("installation CA certificate (public; publish to SSM /iterabase/hosted/central/v1/ca-bundle)", "ca_pem", existing)
	fmt.Print(existing)
	return nil
}

//nolint:gocyclo // Process wiring: one step per collaborator.
func runServe(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("starting central gateway", "version", version.Version(), "commit", version.Commit())

	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	if cfg.ProviderKey == "" {
		return errors.New("CENTRAL_PROVIDER_KEY is required")
	}
	pool, err := connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	policy := billing.NewPolicy(cfg.File)
	st := store.New(pool, policy)
	caPEM, _, err := st.CA(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("no installation CA: run `central ca init` once")
	}
	if err != nil {
		return err
	}
	ca, err := pki.ParseCertPEM([]byte(caPEM))
	if err != nil {
		return err
	}
	signer, err := kmsSigner(ctx, cfg)
	if err != nil {
		return err
	}
	authority, err := pki.NewAuthority(ca, signer)
	if err != nil {
		return err
	}

	reg := registry.New(st, cfg.DatabaseURL, logger)
	if err := reg.Start(ctx); err != nil {
		return fmt.Errorf("registry: %w", err)
	}

	var publisher notify.Publisher = notify.LogPublisher{Logger: logger}
	if cfg.AlertTopicARN != "" {
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return fmt.Errorf("aws config: %w", err)
		}
		publisher = &notify.SNSPublisher{Client: sns.NewFromConfig(awsCfg), TopicARN: cfg.AlertTopicARN}
	} else {
		logger.Warn("CENTRAL_ALERT_TOPIC_ARN is not set; alerts are only logged")
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	fetcher := &fx.Fetcher{URL: cfg.ECBURL, Client: httpClient, Recorder: st, Logger: logger}
	go fetcher.Run(ctx)

	m := metrics.NewWithBuild(prometheus.NewRegistry(), version.Version(), version.Commit())
	m.RegisterDatabasePool(pool)

	srv, err := server.New(server.Deps{
		Config:    cfg,
		Store:     st,
		Registry:  reg,
		Authority: authority,
		Limiter:   ratelimit.NewPostgresLimiter(pool, store.RateTable),
		Verifier:  &opauth.Verifier{Client: httpClient, Audience: cfg.PublicHost, Operators: cfg.Operators, Now: time.Now},
		Alerts:    &notify.Alerts{Store: st, Publisher: publisher, Logger: logger},
		Ops:       publisher,
		Metrics:   m,
		Logger:    logger,
	})
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}
