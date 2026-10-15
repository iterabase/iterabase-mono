package central

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func decodeAuth(t *testing.T, header string) presignedRequest {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(header)
	require.NoError(t, err)
	var p presignedRequest
	require.NoError(t, json.Unmarshal(raw, &p))
	return p
}

func TestSTSAuthorizer_SignsAudience(t *testing.T) {
	a := &STSAuthorizer{
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "session-token"),
		Region:      "eu-west-1",
		Now:         func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) },
	}
	header, err := a.OperatorAuth(context.Background(), "central.iterabase.com")
	require.NoError(t, err)

	p := decodeAuth(t, header)
	assert.Equal(t, "https://sts.eu-west-1.amazonaws.com/", p.URL)
	assert.Equal(t, stsBody, p.Body)
	assert.Equal(t, []string{"central.iterabase.com"}, p.Headers[HeaderAudience])
	assert.Equal(t, []string{"20261014T120000Z"}, p.Headers["X-Amz-Date"])
	assert.Equal(t, []string{"session-token"}, p.Headers["X-Amz-Security-Token"])

	auth := strings.Join(p.Headers["Authorization"], "")
	assert.Contains(t, auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261014/eu-west-1/sts/aws4_request")
	signed := auth[strings.Index(auth, "SignedHeaders=")+len("SignedHeaders="):]
	signed = signed[:strings.Index(signed, ",")]
	assert.Contains(t, strings.Split(signed, ";"), "x-iterabase-central-audience")
	assert.Contains(t, strings.Split(signed, ";"), "host")
	assert.NotContains(t, header, "secret", "the secret key never leaves the process")
}

func TestSTSAuthorizer_RequiresRegionAndAudience(t *testing.T) {
	creds := credentials.NewStaticCredentialsProvider("a", "b", "")
	_, err := (&STSAuthorizer{Credentials: creds}).OperatorAuth(context.Background(), "c")
	assert.ErrorContains(t, err, "region")
	_, err = (&STSAuthorizer{Credentials: creds, Region: "eu-west-1"}).OperatorAuth(context.Background(), "")
	assert.ErrorContains(t, err, "audience")
}

type staticAuth string

func (s staticAuth) OperatorAuth(_ context.Context, audience string) (string, error) {
	return string(s) + "@" + audience, nil
}

// tlsServer starts an HTTPS test server and a client pinned to its certificate.
func tlsServer(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	ts := httptest.NewTLSServer(h)
	t.Cleanup(ts.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	c, err := NewClient(ts.URL, ca, staticAuth("token"))
	require.NoError(t, err)
	return c, ts
}

func TestClient_RegisterIdempotency(t *testing.T) {
	for _, tc := range []struct {
		status  int
		created bool
		errCode string
	}{
		{http.StatusCreated, true, ""},
		{http.StatusOK, false, ""},
		{http.StatusConflict, false, "installation_conflict"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			var gotAuth, gotPath string
			var gotBody map[string]any
			c, _ := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get(HeaderOperatorAuth)
				gotPath = r.Method + " " + r.URL.Path
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &gotBody)
				w.WriteHeader(tc.status)
				if tc.errCode != "" {
					_, _ = w.Write([]byte(`{"error":{"message":"differs","type":"invalid_request_error","code":"` + tc.errCode + `"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"pr-12"}`))
			})
			days := 7
			res, err := c.Register(context.Background(), RegisterRequest{ID: "pr-12", Deployment: "internal", Plan: "none", BillingCurrency: "EUR", TrialDays: &days})
			assert.Equal(t, "token@127.0.0.1", gotAuth)
			assert.Equal(t, "POST /operator/v1/installations", gotPath)
			assert.Equal(t, map[string]any{"id": "pr-12", "deployment": "internal", "plan": "none", "billing_currency": "EUR", "trial_days": float64(7)}, gotBody)
			if tc.errCode != "" {
				var apiErr *APIError
				require.ErrorAs(t, err, &apiErr)
				assert.Equal(t, tc.errCode, apiErr.Code)
				assert.Contains(t, err.Error(), "forge central plan")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.created, res.Created)
			assert.JSONEq(t, `{"id":"pr-12"}`, string(res.Entry))
		})
	}
}

// TestClient_RegisterAllowanceIsADecimalString pins the wire shape central's
// operator API decodes (inference-gateway central/server registerRequest):
// allowance_credits is a JSON string, never a number.
func TestClient_RegisterAllowanceIsADecimalString(t *testing.T) {
	var raw []byte
	c, _ := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"acme-sh"}`))
	})
	for _, allowance := range []string{"5000", "0"} {
		_, err := c.Register(context.Background(), RegisterRequest{ID: "acme-sh", Deployment: "self_hosted_managed", Plan: "none",
			BillingCurrency: "USD", AllowanceCredits: &allowance})
		require.NoError(t, err)
		assert.JSONEq(t, `{"id":"acme-sh","deployment":"self_hosted_managed","plan":"none","billing_currency":"USD","allowance_credits":"`+allowance+`"}`, string(raw))
	}
}

func TestClient_DoesNotTrustSystemOrOtherRoots(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer ts.Close()
	unrelated := mustSelfSigned(t)
	c, err := NewClient(ts.URL, unrelated, staticAuth("t"))
	require.NoError(t, err)
	_, err = c.Show(context.Background(), "acme")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "certificate")
}

func TestClient_IssueAndPaths(t *testing.T) {
	var paths []string
	c, _ := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		switch {
		case strings.HasSuffix(r.URL.Path, "/certificates"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			assert.Contains(t, body["csr_pem"], "CERTIFICATE REQUEST")
			_, _ = w.Write([]byte(`{"certificate_pem":"CERT","ca_pem":"CA","serial":"ab12","not_after":"2027-10-14T00:00:00Z"}`))
		case strings.HasPrefix(r.URL.Path, "/operator/v1/exports/"):
			_, _ = w.Write([]byte("a,b\n"))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	ctx := context.Background()
	kc, err := NewKeyAndCSR("acme")
	require.NoError(t, err)
	cert, err := c.Issue(ctx, "acme", kc.CSRPEM)
	require.NoError(t, err)
	assert.Equal(t, "ab12", cert.Serial)
	_, err = c.Revoke(ctx, "acme", "closed")
	require.NoError(t, err)
	_, err = c.SetPlan(ctx, "acme", "growth")
	require.NoError(t, err)
	_, err = c.ExtendTrial(ctx, "acme", 3)
	require.NoError(t, err)
	_, err = c.Show(ctx, "acme")
	require.NoError(t, err)
	data, err := c.Export(ctx, "2026-10", "csv")
	require.NoError(t, err)
	assert.Equal(t, "a,b\n", string(data))
	assert.Equal(t, []string{
		"POST /operator/v1/installations/acme/certificates",
		"POST /operator/v1/installations/acme/revoke",
		"POST /operator/v1/installations/acme/plan",
		"POST /operator/v1/installations/acme/trial-extension",
		"GET /operator/v1/installations/acme",
		"GET /operator/v1/exports/2026-10?format=csv",
	}, paths)
}

func TestAPIError_Mapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		code   string
		hint   string
	}{
		{403, `{"error":{"code":"operator_forbidden","message":"preview scope"}}`, "operator_forbidden", "preview-scoped"},
		{401, `{"error":{"code":"operator_unauthenticated"}}`, "operator_unauthenticated", "aws sso login"},
		{403, `not json`, "forbidden", "allow-list"},
		{404, `{"error":{"code":"installation_not_found"}}`, "installation_not_found", "register it first"},
		{503, `{"error":{"code":"upstream_unavailable"}}`, "upstream_unavailable", "retry"},
	} {
		err := decodeAPIError(tc.status, []byte(tc.body))
		assert.Equal(t, tc.code, err.Code)
		assert.Contains(t, err.Error(), tc.hint)
	}
}

func TestNewClient_Validation(t *testing.T) {
	_, err := NewClient("http://central", mustSelfSigned(t), staticAuth(""))
	assert.ErrorContains(t, err, "https")
	_, err = NewClient("https://central", []byte("nope"), staticAuth(""))
	assert.ErrorContains(t, err, "PEM")
}

func TestNewKeyAndCSR(t *testing.T) {
	kc, err := NewKeyAndCSR("acme")
	require.NoError(t, err)

	block, _ := pem.Decode(kc.CSRPEM)
	require.NotNil(t, block)
	assert.Equal(t, "CERTIFICATE REQUEST", block.Type)
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	require.NoError(t, err)
	require.NoError(t, csr.CheckSignature())
	assert.Equal(t, "acme", csr.Subject.CommonName)
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	require.True(t, ok)
	assert.Equal(t, elliptic.P256(), pub.Curve)

	kb, _ := pem.Decode(kc.KeyPEM)
	require.NotNil(t, kb)
	key, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	require.NoError(t, err)
	assert.True(t, key.(*ecdsa.PrivateKey).PublicKey.Equal(pub), "CSR carries the generated key")
}

func TestWriteCertificateFiles_Modes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	// An existing world-readable file is tightened, not left as is.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.key"), []byte("old"), 0o644))

	require.NoError(t, WriteCertificateFiles(dir, []byte("KEY"), IssuedCertificate{CertificatePEM: "CERT", CAPEM: "CA"}))
	for name, want := range map[string]string{"tls.key": "KEY", "tls.crt": "CERT", "ca.crt": "CA"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), name)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, want, string(data))
	}
}

func TestSecretManifest(t *testing.T) {
	out := SecretManifest("iterabase-system", "iterabase-central-client", []byte("KEY"),
		IssuedCertificate{CertificatePEM: "CERT", CAPEM: "CA", Serial: "0a1b"})
	var doc struct {
		Kind     string `yaml:"kind"`
		Type     string `yaml:"type"`
		Metadata struct {
			Name      string            `yaml:"name"`
			Namespace string            `yaml:"namespace"`
			Labels    map[string]string `yaml:"labels"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(out), &doc))
	assert.Equal(t, "Secret", doc.Kind)
	assert.Equal(t, "kubernetes.io/tls", doc.Type)
	assert.Equal(t, "iterabase-central-client", doc.Metadata.Name)
	assert.Equal(t, "iterabase-system", doc.Metadata.Namespace)
	assert.Equal(t, "0a1b", doc.Metadata.Labels["iterabase.com/central-certificate-serial"])
	for k, want := range map[string]string{"tls.crt": "CERT", "tls.key": "KEY", "ca.crt": "CA"} {
		got, err := base64.StdEncoding.DecodeString(doc.Data[k])
		require.NoError(t, err)
		assert.Equal(t, want, string(got))
	}
}

func TestInstallationIDPattern(t *testing.T) {
	for _, ok := range []string{"acme", "pr-123", "staging", "abc"} {
		assert.True(t, InstallationIDPattern.MatchString(ok), ok)
	}
	for _, bad := range []string{"", "a", "ab", "Acme", "1abc", "abc-", "a_b", strings.Repeat("a", 64)} {
		assert.False(t, InstallationIDPattern.MatchString(bad), bad)
	}
}

// mustSelfSigned returns an unrelated self-signed CA certificate PEM.
func mustSelfSigned(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
