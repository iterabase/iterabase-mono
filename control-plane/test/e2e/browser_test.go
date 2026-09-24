package e2e_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/artifacts"
	playwrightstage "github.com/nunocgoncalves/iterabase-mono/testkit/e2e/playwright"
)

const (
	browserDoneTitle       = "Browser completed case"
	browserApprovalTitle   = "Browser approval case"
	browserArtifactTitle   = "Browser upload case"
	browserReconnectTitle  = "Browser reconnect case"
	browserArtifactName    = "browser-evidence.txt"
	browserArtifactContent = "Synthetic browser download evidence for HOR-483.\n"
)

// deployedBrowserProxy gives Chromium one stable loopback origin while Go keeps
// ownership of the verified service port-forward. Its dynamic upstream lets the
// browser prove SSE recovery across an intentional Go-controlled network break.
type deployedBrowserProxy struct {
	server *httptest.Server

	mu        sync.RWMutex
	target    *url.URL
	transport http.RoundTripper
}

func newDeployedBrowserProxy(t *testing.T, upstream string, transport http.RoundTripper) *deployedBrowserProxy {
	t.Helper()
	proxy := &deployedBrowserProxy{}
	proxy.setTarget(upstream, transport)
	reverse := &httputil.ReverseProxy{
		Director:      func(*http.Request) {},
		Transport:     proxy,
		FlushInterval: -1,
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, err error) {
			http.Error(response, "deployed browser upstream unavailable: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.server = httptest.NewServer(reverse)
	return proxy
}

func (proxy *deployedBrowserProxy) setTarget(rawURL string, transport http.RoundTripper) {
	target, err := url.Parse(rawURL)
	if err != nil {
		panic(fmt.Sprintf("parse deployed browser upstream %q: %v", rawURL, err))
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	proxy.mu.Lock()
	proxy.target = target
	proxy.transport = transport
	proxy.mu.Unlock()
}

func (proxy *deployedBrowserProxy) clearTarget() {
	proxy.mu.Lock()
	proxy.target = nil
	proxy.mu.Unlock()
}

func (proxy *deployedBrowserProxy) RoundTrip(request *http.Request) (*http.Response, error) {
	proxy.mu.RLock()
	target := proxy.target
	transport := proxy.transport
	proxy.mu.RUnlock()
	if target == nil || transport == nil {
		return nil, fmt.Errorf("verified deployed API forward is unavailable")
	}
	forwarded := request.Clone(request.Context())
	forwarded.URL.Scheme = target.Scheme
	forwarded.URL.Host = target.Host
	forwarded.Host = target.Host
	return transport.RoundTrip(forwarded)
}

func (proxy *deployedBrowserProxy) URL() string { return proxy.server.URL }
func (proxy *deployedBrowserProxy) Close()      { proxy.server.Close() }

type browserCoordinator struct {
	server  *httptest.Server
	restart chan struct{}
	recover chan struct{}

	mu     sync.RWMutex
	status string
	detail string
}

func newBrowserCoordinator() *browserCoordinator {
	coordinator := &browserCoordinator{
		restart: make(chan struct{}, 1), recover: make(chan struct{}, 1), status: "ready",
	}
	coordinator.server = httptest.NewServer(http.HandlerFunc(coordinator.serveHTTP))
	return coordinator
}

func (coordinator *browserCoordinator) serveHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/status":
		status, detail := coordinator.snapshot()
		_, _ = fmt.Fprintf(response, `{"status":%q,"detail":%q}`, status, detail)
	case request.Method == http.MethodPost && request.URL.Path == "/restart":
		status, _ := coordinator.snapshot()
		if status != "ready" {
			http.Error(response, `{"error":"restart already requested"}`, http.StatusConflict)
			return
		}
		coordinator.setStatus("requested", "Go will interrupt the verified API forward")
		coordinator.restart <- struct{}{}
		response.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(response, `{"status":"requested"}`)
	case request.Method == http.MethodPost && request.URL.Path == "/recover":
		status, _ := coordinator.snapshot()
		if status != "outage" {
			http.Error(response, `{"error":"browser outage is not active"}`, http.StatusConflict)
			return
		}
		coordinator.setStatus("recovering", "Go is restoring the verified API forward")
		coordinator.recover <- struct{}{}
		response.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(response, `{"status":"recovering"}`)
	default:
		http.NotFound(response, request)
	}
}

func (coordinator *browserCoordinator) setStatus(status, detail string) {
	coordinator.mu.Lock()
	coordinator.status = status
	coordinator.detail = detail
	coordinator.mu.Unlock()
}

func (coordinator *browserCoordinator) snapshot() (string, string) {
	coordinator.mu.RLock()
	defer coordinator.mu.RUnlock()
	return coordinator.status, coordinator.detail
}

func (coordinator *browserCoordinator) URL() string { return coordinator.server.URL }
func (coordinator *browserCoordinator) Close()      { coordinator.server.Close() }

func setupBrowserFixturesStage(t *testing.T, state *deployedState) {
	t.Helper()
	state.captureBootstrapKeys(t)
	state.createWorkIdentity(t, "browser-journeys@local")
	state.applyWorkFixture(t)
	state.applyBrowserArtifactWorkflow(t)

	artifactID := state.createBrowserArtifact(t)
	done := state.startBrowserWorkItem(t, "e2e/manual-review", browserDoneTitle, "hor-483-browser-done", []any{map[string]any{
		"artifactId": artifactID, "role": "source", "metadata": map[string]any{"name": browserArtifactName},
	}})
	status, body := state.requestJSON(t, http.MethodPost, "/v1/work-blockers/"+done.Blocker.ID+"/responses", state.workKey,
		map[string]any{"outcome": "accepted", "response": map[string]any{"approved": true}})
	requireStatus(t, status, http.StatusOK, body)
	updated := state.databaseQuery(t, fmt.Sprintf(`WITH updated AS (
UPDATE work.artifact_links SET role='output'
WHERE artifact_id='%s' AND work_item_id='%s' RETURNING 1)
SELECT count(*) FROM updated`, artifactID, done.ID))
	if updated != "1" {
		t.Fatalf("link browser download as synthetic output: updated=%q want 1", updated)
	}

	state.startBrowserWorkItem(t, "e2e/manual-review", browserApprovalTitle, "hor-483-browser-approval", nil)
	state.startBrowserWorkItem(t, "e2e/browser-artifact", browserArtifactTitle, "hor-483-browser-artifact", nil)

	state.browserProxy = newDeployedBrowserProxy(t, state.apiForward.URL, state.apiClient.Transport)
}

func (state *deployedState) applyBrowserArtifactWorkflow(t *testing.T) {
	t.Helper()
	manifest := `apiVersion: platform.iterabase.com/v1alpha1
kind: Workflow
metadata:
  name: deployed-browser-artifact
  namespace: iterabase-system
spec:
  key: e2e/browser-artifact
  version: "1"
  poolRef: paused-e2e
  source: {type: manual_api}
  graph:
    entryNode: provide
    maxTransitions: 4
    nodes:
      - key: provide
        label: {en: Provide evidence, pt: Fornecer evidência}
        kind: human_gate
        outcomes: [provided]
        resultPresentation:
          outcomes:
            - outcome: provided
              summary: {en: Evidence provided, pt: Evidência fornecida}
          fields:
            - path: [note]
              label: {en: Note, pt: Nota}
        humanGate:
          type: artifact
          title: {en: Evidence file required, pt: Ficheiro de evidência necessário}
          description: {en: Upload the synthetic evidence file., pt: Carregue o ficheiro de evidência sintético.}
          responseSchema:
            type: object
            additionalProperties: false
            properties:
              note: {type: string}
          presentation:
            outcomes: [{en: Provided, pt: Fornecido}]
            fields:
              - key: note
                label: {en: Note, pt: Nota}
    terminalOutcomes: [{node: provide, outcome: provided}]
  presentation:
    workflowTitle: Browser evidence review
    personaName: Operations reviewer
    locale: en
`
	path := state.writeManifest(t, "browser-artifact-workflow.yaml", manifest)
	state.kubectl(t, 30*time.Second, "apply", "-f", path)
	state.kubectl(t, 3*time.Minute, "wait", "--for=jsonpath={.status.ready}=true", "workflow/deployed-browser-artifact", "-n", controlPlaneNamespace, "--timeout=2m")
}

func (state *deployedState) createBrowserArtifact(t *testing.T) string {
	t.Helper()
	content := []byte(browserArtifactContent)
	digestBytes := sha256.Sum256(content)
	status, body := state.request(t, http.MethodPost, "/v1/artifacts", state.workKey, content, map[string]string{
		"Content-Type": "text/plain", "X-Artifact-SHA256": "sha256:" + hex.EncodeToString(digestBytes[:]),
		"X-Artifact-Source": "hor-483-browser-synthetic",
	})
	requireStatus(t, status, http.StatusCreated, body)
	var artifact artifactMetadata
	mustDecode(t, body, &artifact)
	if artifact.ID == "" || artifact.State != "available" {
		t.Fatalf("create browser artifact: %s", safeResponse(body))
	}
	return artifact.ID
}

func (state *deployedState) startBrowserWorkItem(t *testing.T, workflowKey, title, idempotencyKey string, artifactRefs []any) workItemResponse {
	t.Helper()
	payload := workStartBody(title)
	payload["workflowKey"] = workflowKey
	if len(artifactRefs) > 0 {
		payload["artifactRefs"] = artifactRefs
	}
	status, body := state.requestJSONWithHeaders(t, http.MethodPost, "/v1/work-items", state.workKey, payload,
		map[string]string{"Idempotency-Key": idempotencyKey})
	requireStatus(t, status, http.StatusCreated, body)
	var item workItemResponse
	mustDecode(t, body, &item)
	if item.ID == "" {
		t.Fatalf("browser work fixture %q has no ID", title)
	}
	state.workItemID = item.ID
	return waitForBlockedWorkItem(t, state)
}

func runPlaywrightJourneysStage(t *testing.T, state *deployedState) {
	t.Helper()
	coordinator := newBrowserCoordinator()
	defer coordinator.Close()

	artifactRoot := filepath.Join(state.outputDir, "browser-artifacts")
	if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
		t.Fatalf("create browser artifact root: %v", err)
	}
	packageDirectory := filepath.Join(state.controlRoot, "test", "e2e", "playwright")
	runner := playwrightstage.Runner{Executor: state.runner, Redactor: state.redactor}
	browserContext, cancelBrowser := context.WithCancel(state.ctx)
	defer cancelBrowser()
	runResult := make(chan error, 1)
	go func() {
		err := runner.Run(browserContext, playwrightstage.Invocation{
			Directory: packageDirectory,
			Args:      []string{"--project=chromium"},
			Env: map[string]string{
				"ITERABASE_BROWSER_ENDPOINT":        state.browserProxy.URL(),
				"ITERABASE_BROWSER_WORK_KEY":        state.workKey,
				"ITERABASE_BROWSER_COORDINATOR":     coordinator.URL(),
				"ITERABASE_BROWSER_ARTIFACT_ROOT":   artifactRoot,
				"ITERABASE_BROWSER_DONE_TITLE":      browserDoneTitle,
				"ITERABASE_BROWSER_APPROVAL_TITLE":  browserApprovalTitle,
				"ITERABASE_BROWSER_UPLOAD_TITLE":    browserArtifactTitle,
				"ITERABASE_BROWSER_RECONNECT_TITLE": browserReconnectTitle,
				"ITERABASE_BROWSER_DOWNLOAD_NAME":   browserArtifactName,
				"ITERABASE_BROWSER_DOWNLOAD_BODY":   browserArtifactContent,
			},
			Timeout: 12 * time.Minute, ArtifactDir: filepath.Join(state.outputDir, "playwright-stage-artifacts"),
		})
		if err == nil {
			err = errPlaywrightCompleted
		}
		runResult <- err
	}()

	var runErr error
	runnerFinished := false
	for runErr == nil {
		select {
		case runErr = <-runResult:
			runnerFinished = true
		case <-coordinator.restart:
			coordinator.setStatus("restarting", "Go is closing the verified API forward")
			state.stopAPIForward(t)
			coordinator.setStatus("outage", "The real browser/API boundary is intentionally unavailable")
			select {
			case <-coordinator.recover:
				state.openAPI(t)
				state.startBrowserWorkItem(t, "e2e/manual-review", browserReconnectTitle, "hor-483-browser-reconnect", nil)
				coordinator.setStatus("ready", "The verified API forward and replayable SSE stream recovered")
			case runErr = <-runResult:
				runnerFinished = true
			case <-time.After(45 * time.Second):
				runErr = fmt.Errorf("Playwright did not request bounded browser API recovery")
			}
		}
	}
	if !runnerFinished {
		cancelBrowser()
		runErr = errors.Join(runErr, <-runResult)
	}
	if errors.Is(runErr, errPlaywrightCompleted) {
		runErr = nil
	}

	evidenceErr := collectBrowserEvidence(state, artifactRoot)
	if err := errors.Join(runErr, evidenceErr); err != nil {
		t.Fatalf("run deployed Playwright journeys: %v", err)
	}
}

var errPlaywrightCompleted = errors.New("playwright completed")

func collectBrowserEvidence(state *deployedState, artifactRoot string) error {
	marker := filepath.Join(artifactRoot, "sanitized.marker")
	if _, err := os.Stat(marker); err != nil {
		return fmt.Errorf("Playwright did not produce the sanitized artifact marker: %w", err)
	}
	textEntries := []artifacts.Entry{
		{Name: "report.json", Source: filepath.Join(artifactRoot, "report.json"), Kind: artifacts.Text},
		{Name: "network.jsonl", Source: filepath.Join(artifactRoot, "network.jsonl"), Kind: artifacts.Text},
		{Name: "sanitized.marker", Source: marker, Kind: artifacts.Text},
	}
	for _, output := range []string{"playwright-npm-ci.log", "playwright-test.log"} {
		path := filepath.Join(state.outputDir, output)
		if _, err := os.Stat(path); err == nil {
			textEntries = append(textEntries, artifacts.Entry{Name: output, Source: path, Kind: artifacts.Text})
		}
	}

	secrets := []string{state.adminKey, state.tokenKey, state.workKey}
	state.redactor.Add(browserEvidenceCredentialVariants(secrets...)...)
	stagingRoot, err := os.MkdirTemp(state.outputDir, "browser-sanitized-text-")
	if err != nil {
		return fmt.Errorf("create sanitized browser evidence staging directory: %w", err)
	}
	defer os.RemoveAll(stagingRoot)
	if err := artifacts.Collect(textEntries, stagingRoot, state.redactor); err != nil {
		return fmt.Errorf("stage sanitized browser text evidence: %w", err)
	}
	stagedTextEntries := make([]artifacts.Entry, 0, len(textEntries))
	for _, entry := range textEntries {
		stagedTextEntries = append(stagedTextEntries, artifacts.Entry{
			Name: entry.Name, Source: filepath.Join(stagingRoot, filepath.Clean(entry.Name)), Kind: artifacts.Text,
		})
	}
	if err := validateBrowserEvidence(stagedTextEntries, secrets...); err != nil {
		return fmt.Errorf("validate sanitized browser text evidence: %w", err)
	}

	destination := filepath.Join(state.diagnosticsDir, "browser")
	if err := artifacts.Collect(stagedTextEntries, destination, state.redactor); err != nil {
		return fmt.Errorf("retain sanitized browser text evidence: %w", err)
	}
	for _, entry := range textEntries {
		if err := os.Remove(entry.Source); err != nil {
			return fmt.Errorf("remove raw browser text evidence %s: %w", entry.Source, err)
		}
	}

	opaqueEntries := []artifacts.Entry{{
		Name: "failure-evidence", Source: filepath.Join(artifactRoot, "safe-opaque"), Kind: artifacts.SafeSyntheticOpaque,
	}}
	if err := validateBrowserEvidence(opaqueEntries, secrets...); err != nil {
		return fmt.Errorf("validate sanitized opaque browser evidence: %w", err)
	}
	if err := artifacts.Collect(opaqueEntries, destination, state.redactor); err != nil {
		return fmt.Errorf("retain sanitized opaque browser evidence: %w", err)
	}
	return nil
}

func browserEvidenceCredentialVariants(secrets ...string) []string {
	variants := make([]string, 0, len(secrets)*2)
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		variants = append(variants, secret, base64.StdEncoding.EncodeToString([]byte(secret)))
	}
	return variants
}

func validateBrowserEvidence(entries []artifacts.Entry, secrets ...string) error {
	variants := browserEvidenceCredentialVariants(secrets...)
	for _, entry := range entries {
		err := filepath.Walk(entry.Source, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, variant := range variants {
				if bytes.Contains(data, []byte(variant)) {
					return fmt.Errorf("browser evidence %s contains a credential literal", path)
				}
			}
			if strings.EqualFold(filepath.Ext(path), ".zip") {
				archive, err := zip.OpenReader(path)
				if err != nil {
					return fmt.Errorf("open browser trace %s: %w", path, err)
				}
				defer archive.Close()
				for _, file := range archive.File {
					reader, err := file.Open()
					if err != nil {
						return err
					}
					content, err := io.ReadAll(reader)
					_ = reader.Close()
					if err != nil {
						return err
					}
					for _, variant := range variants {
						if bytes.Contains(content, []byte(variant)) {
							return fmt.Errorf("browser trace %s entry %s contains a credential literal", path, file.Name)
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func cleanupBrowserProxy(_ *testing.T, state *deployedState) {
	if state.browserProxy == nil {
		return
	}
	state.browserProxy.Close()
	state.browserProxy = nil
}

func deployedBrowserScenarioHooks() ([]sharede2e.Hook[*deployedState], []sharede2e.Hook[*deployedState]) {
	return []sharede2e.Hook[*deployedState]{{Name: "deployed-service-evidence", Run: controlPlaneDiagnostics}},
		[]sharede2e.Hook[*deployedState]{
			{Name: "stop-browser-proxy", Run: cleanupBrowserProxy},
			{Name: "stop-port-forwards", Run: cleanupControlPlaneForwards},
			{Name: "delete-kind-cluster", Run: cleanupControlPlaneKind},
		}
}
