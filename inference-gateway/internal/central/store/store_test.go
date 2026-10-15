package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/attribution"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/billing"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/config"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/fx"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/testpg"
)

var admin = Actor{ARN: "arn:aws:iam::111111111111:role/forge", Session: "test"}

func testPolicy() billing.Policy {
	return billing.Policy{
		Tiers: map[string]config.Tier{
			"starter": {WorkflowLimit: 2, MonthlyCredits: 5000},
			"growth":  {WorkflowLimit: 6, MonthlyCredits: 20000},
			"scale":   {WorkflowLimit: 15, MonthlyCredits: 60000},
		},
		Trial: config.Trial{Tier: "starter", DaysBasis: 30},
	}
}

// usdModel costs 1.00 per million input and output tokens.
var usdModel = config.Model{
	BaseURL: "https://provider.test/v1", ProviderModel: "provider-model", Currency: config.USD,
	InputPerMillion: "1.00", CachedInputPerMillion: "0.10", OutputPerMillion: "1.00", EffectiveFrom: "2026-10-01",
}

// NewTestStore starts a database, migrates it and returns a store whose clock
// the caller controls.
func newTestStore(t *testing.T) (*Store, *testpg.DB, *time.Time) {
	t.Helper()
	db := testpg.Start(t)
	ctx := context.Background()
	require.NoError(t, Migrate(ctx, db.Pool))
	s := New(db.Pool, testPolicy())
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	return s, db, &now
}

func days(n int) *int { return &n }

func TestMigrate_ConcurrentAndIdempotent(t *testing.T) {
	db := testpg.Start(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = Migrate(ctx, db.Pool) }()
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	var n int
	require.NoError(t, db.Pool.QueryRow(ctx, `SELECT count(*) FROM central.schema_migrations`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestRegister(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	reg := Registration{ID: "acme", Deployment: billing.DeploymentHosted, Plan: billing.PlanTrial, BillingCurrency: config.EUR, TrialDays: days(7)}

	inst, created, err := s.Register(ctx, reg, admin)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, "1166.666667", billing.Format(inst.TrialCredits, billing.CreditPlaces))
	assert.Equal(t, time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), inst.TrialEndsAt.UTC())

	again, created, err := s.Register(ctx, reg, admin)
	require.NoError(t, err)
	assert.False(t, created, "an identical registration is idempotent")
	assert.Equal(t, inst.EntitlementsVersion, again.EntitlementsVersion)

	differing := reg
	differing.BillingCurrency = config.USD
	_, _, err = s.Register(ctx, differing, admin)
	assert.ErrorIs(t, err, ErrConflict)

	require.NoError(t, s.Revoke(ctx, "acme", "test", admin))
	reactivated, _, err := s.Register(ctx, reg, admin)
	require.NoError(t, err)
	assert.Equal(t, "active", reactivated.Status, "a revoked installation is reactivated by an identical registration")
}

func TestCertificatesAndRevocation(t *testing.T) {
	s, db, _ := newTestStore(t)
	ctx := context.Background()
	_, _, err := s.Register(ctx, Registration{ID: "acme", Deployment: billing.DeploymentHosted, Plan: billing.PlanStarter, BillingCurrency: config.EUR}, admin)
	require.NoError(t, err)

	// A LISTEN connection observes the registry change notifications.
	conn, err := pgx.Connect(ctx, db.URL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(ctx) })
	_, err = conn.Exec(ctx, `LISTEN central_registry_changed`)
	require.NoError(t, err)

	issue := func(serial string) func() (IssuedCertificate, error) {
		return func() (IssuedCertificate, error) {
			return IssuedCertificate{Serial: serial, Fingerprint: "fp-" + serial, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}, nil
		}
	}
	_, err = s.RecordCertificate(ctx, "acme", issue("a1"), admin)
	require.NoError(t, err)
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = conn.WaitForNotification(waitCtx)
	require.NoError(t, err, "issuing notifies the registry")

	_, err = s.RecordCertificate(ctx, "nobody", issue("a2"), admin)
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, s.Revoke(ctx, "acme", "lost laptop", admin))
	require.NoError(t, s.Revoke(ctx, "acme", "again", admin), "idempotent")
	_, certs, err := s.Installation(ctx, "acme")
	require.NoError(t, err)
	require.Len(t, certs, 1)
	assert.NotNil(t, certs[0].RevokedAt)

	called := false
	_, err = s.RecordCertificate(ctx, "acme", func() (IssuedCertificate, error) { called = true; return IssuedCertificate{}, nil }, admin)
	assert.ErrorIs(t, err, ErrRevoked)
	assert.False(t, called, "a revoked installation is never issued a certificate")

	var actions []string
	rows, err := s.pool.Query(ctx, `SELECT action FROM central.audit_events WHERE installation_id = 'acme' ORDER BY id`)
	require.NoError(t, err)
	actions, err = pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"register", "issue_certificate", "revoke"}, actions, "every mutation is audited once")
}

func TestPlanAndTrialExtension(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	_, _, err := s.Register(ctx, Registration{ID: "acme", Deployment: billing.DeploymentHosted, Plan: billing.PlanTrial, BillingCurrency: config.EUR, TrialDays: days(7)}, admin)
	require.NoError(t, err)

	ext, err := s.ExtendTrial(ctx, "acme", 3, admin)
	require.NoError(t, err)
	assert.Equal(t, "1666.666667", billing.Format(ext.TrialCredits, billing.CreditPlaces))
	assert.Equal(t, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC), ext.TrialEndsAt.UTC())

	converted, err := s.SetPlan(ctx, "acme", billing.PlanGrowth, admin)
	require.NoError(t, err)
	assert.Equal(t, billing.PlanGrowth, converted.Plan)
	assert.Greater(t, converted.EntitlementsVersion, ext.EntitlementsVersion)

	_, err = s.ExtendTrial(ctx, "acme", 3, admin)
	assert.ErrorIs(t, err, ErrInvalid, "only a trial can be extended")

	_, _, err = s.Register(ctx, Registration{ID: "internal-a", Deployment: billing.DeploymentInternal, Plan: billing.PlanNone, BillingCurrency: config.EUR}, admin)
	require.NoError(t, err)
	_, err = s.SetPlan(ctx, "internal-a", billing.PlanScale, admin)
	assert.ErrorIs(t, err, ErrInvalid)
}

// TestEntitlementChangesRefreshThePeriodRollup: a plan change or trial
// extension rewrites the current period's rollup in the same transaction, so
// the export agrees with the installation read without further inference.
func TestEntitlementChangesRefreshThePeriodRollup(t *testing.T) {
	s, _, now := newTestStore(t)
	ctx := context.Background()
	_, _, err := s.Register(ctx, Registration{ID: "upgrader", Deployment: billing.DeploymentHosted, Plan: billing.PlanStarter, BillingCurrency: config.USD}, admin)
	require.NoError(t, err)
	_, err = recordAt(s, *now, "upgrader", &billing.Tokens{Input: 30_000_000, Output: 30_000_000}) // 60 USD = 6,000 credits
	require.NoError(t, err)
	exportRow := func(id string) ExportRow {
		rows, err := s.Export(ctx, *now)
		require.NoError(t, err)
		for _, r := range rows {
			if r.InstallationID == id {
				return r
			}
		}
		t.Fatalf("no export row for %s", id)
		return ExportRow{}
	}
	before := exportRow("upgrader")
	assert.Equal(t, "5000.000000", before.AllowanceCredits)
	assert.Equal(t, "1000.000000", before.OverageCredits)
	_, err = s.SetPlan(ctx, "upgrader", billing.PlanScale, admin)
	require.NoError(t, err)
	after := exportRow("upgrader")
	assert.Equal(t, "60000.000000", after.AllowanceCredits, "the upgrade reaches the open period's rollup")
	assert.Equal(t, "0.000000", after.OverageCredits)
	assert.Equal(t, before.ChargeableCredits, after.ChargeableCredits)

	_, _, err = s.Register(ctx, Registration{ID: "trialist", Deployment: billing.DeploymentHosted, Plan: billing.PlanTrial, BillingCurrency: config.USD, TrialDays: days(7)}, admin)
	require.NoError(t, err)
	_, err = recordAt(s, *now, "trialist", &billing.Tokens{Input: 1_000_000, Output: 1_000_000})
	require.NoError(t, err)
	_, err = s.ExtendTrial(ctx, "trialist", 3, admin)
	require.NoError(t, err)
	trial := exportRow("trialist")
	assert.Equal(t, "1666.666667", trial.AllowanceCredits, "the extension's pro-rated credits reach the rollup")
	assert.Equal(t, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC), trial.PeriodEnd.UTC())
}

// TestConcurrentSettlementCannotOverwriteAnEntitlementChange: a record that
// read the old plan and a plan change overlap; the change waits for the
// record, so the rollup ends with the new allowance.
func TestConcurrentSettlementCannotOverwriteAnEntitlementChange(t *testing.T) {
	s, _, now := newTestStore(t)
	ctx := context.Background()
	_, _, err := s.Register(ctx, Registration{ID: "racer", Deployment: billing.DeploymentHosted, Plan: billing.PlanStarter, BillingCurrency: config.USD}, admin)
	require.NoError(t, err)
	_, err = recordAt(s, *now, "racer", &billing.Tokens{Input: 30_000_000, Output: 30_000_000}) // 6,000 credits
	require.NoError(t, err)

	read, release := make(chan struct{}), make(chan struct{})
	s.afterInstallationRead = func() { close(read); <-release }
	recorded := make(chan error, 1)
	go func() {
		_, err := recordAt(s, *now, "racer", &billing.Tokens{Input: 5_000, Output: 5_000}) // 1 credit, read on Starter terms
		recorded <- err
	}()
	<-read // the record holds the Starter entitlements
	s.afterInstallationRead = nil
	changed := make(chan error, 1)
	go func() {
		_, err := s.SetPlan(ctx, "racer", billing.PlanGrowth, admin)
		changed <- err
	}()
	select {
	case err := <-changed:
		close(release)
		<-recorded
		t.Fatalf("the plan change finished while a record held the old entitlements (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-recorded)
	require.NoError(t, <-changed)

	rows, err := s.Export(ctx, *now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "20000.000000", rows[0].AllowanceCredits, "the export carries the Growth allowance")
	assert.Equal(t, "0.000000", rows[0].OverageCredits)
	assert.Equal(t, "6001.000000", rows[0].ChargeableCredits)
}

// TestTrialLocksOneRateAtItsStart: a trial spanning a month boundary uses the
// latest observation before the trial's start for every request, in preflight
// and settlement alike (DES-HOR-661-04/-05).
func TestTrialLocksOneRateAtItsStart(t *testing.T) {
	s, _, now := newTestStore(t)
	ctx := context.Background()
	*now = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	for date, rate := range map[string]string{"2026-09-30": "1.1000", "2026-10-27": "1.2500", "2026-10-31": "1.3000"} {
		d, _ := time.Parse(time.DateOnly, date)
		require.NoError(t, s.RecordObservation(ctx, d, billing.MustDec(rate)))
	}
	_, _, err := s.Register(ctx, Registration{ID: "eur-trial", Deployment: billing.DeploymentHosted, Plan: billing.PlanTrial, BillingCurrency: config.EUR, TrialDays: days(7)}, admin)
	require.NoError(t, err)
	usage := &billing.Tokens{Input: 1_000_000, Output: 1_000_000} // 2.00 USD
	october, err := recordAt(s, time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), "eur-trial", usage)
	require.NoError(t, err)
	november, err := recordAt(s, time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), "eur-trial", usage)
	require.NoError(t, err)
	assert.Equal(t, "160.000000", october.Credits, "2.00 USD / 1.25 (the rate of 27 October)")
	assert.Equal(t, october.Credits, november.Credits, "one rate across 1 November")
	var versions int
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT count(DISTINCT fx_version) FROM central.usage_records WHERE installation_id = 'eur-trial'`).Scan(&versions))
	assert.Equal(t, 1, versions)
	preflight, err := s.LockedRate(ctx, *now)
	require.NoError(t, err)
	assert.Equal(t, "1.2500", billing.Format(preflight.Rate, 4), "preflight keyed by the period start finds the same lock")
}

// TestFreshDatabaseLocksTheCurrentMonthFromHistory: observations recorded
// from the 90-day feed mid-month make the current month's rate available.
func TestFreshDatabaseLocksTheCurrentMonthFromHistory(t *testing.T) {
	s, _, now := newTestStore(t)
	ctx := context.Background()
	history := `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref"><Cube>
<Cube time='2026-10-13'><Cube currency='USD' rate='1.1403'/></Cube>
<Cube time='2026-09-30'><Cube currency='USD' rate='1.1350'/></Cube>
</Cube></gesmes:Envelope>`
	obs, err := fx.Parse(strings.NewReader(history))
	require.NoError(t, err)
	for _, o := range obs {
		require.NoError(t, s.RecordObservation(ctx, o.Date, o.USDPerEUR))
	}
	rate, err := s.LockedRate(ctx, billing.MonthStart(*now))
	require.NoError(t, err)
	assert.Equal(t, "1.1350", billing.Format(rate.Rate, 4), "the last observation before 1 October")
}

func recordAt(s *Store, at time.Time, inst string, usage *billing.Tokens) (Metered, error) {
	return s.Record(context.Background(), UsageRecord{
		RequestID: "req", InstallationID: inst, Model: "managed-model", Price: usdModel, PriceVersion: "pv1",
		Disposition: "completed", Usage: usage, OccurredAt: at,
		Attribution: attribution.Attribution{IdentityID: "id-1", RunID: "run-1", Channel: "chat"},
	})
}

func TestRecord_MeteringFXAlertsAndExport(t *testing.T) {
	s, _, now := newTestStore(t)
	ctx := context.Background()
	_, _, err := s.Register(ctx, Registration{ID: "usd-co", Deployment: billing.DeploymentHosted, Plan: billing.PlanStarter, BillingCurrency: config.USD}, admin)
	require.NoError(t, err)
	_, _, err = s.Register(ctx, Registration{ID: "eur-co", Deployment: billing.DeploymentHosted, Plan: billing.PlanStarter, BillingCurrency: config.EUR}, admin)
	require.NoError(t, err)
	_, _, err = s.Register(ctx, Registration{ID: "pr-1", Deployment: billing.DeploymentInternal, Plan: billing.PlanNone, BillingCurrency: config.EUR}, admin)
	require.NoError(t, err)

	// 1M input + 1M output at 1.00 = 2.00 USD = 200 credits for a USD installation.
	big := &billing.Tokens{Input: 1_000_000, Output: 1_000_000}

	t.Run("USD on a USD price needs no FX", func(t *testing.T) {
		m, err := recordAt(s, *now, "usd-co", big)
		require.NoError(t, err)
		assert.Equal(t, billing.Chargeable, m.Classification)
		assert.Equal(t, "200.000000", m.Credits)
	})

	t.Run("EUR on a USD price fails without a locked rate, then uses it", func(t *testing.T) {
		_, err := recordAt(s, *now, "eur-co", big)
		assert.ErrorIs(t, err, ErrNoRate)
		_, err = s.LockedRate(ctx, billing.MonthStart(*now))
		assert.ErrorIs(t, err, ErrNoRate)

		require.NoError(t, s.RecordObservation(ctx, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), billing.MustDec("1.2000")), "dated in the month: not usable for it")
		require.NoError(t, s.RecordObservation(ctx, time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC), billing.MustDec("1.1000")))
		require.NoError(t, s.RecordObservation(ctx, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), billing.MustDec("1.1403")))
		rate, err := s.LockedRate(ctx, billing.MonthStart(*now))
		require.NoError(t, err)
		assert.Equal(t, "1.1403", billing.Format(rate.Rate, 4), "the latest observation before the month")
		require.NoError(t, s.RecordObservation(ctx, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), billing.MustDec("9")))
		again, err := s.LockedRate(ctx, billing.MonthStart(*now))
		require.NoError(t, err)
		assert.Equal(t, rate.Version, again.Version, "a locked rate never changes")

		m, err := recordAt(s, *now, "eur-co", big)
		require.NoError(t, err)
		assert.Equal(t, "175.392441", m.Credits, "2.00 USD / 1.1403 = 1.753924 EUR")
		var fxVersion int64
		require.NoError(t, s.pool.QueryRow(ctx, `SELECT fx_version FROM central.usage_records WHERE id = $1`, m.RecordID).Scan(&fxVersion))
		assert.Equal(t, rate.Version, fxVersion, "the record keeps its rate version")
	})

	t.Run("internal usage is company-funded and no usage is not charged", func(t *testing.T) {
		m, err := recordAt(s, *now, "pr-1", big)
		require.NoError(t, err)
		assert.Equal(t, billing.CompanyFunded, m.Classification)
		assert.Empty(t, m.NewAlerts, "internal installations never alert")
		m, err = recordAt(s, *now, "usd-co", nil)
		require.NoError(t, err)
		assert.Equal(t, billing.NotCharged, m.Classification)
		assert.Equal(t, "0.000000", m.Credits)
	})

	t.Run("alerts fire once per threshold and period", func(t *testing.T) {
		// usd-co is at 200 of 5,000. 19 more requests: 4,000 (80%).
		var fired []int
		for i := 0; i < 24; i++ {
			m, err := recordAt(s, *now, "usd-co", big)
			require.NoError(t, err)
			for _, a := range m.NewAlerts {
				fired = append(fired, a.Threshold)
			}
		}
		assert.Equal(t, []int{80, 100}, fired, "each threshold fires exactly once")
		used, err := s.UsedCredits(ctx, "usd-co", billing.MonthStart(*now))
		require.NoError(t, err)
		assert.Equal(t, "5000.000000", billing.Format(used, 6))
		alerts, err := s.Alerts(ctx, "usd-co", billing.MonthStart(*now))
		require.NoError(t, err)
		assert.Len(t, alerts, 2)

		pending, err := s.PendingAlerts(ctx)
		require.NoError(t, err)
		require.Len(t, pending, 2)
		require.NoError(t, s.MarkAlertNotified(ctx, pending[0]))
		pending, err = s.PendingAlerts(ctx)
		require.NoError(t, err)
		assert.Len(t, pending, 1)

		// Next month starts a new period: the alerts can fire again.
		next := time.Date(2026, 11, 1, 0, 0, 1, 0, time.UTC)
		require.NoError(t, s.RecordObservation(ctx, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), billing.MustDec("1.15")))
		for i := 0; i < 25; i++ {
			_, err := recordAt(s, next, "usd-co", big)
			require.NoError(t, err)
		}
		alerts, err = s.Alerts(ctx, "usd-co", billing.MonthStart(next))
		require.NoError(t, err)
		assert.Len(t, alerts, 2)
	})

	t.Run("adjustment moves credits to company-funded", func(t *testing.T) {
		m, err := recordAt(s, *now, "eur-co", big)
		require.NoError(t, err)
		require.NoError(t, s.Adjust(ctx, m.RecordID, "provider double-charge, incident 12", admin))
		require.NoError(t, s.Adjust(ctx, m.RecordID, "again", admin), "idempotent")
		used, err := s.UsedCredits(ctx, "eur-co", billing.MonthStart(*now))
		require.NoError(t, err)
		assert.Equal(t, "175.392441", billing.Format(used, 6), "only the first eur-co request stays chargeable")
	})

	t.Run("export reconciles with the usage records", func(t *testing.T) {
		rows, err := s.Export(ctx, *now)
		require.NoError(t, err)
		byID := map[string]ExportRow{}
		for _, r := range rows {
			byID[r.InstallationID] = r
		}
		require.Len(t, byID, 3)
		assert.Equal(t, "5000.000000", byID["usd-co"].ChargeableCredits)
		assert.Equal(t, "0.000000", byID["usd-co"].OverageCredits)
		assert.EqualValues(t, 26, byID["usd-co"].Requests)
		assert.EqualValues(t, 1, byID["usd-co"].NotChargedRequests)
		assert.Equal(t, "175.392441", byID["eur-co"].CompanyFundedCredits)
		assert.Equal(t, "175.392441", byID["pr-1"].CompanyFundedCredits, "internal EUR installation on a USD price")

		november, err := s.Export(ctx, time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		require.Len(t, november, 1)
		assert.Equal(t, "5000.000000", november[0].ChargeableCredits)

		// Tamper with a rollup: the export refuses to produce figures.
		_, err = s.pool.Exec(ctx, `UPDATE central.period_usage SET chargeable_credits = chargeable_credits + 1 WHERE installation_id = 'usd-co'`)
		require.NoError(t, err)
		_, err = s.Export(ctx, *now)
		assert.ErrorIs(t, err, ErrUnreconciled)
	})
}

func TestRecord_TrialPeriodSpansMonths(t *testing.T) {
	s, _, now := newTestStore(t)
	ctx := context.Background()
	*now = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	_, _, err := s.Register(ctx, Registration{ID: "trial-co", Deployment: billing.DeploymentHosted, Plan: billing.PlanTrial, BillingCurrency: config.USD, TrialDays: days(7)}, admin)
	require.NoError(t, err)
	small := &billing.Tokens{Input: 1000, Output: 1000}
	a, err := recordAt(s, *now, "trial-co", small)
	require.NoError(t, err)
	b, err := recordAt(s, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), "trial-co", small)
	require.NoError(t, err)
	var starts []time.Time
	rows, err := s.pool.Query(ctx, `SELECT period_start FROM central.usage_records WHERE id = ANY($1)`, []any{a.RecordID, b.RecordID})
	require.NoError(t, err)
	starts, err = pgx.CollectRows(rows, pgx.RowTo[time.Time])
	require.NoError(t, err)
	require.Len(t, starts, 2)
	assert.True(t, starts[0].Equal(starts[1]), "one trial period across the month boundary")
}

func TestStore_NoPayloadColumns(t *testing.T) {
	s, _, _ := newTestStore(t)
	rows, err := s.pool.Query(context.Background(), `
		SELECT table_name || '.' || column_name FROM information_schema.columns WHERE table_schema = 'central'`)
	require.NoError(t, err)
	columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	for _, c := range columns {
		for _, forbidden := range []string{"prompt", "message", "content", "body", "response", "completion_text"} {
			assert.NotContains(t, c, forbidden, "central stores no payload")
		}
	}
}

func TestErrorsAreDistinct(t *testing.T) {
	assert.False(t, errors.Is(ErrNotFound, ErrConflict))
}
