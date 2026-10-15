// Package central is Forge's client for the central Iterabase inference
// gateway's operator API (HOR-661, DES-HOR-661-02/05). Forge registers
// installations, issues and revokes their mTLS certificates, converts plans
// and extends trials. The contract lives in
// docs/architecture/v2-central-inference-gateway.md.
package central

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const (
	// HeaderOperatorAuth carries the presigned sts:GetCallerIdentity request.
	HeaderOperatorAuth = "X-Iterabase-Operator-Auth"
	// HeaderAudience binds the signature to one central host, so a captured
	// header cannot be replayed against another service that trusts STS.
	HeaderAudience = "X-Iterabase-Central-Audience"

	stsBody        = "Action=GetCallerIdentity&Version=2011-06-15"
	stsContentType = "application/x-www-form-urlencoded; charset=utf-8"
)

// Authorizer produces the operator authentication header value for one
// central audience (host).
type Authorizer interface {
	OperatorAuth(ctx context.Context, audience string) (string, error)
}

// STSAuthorizer signs sts:GetCallerIdentity with the ambient AWS credentials.
// Central replays it to STS to learn the caller's role; the credentials
// themselves never leave this process.
type STSAuthorizer struct {
	Credentials aws.CredentialsProvider
	Region      string
	// Now is the signing clock; nil means time.Now.
	Now func() time.Time
}

// presignedRequest is the JSON shape central decodes from the header.
type presignedRequest struct {
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

// OperatorAuth implements Authorizer.
func (a *STSAuthorizer) OperatorAuth(ctx context.Context, audience string) (string, error) {
	if a.Credentials == nil {
		return "", errors.New("central: no AWS credentials provider")
	}
	if a.Region == "" {
		return "", errors.New("central: AWS region is required for operator authentication")
	}
	if audience == "" {
		return "", errors.New("central: audience is required")
	}
	creds, err := a.Credentials.Retrieve(ctx)
	if err != nil {
		return "", fmt.Errorf("central: load AWS credentials: %w", err)
	}

	endpoint := fmt.Sprintf("https://sts.%s.amazonaws.com/", a.Region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(stsBody))
	if err != nil {
		return "", fmt.Errorf("central: build STS request: %w", err)
	}
	req.Header.Set("Content-Type", stsContentType)
	req.Header.Set(HeaderAudience, audience)

	sum := sha256.Sum256([]byte(stsBody))
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), "sts", a.Region, now().UTC()); err != nil {
		return "", fmt.Errorf("central: sign STS request: %w", err)
	}

	headers := make(map[string][]string, len(req.Header))
	for k, v := range req.Header {
		headers[k] = append([]string(nil), v...)
	}
	raw, err := json.Marshal(presignedRequest{URL: endpoint, Headers: headers, Body: stsBody})
	if err != nil {
		return "", fmt.Errorf("central: encode operator auth: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
