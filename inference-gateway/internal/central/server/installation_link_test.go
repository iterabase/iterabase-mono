package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/store"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/centrallink"
	gatewayconfig "github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/proxy"
	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/snapshot"
)

// catalog is a snapshot.Reader with fixed entries and a wildcard capability.
type catalog map[string]snapshot.CatalogEntry

func (c catalog) CatalogEntry(id string) (snapshot.CatalogEntry, bool) { e, ok := c[id]; return e, ok }
func (c catalog) ListCatalog() []snapshot.CatalogEntry {
	out := make([]snapshot.CatalogEntry, 0, len(c))
	for _, e := range c {
		out = append(out, e)
	}
	return out
}
func (c catalog) Capabilities(string) []snapshot.Capability {
	return []snapshot.Capability{{Resource: "*", Action: "*"}}
}
func (c catalog) RateLimits(string) (snapshot.IdentityRateLimits, bool) {
	return snapshot.IdentityRateLimits{}, false
}
func (c catalog) Fresh(time.Duration) bool { return true }
func (c catalog) LastRefresh() time.Time   { return time.Now() }

// writeInstallationFiles writes the files the inference-gateway chart mounts.
func writeInstallationFiles(t *testing.T, f *fixture, id, centralURL string) gatewayconfig.CentralConfig {
	t.Helper()
	cert := f.issue(id)
	require.NoError(t, f.registry.Refresh(context.Background()))
	dir := t.TempDir()
	keyDER, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	require.NoError(t, err)
	files := map[string][]byte{
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		"ca.crt":  f.authority.CAPEM(),
	}
	for name, data := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	return gatewayconfig.CentralConfig{
		URL:            strings.Replace(centralURL, "127.0.0.1", "localhost", 1),
		ClientCertFile: filepath.Join(dir, "tls.crt"),
		ClientKeyFile:  filepath.Join(dir, "tls.key"),
		CAFile:         filepath.Join(dir, "ca.crt"),
	}
}

// TestInstallationGateway_ManagedRoutesToCentral runs the installation
// gateway's real proxy handler against central over mTLS (DES-HOR-661-03):
// managed models reach central with attribution and no bearer, BYO-key models
// go direct and are never metered.
func TestInstallationGateway_ManagedRoutesToCentral(t *testing.T) {
	f := newFixture(t, 1000)
	_, _, err := f.store.Register(context.Background(),
		store.Registration{ID: "inst-a", Deployment: "hosted", Plan: "starter", BillingCurrency: "USD"}, store.Actor{ARN: "test"})
	require.NoError(t, err)

	// Central, behind a recorder of what it receives from the installation.
	var mu sync.Mutex
	var centralSaw http.Header
	inner := f.server.Handler()
	recorded := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		centralSaw = r.Header.Clone()
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	recorded.TLS = f.server.TLSConfig()
	recorded.EnableHTTP2 = true
	recorded.StartTLS()
	t.Cleanup(recorded.Close)

	link, err := centrallink.New(writeInstallationFiles(t, f, "inst-a", recorded.URL))
	require.NoError(t, err)

	var byoCalls int
	byo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		byoCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"byo","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`)
	}))
	t.Cleanup(byo.Close)

	h := proxy.NewHandler(catalog{
		"assistant": {ModelID: "assistant", BackendRef: "central", BackendKind: "managed", BackendModelID: "managed-model", Available: true},
		"byo":       {ModelID: "byo", BackendRef: "byo", BackendKind: "external", BackendURL: byo.URL + "/v1", Available: true},
	}, nil, nil, nil, slog.Default()).WithCentral(link)

	call := func(model string, stream bool) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, model, stream)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer client-key-never-forwarded")
		ctx := middleware.WithIdentityID(req.Context(), "identity-7")
		ctx = middleware.WithWorkloadContext(ctx, middleware.WorkloadContext{
			RunID: "run-7", TurnID: "turn-7", AssignedModel: model, EffectiveIdentity: "identity-7",
			WorkflowKey: "invoices/intake", WorkItemID: "wi-7", Channel: "browser",
		})
		rec := httptest.NewRecorder()
		h.ChatCompletions(rec, req.WithContext(ctx))
		return rec
	}

	for _, stream := range []bool{false, true} {
		rec := call("assistant", stream)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"model":"assistant"`, "the installation's alias comes back")
		mu.Lock()
		assert.Empty(t, centralSaw.Get("Authorization"), "no bearer to central: the certificate is the identity")
		assert.Equal(t, "invoices/intake", centralSaw.Get("X-Iterabase-Workflow-Key"))
		mu.Unlock()
	}

	var identity, run, turn, workflow, workItem, channel string
	require.NoError(t, f.db.Pool.QueryRow(context.Background(), `
		SELECT identity_id, run_id, turn_id, workflow_key, work_item_id, channel FROM central.usage_records
		WHERE installation_id = 'inst-a' LIMIT 1`).Scan(&identity, &run, &turn, &workflow, &workItem, &channel))
	assert.Equal(t, []string{"identity-7", "run-7", "turn-7", "invoices/intake", "wi-7", "browser"},
		[]string{identity, run, turn, workflow, workItem, channel})
	assert.Equal(t, "provider/model-v1", f.lastCall().body["model"])

	// BYO-key models never reach central and consume no managed credits.
	before := countRecords(t, f, "inst-a")
	centralCalls := f.callCount()
	rec := call("byo", false)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, byoCalls)
	assert.Equal(t, centralCalls, f.callCount())
	assert.Equal(t, before, countRecords(t, f, "inst-a"))

	// A revoked installation's managed calls fail; BYO keeps working.
	resp, _ := f.operator("admin", http.MethodPost, "/operator/v1/installations/inst-a/revoke", map[string]string{"reason": "test"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rec = call("assistant", false)
	assert.Equal(t, http.StatusBadGateway, rec.Code, "central's 403 is a non-retryable failure at the installation")
	assert.Contains(t, rec.Body.String(), "upstream_auth_failed")
	assert.Equal(t, http.StatusOK, call("byo", false).Code)
}
