package central

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Deployment types and plans accepted by central's registry.
var (
	Deployments = []string{"hosted", "self_hosted_managed", "internal"}
	Plans       = []string{"trial", "starter", "growth", "scale", "none"}
	Currencies  = []string{"EUR", "USD"}
)

// Operator is the operator API surface Forge uses. The concrete Client talks
// HTTPS to central; tests substitute fakes.
type Operator interface {
	Register(ctx context.Context, req RegisterRequest) (RegisterResult, error)
	Issue(ctx context.Context, installation string, csrPEM []byte) (IssuedCertificate, error)
	Revoke(ctx context.Context, installation, reason string) (json.RawMessage, error)
	SetPlan(ctx context.Context, installation, plan string) (json.RawMessage, error)
	ExtendTrial(ctx context.Context, installation string, days int) (json.RawMessage, error)
	Show(ctx context.Context, installation string) (json.RawMessage, error)
	Export(ctx context.Context, period, format string) ([]byte, error)
}

// RegisterRequest is POST /operator/v1/installations. Credit amounts are
// decimal strings on the wire, like every amount in central's contract.
type RegisterRequest struct {
	ID               string  `json:"id"`
	Deployment       string  `json:"deployment"`
	Plan             string  `json:"plan"`
	BillingCurrency  string  `json:"billing_currency"`
	TrialDays        *int    `json:"trial_days,omitempty"`
	AllowanceCredits *string `json:"allowance_credits,omitempty"`
}

// RegisterResult reports whether the entry was created or already existed
// identically (a revoked one is reactivated by central).
type RegisterResult struct {
	Created bool
	Entry   json.RawMessage
}

// IssuedCertificate is the certificate central signed for a CSR.
type IssuedCertificate struct {
	CertificatePEM string    `json:"certificate_pem"`
	CAPEM          string    `json:"ca_pem"`
	Serial         string    `json:"serial"`
	NotAfter       time.Time `json:"not_after"`
}

// APIError is a non-2xx operator API response in the OpenAI error envelope.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	hint := errorHints[e.Code]
	if hint == "" {
		switch e.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			hint = "the AWS identity is not on central's operator allow-list for this action"
		case http.StatusConflict:
			hint = "the installation already exists with different settings"
		case http.StatusNotFound:
			hint = "no such installation"
		}
	}
	msg := fmt.Sprintf("central: %d %s", e.Status, e.Code)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if hint != "" {
		msg += " (" + hint + ")"
	}
	return msg
}

// errorHints explains central error codes an operator can act on.
var errorHints = map[string]string{
	"operator_unauthenticated": "central could not verify the AWS identity; refresh credentials (e.g. aws sso login) and check --region",
	"operator_forbidden":       "the AWS role is not allowed this action; preview-scoped roles may only manage internal pr-*/staging installations",
	"installation_conflict":    "the installation already exists with different settings; use 'forge central plan' or 'extend-trial' to change it",
	"installation_not_found":   "no such installation; register it first",
	"installation_revoked":     "the installation is revoked; re-register it to reactivate",
	"invalid_csr":              "central rejected the certificate request",
	"invalid_request":          "central rejected the request parameters",
	"upstream_unavailable":     "central is temporarily unavailable; retry",
}

// Client is the HTTPS operator API client.
type Client struct {
	base     *url.URL
	http     *http.Client
	auth     Authorizer
	audience string
}

// NewClient builds a client for centralURL that trusts only caBundlePEM.
// System roots are deliberately not used: central's serving certificate is
// issued by the Iterabase central CA.
func NewClient(centralURL string, caBundlePEM []byte, auth Authorizer) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(centralURL, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("central: --central-url must be an https URL, got %q", centralURL)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBundlePEM) {
		return nil, errors.New("central: --ca-bundle contains no PEM certificates")
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13},
		Proxy:           http.ProxyFromEnvironment,
	}
	return newClient(u, &http.Client{Transport: transport, Timeout: 60 * time.Second}, auth), nil
}

func newClient(base *url.URL, hc *http.Client, auth Authorizer) *Client {
	return &Client{base: base, http: hc, auth: auth, audience: base.Hostname()}
}

// Register implements Operator.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (RegisterResult, error) {
	status, body, err := c.do(ctx, http.MethodPost, "/operator/v1/installations", req)
	if err != nil {
		return RegisterResult{}, err
	}
	return RegisterResult{Created: status == http.StatusCreated, Entry: body}, nil
}

// Issue implements Operator.
func (c *Client) Issue(ctx context.Context, installation string, csrPEM []byte) (IssuedCertificate, error) {
	_, body, err := c.do(ctx, http.MethodPost, installationPath(installation, "certificates"),
		map[string]string{"csr_pem": string(csrPEM)})
	if err != nil {
		return IssuedCertificate{}, err
	}
	var out IssuedCertificate
	if err := json.Unmarshal(body, &out); err != nil {
		return IssuedCertificate{}, fmt.Errorf("central: decode issued certificate: %w", err)
	}
	if out.CertificatePEM == "" || out.CAPEM == "" {
		return IssuedCertificate{}, errors.New("central: issued certificate response is missing PEM data")
	}
	return out, nil
}

// Revoke implements Operator.
func (c *Client) Revoke(ctx context.Context, installation, reason string) (json.RawMessage, error) {
	_, body, err := c.do(ctx, http.MethodPost, installationPath(installation, "revoke"), map[string]string{"reason": reason})
	return body, err
}

// SetPlan implements Operator.
func (c *Client) SetPlan(ctx context.Context, installation, plan string) (json.RawMessage, error) {
	_, body, err := c.do(ctx, http.MethodPost, installationPath(installation, "plan"), map[string]string{"plan": plan})
	return body, err
}

// ExtendTrial implements Operator.
func (c *Client) ExtendTrial(ctx context.Context, installation string, days int) (json.RawMessage, error) {
	_, body, err := c.do(ctx, http.MethodPost, installationPath(installation, "trial-extension"), map[string]int{"days": days})
	return body, err
}

// Show implements Operator.
func (c *Client) Show(ctx context.Context, installation string) (json.RawMessage, error) {
	_, body, err := c.do(ctx, http.MethodGet, installationPath(installation, ""), nil)
	return body, err
}

// Export implements Operator.
func (c *Client) Export(ctx context.Context, period, format string) ([]byte, error) {
	_, body, err := c.do(ctx, http.MethodGet, "/operator/v1/exports/"+url.PathEscape(period)+"?format="+url.QueryEscape(format), nil)
	return body, err
}

func installationPath(id, action string) string {
	p := "/operator/v1/installations/" + url.PathEscape(id)
	if action != "" {
		p += "/" + action
	}
	return p
}

func (c *Client) do(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, fmt.Errorf("central: encode request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	target, err := c.base.Parse(c.base.Path + path)
	if err != nil {
		return 0, nil, fmt.Errorf("central: build URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return 0, nil, fmt.Errorf("central: build request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	auth, err := c.auth.OperatorAuth(ctx, c.audience)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set(HeaderOperatorAuth, auth)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("central: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("central: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, nil, decodeAPIError(resp.StatusCode, respBody)
	}
	return resp.StatusCode, respBody, nil
}

func decodeAPIError(status int, body []byte) *APIError {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	e := &APIError{Status: status}
	if json.Unmarshal(body, &envelope) == nil {
		e.Code = envelope.Error.Code
		e.Message = envelope.Error.Message
	}
	if e.Code == "" {
		e.Code = strings.ToLower(strings.ReplaceAll(http.StatusText(status), " ", "_"))
	}
	return e
}
