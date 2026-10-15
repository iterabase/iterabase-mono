// Package billing is the central gateway's credit policy as pure functions:
// supplier amounts, FX conversion, credits, classification, periods,
// allowances and alert thresholds (DES-HOR-661-04/05/06; PRD REQ-048).
// Amounts are exact rationals (math/big.Rat), never floats.
package billing

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/config"
)

// Deployments.
const (
	DeploymentHosted            = "hosted"
	DeploymentSelfHostedManaged = "self_hosted_managed"
	DeploymentInternal          = "internal"
)

// Plans.
const (
	PlanTrial   = "trial"
	PlanStarter = "starter"
	PlanGrowth  = "growth"
	PlanScale   = "scale"
	PlanNone    = "none"
)

// Classifications.
const (
	Chargeable    = "chargeable"
	CompanyFunded = "company_funded"
	NotCharged    = "not_charged"
)

// Period kinds.
const (
	PeriodMonth = "month"
	PeriodTrial = "trial"
)

// CreditPlaces is the precision credits are kept at.
const CreditPlaces = 6

// creditUnit is one credit: 0.01 of the installation's billing currency.
var creditUnit = big.NewRat(1, 100)

// AlertThresholds are the allowance percentages that alert (DES-HOR-661-06).
var AlertThresholds = []int{80, 100}

// Installation is the registry state the policy needs.
type Installation struct {
	ID                  string
	Deployment          string
	Plan                string
	BillingCurrency     string
	TrialDays           *int // as registered
	TrialStartedAt      *time.Time
	TrialEndsAt         *time.Time
	TrialCredits        *big.Rat
	AllowanceCredits    *big.Rat // self_hosted_managed only
	Status              string
	EntitlementsVersion int64
}

// Tokens is the provider's token accounting for one request.
type Tokens struct {
	Input       int // prompt tokens, including cached ones
	CachedInput int
	Output      int
}

// Dec parses a decimal string.
func Dec(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("billing: invalid decimal %q", s)
	}
	return r, nil
}

// MustDec parses a decimal string known to be valid (validated config).
func MustDec(s string) *big.Rat {
	r, err := Dec(s)
	if err != nil {
		panic(err)
	}
	return r
}

// Format renders r at places decimal places, rounding half away from zero.
func Format(r *big.Rat, places int) string {
	if r == nil {
		return new(big.Rat).FloatString(places)
	}
	return r.FloatString(places)
}

// Round rounds r to places decimal places (half away from zero).
func Round(r *big.Rat, places int) *big.Rat {
	return MustDec(Format(r, places))
}

// SupplierAmount is (uncached input × input + cached input × cached input +
// output × output) / 1e6 in the model's currency.
func SupplierAmount(m config.Model, t Tokens) *big.Rat {
	cached := min(max(t.CachedInput, 0), max(t.Input, 0))
	uncached := max(t.Input, 0) - cached
	sum := new(big.Rat)
	sum.Add(sum, mulInt(MustDec(m.InputPerMillion), uncached))
	sum.Add(sum, mulInt(MustDec(m.CachedInputPerMillion), cached))
	sum.Add(sum, mulInt(MustDec(m.OutputPerMillion), max(t.Output, 0)))
	return sum.Quo(sum, big.NewRat(1_000_000, 1))
}

func mulInt(r *big.Rat, n int) *big.Rat {
	return new(big.Rat).Mul(r, new(big.Rat).SetInt64(int64(n)))
}

// ErrNoRate means a conversion needs an FX rate and none was supplied.
var ErrNoRate = errors.New("billing: FX rate required")

// NeedsFX reports whether a supplier currency differs from the billing one.
func NeedsFX(supplier, billing string) bool { return supplier != billing }

// Convert converts amount from the supplier currency to the billing currency.
// usdPerEUR is the ECB reference rate (USD per 1 EUR); it is ignored when the
// currencies match.
func Convert(amount *big.Rat, supplier, billing string, usdPerEUR *big.Rat) (*big.Rat, error) {
	if !NeedsFX(supplier, billing) {
		return new(big.Rat).Set(amount), nil
	}
	if usdPerEUR == nil || usdPerEUR.Sign() <= 0 {
		return nil, ErrNoRate
	}
	switch {
	case supplier == config.USD && billing == config.EUR:
		return new(big.Rat).Quo(amount, usdPerEUR), nil
	case supplier == config.EUR && billing == config.USD:
		return new(big.Rat).Mul(amount, usdPerEUR), nil
	}
	return nil, fmt.Errorf("billing: unsupported conversion %s -> %s", supplier, billing)
}

// Credits is converted / 0.01, kept at six decimal places.
func Credits(converted *big.Rat) *big.Rat {
	return Round(new(big.Rat).Quo(converted, creditUnit), CreditPlaces)
}

// Classify assigns a request's classification: internal installations are
// company-funded, requests without provider usage are not charged, and
// everything else is chargeable (authorised consumption, not success-only).
func Classify(deployment string, usageReported bool) string {
	switch {
	case !usageReported:
		return NotCharged
	case deployment == DeploymentInternal:
		return CompanyFunded
	default:
		return Chargeable
	}
}

// Policy is the configured tier and trial policy.
type Policy struct {
	Tiers map[string]config.Tier
	Trial config.Trial
}

// NewPolicy builds the policy from the configuration document.
func NewPolicy(f config.File) Policy { return Policy{Tiers: f.Tiers, Trial: f.Trial} }

// TrialCredits is tier.monthly_credits × days / days_basis.
func (p Policy) TrialCredits(days int) *big.Rat {
	monthly := int64(p.Tiers[p.Trial.Tier].MonthlyCredits)
	r := big.NewRat(monthly*int64(days), int64(p.Trial.DaysBasis))
	return Round(r, CreditPlaces)
}

// Period is one usage period.
type Period struct {
	Kind      string
	Start     time.Time
	End       time.Time
	Allowance *big.Rat
}

// MonthStart is the first instant of t's calendar month in UTC.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// PeriodAt is the installation's usage period containing at. A trial has one
// period from its start to its end, and usage after the end (if any) stays in
// it; every other plan uses calendar months in UTC.
func (p Policy) PeriodAt(inst Installation, at time.Time) Period {
	if inst.Plan == PlanTrial && inst.TrialStartedAt != nil && inst.TrialEndsAt != nil {
		allowance := new(big.Rat)
		if inst.TrialCredits != nil {
			allowance.Set(inst.TrialCredits)
		}
		return Period{Kind: PeriodTrial, Start: inst.TrialStartedAt.UTC(), End: inst.TrialEndsAt.UTC(), Allowance: allowance}
	}
	start := MonthStart(at)
	return Period{Kind: PeriodMonth, Start: start, End: start.AddDate(0, 1, 0), Allowance: p.MonthlyAllowance(inst)}
}

// MonthlyAllowance is a monthly period's included credits.
func (p Policy) MonthlyAllowance(inst Installation) *big.Rat {
	switch inst.Deployment {
	case DeploymentSelfHostedManaged:
		if inst.AllowanceCredits != nil {
			return new(big.Rat).Set(inst.AllowanceCredits)
		}
		return new(big.Rat)
	case DeploymentInternal:
		return new(big.Rat)
	}
	if t, ok := p.Tiers[inst.Plan]; ok {
		return new(big.Rat).SetInt64(int64(t.MonthlyCredits))
	}
	return new(big.Rat)
}

// WorkflowLimit is the active-workflow limit, or nil when unlimited. A trial
// gets its source tier's limit.
func (p Policy) WorkflowLimit(inst Installation) *int {
	if inst.Deployment != DeploymentHosted {
		return nil
	}
	plan := inst.Plan
	if plan == PlanTrial {
		plan = p.Trial.Tier
	}
	t, ok := p.Tiers[plan]
	if !ok {
		return nil
	}
	limit := t.WorkflowLimit
	return &limit
}

// CrossedThresholds returns the thresholds that used credits have reached for
// a positive allowance.
func CrossedThresholds(used, allowance *big.Rat) []int {
	if allowance == nil || allowance.Sign() <= 0 {
		return nil
	}
	var out []int
	for _, pct := range AlertThresholds {
		limit := new(big.Rat).Mul(allowance, big.NewRat(int64(pct), 100))
		if used.Cmp(limit) >= 0 {
			out = append(out, pct)
		}
	}
	return out
}

// Overage is max(0, used - allowance).
func Overage(used, allowance *big.Rat) *big.Rat {
	o := new(big.Rat).Sub(used, allowance)
	if o.Sign() < 0 {
		return new(big.Rat)
	}
	return o
}
