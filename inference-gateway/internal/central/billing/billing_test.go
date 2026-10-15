package billing

import (
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/config"
)

func model(currency string) config.Model {
	return config.Model{Currency: currency, InputPerMillion: "0.50", CachedInputPerMillion: "0.13", OutputPerMillion: "1.50"}
}

func policy() Policy {
	return Policy{
		Tiers: map[string]config.Tier{
			"starter": {WorkflowLimit: 2, MonthlyCredits: 5000},
			"growth":  {WorkflowLimit: 6, MonthlyCredits: 20000},
			"scale":   {WorkflowLimit: 15, MonthlyCredits: 60000},
		},
		Trial: config.Trial{Tier: "starter", DaysBasis: 30},
	}
}

// TestCreditPolicyFixtures is the credit-policy fixture table (DES-HOR-661-04,
// REQ-048): 1,000,000 input tokens of which 200,000 cached, 100,000 output, at
// 0.50 / 0.13 / 1.50 per million = 0.576 in the supplier currency, with the
// ECB rate 1.1403 USD per EUR.
func TestCreditPolicyFixtures(t *testing.T) {
	tokens := Tokens{Input: 1_000_000, CachedInput: 200_000, Output: 100_000}
	rate := MustDec("1.1403")
	for _, tc := range []struct {
		name               string
		supplier, billing  string
		deployment         string
		usage              bool
		wantCredits        string
		wantClassification string
		wantFX             bool
	}{
		{"EUR price, EUR billing", config.EUR, config.EUR, DeploymentHosted, true, "57.600000", Chargeable, false},
		{"USD price, EUR billing", config.USD, config.EUR, DeploymentHosted, true, "50.513023", Chargeable, true},
		{"USD price, USD billing (no FX)", config.USD, config.USD, DeploymentHosted, true, "57.600000", Chargeable, false},
		{"EUR price, USD billing", config.EUR, config.USD, DeploymentHosted, true, "65.681280", Chargeable, true},
		{"self-hosted managed", config.USD, config.USD, DeploymentSelfHostedManaged, true, "57.600000", Chargeable, false},
		{"internal is company-funded", config.USD, config.EUR, DeploymentInternal, true, "50.513023", CompanyFunded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			amount := SupplierAmount(model(tc.supplier), tokens)
			assert.Equal(t, "0.576000000000", Format(amount, 12))
			assert.Equal(t, tc.wantFX, NeedsFX(tc.supplier, tc.billing))
			var r *big.Rat
			if tc.wantFX {
				r = rate
			}
			converted, err := Convert(amount, tc.supplier, tc.billing, r)
			require.NoError(t, err)
			assert.Equal(t, tc.wantCredits, Format(Credits(converted), CreditPlaces))
			assert.Equal(t, tc.wantClassification, Classify(tc.deployment, tc.usage))
		})
	}

	t.Run("no usage reported is not charged", func(t *testing.T) {
		assert.Equal(t, NotCharged, Classify(DeploymentHosted, false))
		assert.Equal(t, NotCharged, Classify(DeploymentInternal, false))
	})
	t.Run("a needed rate is required", func(t *testing.T) {
		_, err := Convert(big.NewRat(1, 1), config.USD, config.EUR, nil)
		assert.ErrorIs(t, err, ErrNoRate)
	})
	t.Run("same currency ignores a rate", func(t *testing.T) {
		got, err := Convert(big.NewRat(3, 2), config.EUR, config.EUR, nil)
		require.NoError(t, err)
		assert.Equal(t, "1.50", Format(got, 2))
	})
}

func TestSupplierAmount_ClampsCachedTokens(t *testing.T) {
	// Cached tokens can never exceed the prompt; negative counts count as zero.
	amount := SupplierAmount(model(config.USD), Tokens{Input: 100, CachedInput: 500, Output: -3})
	assert.Equal(t, "0.000013000000", Format(amount, 12))
}

func TestTrialAllowance(t *testing.T) {
	p := policy()
	assert.Equal(t, "1166.666667", Format(p.TrialCredits(7), CreditPlaces), "7-day trial: 5,000 × 7 / 30")
	assert.Equal(t, "500.000000", Format(p.TrialCredits(3), CreditPlaces), "a 3-day extension adds 500")

	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 10)
	credits := new(big.Rat).Add(p.TrialCredits(7), p.TrialCredits(3))
	inst := Installation{Deployment: DeploymentHosted, Plan: PlanTrial, TrialStartedAt: &start, TrialEndsAt: &end, TrialCredits: credits}
	period := p.PeriodAt(inst, start.AddDate(0, 0, 5))
	assert.Equal(t, PeriodTrial, period.Kind)
	assert.Equal(t, start, period.Start)
	assert.Equal(t, end, period.End, "one period spanning the month boundary")
	assert.Equal(t, "1666.666667", Format(period.Allowance, CreditPlaces))
	require.NotNil(t, p.WorkflowLimit(inst))
	assert.Equal(t, 2, *p.WorkflowLimit(inst), "a trial gets Starter's workflow limit")
}

func TestPeriods(t *testing.T) {
	p := policy()
	at := time.Date(2026, 11, 30, 23, 59, 0, 0, time.FixedZone("x", -3*3600)) // 1 Dec 02:59 UTC
	for _, tc := range []struct {
		inst      Installation
		allowance string
		limit     *int
	}{
		{Installation{Deployment: DeploymentHosted, Plan: PlanStarter}, "5000", intp(2)},
		{Installation{Deployment: DeploymentHosted, Plan: PlanScale}, "60000", intp(15)},
		{Installation{Deployment: DeploymentSelfHostedManaged, Plan: PlanNone, AllowanceCredits: MustDec("2500")}, "2500", nil},
		{Installation{Deployment: DeploymentSelfHostedManaged, Plan: PlanNone}, "0", nil},
		{Installation{Deployment: DeploymentInternal, Plan: PlanNone}, "0", nil},
	} {
		period := p.PeriodAt(tc.inst, at)
		assert.Equal(t, PeriodMonth, period.Kind)
		assert.Equal(t, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), period.Start, "calendar months in UTC")
		assert.Equal(t, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), period.End)
		assert.Equal(t, tc.allowance, Format(period.Allowance, 0))
		assert.Equal(t, tc.limit, p.WorkflowLimit(tc.inst))
	}
}

func TestCrossedThresholds(t *testing.T) {
	allowance := MustDec("5000")
	assert.Empty(t, CrossedThresholds(MustDec("3999.999999"), allowance))
	assert.Equal(t, []int{80}, CrossedThresholds(MustDec("4000"), allowance))
	assert.Equal(t, []int{80, 100}, CrossedThresholds(MustDec("5000"), allowance))
	assert.Empty(t, CrossedThresholds(MustDec("99999"), new(big.Rat)), "no allowance, no alerts")
	assert.Equal(t, "250", Format(Overage(MustDec("5250"), allowance), 0))
	assert.Equal(t, "0", Format(Overage(MustDec("10"), allowance), 0))
}

func intp(v int) *int { return &v }
