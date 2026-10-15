package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/attribution"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/billing"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/config"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/store"
	gatewaymw "github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/proxy"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/ratelimit"
)

// fxAlertEvery throttles the operations alert for a missing FX rate.
const fxAlertEvery = time.Hour

// chatCompletions proxies one installation request to the provider and meters
// it. Before dispatch it needs the FX rate (when the currencies differ) and
// room under the provider-wide and per-installation limits; after the
// response it records usage in one transaction, and a failed write withholds
// the response (DES-HOR-661-04/07/08).
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	inst := installationFrom(r.Context())
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		proxy.WriteError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body", "invalid_request_error")
		return
	}
	name, stream, err := proxy.ParseChatRequest(body)
	if err != nil {
		proxy.WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON in request body", "invalid_request_error")
		return
	}
	model, ok := s.deps.Config.Models[name]
	if !ok {
		proxy.WriteError(w, http.StatusNotFound, "invalid_request_error", fmt.Sprintf("model '%s' not found", name), codeModelNotFound)
		return
	}
	md := &gatewaymw.MetricsData{Model: name, Streaming: stream}
	*r = *r.WithContext(gatewaymw.SetMetricsData(r.Context(), md))

	attr, dropped := attribution.FromHeader(r.Header)
	if dropped > 0 {
		s.cm.attributionDropped.Add(float64(dropped))
	}
	attr.RequestID = gatewaymw.RequestIDFromContext(r.Context())

	now := s.deps.Now()
	if billing.NeedsFX(model.Currency, inst.BillingCurrency) {
		periodStart := s.policy.PeriodAt(inst, now).Start
		if _, err := s.deps.Store.LockedRate(r.Context(), periodStart); err != nil {
			s.fxUnavailable(r.Context(), now, periodStart, err)
			proxy.WriteUpstreamUnavailable(w, "")
			return
		}
	}

	allowed, retryAfter, err := s.checkLimits(r.Context(), name, inst.ID)
	if err != nil {
		s.deps.Logger.Error("rate limit check failed; refusing (central fails closed)", "error", err)
		proxy.WriteUpstreamUnavailable(w, "")
		return
	}
	if !allowed {
		proxy.WriteUpstreamRateLimited(w, retryAfter)
		return
	}

	body = proxy.RewriteModelInRequest(body, model.ProviderModel)
	header := http.Header{}
	header.Set("Authorization", "Bearer "+s.deps.Config.ProviderKey)
	target := &proxy.Target{
		URL:           proxy.ChatCompletionsURL(model.BaseURL),
		Client:        s.deps.ProviderClient,
		Header:        header,
		ResponseModel: name,
		MetricsModel:  name,
		BackendRef:    "provider",
	}
	s.forwarder.Forward(w, r, body, stream, target, proxy.Hooks{
		Settle: func(ctx context.Context, o proxy.Outcome) error {
			return s.meter(ctx, inst, name, model, stream, attr, o)
		},
	})
}

// meter records one outcome and publishes any alert it crossed.
func (s *Server) meter(ctx context.Context, inst billing.Installation, name string, model config.Model, stream bool,
	attr attribution.Attribution, o proxy.Outcome) error {
	rec := store.UsageRecord{
		RequestID: attr.RequestID, InstallationID: inst.ID, Model: name, Price: model,
		PriceVersion: s.deps.Config.PriceVersion, Stream: stream, Disposition: string(o.Disposition),
		ErrorCode: o.ErrorCode, UpstreamStatus: o.UpstreamStatus, OccurredAt: s.deps.Now(), Attribution: attr,
	}
	if o.Usage != nil {
		rec.Usage = &billing.Tokens{Input: o.Usage.PromptTokens, CachedInput: o.Usage.CachedPromptTokens, Output: o.Usage.CompletionTokens}
	}
	metered, err := s.deps.Store.Record(ctx, rec)
	if err != nil {
		s.cm.meteringFailures.Inc()
		if errors.Is(err, store.ErrNoRate) {
			s.fxUnavailable(ctx, rec.OccurredAt, s.policy.PeriodAt(inst, rec.OccurredAt).Start, err)
		}
		return err
	}
	s.cm.usageRecords.WithLabelValues(metered.Classification).Inc()
	for _, alert := range metered.NewAlerts {
		s.cm.alertsCrossed.WithLabelValues(strconv.Itoa(alert.Threshold)).Inc()
		if err := s.deps.Alerts.Publish(ctx, alert); err != nil {
			s.deps.Logger.Warn("credit alert publish failed; the retry loop will resend", "installation_id", alert.InstallationID, "error", err)
		}
	}
	return nil
}

// checkLimits admits the request against the provider-wide and the
// per-installation one-minute limits atomically: a request either scope
// refuses consumes no quota anywhere (DES-HOR-661-07). Any database error
// fails closed.
func (s *Server) checkLimits(ctx context.Context, model, installationID string) (bool, string, error) {
	limits := s.deps.Config.Limits
	res, err := s.deps.Limiter.Admit(ctx,
		ratelimit.Check{Key: store.ProviderScope(model), RPM: limits.Provider.RPM, TPM: limits.Provider.TPM},
		ratelimit.Check{Key: store.InstallationScope(installationID), RPM: limits.Installation.RPM, TPM: limits.Installation.TPM})
	if err != nil {
		return false, "", err
	}
	if !res.Allowed {
		return false, retryAfter(res.ResetAt, s.deps.Now()), nil
	}
	return true, "", nil
}

func retryAfter(reset, now time.Time) string {
	return strconv.Itoa(int(math.Max(1, math.Ceil(reset.Sub(now).Seconds()))))
}

// fxUnavailable logs the fx_rate_unavailable token (alarmed by a CloudWatch
// metric filter) and alerts operations at most hourly per month.
func (s *Server) fxUnavailable(ctx context.Context, now, periodStart time.Time, cause error) {
	lockDate := store.LockDate(periodStart)
	s.cm.fxUnavailable.Inc()
	s.deps.Logger.Error("fx_rate_unavailable: no locked ECB rate for the billing period; refusing FX-dependent inference",
		"period_start", lockDate, "error", cause)
	if last, ok := s.fxAlerted.Load(lockDate); ok && now.Sub(last.(time.Time)) < fxAlertEvery {
		return
	}
	s.fxAlerted.Store(lockDate, now)
	if s.deps.Ops != nil {
		msg := fmt.Sprintf(`{"type":"fx_rate_unavailable","period_start":%q}`, lockDate)
		if err := s.deps.Ops.Publish(context.WithoutCancel(ctx), "Iterabase central: FX rate unavailable for the period starting "+lockDate, msg); err != nil {
			s.deps.Logger.Warn("operations alert publish failed", "error", err)
		}
	}
}

// listModels lists the price book's models.
func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	names := make([]string, 0, len(s.deps.Config.Models))
	for name := range s.deps.Config.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]any{"id": name, "object": "model", "created": 0, "owned_by": "iterabase"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
