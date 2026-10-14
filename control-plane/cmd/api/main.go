// Command api runs the control-plane HTTP API (cmd/api). It also owns database
// migrations and admin bootstrap via subcommands, intended to run as RBAC-less
// init containers before the manager/api start.
//
//	usage:
//	  control-plane-api [--config path] serve
//	  control-plane-api [--config path] migrate up
//	  control-plane-api [--config path] migrate down [steps]
//	  control-plane-api [--config path] bootstrap [--admin-email e] [--admin-locale en|pt] [--recover-admin]
//
// The API serves the browser shell, the browser-authentication journey, and the
// customer work, artifact, and People APIs under V2 authority (HOR-582).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	artifactstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/database"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/logging"
	cpmetrics "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/metrics"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/server"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/version"
	workstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/work"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/workflow"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "", "path to config file (optional; env + defaults used if empty)")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading config: %v\n", err)
		os.Exit(1)
	}

	logger, _ := logging.New(cfg.Logging.Level, cfg.Logging.Format)
	logger.Info("starting control-plane api",
		"version", version.Version(), "commit", version.Commit(), "date", version.Date(),
		"command", args[0])

	switch args[0] {
	case "serve":
		if err := runServeCmd(cfg, logger); err != nil {
			logger.Error("api serve failed", "error", err)
			os.Exit(1)
		}
	case "migrate":
		if err := runMigrate(cfg, logger, args[1:]); err != nil {
			logger.Error("api migrate failed", "error", err)
			os.Exit(1)
		}
	case "bootstrap":
		if err := runBootstrap(cfg, logger, args[1:]); err != nil {
			logger.Error("api bootstrap failed", "error", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		printUsage()
		os.Exit(1)
	}
}

// runServeCmd wires the SIGINT/SIGTERM signal context to runServe and returns
// its error. The signal context's stop() is deferred here (not in main) so it
// always runs on return — main os.Exit's on error, which would skip a defer.
func runServeCmd(cfg *config.Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runServe(ctx, cfg, logger)
}

// runServe runs the HTTP API until ctx is canceled (SIGINT/SIGTERM in main,
// a test context in tests). It serves HTTPS when cfg.API.TLSCertFile +
// TLSKeyFile are both set (cert-manager leaf, internal issuer), else plain
// HTTP — tls opt-in (cert-manager leaf, internal issuer).
//
//nolint:gocyclo // Startup validates optional artifacts, TLS, metrics, and graceful shutdown explicitly.
func runServe(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	pool, err := database.Connect(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connecting database: %w", err)
	}
	defer pool.Close()

	if err := config.ValidateServe(cfg); err != nil {
		return err
	}
	if err := config.ValidateArtifactServe(cfg); err != nil {
		return err
	}
	var artifacts *artifactstore.Service
	if cfg.Artifact.Enabled {
		artifacts, err = artifactstore.NewConfiguredService(cfg, pool, logger)
		if err != nil {
			return fmt.Errorf("configure artifact service: %w", err)
		}
		if err := artifacts.Ready(ctx); err != nil {
			return fmt.Errorf("artifact service not ready: %w", err)
		}
		artifacts.StartSweeper(ctx)
	}

	store := identity.NewStore(pool)
	if err := seedOrganizationName(ctx, store, cfg.Organization.Name, logger); err != nil {
		return err
	}
	authServices, authWorker, err := buildAuthServices(cfg, logger, store)
	if err != nil {
		return fmt.Errorf("configuring browser authentication: %w", err)
	}
	go authWorker.Run(ctx)
	logger.Info("authentication-email worker started", "origin", cfg.Auth.PublicOrigin)

	m := cpmetrics.New("api", version.Version(), version.Commit())
	m.RegisterDatabasePool(pool)
	httpSrv := &http.Server{
		Addr: cfg.API.Addr,
		Handler: server.New(server.Services{
			Pool:      pool,
			Store:     store,
			Work:      workstore.NewStore(pool),
			Workflows: workflow.NewStore(pool),
			Artifacts: artifacts,
			Metrics:   m,
			Auth:      authServices,
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	tlsEnabled := cfg.API.TLSCertFile != "" && cfg.API.TLSKeyFile != ""
	errCh := make(chan error, 2)
	go func() {
		if tlsEnabled {
			errCh <- httpSrv.ListenAndServeTLS(cfg.API.TLSCertFile, cfg.API.TLSKeyFile)
		} else {
			errCh <- httpSrv.ListenAndServe()
		}
	}()
	logger.Info("listening", "addr", cfg.API.Addr, "tls", tlsEnabled)

	var metricsSrv *http.Server
	if cfg.Metrics.Addr != "" {
		metricsSrv = &http.Server{Addr: cfg.Metrics.Addr, Handler: m.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() { errCh <- metricsSrv.ListenAndServe() }()
		logger.Info("metrics listening", "addr", cfg.Metrics.Addr)
	}

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if metricsSrv != nil {
			_ = metricsSrv.Shutdown(shutdownCtx)
		}
		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}

// seedOrganizationName stores the deployment's installation name
// (organization.name / ORGANIZATION_NAME) while none is stored; an Admin-set
// name is never overwritten (DES-HOR-459-08).
func seedOrganizationName(ctx context.Context, store *identity.Store, seed string, logger *slog.Logger) error {
	seeded, err := store.SeedOrganizationName(ctx, seed)
	if err != nil {
		return fmt.Errorf("seeding organization name: %w", err)
	}
	if seeded {
		logger.Info("organization name seeded from configuration")
	}
	return nil
}

func runMigrate(cfg *config.Config, logger *slog.Logger, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("migrate requires a direction: up|down [steps]")
	}
	switch args[0] {
	case "up":
		if err := database.MigrateUp(cfg.Database.URL); err != nil {
			return err
		}
		logger.Info("migrations applied")
	case "down":
		steps := 0
		if len(args) >= 2 {
			if _, err := fmt.Sscanf(args[1], "%d", &steps); err != nil {
				return fmt.Errorf("invalid steps %q: %w", args[1], err)
			}
		}
		if err := database.MigrateDown(cfg.Database.URL, steps); err != nil {
			return err
		}
		logger.Info("migrations rolled back", "steps", steps)
	default:
		return fmt.Errorf("unknown migrate direction: %s (use up|down)", args[0])
	}
	return nil
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: control-plane-api [--config path] <serve | migrate up | migrate down [steps] | bootstrap [flags]>")
}

// buildAuthServices wires the always-on browser-authentication surface. It
// fails closed without a valid origin, session bounds, trusted-proxy
// configuration, GeoIP database, or verified-TLS SMTP transport.
func buildAuthServices(cfg *config.Config, logger *slog.Logger, store *identity.Store) (*server.AuthServices, *identity.AuthEmailWorker, error) {
	origin, err := config.AuthPublicOrigin(cfg)
	if err != nil {
		return nil, nil, err
	}
	idle, absolute, recent, err := config.AuthSessionTTLs(cfg)
	if err != nil {
		return nil, nil, err
	}
	proxies, err := config.AuthTrustedProxyNets(cfg)
	if err != nil {
		return nil, nil, err
	}
	location, err := buildLocationResolver(cfg.Auth.GeoIPDatabase)
	if err != nil {
		return nil, nil, err
	}
	roots, err := identity.LoadSMTPRootCAs(cfg.Auth.Email.CAFile)
	if err != nil {
		return nil, nil, err
	}
	sender, err := identity.NewSMTPSender(identity.SMTPConfig{
		Host:     cfg.Auth.Email.Host,
		Port:     cfg.Auth.Email.Port,
		Mode:     cfg.Auth.Email.Mode,
		From:     cfg.Auth.Email.From,
		Username: cfg.Auth.Email.Username,
		Password: cfg.Auth.Email.Password,
		RootCAs:  roots,
	})
	if err != nil {
		return nil, nil, err
	}
	authServices := &server.AuthServices{
		PublicOrigin:    origin,
		IdleTTL:         idle,
		AbsoluteTTL:     absolute,
		RecentAuthTTL:   recent,
		TrustedProxies:  proxies,
		ForwardedHeader: cfg.Auth.ForwardedHeader,
		Store:           store,
		Location:        location,
		Logger:          logger,
		Now:             time.Now,
	}
	worker := &identity.AuthEmailWorker{
		Store:  store,
		Sender: sender,
		Origin: origin,
		Config: identity.AuthEmailWorkerConfig{
			Owner:                bootstrapWorkerOwner(),
			Interval:             5 * time.Second,
			BatchSize:            20,
			LeaseTTL:             5 * time.Minute,
			MaxAttempts:          5,
			HousekeepingInterval: 10 * time.Minute,
		},
		Logger: logger,
		Now:    time.Now,
	}
	return authServices, worker, nil
}

func buildLocationResolver(path string) (identity.LocationResolver, error) {
	if path == "" {
		return identity.NoLocation{}, nil
	}
	return identity.OpenMaxMindLocation(path)
}

func bootstrapWorkerOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "control-plane-api"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// runBootstrap is the RBAC-less first-boot init step. On an installation
// without a bootstrap marker it makes the configured first Admin a
// `setup_pending` Admin and queues normal setup email (DES-HOR-582-03); every
// later run is a strict no-op. --recover-admin is the cluster-operator recovery
// path and is permitted only with no active Admin. No credential is ever
// created or printed.
func runBootstrap(cfg *config.Config, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	var adminEmail string
	var adminLocale string
	var recoverAdmin bool
	fs.StringVar(&adminEmail, "admin-email", "",
		"Email of the first Admin (defaults to auth.bootstrap.admin_email)")
	fs.StringVar(&adminLocale, "admin-locale", "",
		"Locale (en|pt) for the first Admin setup email (defaults to auth.bootstrap.admin_locale)")
	fs.BoolVar(&recoverAdmin, "recover-admin", false,
		"Cluster-operator recovery: only when no active Admin exists; revokes the named human's password, sessions, and owned keys")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := config.ValidateAuthServe(cfg); err != nil {
		return fmt.Errorf("bootstrap configuration: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connecting database: %w", err)
	}
	defer pool.Close()
	store := identity.NewStore(pool)

	if adminEmail == "" {
		adminEmail = cfg.Auth.Bootstrap.AdminEmail
	}
	if adminLocale == "" {
		adminLocale = cfg.Auth.Bootstrap.AdminLocale
	}
	result, err := store.BootstrapAdmin(ctx, identity.BootstrapOptions{
		AdminEmail:  adminEmail,
		AdminLocale: adminLocale,
		Recover:     recoverAdmin,
		Now:         time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	logger.Info("bootstrap completed",
		"state", result.State, "admin_email", result.AdminEmail, "recover", recoverAdmin)
	fmt.Printf("Bootstrap %s for %s. First-time setup instructions are queued when needed; no credential is printed and no browser session is created.\n",
		result.State, result.AdminEmail)
	return nil
}
