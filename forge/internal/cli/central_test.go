package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/central"
)

type fakeOperator struct {
	registered []central.RegisterRequest
	created    bool
	csr        []byte
	exported   string
}

func (f *fakeOperator) Register(_ context.Context, req central.RegisterRequest) (central.RegisterResult, error) {
	f.registered = append(f.registered, req)
	return central.RegisterResult{Created: f.created, Entry: json.RawMessage(`{"id":"` + req.ID + `"}`)}, nil
}

func (f *fakeOperator) Issue(_ context.Context, _ string, csrPEM []byte) (central.IssuedCertificate, error) {
	f.csr = csrPEM
	return central.IssuedCertificate{CertificatePEM: "CERT", CAPEM: "CA", Serial: "0a", NotAfter: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeOperator) Revoke(context.Context, string, string) (json.RawMessage, error) {
	return json.RawMessage(`{"status":"revoked"}`), nil
}

func (f *fakeOperator) SetPlan(context.Context, string, string) (json.RawMessage, error) {
	return json.RawMessage(`{"plan":"growth"}`), nil
}

func (f *fakeOperator) ExtendTrial(context.Context, string, int) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func (f *fakeOperator) Show(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"acme"}`), nil
}

func (f *fakeOperator) Export(_ context.Context, period, format string) ([]byte, error) {
	f.exported = period + "/" + format
	return []byte("installation_id,credits\n"), nil
}

func runCentral(t *testing.T, op central.Operator, args ...string) (string, string, error) {
	t.Helper()
	orig := newCentralOperator
	newCentralOperator = func(*cobra.Command) (central.Operator, error) { return op, nil }
	t.Cleanup(func() { newCentralOperator = orig })

	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"central"}, args...))
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

func TestCentralRegister(t *testing.T) {
	op := &fakeOperator{created: true}
	out, errOut, err := runCentral(t, op, "register", "--installation", "acme", "--deployment", "hosted",
		"--plan", "trial", "--trial-days", "7", "--currency", "USD")
	require.NoError(t, err)
	assert.Contains(t, errOut, "registered acme")
	assert.Contains(t, out, `"id": "acme"`)
	require.Len(t, op.registered, 1)
	assert.Equal(t, "USD", op.registered[0].BillingCurrency)
	require.NotNil(t, op.registered[0].TrialDays)
	assert.Equal(t, 7, *op.registered[0].TrialDays)
	assert.Nil(t, op.registered[0].AllowanceCredits)

	op = &fakeOperator{created: false}
	_, errOut, err = runCentral(t, op, "register", "--installation", "pr-7", "--deployment", "internal", "--plan", "none")
	require.NoError(t, err)
	assert.Contains(t, errOut, "already registered")
}

func TestCentralRegister_Allowance(t *testing.T) {
	op := &fakeOperator{created: true}
	_, _, err := runCentral(t, op, "register", "--installation", "acme-sh", "--deployment", "self_hosted_managed",
		"--plan", "none", "--allowance", "5000")
	require.NoError(t, err)
	require.Len(t, op.registered, 1)
	require.NotNil(t, op.registered[0].AllowanceCredits)
	assert.Equal(t, "5000", *op.registered[0].AllowanceCredits)
}

func TestCentralRegister_Validation(t *testing.T) {
	for name, args := range map[string][]string{
		"bad id":                {"register", "--installation", "Acme", "--deployment", "hosted", "--plan", "starter"},
		"bad deployment":        {"register", "--installation", "acme", "--deployment", "cloud", "--plan", "starter"},
		"bad currency":          {"register", "--installation", "acme", "--deployment", "hosted", "--plan", "starter", "--currency", "GBP"},
		"trial needs days":      {"register", "--installation", "acme", "--deployment", "hosted", "--plan", "trial"},
		"negative allowance":    {"register", "--installation", "acme", "--deployment", "self_hosted_managed", "--plan", "none", "--allowance", "-1"},
		"non-decimal allowance": {"register", "--installation", "acme", "--deployment", "self_hosted_managed", "--plan", "none", "--allowance", "5e3"},
	} {
		t.Run(name, func(t *testing.T) {
			op := &fakeOperator{}
			_, _, err := runCentral(t, op, args...)
			require.Error(t, err)
			assert.Empty(t, op.registered, "no request is sent for invalid input")
		})
	}
}

func TestCentralIssue_WritesFiles(t *testing.T) {
	op := &fakeOperator{}
	dir := filepath.Join(t.TempDir(), "certs")
	_, errOut, err := runCentral(t, op, "issue", "--installation", "acme", "--out", dir)
	require.NoError(t, err)
	assert.Contains(t, errOut, "issued certificate 0a for acme")

	block, _ := pem.Decode(op.csr)
	require.NotNil(t, block)
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, "acme", csr.Subject.CommonName)

	keyPEM, err := os.ReadFile(filepath.Join(dir, "tls.key"))
	require.NoError(t, err)
	kb, _ := pem.Decode(keyPEM)
	require.NotNil(t, kb)
	assert.Equal(t, "PRIVATE KEY", kb.Type)
	for _, name := range []string{"tls.key", "tls.crt", "ca.crt"} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), name)
	}
}

func TestCentralIssue_SecretManifest(t *testing.T) {
	op := &fakeOperator{}
	out, _, err := runCentral(t, op, "issue", "--installation", "pr-9", "--secret-manifest", "--namespace", "iterabase", "--secret-name", "central-client")
	require.NoError(t, err)
	assert.Contains(t, out, "type: kubernetes.io/tls")
	assert.Contains(t, out, "name: central-client")
	assert.Contains(t, out, "namespace: iterabase")
	assert.NotContains(t, out, "PRIVATE KEY", "key material is base64-encoded in data")
}

func TestCentralIssue_RequiresExactlyOneOutput(t *testing.T) {
	for _, args := range [][]string{
		{"issue", "--installation", "acme"},
		{"issue", "--installation", "acme", "--out", t.TempDir(), "--secret-manifest"},
	} {
		op := &fakeOperator{}
		_, _, err := runCentral(t, op, args...)
		require.Error(t, err)
		assert.Nil(t, op.csr)
	}
}

func TestCentralOtherCommands(t *testing.T) {
	op := &fakeOperator{}
	out, _, err := runCentral(t, op, "revoke", "--installation", "pr-9", "--reason", "preview closed")
	require.NoError(t, err)
	assert.Contains(t, out, "revoked")

	_, _, err = runCentral(t, op, "revoke", "--installation", "pr-9")
	assert.ErrorContains(t, err, "--reason")

	_, _, err = runCentral(t, op, "plan", "--installation", "acme", "--plan", "trial")
	assert.ErrorContains(t, err, "--plan")
	out, _, err = runCentral(t, op, "plan", "--installation", "acme", "--plan", "growth")
	require.NoError(t, err)
	assert.Contains(t, out, "growth")

	_, _, err = runCentral(t, op, "extend-trial", "--installation", "acme", "--days", "0")
	assert.ErrorContains(t, err, "--days")
	_, _, err = runCentral(t, op, "extend-trial", "--installation", "acme", "--days", "3")
	require.NoError(t, err)

	out, _, err = runCentral(t, op, "show", "--installation", "acme")
	require.NoError(t, err)
	assert.Contains(t, out, `"id": "acme"`)

	_, _, err = runCentral(t, op, "export", "--period", "2026-13")
	assert.ErrorContains(t, err, "YYYY-MM")
	path := filepath.Join(t.TempDir(), "export.csv")
	_, _, err = runCentral(t, op, "export", "--period", "2026-10", "--out", path)
	require.NoError(t, err)
	assert.Equal(t, "2026-10/csv", op.exported)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(data), "installation_id"))
}

func TestCentralOperator_RequiresURLAndCA(t *testing.T) {
	t.Setenv("FORGE_CENTRAL_URL", "")
	t.Setenv("FORGE_CENTRAL_CA_BUNDLE", "")
	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"central", "show", "--installation", "acme"})
	err := root.Execute()
	assert.ErrorContains(t, err, "--central-url")
}
