// Package opauth authenticates central operator requests with a presigned
// sts:GetCallerIdentity proof (DES-HOR-661-02; the Vault AWS-auth pattern):
// the caller signs a GetCallerIdentity POST that includes central's audience
// header, central replays it to STS, and the returned role is matched against
// the operator allow-list. No shared secret exists.
package opauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/config"
)

// Header carries the proof.
const Header = "X-Iterabase-Operator-Auth"

// AudienceHeader must be among the proof's signed headers.
const AudienceHeader = "X-Iterabase-Central-Audience"

// RequestBody is the only accepted STS request body.
const RequestBody = "Action=GetCallerIdentity&Version=2011-06-15"

// Proof freshness bounds.
const (
	MaxAge  = 15 * time.Minute
	MaxSkew = 5 * time.Minute
)

// Errors map to operator_unauthenticated and operator_forbidden.
var (
	ErrUnauthenticated = errors.New("opauth: unauthenticated")
	ErrForbidden       = errors.New("opauth: forbidden")
)

// Proof is the decoded header.
type Proof struct {
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

// Caller is an authenticated operator.
type Caller struct {
	RoleARN string
	Session string
	Scope   string
}

// HTTPDoer sends the replayed STS request.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Verifier verifies proofs.
type Verifier struct {
	Client    HTTPDoer
	Audience  string
	Operators []config.Operator
	Now       func() time.Time
}

var stsHost = regexp.MustCompile(`^sts\.[a-z0-9-]+\.amazonaws\.com$`)

func unauthenticated(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnauthenticated, fmt.Sprintf(format, args...))
}

// Verify authenticates a proof header and authorizes its role.
//
//nolint:gocyclo // A flat sequence of independent proof checks.
func (v *Verifier) Verify(ctx context.Context, header string) (Caller, error) {
	if header == "" {
		return Caller{}, unauthenticated("missing %s", Header)
	}
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return Caller{}, unauthenticated("proof is not base64")
	}
	var p Proof
	if err := json.Unmarshal(raw, &p); err != nil {
		return Caller{}, unauthenticated("proof is not JSON")
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Scheme != "https" || !stsHost.MatchString(u.Host) || (u.Path != "/" && u.Path != "") ||
		u.RawQuery != "" || u.User != nil || u.Fragment != "" {
		return Caller{}, unauthenticated("proof URL must be a regional STS endpoint")
	}
	if p.Body != RequestBody {
		return Caller{}, unauthenticated("proof must be a GetCallerIdentity request")
	}
	headers := http.Header{}
	for k, vs := range p.Headers {
		for _, val := range vs {
			headers.Add(k, val)
		}
	}
	if got := headers.Get(AudienceHeader); got != v.Audience {
		return Caller{}, unauthenticated("proof audience %q is not this central", got)
	}
	auth := headers.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") || !signedHeadersInclude(auth, strings.ToLower(AudienceHeader)) {
		return Caller{}, unauthenticated("the audience header must be signed")
	}
	signedAt, err := time.Parse("20060102T150405Z", headers.Get("X-Amz-Date"))
	now := v.Now()
	if err != nil || now.Sub(signedAt) > MaxAge || signedAt.Sub(now) > MaxSkew {
		return Caller{}, unauthenticated("proof is missing a valid recent X-Amz-Date")
	}

	identity, err := v.replay(ctx, u.String(), headers, p.Body)
	if err != nil {
		return Caller{}, err
	}
	account, role, session, ok := parseCallerARN(identity)
	if !ok {
		return Caller{}, fmt.Errorf("%w: caller %s is not an IAM role", ErrForbidden, identity)
	}
	for _, op := range v.Operators {
		opAccount, opRole, ok := parseRoleARN(op.RoleARN)
		if ok && opAccount == account && opRole == role {
			return Caller{RoleARN: fmt.Sprintf("arn:aws:iam::%s:role/%s", account, role), Session: session, Scope: op.Scope}, nil
		}
	}
	return Caller{}, fmt.Errorf("%w: role %s is not an operator", ErrForbidden, role)
}

func signedHeadersInclude(authorization, header string) bool {
	_, after, ok := strings.Cut(authorization, "SignedHeaders=")
	if !ok {
		return false
	}
	list, _, _ := strings.Cut(after, ",")
	for _, h := range strings.Split(strings.TrimSpace(list), ";") {
		if h == header {
			return true
		}
	}
	return false
}

type getCallerIdentityResponse struct {
	Result struct {
		Arn string `xml:"Arn"`
	} `xml:"GetCallerIdentityResult"`
}

func (v *Verifier) replay(ctx context.Context, target string, headers http.Header, body string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewBufferString(body))
	if err != nil {
		return "", unauthenticated("bad proof request")
	}
	req.Header = headers.Clone()
	req.Header.Del("Content-Length")
	resp, err := v.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("opauth: STS unreachable: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("opauth: read STS response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", unauthenticated("STS rejected the proof (%d)", resp.StatusCode)
	}
	var out getCallerIdentityResponse
	if err := xml.Unmarshal(data, &out); err != nil || out.Result.Arn == "" {
		return "", unauthenticated("unreadable STS response")
	}
	return out.Result.Arn, nil
}

// parseCallerARN accepts an assumed-role session ARN
// (arn:aws:sts::<account>:assumed-role/<role>/<session>) or a role ARN.
func parseCallerARN(arn string) (account, role, session string, ok bool) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" {
		return "", "", "", false
	}
	account, resource := parts[4], parts[5]
	switch parts[2] {
	case "sts":
		segs := strings.Split(resource, "/")
		if len(segs) == 3 && segs[0] == "assumed-role" {
			return account, segs[1], segs[2], true
		}
	case "iam":
		if a, r, ok := parseRoleARN(arn); ok {
			return a, r, "", true
		}
	}
	return "", "", "", false
}

// parseRoleARN reads arn:aws:iam::<account>:role[/path]/<name>, ignoring the
// path (an assumed-role ARN does not carry it).
func parseRoleARN(arn string) (account, role string, ok bool) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "iam" || !strings.HasPrefix(parts[5], "role/") {
		return "", "", false
	}
	segs := strings.Split(parts[5], "/")
	return parts[4], segs[len(segs)-1], segs[len(segs)-1] != ""
}

// Authorizes reports whether the caller's scope allows acting on an
// installation. Preview operators may touch only internal installations named
// staging or pr-*.
func (c Caller) Authorizes(installationID, deployment string) bool {
	if c.Scope == config.ScopeAdmin {
		return true
	}
	return c.Scope == config.ScopePreview && deployment == "internal" &&
		(installationID == "staging" || strings.HasPrefix(installationID, "pr-"))
}
