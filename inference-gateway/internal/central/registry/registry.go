// Package registry is each central replica's in-memory view of the
// installation registry, refreshed on Postgres NOTIFY central_registry_changed
// and every 30 s (DES-HOR-661-02). Every installation request is authorized
// against it; when it has not refreshed for two minutes it fails closed
// (DES-HOR-661-08).
package registry

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/billing"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/store"
)

// Defaults.
const (
	RefreshInterval = 30 * time.Second
	MaxStaleness    = 2 * time.Minute
	notifyChannel   = "central_registry_changed"
	reconnectDelay  = 2 * time.Second
)

// Errors.
var (
	// ErrRevoked covers a revoked, expired or unknown certificate or
	// installation: installation_revoked.
	ErrRevoked = errors.New("registry: installation or certificate not authorized")
	// ErrStale means the registry cannot vouch for anything: retryable.
	ErrStale = errors.New("registry: registry is stale")
)

// Loader reads the whole registry.
type Loader interface {
	LoadRegistry(ctx context.Context) ([]billing.Installation, []store.Certificate, error)
}

// Cache is the in-memory registry.
type Cache struct {
	loader Loader
	dsn    string
	logger *slog.Logger
	now    func() time.Time

	mu            sync.RWMutex
	installations map[string]billing.Installation
	certs         map[string]store.Certificate
	lastRefresh   time.Time
	refreshMu     sync.Mutex
}

// New creates a cache. dsn is used for the LISTEN connection.
func New(loader Loader, dsn string, logger *slog.Logger) *Cache {
	return &Cache{loader: loader, dsn: dsn, logger: logger, now: time.Now}
}

// SetClock replaces the clock (tests).
func (c *Cache) SetClock(now func() time.Time) { c.now = now }

// Refresh reloads the whole registry.
func (c *Cache) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	started := c.now()
	installations, certs, err := c.loader.LoadRegistry(ctx)
	if err != nil {
		return err
	}
	im := make(map[string]billing.Installation, len(installations))
	for _, inst := range installations {
		im[inst.ID] = inst
	}
	cm := make(map[string]store.Certificate, len(certs))
	for _, cert := range certs {
		cm[cert.Serial] = cert
	}
	c.mu.Lock()
	c.installations, c.certs, c.lastRefresh = im, cm, started
	c.mu.Unlock()
	return nil
}

// Start loads the registry and keeps it fresh until ctx is done.
func (c *Cache) Start(ctx context.Context) error {
	if err := c.Refresh(ctx); err != nil {
		return err
	}
	go c.poll(ctx)
	go c.listen(ctx)
	return nil
}

func (c *Cache) poll(ctx context.Context) {
	ticker := time.NewTicker(RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.Refresh(ctx); err != nil && ctx.Err() == nil {
				c.logger.Error("registry refresh failed", "error", err)
			}
		}
	}
}

// listen refreshes on every notification, reconnecting after failures. A
// reconnect also refreshes, so a notification missed while disconnected is
// never lost.
func (c *Cache) listen(ctx context.Context) {
	for ctx.Err() == nil {
		if err := c.listenOnce(ctx); err != nil && ctx.Err() == nil {
			c.logger.Warn("registry LISTEN connection lost; reconnecting", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectDelay):
			}
		}
	}
}

func (c *Cache) listenOnce(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, c.dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	if err := c.Refresh(ctx); err != nil {
		return err
	}
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		if err := c.Refresh(ctx); err != nil {
			return err
		}
	}
}

// Fresh reports whether the registry refreshed within MaxStaleness.
func (c *Cache) Fresh() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return !c.lastRefresh.IsZero() && c.now().Sub(c.lastRefresh) < MaxStaleness
}

// Authorize checks a verified client certificate against the registry: the
// installation must be active, and the serial must be its own, unrevoked and
// unexpired. It returns the installation.
func (c *Cache) Authorize(installationID, serial string) (billing.Installation, error) {
	if !c.Fresh() {
		return billing.Installation{}, ErrStale
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	inst, ok := c.installations[installationID]
	if !ok || inst.Status != "active" {
		return billing.Installation{}, ErrRevoked
	}
	cert, ok := c.certs[serial]
	if !ok || cert.InstallationID != installationID || cert.RevokedAt != nil || !c.now().Before(cert.NotAfter) {
		return billing.Installation{}, ErrRevoked
	}
	return inst, nil
}
