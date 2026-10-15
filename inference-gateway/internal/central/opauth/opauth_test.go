package opauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/config"
)

const audience = "central.test"

// signedProof signs a GetCallerIdentity POST the way the Forge client does.
func signedProof(t *testing.T, signedAt time.Time, aud string, signAudience bool, mutate func(*http.Request)) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://sts.eu-west-1.amazonaws.com/", strings.NewReader(RequestBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	if signAudience {
		req.Header.Set(AudienceHeader, aud)
	}
	sum := sha256.Sum256([]byte(RequestBody))
	err = v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: "AKIDTEST", SecretAccessKey: "secret", SessionToken: "token"},
		req, hex.EncodeToString(sum[:]), "sts", "eu-west-1", signedAt)
	require.NoError(t, err)
	if !signAudience {
		req.Header.Set(AudienceHeader, aud) // added after signing: not covered
	}
	if mutate != nil {
		mutate(req)
	}
	raw, err := json.Marshal(map[string]any{"url": req.URL.String(), "headers": req.Header, "body": RequestBody})
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw)
}

// fakeSTS answers as STS would for a valid signature.
type fakeSTS struct {
	arn    string
	status int
	got    *http.Request
}

func (f *fakeSTS) Do(r *http.Request) (*http.Response, error) {
	f.got = r
	body, _ := io.ReadAll(r.Body)
	if string(body) != RequestBody {
		return nil, errors.New("unexpected body")
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	xml := `<GetCallerIdentityResponse><GetCallerIdentityResult><Arn>` + f.arn + `</Arn><UserId>AROA:s</UserId><Account>111111111111</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewBufferString(xml))}, nil
}

func verifier(sts HTTPDoer, now time.Time) *Verifier {
	return &Verifier{
		Client: sts, Audience: audience, Now: func() time.Time { return now },
		Operators: []config.Operator{
			{RoleARN: "arn:aws:iam::111111111111:role/iterabase/iterabase-hosted-forge", Scope: config.ScopeAdmin},
			{RoleARN: "arn:aws:iam::222222222222:role/ci-preview", Scope: config.ScopePreview},
		},
	}
}

func TestVerify_AcceptsAllowListedRoles(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	sts := &fakeSTS{arn: "arn:aws:sts::111111111111:assumed-role/iterabase-hosted-forge/nuno"}
	caller, err := verifier(sts, now).Verify(context.Background(), signedProof(t, now.Add(-time.Minute), audience, true, nil))
	require.NoError(t, err)
	assert.Equal(t, Caller{RoleARN: "arn:aws:iam::111111111111:role/iterabase-hosted-forge", Session: "nuno", Scope: config.ScopeAdmin}, caller)
	assert.Equal(t, "sts.eu-west-1.amazonaws.com", sts.got.URL.Host, "the proof is replayed to STS unchanged")
	assert.NotEmpty(t, sts.got.Header.Get("Authorization"))

	sts.arn = "arn:aws:sts::222222222222:assumed-role/ci-preview/gha-1"
	caller, err = verifier(sts, now).Verify(context.Background(), signedProof(t, now, audience, true, nil))
	require.NoError(t, err)
	assert.Equal(t, config.ScopePreview, caller.Scope)
}

func TestVerify_Rejects(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	good := func() string { return signedProof(t, now, audience, true, nil) }
	for name, tc := range map[string]struct {
		header string
		arn    string
		status int
		want   error
	}{
		"missing":                  {"", "", 0, ErrUnauthenticated},
		"not base64":               {"%%%", "", 0, ErrUnauthenticated},
		"wrong audience":           {signedProof(t, now, "other.test", true, nil), "", 0, ErrUnauthenticated},
		"unsigned audience":        {signedProof(t, now, audience, false, nil), "", 0, ErrUnauthenticated},
		"stale":                    {signedProof(t, now.Add(-16*time.Minute), audience, true, nil), "", 0, ErrUnauthenticated},
		"future":                   {signedProof(t, now.Add(6*time.Minute), audience, true, nil), "", 0, ErrUnauthenticated},
		"STS rejects":              {good(), "arn:aws:sts::111111111111:assumed-role/iterabase-hosted-forge/x", 403, ErrUnauthenticated},
		"unknown role":             {good(), "arn:aws:sts::111111111111:assumed-role/someone-else/x", 0, ErrForbidden},
		"same role, other account": {good(), "arn:aws:sts::999999999999:assumed-role/iterabase-hosted-forge/x", 0, ErrForbidden},
		"IAM user":                 {good(), "arn:aws:iam::111111111111:user/iterabase-hosted-forge", 0, ErrForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := verifier(&fakeSTS{arn: tc.arn, status: tc.status}, now).Verify(context.Background(), tc.header)
			assert.ErrorIs(t, err, tc.want)
		})
	}

	t.Run("non-STS URL", func(t *testing.T) {
		raw, _ := json.Marshal(Proof{URL: "https://evil.example/", Headers: map[string][]string{AudienceHeader: {audience}}, Body: RequestBody})
		_, err := verifier(&fakeSTS{}, now).Verify(context.Background(), base64.StdEncoding.EncodeToString(raw))
		assert.ErrorIs(t, err, ErrUnauthenticated)
	})
	t.Run("other action", func(t *testing.T) {
		raw, _ := json.Marshal(Proof{URL: "https://sts.eu-west-1.amazonaws.com/", Body: "Action=AssumeRole&Version=2011-06-15"})
		_, err := verifier(&fakeSTS{}, now).Verify(context.Background(), base64.StdEncoding.EncodeToString(raw))
		assert.ErrorIs(t, err, ErrUnauthenticated)
	})
}

func TestCaller_Authorizes(t *testing.T) {
	admin := Caller{Scope: config.ScopeAdmin}
	preview := Caller{Scope: config.ScopePreview}
	assert.True(t, admin.Authorizes("acme", "hosted"))
	assert.True(t, preview.Authorizes("pr-42", "internal"))
	assert.True(t, preview.Authorizes("staging", "internal"))
	assert.False(t, preview.Authorizes("acme", "internal"))
	assert.False(t, preview.Authorizes("pr-42", "hosted"))
}
