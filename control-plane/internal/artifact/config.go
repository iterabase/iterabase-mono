package artifact

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/config"
)

// NewConfiguredService builds the api/gateway artifact domain from process
// config with only the ingest and serve zone credentials.
func NewConfiguredService(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger) (*Service, error) {
	a := cfg.Artifact
	ingest, err := NewMinIOStore(MinIOConfig{Endpoint: a.Endpoint, AccessKey: a.IngestAccessKey,
		SecretKey: a.IngestSecretKey, Bucket: a.Bucket, Secure: a.Secure})
	if err != nil {
		return nil, err
	}
	serve, err := NewMinIOStore(MinIOConfig{Endpoint: a.Endpoint, AccessKey: a.ServeAccessKey,
		SecretKey: a.ServeSecretKey, Bucket: a.Bucket, Secure: a.Secure})
	if err != nil {
		return nil, err
	}
	retention, _ := config.ArtifactDurations(cfg)
	return NewService(NewStore(pool), ingest, serve, Config{
		DefaultRetention: retention, QuarantineMaxBytes: a.QuarantineMaxBytes,
		MaxConcurrentUploads: a.MaxConcurrentUploads,
	}, log), nil
}
