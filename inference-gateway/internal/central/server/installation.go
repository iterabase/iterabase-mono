package server

import (
	"context"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/billing"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/store"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/proxy"
)

// periodView is an installation's current usage period.
type periodView struct {
	Kind             string    `json:"kind"`
	Start            time.Time `json:"start"`
	End              time.Time `json:"end"`
	AllowanceCredits string    `json:"allowance_credits"`
	UsedCredits      string    `json:"used_credits"`
	OverageCredits   string    `json:"overage_credits"`
}

type alertView struct {
	Threshold   int       `json:"threshold"`
	PeriodStart time.Time `json:"period_start"`
	CrossedAt   time.Time `json:"crossed_at"`
}

func (s *Server) currentPeriod(ctx context.Context, inst billing.Installation) (periodView, []alertView, error) {
	period := s.policy.PeriodAt(inst, s.deps.Now())
	used, err := s.deps.Store.UsedCredits(ctx, inst.ID, period.Start)
	if err != nil {
		return periodView{}, nil, err
	}
	alerts, err := s.deps.Store.Alerts(ctx, inst.ID, period.Start)
	if err != nil {
		return periodView{}, nil, err
	}
	views := make([]alertView, 0, len(alerts))
	for _, a := range alerts {
		views = append(views, alertView{Threshold: a.Threshold, PeriodStart: a.PeriodStart, CrossedAt: a.CrossedAt})
	}
	return periodView{
		Kind: period.Kind, Start: period.Start, End: period.End,
		AllowanceCredits: trimDecimal(period.Allowance),
		UsedCredits:      billing.Format(used, billing.CreditPlaces),
		OverageCredits:   billing.Format(billing.Overage(used, period.Allowance), billing.CreditPlaces),
	}, views, nil
}

// trimDecimal renders r at credit precision without trailing zeros
// ("5000", "1166.666667").
func trimDecimal(r *big.Rat) string {
	out := billing.Format(r, billing.CreditPlaces)
	out = strings.TrimRight(out, "0")
	return strings.TrimSuffix(out, ".")
}

// installationRead serves GET /v1/installation: the installation's own
// entitlements, current period and alerts (DES-HOR-661-05/06). It reads the
// database, not the cache, so a plan change shows at once.
func (s *Server) installationRead(w http.ResponseWriter, r *http.Request) {
	inst, _, err := s.deps.Store.Installation(r.Context(), installationFrom(r.Context()).ID)
	if err != nil {
		s.deps.Logger.Error("installation read failed", "error", err)
		proxy.WriteUpstreamUnavailable(w, "")
		return
	}
	period, alerts, err := s.currentPeriod(r.Context(), inst)
	if err != nil {
		s.deps.Logger.Error("installation usage read failed", "error", err)
		proxy.WriteUpstreamUnavailable(w, "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installation_id":      inst.ID,
		"deployment":           inst.Deployment,
		"plan":                 inst.Plan,
		"workflow_limit":       s.policy.WorkflowLimit(inst),
		"trial_ends_at":        inst.TrialEndsAt,
		"billing_currency":     inst.BillingCurrency,
		"period":               period,
		"alerts":               alerts,
		"entitlements_version": inst.EntitlementsVersion,
		"as_of":                s.deps.Now().UTC(),
	})
}

// installationView is the operator's view of a registry entry.
func (s *Server) installationView(ctx context.Context, inst billing.Installation, certs []store.Certificate) (map[string]any, error) {
	view := map[string]any{
		"id":                   inst.ID,
		"deployment":           inst.Deployment,
		"plan":                 inst.Plan,
		"billing_currency":     inst.BillingCurrency,
		"status":               inst.Status,
		"workflow_limit":       s.policy.WorkflowLimit(inst),
		"trial_started_at":     inst.TrialStartedAt,
		"trial_ends_at":        inst.TrialEndsAt,
		"trial_credits":        formatOptional(inst.TrialCredits),
		"allowance_credits":    formatOptional(inst.AllowanceCredits),
		"entitlements_version": inst.EntitlementsVersion,
	}
	if certs != nil {
		view["certificates"] = certs
	}
	period, alerts, err := s.currentPeriod(ctx, inst)
	if err != nil {
		return nil, err
	}
	view["period"] = period
	view["alerts"] = alerts
	return view, nil
}

func formatOptional(r *big.Rat) any {
	if r == nil {
		return nil
	}
	return billing.Format(r, billing.CreditPlaces)
}
