// Command artifact-coordinator runs the trusted V2 artifact lifecycle worker
// (docs/architecture/v2-artifact-processing.md, DES-HOR-460-07): it is the
// only process holding the lifecycle object credential. It leases security
// attempts, streams exact quarantine bytes to the isolated scanner over mTLS,
// admits email container children, promotes cleared originals, and purges
// deleted, rejected, expired, and orphaned bytes. It has no customer-facing
// listener; its HTTP port serves health and metrics only.
//
//	usage:
//	  control-plane-artifact-coordinator [--config path] serve
//	  control-plane-artifact-coordinator [--config path] rescan --reason <code> <artifact-id>...
//
// rescan is the operator security-advisory path (architecture section 18): it
// withdraws each cleared artifact from the library and every read, then
// queues an exact re-scan that either re-registers or rejects and purges it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/database"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/logging"
	cpmetrics "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/metrics"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/version"
)

func main() { os.Exit(realMain()) }

func realMain() int {
	var configPath string
	flag.StringVar(&configPath, "config", "", "path to config file (optional; env + defaults used if empty)")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 || (args[0] != "serve" && args[0] != "rescan") {
		fmt.Fprintln(os.Stderr, "usage: control-plane-artifact-coordinator [--config path] serve | rescan --reason <code> <artifact-id>...")
		return 1
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading config: %v\n", err)
		return 1
	}
	logger, _ := logging.New(cfg.Logging.Level, cfg.Logging.Format)
	logger.Info("starting artifact coordinator", "version", version.Version(), "commit", version.Commit())
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if args[0] == "rescan" {
		if err := rescan(ctx, cfg, logger, args[1:]); err != nil {
			logger.Error("artifact rescan failed", "error", err)
			return 1
		}
		return 0
	}
	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("artifact coordinator failed", "error", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	if err := config.ValidateArtifactCoordinator(cfg); err != nil {
		return err
	}
	pool, err := database.Connect(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connecting database: %w", err)
	}
	defer pool.Close()
	a := cfg.Artifact
	objects, err := artifact.NewMinIOStore(artifact.MinIOConfig{Endpoint: a.Endpoint, AccessKey: a.LifecycleAccessKey,
		SecretKey: a.LifecycleSecretKey, Bucket: a.Bucket, Secure: a.Secure})
	if err != nil {
		return err
	}
	scanner, err := artifact.NewMTLSScanner(artifact.MTLSScannerConfig{URL: a.ScannerURL, ServerSPIFFEID: a.ScannerSPIFFEID,
		CAFile: a.ScannerCAFile, ClientCertFile: a.ClientCertFile, ClientKeyFile: a.ClientKeyFile})
	if err != nil {
		return err
	}
	m := cpmetrics.New("artifact-coordinator", version.Version(), version.Commit())
	m.RegisterDatabasePool(pool)
	_, sweep := config.ArtifactDurations(cfg)
	owner, _ := os.Hostname()
	coord := artifact.NewCoordinator(artifact.NewStore(pool), objects, scanner, artifact.CoordinatorConfig{
		Owner: owner, Concurrency: a.ScanConcurrency, SweepInterval: sweep, QuarantineCap: a.QuarantineMaxBytes,
	}, logger, m)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		rctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := coord.Ready(rctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", m.Handler())
	srv := &http.Server{Addr: a.CoordinatorAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	done := make(chan struct{})
	go func() {
		coord.Run(ctx)
		close(done)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	<-done
	return nil
}

func rescan(ctx context.Context, cfg *config.Config, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("rescan", flag.ContinueOnError)
	reason := fs.String("reason", "", "bounded advisory code, e.g. security_advisory or engine_defect")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *reason == "" || len(*reason) > 64 || fs.NArg() == 0 {
		return errors.New("rescan requires --reason <code> and at least one artifact id")
	}
	pool, err := database.Connect(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connecting database: %w", err)
	}
	defer pool.Close()
	store := artifact.NewStore(pool)
	for _, id := range fs.Args() {
		if _, err := store.WithdrawForRescan(ctx, id, *reason); err != nil {
			return fmt.Errorf("withdraw %s: %w", id, err)
		}
		logger.Info("artifact withdrawn for re-scan", "artifact", id, "reason", *reason)
	}
	return nil
}
