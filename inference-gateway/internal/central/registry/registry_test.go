package registry

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/billing"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/config"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/store"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/testpg"
)

var actor = store.Actor{ARN: "arn:aws:iam::1:role/forge"}

func TestCache_RevocationPropagatesViaNotify(t *testing.T) {
	db := testpg.Start(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, store.Migrate(ctx, db.Pool))
	s := store.New(db.Pool, billing.Policy{})
	_, _, err := s.Register(ctx, store.Registration{ID: "acme", Deployment: billing.DeploymentInternal, Plan: billing.PlanNone, BillingCurrency: config.EUR}, actor)
	require.NoError(t, err)
	_, err = s.RecordCertificate(ctx, "acme", func() (store.IssuedCertificate, error) {
		return store.IssuedCertificate{Serial: "s1", Fingerprint: "f1", NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}, nil
	}, actor)
	require.NoError(t, err)

	c := New(s, db.URL, slog.Default())
	require.NoError(t, c.Start(ctx))

	inst, err := c.Authorize("acme", "s1")
	require.NoError(t, err)
	assert.Equal(t, "acme", inst.ID)
	_, err = c.Authorize("acme", "unknown-serial")
	assert.ErrorIs(t, err, ErrRevoked)
	_, err = c.Authorize("other", "s1")
	assert.ErrorIs(t, err, ErrRevoked, "a serial only authorizes its own installation")

	require.NoError(t, s.Revoke(ctx, "acme", "test", actor))
	assert.Eventually(t, func() bool {
		_, err := c.Authorize("acme", "s1")
		return errors.Is(err, ErrRevoked)
	}, 5*time.Second, 20*time.Millisecond, "revocation reaches the replica well before the 30 s poll")
}

type fakeLoader struct {
	installations []billing.Installation
	certs         []store.Certificate
	err           error
}

func (f *fakeLoader) LoadRegistry(context.Context) ([]billing.Installation, []store.Certificate, error) {
	return f.installations, f.certs, f.err
}

func TestCache_FailsClosedWhenStale(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	loader := &fakeLoader{
		installations: []billing.Installation{{ID: "acme", Status: "active"}},
		certs: []store.Certificate{
			{Serial: "s1", InstallationID: "acme", NotAfter: now.Add(time.Hour)},
			{Serial: "old", InstallationID: "acme", NotAfter: now.Add(-time.Second)},
		},
	}
	c := New(loader, "", slog.Default())
	c.SetClock(func() time.Time { return now })

	_, err := c.Authorize("acme", "s1")
	assert.ErrorIs(t, err, ErrStale, "never loaded")

	require.NoError(t, c.Refresh(context.Background()))
	_, err = c.Authorize("acme", "s1")
	require.NoError(t, err)
	_, err = c.Authorize("acme", "old")
	assert.ErrorIs(t, err, ErrRevoked, "expired certificate")

	loader.err = errors.New("database down")
	now = now.Add(MaxStaleness - time.Second)
	assert.Error(t, c.Refresh(context.Background()))
	_, err = c.Authorize("acme", "s1")
	require.NoError(t, err, "still within the staleness bound")

	now = now.Add(2 * time.Second)
	_, err = c.Authorize("acme", "s1")
	assert.ErrorIs(t, err, ErrStale, "a stale registry admits nobody")
	assert.False(t, c.Fresh())
}
