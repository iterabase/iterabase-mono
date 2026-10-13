package e2e_test

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/artifacts"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

func TestDeployedBrowserProxyUsesStableOriginAcrossGoOwnedUpstreams(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(response, "first:"+request.URL.Path)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(response, "second:"+request.URL.Path)
	}))
	defer second.Close()

	proxy := newDeployedBrowserProxy(t, first.URL, http.DefaultTransport)
	defer proxy.Close()
	stableURL := proxy.URL()
	assertBrowserProxyBody(t, stableURL+"/dashboard", "first:/dashboard")

	proxy.clearTarget()
	response, err := http.Get(stableURL + "/v1/work-events") //nolint:gosec // loopback test server
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("browser outage status=%d want %d", response.StatusCode, http.StatusBadGateway)
	}

	proxy.setTarget(second.URL, http.DefaultTransport)
	if proxy.URL() != stableURL {
		t.Fatalf("browser origin changed: got %q want %q", proxy.URL(), stableURL)
	}
	assertBrowserProxyBody(t, stableURL+"/dashboard", "second:/dashboard")
}

func assertBrowserProxyBody(t *testing.T, url, want string) {
	t.Helper()
	response, err := http.Get(url) //nolint:gosec // loopback test server
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != want {
		t.Fatalf("proxy response status=%d body=%q want %q", response.StatusCode, body, want)
	}
}

func TestBrowserCoordinatorRequiresExplicitBoundedRecovery(t *testing.T) {
	coordinator := newBrowserCoordinator()
	defer coordinator.Close()

	response, err := http.Post(coordinator.URL()+"/restart", "application/json", nil) //nolint:gosec // loopback test server
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("restart status=%d", response.StatusCode)
	}
	<-coordinator.restart
	coordinator.setStatus("outage", "intentional test outage")
	response, err = http.Post(coordinator.URL()+"/recover", "application/json", nil) //nolint:gosec // loopback test server
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("recover status=%d", response.StatusCode)
	}
	<-coordinator.recover
	status, _ := coordinator.snapshot()
	if status != "recovering" {
		t.Fatalf("coordinator status=%q want recovering", status)
	}
}

func TestCollectBrowserEvidenceRedactsTextBeforeRetentionAndRemovesRawCopies(t *testing.T) {
	state, artifactRoot := newBrowserEvidenceCollectionFixture(t, false)
	if err := collectBrowserEvidence(state, artifactRoot); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		filepath.Join(artifactRoot, "report.json"),
		filepath.Join(artifactRoot, "network.jsonl"),
		filepath.Join(artifactRoot, "sanitized.marker"),
		filepath.Join(state.outputDir, "playwright-npm-ci.log"),
		filepath.Join(state.outputDir, "playwright-test.log"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("raw browser text evidence still exists at %s: %v", path, err)
		}
	}

	retainedRoot := filepath.Join(state.diagnosticsDir, "browser")
	for _, name := range []string{"report.json", "network.jsonl", "playwright-npm-ci.log", "playwright-test.log"} {
		data, err := os.ReadFile(filepath.Join(retainedRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, variant := range browserEvidenceCredentialVariants(state.browserSecrets()...) {
			if bytes.Contains(data, []byte(variant)) {
				t.Fatalf("retained %s contains credential variant %q", name, variant)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(retainedRoot, "failure-evidence", "screenshot.png")); err != nil {
		t.Fatalf("sanitized opaque evidence was not retained: %v", err)
	}
}

func TestCollectBrowserEvidenceRejectsUnsafeOpaqueAfterRetainingSafeText(t *testing.T) {
	state, artifactRoot := newBrowserEvidenceCollectionFixture(t, true)
	err := collectBrowserEvidence(state, artifactRoot)
	if err == nil || !strings.Contains(err.Error(), "validate sanitized opaque browser evidence") {
		t.Fatalf("collection error=%v", err)
	}

	retainedReport := filepath.Join(state.diagnosticsDir, "browser", "report.json")
	data, readErr := os.ReadFile(retainedReport)
	if readErr != nil {
		t.Fatalf("sanitized report was not retained: %v", readErr)
	}
	if bytes.Contains(data, []byte(state.workKey)) {
		t.Fatalf("sanitized report retained work key: %s", data)
	}
	if _, statErr := os.Stat(filepath.Join(state.diagnosticsDir, "browser", "failure-evidence")); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe opaque evidence was retained: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(artifactRoot, "report.json")); !os.IsNotExist(statErr) {
		t.Fatalf("raw report still exists: %v", statErr)
	}
}

func newBrowserEvidenceCollectionFixture(t *testing.T, unsafeOpaque bool) (*deployedState, string) {
	t.Helper()
	outputDir := t.TempDir()
	state := &deployedState{
		outputDir: outputDir, diagnosticsDir: filepath.Join(outputDir, "diagnostics"),
		adminKey: "cp-browser-admin", workKey: "cp-browser-work",
		operator: webSession{password: "browser-operator-password"},
	}
	state.redactor = redact.New(state.browserSecrets()...)
	artifactRoot := filepath.Join(outputDir, "browser-artifacts")
	if err := os.MkdirAll(filepath.Join(artifactRoot, "safe-opaque"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		filepath.Join(artifactRoot, "report.json"):                   []byte(`{"received":"` + state.workKey + `","encoded":"` + base64.StdEncoding.EncodeToString([]byte(state.workKey)) + `"}`),
		filepath.Join(artifactRoot, "network.jsonl"):                 []byte(`{"message":"` + state.adminKey + `"}`),
		filepath.Join(artifactRoot, "sanitized.marker"):              []byte("Playwright evidence sanitized\n"),
		filepath.Join(outputDir, "playwright-npm-ci.log"):            []byte("npm completed\n"),
		filepath.Join(outputDir, "playwright-test.log"):              []byte("test password=" + state.operator.password + "\n"),
		filepath.Join(artifactRoot, "safe-opaque", "screenshot.png"): {0, 1, 2, 3},
	}
	if unsafeOpaque {
		files[filepath.Join(artifactRoot, "safe-opaque", "screenshot.png")] = []byte(state.workKey)
	}
	for path, content := range files {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return state, artifactRoot
}

func TestValidateBrowserEvidenceRejectsCredentialsInTextAndTraceArchives(t *testing.T) {
	secret := "cp-browser-secret"
	for name, create := range map[string]func(string) error{
		"literal-text": func(path string) error { return os.WriteFile(path, []byte("Bearer "+secret), 0o600) },
		"base64-text": func(path string) error {
			return os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString([]byte(secret))), 0o600)
		},
		"trace-entry": func(path string) error {
			return writeBrowserTrace(path, []byte(`{"authorization":"Bearer `+secret+`"}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "evidence.txt")
			if name == "trace-entry" {
				path = filepath.Join(root, "trace.zip")
			}
			if err := create(path); err != nil {
				t.Fatal(err)
			}
			err := validateBrowserEvidence([]artifacts.Entry{{Name: name, Source: path, Kind: artifacts.Text}}, secret)
			if err == nil || !strings.Contains(err.Error(), "credential literal") {
				t.Fatalf("validation error=%v", err)
			}
		})
	}
}

func TestValidateBrowserEvidenceAcceptsSanitizedSyntheticArtifacts(t *testing.T) {
	root := t.TempDir()
	text := filepath.Join(root, "network.jsonl")
	if err := os.WriteFile(text, []byte(`{"path":"/v1/work-items","status":200}`), 0o600); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(root, "trace.zip")
	if err := writeBrowserTrace(trace, []byte(`{"authorization":"Bearer ********"}`)); err != nil {
		t.Fatal(err)
	}
	entries := []artifacts.Entry{
		{Name: "network", Source: text, Kind: artifacts.Text},
		{Name: "trace", Source: trace, Kind: artifacts.SafeSyntheticOpaque},
	}
	if err := validateBrowserEvidence(entries, "cp-browser-secret"); err != nil {
		t.Fatal(err)
	}
}

func writeBrowserTrace(path string, content []byte) error {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	entry, err := archive.Create("trace.network")
	if err != nil {
		return err
	}
	if _, err := entry.Write(content); err != nil {
		return err
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buffer.Bytes(), 0o600)
}
