package server

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/billing"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/config"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/opauth"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/pki"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/store"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/proxy"
)

// Bounds on operator inputs.
const (
	maxOperatorBody = 64 << 10
	maxTrialDays    = 90
)

func actor(c opauth.Caller) store.Actor { return store.Actor{ARN: c.RoleARN, Session: c.Session} }

// opError writes an operator error and counts the request.
func (s *Server) opError(w http.ResponseWriter, action string, status int, code, message string) {
	s.cm.operatorRequests.WithLabelValues(action, fmt.Sprintf("%dxx", status/100)).Inc()
	errType := "invalid_request_error"
	switch status {
	case http.StatusForbidden:
		errType = "permission_error"
	case http.StatusInternalServerError, http.StatusServiceUnavailable:
		errType = "api_error"
	}
	proxy.WriteError(w, status, errType, message, code)
}

func (s *Server) opOK(w http.ResponseWriter, action string, status int, v any) {
	s.refreshRegistry(action)
	s.cm.operatorRequests.WithLabelValues(action, fmt.Sprintf("%dxx", status/100)).Inc()
	writeJSON(w, status, v)
}

// storeError maps store errors to operator responses.
func (s *Server) storeError(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.opError(w, action, http.StatusNotFound, codeInstallationNotFound, "installation not found")
	case errors.Is(err, store.ErrConflict):
		s.opError(w, action, http.StatusConflict, codeInstallationConflict, "the registration differs from the existing installation")
	case errors.Is(err, store.ErrRevoked):
		s.opError(w, action, http.StatusConflict, codeInstallationConflict, "the installation is revoked; register it again to reactivate it")
	case errors.Is(err, store.ErrInvalid):
		s.opError(w, action, http.StatusBadRequest, codeInvalidRequest, err.Error())
	default:
		s.deps.Logger.Error("operator request failed", "action", action, "error", err)
		s.opError(w, action, http.StatusServiceUnavailable, proxy.CodeUpstreamUnavailable, "central store unavailable")
	}
}

// refreshRegistry applies a registry change on this replica before the
// operator gets its answer, so a revocation is refused on the very next
// request here; other replicas follow on NOTIFY within milliseconds.
func (s *Server) refreshRegistry(action string) {
	switch action {
	case "show", "export", "adjust":
		return
	}
	if err := s.deps.Registry.Refresh(context.Background()); err != nil {
		s.deps.Logger.Warn("registry refresh after an operator change failed; NOTIFY and polling will catch up", "error", err)
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxOperatorBody))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// loadAuthorized reads an installation and checks the caller's scope over it.
func (s *Server) loadAuthorized(w http.ResponseWriter, r *http.Request, action string) (billing.Installation, []store.Certificate, bool) {
	id := chi.URLParam(r, "id")
	inst, certs, err := s.deps.Store.Installation(r.Context(), id)
	if err != nil {
		s.storeError(w, action, err)
		return inst, nil, false
	}
	if !callerFrom(r.Context()).Authorizes(inst.ID, inst.Deployment) {
		s.opError(w, action, http.StatusForbidden, codeOperatorForbidden, "the operator's scope does not cover this installation")
		return inst, nil, false
	}
	return inst, certs, true
}

type registerRequest struct {
	ID               string  `json:"id"`
	Deployment       string  `json:"deployment"`
	Plan             string  `json:"plan"`
	BillingCurrency  string  `json:"billing_currency"`
	TrialDays        *int    `json:"trial_days,omitempty"`
	AllowanceCredits *string `json:"allowance_credits,omitempty"`
}

// validate turns a request into a registration or explains why not.
//
//nolint:gocyclo // A flat list of independent field rules.
func (req registerRequest) validate() (store.Registration, error) {
	reg := store.Registration{ID: req.ID, Deployment: req.Deployment, Plan: req.Plan, BillingCurrency: req.BillingCurrency, TrialDays: req.TrialDays}
	if !pki.InstallationIDPattern.MatchString(req.ID) {
		return reg, errors.New("id must match ^[a-z][a-z0-9-]{1,61}[a-z0-9]$")
	}
	if req.BillingCurrency != config.EUR && req.BillingCurrency != config.USD {
		return reg, errors.New("billing_currency must be EUR or USD")
	}
	switch req.Deployment {
	case billing.DeploymentHosted:
		switch req.Plan {
		case billing.PlanTrial:
			if req.TrialDays == nil || *req.TrialDays < 1 || *req.TrialDays > maxTrialDays {
				return reg, fmt.Errorf("a trial needs trial_days between 1 and %d", maxTrialDays)
			}
		case billing.PlanStarter, billing.PlanGrowth, billing.PlanScale:
			if req.TrialDays != nil {
				return reg, errors.New("trial_days applies only to a trial")
			}
		default:
			return reg, errors.New("a hosted plan is trial, starter, growth or scale")
		}
		if req.AllowanceCredits != nil {
			return reg, errors.New("allowance_credits applies only to self_hosted_managed")
		}
	case billing.DeploymentSelfHostedManaged, billing.DeploymentInternal:
		if req.Plan != billing.PlanNone || req.TrialDays != nil {
			return reg, fmt.Errorf("%s installations have plan none and no trial", req.Deployment)
		}
		if req.AllowanceCredits != nil {
			if req.Deployment != billing.DeploymentSelfHostedManaged {
				return reg, errors.New("allowance_credits applies only to self_hosted_managed")
			}
			a, ok := new(big.Rat).SetString(*req.AllowanceCredits)
			if !ok || a.Sign() < 0 {
				return reg, errors.New("allowance_credits must be a non-negative decimal string")
			}
			reg.AllowanceCredits = billing.Round(a, billing.CreditPlaces)
		}
	default:
		return reg, errors.New("deployment must be hosted, self_hosted_managed or internal")
	}
	return reg, nil
}

func (s *Server) registerInstallation(w http.ResponseWriter, r *http.Request) {
	const action = "register"
	var req registerRequest
	if err := decode(r, &req); err != nil {
		s.opError(w, action, http.StatusBadRequest, codeInvalidRequest, "invalid JSON body")
		return
	}
	reg, err := req.validate()
	if err != nil {
		s.opError(w, action, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	caller := callerFrom(r.Context())
	if !caller.Authorizes(reg.ID, reg.Deployment) {
		s.opError(w, action, http.StatusForbidden, codeOperatorForbidden, "the operator's scope does not cover this installation")
		return
	}
	inst, created, err := s.deps.Store.Register(r.Context(), reg, actor(caller))
	if err != nil {
		s.storeError(w, action, err)
		return
	}
	view, err := s.installationView(r.Context(), inst, nil)
	if err != nil {
		s.storeError(w, action, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.opOK(w, action, status, view)
}

func (s *Server) showInstallation(w http.ResponseWriter, r *http.Request) {
	const action = "show"
	inst, certs, ok := s.loadAuthorized(w, r, action)
	if !ok {
		return
	}
	if certs == nil {
		certs = []store.Certificate{}
	}
	view, err := s.installationView(r.Context(), inst, certs)
	if err != nil {
		s.storeError(w, action, err)
		return
	}
	s.opOK(w, action, http.StatusOK, view)
}

func (s *Server) issueCertificate(w http.ResponseWriter, r *http.Request) {
	const action = "issue"
	inst, _, ok := s.loadAuthorized(w, r, action)
	if !ok {
		return
	}
	var req struct {
		CSRPEM string `json:"csr_pem"`
	}
	if err := decode(r, &req); err != nil || req.CSRPEM == "" {
		s.opError(w, action, http.StatusBadRequest, codeInvalidRequest, "csr_pem is required")
		return
	}
	var der []byte
	issued, err := s.deps.Store.RecordCertificate(r.Context(), inst.ID, func() (store.IssuedCertificate, error) {
		cert, err := s.deps.Authority.IssueInstallation([]byte(req.CSRPEM), inst.ID)
		if err != nil {
			return store.IssuedCertificate{}, err
		}
		der = cert.DER
		return store.IssuedCertificate{Serial: cert.Serial, Fingerprint: cert.Fingerprint, NotBefore: cert.NotBefore, NotAfter: cert.NotAfter}, nil
	}, actor(callerFrom(r.Context())))
	if errors.Is(err, pki.ErrInvalidCSR) {
		s.opError(w, action, http.StatusBadRequest, codeInvalidCSR, err.Error())
		return
	}
	if err != nil {
		s.storeError(w, action, err)
		return
	}
	s.opOK(w, action, http.StatusCreated, map[string]any{
		"certificate_pem": string(pki.EncodeCert(der)),
		"ca_pem":          string(s.deps.Authority.CAPEM()),
		"serial":          issued.Serial,
		"not_after":       issued.NotAfter.UTC().Format(time.RFC3339),
	})
}

func (s *Server) revokeInstallation(w http.ResponseWriter, r *http.Request) {
	const action = "revoke"
	inst, _, ok := s.loadAuthorized(w, r, action)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &req); err != nil || strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 500 {
		s.opError(w, action, http.StatusBadRequest, codeInvalidRequest, "a reason (at most 500 characters) is required")
		return
	}
	if err := s.deps.Store.Revoke(r.Context(), inst.ID, req.Reason, actor(callerFrom(r.Context()))); err != nil {
		s.storeError(w, action, err)
		return
	}
	s.opOK(w, action, http.StatusOK, map[string]any{"id": inst.ID, "status": "revoked"})
}

func (s *Server) setPlan(w http.ResponseWriter, r *http.Request) {
	const action = "plan"
	inst, _, ok := s.loadAuthorized(w, r, action)
	if !ok {
		return
	}
	var req struct {
		Plan string `json:"plan"`
	}
	if err := decode(r, &req); err != nil {
		s.opError(w, action, http.StatusBadRequest, codeInvalidRequest, "invalid JSON body")
		return
	}
	if _, paid := s.deps.Config.Tiers[req.Plan]; !paid {
		s.opError(w, action, http.StatusBadRequest, codeInvalidRequest, "plan must be starter, growth or scale")
		return
	}
	updated, err := s.deps.Store.SetPlan(r.Context(), inst.ID, req.Plan, actor(callerFrom(r.Context())))
	if err != nil {
		s.storeError(w, action, err)
		return
	}
	view, err := s.installationView(r.Context(), updated, nil)
	if err != nil {
		s.storeError(w, action, err)
		return
	}
	s.opOK(w, action, http.StatusOK, view)
}

func (s *Server) extendTrial(w http.ResponseWriter, r *http.Request) {
	const action = "extend_trial"
	inst, _, ok := s.loadAuthorized(w, r, action)
	if !ok {
		return
	}
	var req struct {
		Days int `json:"days"`
	}
	if err := decode(r, &req); err != nil || req.Days < 1 || req.Days > maxTrialDays {
		s.opError(w, action, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf("days must be between 1 and %d", maxTrialDays))
		return
	}
	updated, err := s.deps.Store.ExtendTrial(r.Context(), inst.ID, req.Days, actor(callerFrom(r.Context())))
	if err != nil {
		s.storeError(w, action, err)
		return
	}
	view, err := s.installationView(r.Context(), updated, nil)
	if err != nil {
		s.storeError(w, action, err)
		return
	}
	s.opOK(w, action, http.StatusOK, view)
}

// requireAdmin refuses preview-scoped operators.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request, action string) bool {
	if callerFrom(r.Context()).Scope != config.ScopeAdmin {
		s.opError(w, action, http.StatusForbidden, codeOperatorForbidden, "requires an admin operator")
		return false
	}
	return true
}

func (s *Server) adjustUsage(w http.ResponseWriter, r *http.Request) {
	const action = "adjust"
	if !s.requireAdmin(w, r, action) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "record_id"))
	if err != nil {
		s.opError(w, action, http.StatusBadRequest, codeInvalidRequest, "record_id must be a UUID")
		return
	}
	var req struct {
		Classification string `json:"classification"`
		Reason         string `json:"reason"`
	}
	if err := decode(r, &req); err != nil || req.Classification != billing.CompanyFunded ||
		strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 500 {
		s.opError(w, action, http.StatusBadRequest, codeInvalidRequest, "classification must be company_funded and a reason is required")
		return
	}
	if err := s.deps.Store.Adjust(r.Context(), id, req.Reason, actor(callerFrom(r.Context()))); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.opError(w, action, http.StatusNotFound, codeInvalidRequest, "usage record not found")
			return
		}
		s.storeError(w, action, err)
		return
	}
	s.opOK(w, action, http.StatusOK, map[string]any{"record_id": id.String(), "classification": billing.CompanyFunded})
}

// exportColumns is the CSV header of the monthly export.
var exportColumns = []string{
	"installation_id", "deployment", "plan", "billing_currency", "period_kind", "period_start", "period_end",
	"allowance_credits", "chargeable_credits", "company_funded_credits", "overage_credits", "requests",
	"not_charged_requests",
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	const action = "export"
	if !s.requireAdmin(w, r, action) {
		return
	}
	month, err := time.Parse("2006-01", chi.URLParam(r, "period"))
	if err != nil {
		s.opError(w, action, http.StatusBadRequest, codeInvalidRequest, "period must be YYYY-MM")
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" {
		s.opError(w, action, http.StatusBadRequest, codeInvalidRequest, "format must be json or csv")
		return
	}
	rows, err := s.deps.Store.Export(r.Context(), month)
	if errors.Is(err, store.ErrUnreconciled) {
		s.deps.Logger.Error("export does not reconcile", "error", err)
		s.opError(w, action, http.StatusInternalServerError, "export_unreconciled", "the export does not reconcile with usage records")
		return
	}
	if err != nil {
		s.storeError(w, action, err)
		return
	}
	if rows == nil {
		rows = []store.ExportRow{}
	}
	if format == "json" {
		s.opOK(w, action, http.StatusOK, map[string]any{"period": month.Format("2006-01"), "rows": rows})
		return
	}
	s.cm.operatorRequests.WithLabelValues(action, "2xx").Inc()
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="iterabase-usage-%s.csv"`, month.Format("2006-01")))
	out := csv.NewWriter(w)
	_ = out.Write(exportColumns)
	for _, row := range rows {
		_ = out.Write([]string{
			row.InstallationID, row.Deployment, row.Plan, row.BillingCurrency, row.PeriodKind,
			row.PeriodStart.Format(time.RFC3339), row.PeriodEnd.Format(time.RFC3339), row.AllowanceCredits,
			row.ChargeableCredits, row.CompanyFundedCredits, row.OverageCredits,
			strconv.FormatInt(row.Requests, 10), strconv.FormatInt(row.NotChargedRequests, 10),
		})
	}
	out.Flush()
}
