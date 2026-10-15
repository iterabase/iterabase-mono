package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/attribution"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/billing"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/config"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/ratelimit"
)

// ---------------------------------------------------------------------------
// FX (DES-HOR-661-04)
// ---------------------------------------------------------------------------

// Rate is a period's locked rate (USD per 1 EUR).
type Rate struct {
	Version int64
	Rate    *big.Rat
}

// RecordObservation stores an ECB reference observation.
func (s *Store) RecordObservation(ctx context.Context, date time.Time, usdPerEUR *big.Rat) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO central.fx_observations (observation_date, quote, rate) VALUES ($1, 'USD', $2::numeric)
		ON CONFLICT (observation_date, quote) DO NOTHING`, date.Format(time.DateOnly), billing.Format(usdPerEUR, 10))
	return err
}

// LockedRate returns the locked rate of the billing period starting at
// periodStart (a calendar month, or a trial's whole period), locking the latest
// observation dated before the period's start date on first need. Preflight
// and settlement both key by the period, so a trial spanning a month boundary
// uses one rate. ErrNoRate when there is none.
func (s *Store) LockedRate(ctx context.Context, periodStart time.Time) (Rate, error) {
	var r Rate
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		r, err = lockedRate(ctx, tx, periodStart)
		return err
	})
	return r, err
}

// LockDate is the date a period's rate is locked by.
func LockDate(periodStart time.Time) string { return periodStart.UTC().Format(time.DateOnly) }

func lockedRate(ctx context.Context, tx pgx.Tx, periodStart time.Time) (Rate, error) {
	lockDate := LockDate(periodStart)
	read := func() (Rate, error) {
		var (
			r    Rate
			rate string
		)
		err := tx.QueryRow(ctx, `SELECT version, rate::text FROM central.fx_rates WHERE lock_date = $1 AND quote = 'USD'`,
			lockDate).Scan(&r.Version, &rate)
		if err != nil {
			return r, err
		}
		r.Rate = billing.MustDec(rate)
		return r, nil
	}
	r, err := read()
	if !errors.Is(err, pgx.ErrNoRows) {
		return r, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO central.fx_rates (lock_date, quote, rate, observation_date)
		SELECT $1::date, 'USD', rate, observation_date FROM central.fx_observations
		WHERE quote = 'USD' AND observation_date < $1::date
		ORDER BY observation_date DESC LIMIT 1
		ON CONFLICT (lock_date, quote) DO NOTHING`, lockDate); err != nil {
		return r, err
	}
	r, err = read()
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNoRate
	}
	return r, err
}

// ---------------------------------------------------------------------------
// Metering
// ---------------------------------------------------------------------------

// UsageRecord is one request to meter. Usage is nil when the provider
// reported none.
type UsageRecord struct {
	RequestID      string
	InstallationID string
	Model          string
	Price          config.Model
	PriceVersion   string
	Stream         bool
	Disposition    string
	ErrorCode      string
	UpstreamStatus int
	Usage          *billing.Tokens
	OccurredAt     time.Time
	Attribution    attribution.Attribution
}

// Alert is an allowance threshold crossing (DES-HOR-661-06).
type Alert struct {
	InstallationID   string    `json:"installation_id"`
	PeriodStart      time.Time `json:"period_start"`
	Threshold        int       `json:"threshold"`
	CrossedAt        time.Time `json:"crossed_at"`
	UsedCredits      string    `json:"used_credits"`
	AllowanceCredits string    `json:"allowance_credits"`
}

// Metered is what Record wrote.
type Metered struct {
	RecordID       uuid.UUID
	Classification string
	Credits        string
	NewAlerts      []Alert
}

// Record meters one request in one transaction: the usage record, the period
// rollup, the rate counters' tokens and the alert evaluation. An error means
// nothing was recorded (DES-HOR-661-08: the caller must not serve the
// response).
//
//nolint:gocyclo // One transaction with a fixed sequence of steps.
func (s *Store) Record(ctx context.Context, in UsageRecord) (Metered, error) {
	var out Metered
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// FOR SHARE serializes settlement with entitlement changes, which take
		// FOR UPDATE: a plan change or trial extension waits for in-flight
		// records and then refreshes the rollup, and a record that starts after
		// a change reads the new entitlements. Concurrent records do not block
		// each other.
		inst, err := scanInstallation(tx.QueryRow(ctx, `SELECT `+installationColumns+` FROM central.installations WHERE id = $1 FOR SHARE`, in.InstallationID))
		if err != nil {
			return fmt.Errorf("store: record: installation: %w", err)
		}
		if s.afterInstallationRead != nil {
			s.afterInstallationRead()
		}
		period := s.policy.PeriodAt(inst, in.OccurredAt)
		out.Classification = billing.Classify(inst.Deployment, in.Usage != nil)

		amount, credits := new(big.Rat), new(big.Rat)
		var tokens billing.Tokens
		var fxVersion *int64
		var fxRate *string
		if in.Usage != nil {
			tokens = *in.Usage
			amount = billing.SupplierAmount(in.Price, tokens)
			var rate *big.Rat
			if billing.NeedsFX(in.Price.Currency, inst.BillingCurrency) {
				locked, err := lockedRate(ctx, tx, period.Start)
				if err != nil {
					return err
				}
				rate = locked.Rate
				v, r := locked.Version, billing.Format(locked.Rate, 10)
				fxVersion, fxRate = &v, &r
			}
			converted, err := billing.Convert(amount, in.Price.Currency, inst.BillingCurrency, rate)
			if err != nil {
				return err
			}
			credits = billing.Credits(converted)
		}
		out.Credits = billing.Format(credits, billing.CreditPlaces)

		var upstreamStatus *int
		if in.UpstreamStatus != 0 {
			upstreamStatus = &in.UpstreamStatus
		}
		a := in.Attribution
		if err := tx.QueryRow(ctx, `
			INSERT INTO central.usage_records (request_id, installation_id, occurred_at, period_start, model,
				provider_model, stream, disposition, error_code, upstream_status, input_tokens, cached_input_tokens,
				output_tokens, supplier_currency, supplier_amount, price_version, fx_version, fx_rate,
				billing_currency, credits, classification, identity_id, run_id, turn_id, workflow_key,
				work_item_id, channel)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::numeric, $16, $17,
				$18::numeric, $19, $20::numeric, $21, $22, $23, $24, $25, $26, $27)
			RETURNING id`,
			in.RequestID, in.InstallationID, in.OccurredAt, period.Start, in.Model, in.Price.ProviderModel, in.Stream,
			in.Disposition, in.ErrorCode, upstreamStatus, tokens.Input, tokens.CachedInput, tokens.Output,
			in.Price.Currency, billing.Format(amount, 12), in.PriceVersion, fxVersion, fxRate, inst.BillingCurrency,
			out.Credits, out.Classification, a.IdentityID, a.RunID, a.TurnID, a.WorkflowKey, a.WorkItemID, a.Channel,
		).Scan(&out.RecordID); err != nil {
			return fmt.Errorf("store: record usage: %w", err)
		}

		chargeable, companyFunded, notCharged := "0", "0", 0
		switch out.Classification {
		case billing.Chargeable:
			chargeable = out.Credits
		case billing.CompanyFunded:
			companyFunded = out.Credits
		default:
			notCharged = 1
		}
		var used string
		if err := tx.QueryRow(ctx, `
			INSERT INTO central.period_usage AS p (installation_id, period_start, period_end, period_kind,
				allowance_credits, chargeable_credits, company_funded_credits, requests, not_charged_requests)
			VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7::numeric, 1, $8)
			ON CONFLICT (installation_id, period_start) DO UPDATE SET
				period_end = EXCLUDED.period_end,
				allowance_credits = EXCLUDED.allowance_credits,
				chargeable_credits = p.chargeable_credits + EXCLUDED.chargeable_credits,
				company_funded_credits = p.company_funded_credits + EXCLUDED.company_funded_credits,
				requests = p.requests + 1,
				not_charged_requests = p.not_charged_requests + EXCLUDED.not_charged_requests,
				updated_at = now()
			RETURNING chargeable_credits::text`,
			in.InstallationID, period.Start, period.End, period.Kind, billing.Format(period.Allowance, billing.CreditPlaces),
			chargeable, companyFunded, notCharged).Scan(&used); err != nil {
			return fmt.Errorf("store: period rollup: %w", err)
		}

		if total := tokens.Input + tokens.Output; total > 0 {
			for _, scope := range []string{ProviderScope(in.Model), InstallationScope(in.InstallationID)} {
				if err := ratelimit.AddTokens(ctx, tx, RateTable, scope, in.OccurredAt, total); err != nil {
					return err
				}
			}
		}

		if out.Classification != billing.Chargeable {
			return nil
		}
		usedRat := billing.MustDec(used)
		for _, threshold := range billing.CrossedThresholds(usedRat, period.Allowance) {
			alert := Alert{
				InstallationID: in.InstallationID, PeriodStart: period.Start, Threshold: threshold,
				CrossedAt: in.OccurredAt.UTC(), UsedCredits: used,
				AllowanceCredits: billing.Format(period.Allowance, billing.CreditPlaces),
			}
			tag, err := tx.Exec(ctx, `
				INSERT INTO central.alerts (installation_id, period_start, threshold, crossed_at, used_credits, allowance_credits)
				VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric)
				ON CONFLICT DO NOTHING`,
				alert.InstallationID, alert.PeriodStart, alert.Threshold, alert.CrossedAt, alert.UsedCredits, alert.AllowanceCredits)
			if err != nil {
				return fmt.Errorf("store: alert: %w", err)
			}
			if tag.RowsAffected() == 1 {
				out.NewAlerts = append(out.NewAlerts, alert)
			}
		}
		return nil
	})
	return out, err
}

// ProviderScope is the provider-wide rate-limit scope for a model.
func ProviderScope(model string) string { return "provider:" + model }

// InstallationScope is an installation's rate-limit scope.
func InstallationScope(id string) string { return "installation:" + id }

// Adjust reclassifies a chargeable record as company-funded (a verified
// service fault). Idempotent for an already company-funded record.
func (s *Store) Adjust(ctx context.Context, recordID uuid.UUID, reason string, actor Actor) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			installationID, classification, credits string
			periodStart                             time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT installation_id, classification, credits::text, period_start FROM central.usage_records
			WHERE id = $1 FOR UPDATE`, recordID).Scan(&installationID, &classification, &credits, &periodStart)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch classification {
		case billing.CompanyFunded:
			return nil
		case billing.NotCharged:
			return fmt.Errorf("%w: a not-charged record has nothing to adjust", ErrInvalid)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE central.usage_records SET classification = 'company_funded', adjusted_at = now(), adjustment_reason = $2
			WHERE id = $1`, recordID, reason); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE central.period_usage SET chargeable_credits = chargeable_credits - $3::numeric,
				company_funded_credits = company_funded_credits + $3::numeric, updated_at = now()
			WHERE installation_id = $1 AND period_start = $2`, installationID, periodStart, credits); err != nil {
			return err
		}
		return audit(ctx, tx, actor, installationID, "adjust_usage", map[string]any{
			"record_id": recordID.String(), "credits": credits, "reason": reason,
		})
	})
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// UsedCredits is a period's chargeable credits so far.
func (s *Store) UsedCredits(ctx context.Context, installationID string, periodStart time.Time) (*big.Rat, error) {
	var used string
	err := s.pool.QueryRow(ctx, `
		SELECT chargeable_credits::text FROM central.period_usage WHERE installation_id = $1 AND period_start = $2`,
		installationID, periodStart).Scan(&used)
	if errors.Is(err, pgx.ErrNoRows) {
		return new(big.Rat), nil
	}
	if err != nil {
		return nil, err
	}
	return billing.MustDec(used), nil
}

const alertColumns = `installation_id, period_start, threshold, crossed_at, used_credits::text, allowance_credits::text`

func scanAlert(row pgx.Row) (Alert, error) {
	var a Alert
	err := row.Scan(&a.InstallationID, &a.PeriodStart, &a.Threshold, &a.CrossedAt, &a.UsedCredits, &a.AllowanceCredits)
	a.PeriodStart, a.CrossedAt = a.PeriodStart.UTC(), a.CrossedAt.UTC()
	return a, err
}

// Alerts lists a period's alerts.
func (s *Store) Alerts(ctx context.Context, installationID string, periodStart time.Time) ([]Alert, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+alertColumns+` FROM central.alerts
		WHERE installation_id = $1 AND period_start = $2 ORDER BY threshold`, installationID, periodStart)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Alert, error) { return scanAlert(r) })
}

// PendingAlerts lists alerts not yet published to Iterabase.
func (s *Store) PendingAlerts(ctx context.Context) ([]Alert, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+alertColumns+` FROM central.alerts WHERE notified_at IS NULL ORDER BY crossed_at LIMIT 100`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Alert, error) { return scanAlert(r) })
}

// MarkAlertNotified records that an alert was published.
func (s *Store) MarkAlertNotified(ctx context.Context, a Alert) error {
	_, err := s.pool.Exec(ctx, `UPDATE central.alerts SET notified_at = now()
		WHERE installation_id = $1 AND period_start = $2 AND threshold = $3`, a.InstallationID, a.PeriodStart, a.Threshold)
	return err
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

// ExportRow is one installation period in the monthly invoicing export.
// Payload-free: identifiers, plans and amounts only.
type ExportRow struct {
	InstallationID       string    `json:"installation_id"`
	Deployment           string    `json:"deployment"`
	Plan                 string    `json:"plan"`
	BillingCurrency      string    `json:"billing_currency"`
	PeriodKind           string    `json:"period_kind"`
	PeriodStart          time.Time `json:"period_start"`
	PeriodEnd            time.Time `json:"period_end"`
	AllowanceCredits     string    `json:"allowance_credits"`
	ChargeableCredits    string    `json:"chargeable_credits"`
	CompanyFundedCredits string    `json:"company_funded_credits"`
	OverageCredits       string    `json:"overage_credits"`
	Requests             int64     `json:"requests"`
	NotChargedRequests   int64     `json:"not_charged_requests"`
}

// ErrUnreconciled means the period rollups disagree with the usage records.
var ErrUnreconciled = errors.New("store: export does not reconcile with usage records")

// Export lists every installation period that starts in month, and verifies
// that each period's rollup equals the sum of its usage records.
func (s *Store) Export(ctx context.Context, month time.Time) ([]ExportRow, error) {
	start := billing.MonthStart(month)
	end := start.AddDate(0, 1, 0)
	rows, err := s.pool.Query(ctx, `
		SELECT p.installation_id, i.deployment, i.plan, i.billing_currency, p.period_kind, p.period_start,
			p.period_end, p.allowance_credits::text, p.chargeable_credits::text, p.company_funded_credits::text,
			p.requests, p.not_charged_requests,
			(SELECT COALESCE(sum(credits) FILTER (WHERE classification = 'chargeable'), 0) <> p.chargeable_credits
			     OR COALESCE(sum(credits) FILTER (WHERE classification = 'company_funded'), 0) <> p.company_funded_credits
			     OR count(*) <> p.requests
			 FROM central.usage_records u
			 WHERE u.installation_id = p.installation_id AND u.period_start = p.period_start) AS mismatch
		FROM central.period_usage p
		JOIN central.installations i ON i.id = p.installation_id
		WHERE p.period_start >= $1 AND p.period_start < $2
		ORDER BY p.installation_id, p.period_start`, start, end)
	if err != nil {
		return nil, err
	}
	var out []ExportRow
	for rows.Next() {
		var (
			r        ExportRow
			mismatch bool
		)
		if err := rows.Scan(&r.InstallationID, &r.Deployment, &r.Plan, &r.BillingCurrency, &r.PeriodKind,
			&r.PeriodStart, &r.PeriodEnd, &r.AllowanceCredits, &r.ChargeableCredits, &r.CompanyFundedCredits,
			&r.Requests, &r.NotChargedRequests, &mismatch); err != nil {
			rows.Close()
			return nil, err
		}
		if mismatch {
			rows.Close()
			return nil, fmt.Errorf("%w: %s period %s", ErrUnreconciled, r.InstallationID, r.PeriodStart.Format(time.RFC3339))
		}
		r.PeriodStart, r.PeriodEnd = r.PeriodStart.UTC(), r.PeriodEnd.UTC()
		r.OverageCredits = billing.Format(billing.Overage(billing.MustDec(r.ChargeableCredits), billing.MustDec(r.AllowanceCredits)), billing.CreditPlaces)
		out = append(out, r)
	}
	return out, rows.Err()
}
