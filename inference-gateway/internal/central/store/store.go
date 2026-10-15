// Package store is the central gateway's Postgres store (schema central):
// the installation registry, certificates, audit, FX, usage records, period
// rollups, alerts and rate counters. Metering writes are one transaction per
// request (DES-HOR-661-04/08).
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/billing"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/ratelimit"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationLockKey is the advisory lock serialising migrations across replicas.
const migrationLockKey int64 = 6610661

// RateTable is central's rate-limit counter table (DES-HOR-661-07).
var RateTable = ratelimit.Table{Name: "central.rate_counters", KeyColumn: "scope"}

// Errors.
var (
	ErrNotFound = errors.New("store: not found")
	ErrConflict = errors.New("store: conflicts with the existing entry")
	ErrRevoked  = errors.New("store: installation is revoked")
	ErrInvalid  = errors.New("store: invalid for the installation's state")
	ErrNoRate   = errors.New("store: no FX rate can be locked for the month")
)

// Migrate applies the embedded migrations under an advisory lock, so
// concurrently starting replicas cannot race.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("store: migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS central;
		CREATE TABLE IF NOT EXISTS central.schema_migrations (
			version    text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("store: migration table: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var applied bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM central.schema_migrations WHERE version = $1)`, name).Scan(&applied); err != nil {
			return fmt.Errorf("store: read migrations: %w", err)
		}
		if applied {
			continue
		}
		sql, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO central.schema_migrations (version) VALUES ($1)`, name)
			return err
		})
		if err != nil {
			return fmt.Errorf("store: apply %s: %w", name, err)
		}
	}
	return nil
}

// Store is the central store.
type Store struct {
	pool   *pgxpool.Pool
	policy billing.Policy
	now    func() time.Time

	// afterInstallationRead runs inside Record after it reads the
	// installation (tests use it to overlap settlement with a change).
	afterInstallationRead func()
}

// New creates a store.
func New(pool *pgxpool.Pool, policy billing.Policy) *Store {
	return &Store{pool: pool, policy: policy, now: time.Now}
}

// SetClock replaces the store's clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Pool is the underlying pool.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ping checks the database.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Actor is the operator a change is attributed to.
type Actor struct {
	ARN     string
	Session string
}

func audit(ctx context.Context, tx pgx.Tx, actor Actor, installationID, action string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO central.audit_events (actor_arn, actor_session, installation_id, action, detail)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5)`, actor.ARN, actor.Session, installationID, action, detail)
	if err != nil {
		return fmt.Errorf("store: audit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// CA
// ---------------------------------------------------------------------------

// CA returns the CA certificate PEM and its KMS key, or ErrNotFound.
func (s *Store) CA(ctx context.Context) (certPEM, kmsKeyID string, err error) {
	err = s.pool.QueryRow(ctx, `SELECT certificate_pem, kms_key_id FROM central.ca`).Scan(&certPEM, &kmsKeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return certPEM, kmsKeyID, err
}

// InsertCA stores the CA certificate once; an existing CA is kept.
func (s *Store) InsertCA(ctx context.Context, certPEM, kmsKeyID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO central.ca (certificate_pem, kms_key_id) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, certPEM, kmsKeyID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

const installationColumns = `id, deployment, plan, billing_currency, registered_trial_days, trial_started_at,
	trial_ends_at, trial_credits::text, allowance_credits::text, status, entitlements_version`

func scanInstallation(row pgx.Row) (billing.Installation, error) {
	var (
		inst                    billing.Installation
		trialCredits, allowance *string
	)
	err := row.Scan(&inst.ID, &inst.Deployment, &inst.Plan, &inst.BillingCurrency, &inst.TrialDays,
		&inst.TrialStartedAt, &inst.TrialEndsAt, &trialCredits, &allowance, &inst.Status, &inst.EntitlementsVersion)
	if err != nil {
		return inst, err
	}
	if trialCredits != nil {
		inst.TrialCredits = billing.MustDec(*trialCredits)
	}
	if allowance != nil {
		inst.AllowanceCredits = billing.MustDec(*allowance)
	}
	return inst, nil
}

// Certificate is a certificate's registry state.
type Certificate struct {
	Serial         string     `json:"serial"`
	InstallationID string     `json:"-"`
	Fingerprint    string     `json:"fingerprint_sha256"`
	NotBefore      time.Time  `json:"not_before"`
	NotAfter       time.Time  `json:"not_after"`
	IssuedAt       time.Time  `json:"issued_at"`
	RevokedAt      *time.Time `json:"revoked_at"`
}

const certificateColumns = `serial, installation_id, fingerprint_sha256, not_before, not_after, issued_at, revoked_at`

func scanCertificate(row pgx.Row) (Certificate, error) {
	var c Certificate
	err := row.Scan(&c.Serial, &c.InstallationID, &c.Fingerprint, &c.NotBefore, &c.NotAfter, &c.IssuedAt, &c.RevokedAt)
	return c, err
}

// LoadRegistry reads every installation and certificate (the in-memory
// registry is small and reloaded whole).
func (s *Store) LoadRegistry(ctx context.Context) ([]billing.Installation, []Certificate, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+installationColumns+` FROM central.installations`)
	if err != nil {
		return nil, nil, err
	}
	installations, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (billing.Installation, error) { return scanInstallation(r) })
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT `+certificateColumns+` FROM central.certificates`)
	if err != nil {
		return nil, nil, err
	}
	certs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Certificate, error) { return scanCertificate(r) })
	return installations, certs, err
}

// Installation reads one installation and its certificates.
func (s *Store) Installation(ctx context.Context, id string) (billing.Installation, []Certificate, error) {
	inst, err := scanInstallation(s.pool.QueryRow(ctx, `SELECT `+installationColumns+` FROM central.installations WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return inst, nil, ErrNotFound
	}
	if err != nil {
		return inst, nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+certificateColumns+` FROM central.certificates WHERE installation_id = $1 ORDER BY issued_at`, id)
	if err != nil {
		return inst, nil, err
	}
	certs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Certificate, error) { return scanCertificate(r) })
	return inst, certs, err
}

// Registration is an operator's registration request (already validated).
type Registration struct {
	ID               string
	Deployment       string
	Plan             string
	BillingCurrency  string
	TrialDays        *int
	AllowanceCredits *big.Rat
}

func sameRegistration(inst billing.Installation, reg Registration) bool {
	sameDays := (inst.TrialDays == nil) == (reg.TrialDays == nil) && (inst.TrialDays == nil || *inst.TrialDays == *reg.TrialDays)
	sameAllowance := (inst.AllowanceCredits == nil) == (reg.AllowanceCredits == nil) &&
		(inst.AllowanceCredits == nil || inst.AllowanceCredits.Cmp(reg.AllowanceCredits) == 0)
	return inst.Deployment == reg.Deployment && inst.Plan == reg.Plan && inst.BillingCurrency == reg.BillingCurrency &&
		sameDays && sameAllowance
}

// Register creates an installation. An identical existing entry is returned
// unchanged (a revoked one is reactivated, its old certificates staying
// revoked); a differing one is ErrConflict.
func (s *Store) Register(ctx context.Context, reg Registration, actor Actor) (inst billing.Installation, created bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		existing, err := scanInstallation(tx.QueryRow(ctx, `SELECT `+installationColumns+` FROM central.installations WHERE id = $1 FOR UPDATE`, reg.ID))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			now := s.now().UTC()
			var started, ends *time.Time
			var trialCredits, allowance *string
			if reg.Plan == billing.PlanTrial && reg.TrialDays != nil {
				end := now.AddDate(0, 0, *reg.TrialDays)
				started, ends = &now, &end
				c := billing.Format(s.policy.TrialCredits(*reg.TrialDays), billing.CreditPlaces)
				trialCredits = &c
			}
			if reg.AllowanceCredits != nil {
				a := billing.Format(reg.AllowanceCredits, billing.CreditPlaces)
				allowance = &a
			}
			inst, err = scanInstallation(tx.QueryRow(ctx, `
				INSERT INTO central.installations (id, deployment, plan, billing_currency, registered_trial_days,
					trial_started_at, trial_ends_at, trial_credits, allowance_credits)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9::numeric)
				RETURNING `+installationColumns,
				reg.ID, reg.Deployment, reg.Plan, reg.BillingCurrency, reg.TrialDays, started, ends, trialCredits, allowance))
			if err != nil {
				return err
			}
			created = true
			return audit(ctx, tx, actor, reg.ID, "register", map[string]any{
				"deployment": reg.Deployment, "plan": reg.Plan, "billing_currency": reg.BillingCurrency,
				"trial_days": reg.TrialDays, "allowance_credits": allowance,
			})
		case err != nil:
			return err
		}
		if !sameRegistration(existing, reg) {
			return ErrConflict
		}
		inst = existing
		if existing.Status != "revoked" {
			return nil
		}
		inst, err = scanInstallation(tx.QueryRow(ctx, `
			UPDATE central.installations SET status = 'active', revoked_at = NULL,
				entitlements_version = entitlements_version + 1, updated_at = now()
			WHERE id = $1 RETURNING `+installationColumns, reg.ID))
		if err != nil {
			return err
		}
		return audit(ctx, tx, actor, reg.ID, "reactivate", nil)
	})
	return inst, created, err
}

// IssuedCertificate is a certificate to record.
type IssuedCertificate struct {
	Serial      string
	Fingerprint string
	NotBefore   time.Time
	NotAfter    time.Time
}

// RecordCertificate records an issued certificate for an active installation.
// issue runs inside the transaction before the insert, so an installation
// revoked concurrently is never issued a certificate.
func (s *Store) RecordCertificate(ctx context.Context, installationID string, issue func() (IssuedCertificate, error), actor Actor) (IssuedCertificate, error) {
	var out IssuedCertificate
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM central.installations WHERE id = $1 FOR SHARE`, installationID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != "active" {
			return ErrRevoked
		}
		out, err = issue()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO central.certificates (serial, installation_id, fingerprint_sha256, not_before, not_after)
			VALUES ($1, $2, $3, $4, $5)`, out.Serial, installationID, out.Fingerprint, out.NotBefore, out.NotAfter); err != nil {
			return err
		}
		return audit(ctx, tx, actor, installationID, "issue_certificate", map[string]any{
			"serial": out.Serial, "not_after": out.NotAfter,
		})
	})
	return out, err
}

// Revoke revokes an installation and all its certificates. Idempotent.
func (s *Store) Revoke(ctx context.Context, id, reason string, actor Actor) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM central.installations WHERE id = $1 FOR UPDATE`, id).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		certs, err := tx.Exec(ctx, `UPDATE central.certificates SET revoked_at = now() WHERE installation_id = $1 AND revoked_at IS NULL`, id)
		if err != nil {
			return err
		}
		if status == "revoked" && certs.RowsAffected() == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE central.installations SET status = 'revoked', revoked_at = COALESCE(revoked_at, now()),
				entitlements_version = entitlements_version + 1, updated_at = now()
			WHERE id = $1`, id); err != nil {
			return err
		}
		return audit(ctx, tx, actor, id, "revoke", map[string]any{"reason": reason, "certificates": certs.RowsAffected()})
	})
}

// SetPlan converts or changes a hosted installation's paid plan.
func (s *Store) SetPlan(ctx context.Context, id, plan string, actor Actor) (billing.Installation, error) {
	var inst billing.Installation
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		existing, err := scanInstallation(tx.QueryRow(ctx, `SELECT `+installationColumns+` FROM central.installations WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if existing.Deployment != billing.DeploymentHosted {
			return fmt.Errorf("%w: only hosted installations have a plan", ErrInvalid)
		}
		inst = existing
		if existing.Plan == plan {
			return nil
		}
		inst, err = scanInstallation(tx.QueryRow(ctx, `
			UPDATE central.installations SET plan = $2, entitlements_version = entitlements_version + 1, updated_at = now()
			WHERE id = $1 RETURNING `+installationColumns, id, plan))
		if err != nil {
			return err
		}
		if err := s.refreshPeriod(ctx, tx, inst); err != nil {
			return err
		}
		return audit(ctx, tx, actor, id, "set_plan", map[string]any{"from": existing.Plan, "to": plan})
	})
	return inst, err
}

// refreshPeriod rewrites the current period's rollup allowance and end from
// changed entitlements, in the same transaction as the change, so the
// invoicing export always agrees with the installation read (DES-HOR-661-05).
// A period with no usage yet has no row; the first metered request creates it.
func (s *Store) refreshPeriod(ctx context.Context, tx pgx.Tx, inst billing.Installation) error {
	period := s.policy.PeriodAt(inst, s.now())
	_, err := tx.Exec(ctx, `
		UPDATE central.period_usage SET allowance_credits = $3::numeric, period_end = $4, updated_at = now()
		WHERE installation_id = $1 AND period_start = $2`,
		inst.ID, period.Start, billing.Format(period.Allowance, billing.CreditPlaces), period.End)
	if err != nil {
		return fmt.Errorf("store: refresh period rollup: %w", err)
	}
	return nil
}

// ExtendTrial moves a trial's end and adds the pro-rated credits for days.
func (s *Store) ExtendTrial(ctx context.Context, id string, days int, actor Actor) (billing.Installation, error) {
	var inst billing.Installation
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		existing, err := scanInstallation(tx.QueryRow(ctx, `SELECT `+installationColumns+` FROM central.installations WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if existing.Plan != billing.PlanTrial {
			return fmt.Errorf("%w: the installation is not on a trial", ErrInvalid)
		}
		added := billing.Format(s.policy.TrialCredits(days), billing.CreditPlaces)
		inst, err = scanInstallation(tx.QueryRow(ctx, `
			UPDATE central.installations SET trial_ends_at = trial_ends_at + make_interval(days => $2),
				trial_credits = trial_credits + $3::numeric,
				entitlements_version = entitlements_version + 1, updated_at = now()
			WHERE id = $1 RETURNING `+installationColumns, id, days, added))
		if err != nil {
			return err
		}
		if err := s.refreshPeriod(ctx, tx, inst); err != nil {
			return err
		}
		return audit(ctx, tx, actor, id, "extend_trial", map[string]any{"days": days, "credits_added": added, "trial_ends_at": inst.TrialEndsAt})
	})
	return inst, err
}
