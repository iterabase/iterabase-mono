package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// windowSize is the fixed rate-limit window (DES-HOR-661-07).
const windowSize = time.Minute

// retainWindows is how long old windows are kept before Prune deletes them.
const retainWindows = 10 * time.Minute

// Table names a counters table: (key, window_start, requests, tokens) with a
// primary key on (key, window_start).
type Table struct {
	Name      string // schema-qualified, e.g. ratelimit.counters
	KeyColumn string // e.g. identity_id or scope
}

// Installation is the control-plane table the installation gateway uses
// (created by the control-plane migrations; the gateway role has DML on it).
var Installation = Table{Name: "ratelimit.counters", KeyColumn: "identity_id"}

var identPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*(\.[a-z_][a-z0-9_]*)?$`)

// PostgresLimiter implements Limiter with exact one-minute window counters in
// Postgres, shared by every replica (DES-HOR-661-07). It replaces Redis.
type PostgresLimiter struct {
	pool  *pgxpool.Pool
	table Table
	now   func() time.Time
}

// NewPostgresLimiter creates a limiter on table. It panics on an invalid
// table identifier, which is a programming error.
func NewPostgresLimiter(pool *pgxpool.Pool, table Table) *PostgresLimiter {
	if !identPattern.MatchString(table.Name) || !identPattern.MatchString(table.KeyColumn) {
		panic(fmt.Sprintf("ratelimit: invalid table %q / column %q", table.Name, table.KeyColumn))
	}
	return &PostgresLimiter{pool: pool, table: table, now: time.Now}
}

// SetClock replaces the limiter's clock (tests; the central gateway uses its
// own clock for both the check and the usage write).
func (l *PostgresLimiter) SetClock(now func() time.Time) { l.now = now }

// window returns the current window start and its reset time.
func (l *PostgresLimiter) window() (time.Time, time.Time) {
	start := l.now().UTC().Truncate(windowSize)
	return start, start.Add(windowSize)
}

// CheckRPM increments the window's request count when it is under limit.
// limit <= 0 means unlimited (the request is still counted).
func (l *PostgresLimiter) CheckRPM(ctx context.Context, keyID string, limit int) (*Result, error) {
	start, reset := l.window()
	if limit <= 0 {
		_, err := l.pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %[1]s (%[2]s, window_start, requests, tokens) VALUES ($1, $2, 1, 0)
			ON CONFLICT (%[2]s, window_start) DO UPDATE SET requests = %[1]s.requests + 1`,
			l.table.Name, l.table.KeyColumn), keyID, start)
		if err != nil {
			return nil, fmt.Errorf("ratelimit: count request: %w", err)
		}
		return &Result{Allowed: true, Limit: limit, ResetAt: reset}, nil
	}
	var requests int
	err := l.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %[1]s (%[2]s, window_start, requests, tokens) VALUES ($1, $2, 1, 0)
		ON CONFLICT (%[2]s, window_start) DO UPDATE SET requests = %[1]s.requests + 1
		WHERE %[1]s.requests < $3
		RETURNING requests`, l.table.Name, l.table.KeyColumn), keyID, start, limit).Scan(&requests)
	if errors.Is(err, pgx.ErrNoRows) {
		return &Result{Allowed: false, Limit: limit, Remaining: 0, ResetAt: reset}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ratelimit: check requests: %w", err)
	}
	return &Result{Allowed: true, Limit: limit, Remaining: max(limit-requests, 0), ResetAt: reset}, nil
}

// Check is one scope of an atomic admission: its request and token limits for
// the current window (<= 0 means unlimited).
type Check struct {
	Key string
	RPM int
	TPM int
}

// Admit admits one request against every check in a single transaction: each
// scope's tokens must be under its TPM limit and its request count under its
// RPM limit, and only then are all the request counts incremented. A request
// refused by any scope consumes no other scope's quota, so one installation
// above its own limit cannot exhaust a shared provider-wide limit
// (DES-HOR-661-07). Callers pass checks in a fixed order (provider scope
// first) so concurrent admissions lock rows in the same order.
func (l *PostgresLimiter) Admit(ctx context.Context, checks ...Check) (*Result, error) {
	start, reset := l.window()
	var out *Result
	err := pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		for _, c := range checks {
			if c.TPM <= 0 {
				continue
			}
			var tokens int64
			err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT tokens FROM %s WHERE %s = $1 AND window_start = $2`,
				l.table.Name, l.table.KeyColumn), c.Key, start).Scan(&tokens)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("ratelimit: check tokens: %w", err)
			}
			if tokens >= int64(c.TPM) {
				out = &Result{Allowed: false, Limit: c.TPM, ResetAt: reset}
				return errRefused
			}
		}
		remaining := -1
		for _, c := range checks {
			limit := c.RPM
			if limit <= 0 {
				limit = math.MaxInt32
			}
			var requests int
			err := tx.QueryRow(ctx, fmt.Sprintf(`
				INSERT INTO %[1]s (%[2]s, window_start, requests, tokens) VALUES ($1, $2, 1, 0)
				ON CONFLICT (%[2]s, window_start) DO UPDATE SET requests = %[1]s.requests + 1
				WHERE %[1]s.requests < $3
				RETURNING requests`, l.table.Name, l.table.KeyColumn), c.Key, start, limit).Scan(&requests)
			if errors.Is(err, pgx.ErrNoRows) {
				out = &Result{Allowed: false, Limit: c.RPM, ResetAt: reset}
				return errRefused
			}
			if err != nil {
				return fmt.Errorf("ratelimit: check requests: %w", err)
			}
			if c.RPM > 0 && (remaining < 0 || c.RPM-requests < remaining) {
				remaining = c.RPM - requests
			}
		}
		out = &Result{Allowed: true, Remaining: max(remaining, 0), ResetAt: reset}
		return nil
	})
	if errors.Is(err, errRefused) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// errRefused rolls an admission back; it never leaves Admit.
var errRefused = errors.New("ratelimit: refused")

// CheckTPM reports whether the window's tokens are under limit, without
// counting. limit <= 0 means unlimited.
func (l *PostgresLimiter) CheckTPM(ctx context.Context, keyID string, limit int) (*Result, error) {
	start, reset := l.window()
	if limit <= 0 {
		return &Result{Allowed: true, Limit: limit, ResetAt: reset}, nil
	}
	var tokens int64
	err := l.pool.QueryRow(ctx, fmt.Sprintf(`SELECT tokens FROM %s WHERE %s = $1 AND window_start = $2`,
		l.table.Name, l.table.KeyColumn), keyID, start).Scan(&tokens)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("ratelimit: check tokens: %w", err)
	}
	remaining := int64(limit) - tokens
	return &Result{Allowed: tokens < int64(limit), Limit: limit, Remaining: int(max(remaining, 0)), ResetAt: reset}, nil
}

// IncrementTPM adds tokens to the current window.
func (l *PostgresLimiter) IncrementTPM(ctx context.Context, keyID string, tokens int) error {
	if tokens <= 0 {
		return nil
	}
	return AddTokens(ctx, l.pool, l.table, keyID, l.now(), tokens)
}

// Execer is satisfied by a pgx pool, connection or transaction.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// AddTokens adds tokens to key's window at time at through db — a pool, or a
// transaction so the central gateway counts inside its usage write.
func AddTokens(ctx context.Context, db Execer, table Table, keyID string, at time.Time, tokens int) error {
	if tokens <= 0 {
		return nil
	}
	_, err := db.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %[1]s (%[2]s, window_start, requests, tokens) VALUES ($1, $2, 0, $3)
		ON CONFLICT (%[2]s, window_start) DO UPDATE SET tokens = %[1]s.tokens + EXCLUDED.tokens`,
		table.Name, table.KeyColumn), keyID, at.UTC().Truncate(windowSize), tokens)
	if err != nil {
		return fmt.Errorf("ratelimit: add tokens: %w", err)
	}
	return nil
}

// Prune deletes windows older than ten minutes.
func (l *PostgresLimiter) Prune(ctx context.Context) error {
	_, err := l.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE window_start < $1`, l.table.Name),
		l.now().UTC().Add(-retainWindows))
	if err != nil {
		return fmt.Errorf("ratelimit: prune: %w", err)
	}
	return nil
}

// RunPruner prunes every interval until ctx is done. Errors are passed to
// onErr (rate limiting must not take the gateway down).
func (l *PostgresLimiter) RunPruner(ctx context.Context, interval time.Duration, onErr func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := l.Prune(ctx); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}
