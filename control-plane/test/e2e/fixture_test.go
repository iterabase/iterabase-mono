package e2e_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/artifacts"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/diagnostics"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/httpx"
	kindcluster "github.com/nunocgoncalves/iterabase-mono/testkit/e2e/kind"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/kube"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

const (
	controlPlaneNamespace = "iterabase-system"
	controlPlaneRelease   = "iterabase"

	// Control-plane-owned DES-HOR-545-01 storage expectations passed into the
	// shared testkit helper as an LVMStorageContract (testkit/AGENTS.md).
	lvmPlatformStorageClass  = "iterabase-lvm-xfs"
	lvmAgentPoolStorageClass = "iterabase-agentpool-lvm-xfs"
	lvmProvisioner           = "local.csi.openebs.io"
	lvmDataVolumeGroupName   = "iterabase-data"
	lvmNodeTopologyKey       = "openebs.io/nodename"
)

func lvmStorageContract() kindcluster.LVMStorageContract {
	return kindcluster.LVMStorageContract{
		DataVolumeGroupName: lvmDataVolumeGroupName,
		Provisioner:         lvmProvisioner,
		NodeTopologyKey:     lvmNodeTopologyKey,
		StorageClasses: []kindcluster.StorageClassExpectation{
			{Name: lvmPlatformStorageClass, Shared: false},
			{Name: lvmAgentPoolStorageClass, Shared: true},
		},
	}
}

type requestEvidence struct {
	At     time.Time      `json:"at"`
	Method string         `json:"method"`
	Path   string         `json:"path"`
	Status int            `json:"status"`
	Fields map[string]any `json:"fields,omitempty"`
}

type deployedImage struct {
	name         string
	repository   string
	tag          string
	digest       string
	configDigest string
	sourceSHA    string
	archive      string
}

func (image *deployedImage) reference() string { return image.repository + ":" + image.tag }

// deployedState is an owner-local fixture API. Every scenario creates its own
// state and fresh Kind cluster; later execution/browser scenarios may compose
// these mechanics without sharing mutable clusters between contracts.
type deployedState struct {
	ctx               context.Context
	repoRoot          string
	controlRoot       string
	chartsRoot        string
	outputDir         string
	diagnosticsDir    string
	requestLog        string
	redactor          *redact.Redactor
	runner            process.Runner
	cluster           *kindcluster.Cluster
	client            kube.Client
	platform          kube.Chart
	substrate         kube.Chart
	lvmSubstrate      kube.Chart
	forwards          []*kube.Forward
	apiForward        *kube.Forward
	apiClient         *http.Client
	browserProxy      *deployedBrowserProxy
	imageRepo         string
	imageTag          string
	imageDigest       string
	imageConfigDigest string
	imageArchive      string
	harnessImage      deployedImage
	toolRunnerImage   deployedImage
	inferenceImage    deployedImage
	runtimeImage      deployedImage
	toolDigests       map[string]string
	toolV2            []toolFixture
	fluxRevision      string
	fluxDigest        string
	adminKey          string
	tokenKey          string
	workKey           string
	workIdentityID    string
	workItemID        string
	initialCursor     int64
	firstAttemptJSON  []byte
	firstAttemptID    string
	feedbackID        string
	revisedAttemptID  string
	artifactID        string
	mu                sync.Mutex
}

func newDeployedState(t *testing.T) *deployedState {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	repoRoot := os.Getenv("ITERABASE_REPOSITORY_ROOT")
	if repoRoot == "" {
		repoRoot = filepath.Join(cwd, "..", "..", "..")
	}
	repoRoot, err = filepath.Abs(repoRoot)
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	outputDir := filepath.Join(t.TempDir(), "evidence")
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		t.Fatalf("create evidence directory: %v", err)
	}
	diagnosticsDir := filepath.Join(outputDir, "diagnostics")
	if configured := os.Getenv("ITERABASE_E2E_DIAGNOSTICS"); configured != "" {
		diagnosticsDir = configured
	}
	if err := os.MkdirAll(diagnosticsDir, 0o700); err != nil {
		t.Fatalf("create diagnostics directory: %v", err)
	}
	requestLog := filepath.Join(outputDir, "customer-safe-requests.jsonl")
	if err := os.WriteFile(requestLog, nil, 0o600); err != nil {
		t.Fatalf("create request evidence: %v", err)
	}
	redactor := redact.New()
	state := &deployedState{
		ctx: context.Background(), repoRoot: repoRoot,
		controlRoot: filepath.Join(repoRoot, "control-plane"), chartsRoot: filepath.Join(repoRoot, "charts"),
		outputDir: outputDir, diagnosticsDir: diagnosticsDir, requestLog: requestLog, redactor: redactor,
		runner: process.Runner{Redactor: redactor, OutputDir: outputDir},
	}
	state.resolveRuntime(t)
	return state
}

func (state *deployedState) resolveRuntime(t *testing.T) {
	t.Helper()
	mode := sharede2e.FixtureMode(os.Getenv("ITERABASE_E2E_FIXTURE_MODE"))
	if mode != sharede2e.FixtureSource {
		t.Fatalf("control-plane deployed scenarios support the source fixture, got %q", mode)
	}
	controlImage := runtimeImage(t, "control-plane", "CONTROL_PLANE", true)
	state.imageRepo = controlImage.repository
	state.imageTag = controlImage.tag
	state.imageDigest = controlImage.digest
	state.imageConfigDigest = controlImage.configDigest
	state.imageArchive = controlImage.archive
	state.harnessImage = runtimeImage(t, "harness", "HARNESS", false)
	state.toolRunnerImage = runtimeImage(t, "tool-runner", "TOOL_RUNNER", false)
	state.inferenceImage = runtimeImage(t, "inference-gateway", "INFERENCE_GATEWAY", false)
	state.runtimeImage = runtimeImage(t, "runtime-fixture", "FORGE_E2E_RUNTIME", false)
	platform := os.Getenv("ITERABASE_PLATFORM_LOCAL_CHART")
	if platform == "" {
		t.Fatal("source runtime requires ITERABASE_PLATFORM_LOCAL_CHART")
	}
	platform, err := filepath.Abs(platform)
	if err != nil {
		t.Fatalf("resolve source platform chart: %v", err)
	}
	state.platform = kube.Chart{Mode: mode, LocalPath: platform}
	state.substrate = kube.Chart{Mode: mode, LocalPath: filepath.Join(filepath.Dir(platform), "cert-manager-substrate")}
	state.lvmSubstrate = kube.Chart{Mode: mode, LocalPath: filepath.Join(filepath.Dir(platform), "lvm-storage-substrate")}
}

func runtimeImage(t *testing.T, name, prefix string, required bool) deployedImage {
	t.Helper()
	archiveEnv := map[string]string{
		"CONTROL_PLANE":     "FORGE_E2E_CONTROL_PLANE_IMAGE_ARCHIVE",
		"HARNESS":           "FORGE_E2E_HARNESS_IMAGE_ARCHIVE",
		"TOOL_RUNNER":       "FORGE_E2E_TOOL_RUNNER_IMAGE_ARCHIVE",
		"INFERENCE_GATEWAY": "FORGE_E2E_INFERENCE_IMAGE_ARCHIVE",
		"FORGE_E2E_RUNTIME": "FORGE_E2E_RUNTIME_IMAGE_ARCHIVE",
	}[prefix]
	image := deployedImage{
		name: name, repository: os.Getenv(prefix + "_IMAGE_REPO"), tag: os.Getenv(prefix + "_IMAGE_TAG"),
		digest: os.Getenv(prefix + "_IMAGE_DIGEST"), configDigest: os.Getenv(prefix + "_IMAGE_CONFIG_DIGEST"),
		sourceSHA: os.Getenv(prefix + "_IMAGE_SOURCE_SHA"), archive: os.Getenv(archiveEnv),
	}
	if image.repository == "" && image.tag == "" && image.digest == "" && image.configDigest == "" && image.archive == "" && !required {
		return deployedImage{}
	}
	if image.repository == "" || image.tag == "" || image.archive == "" ||
		!regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(image.digest) ||
		!regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(image.configDigest) {
		t.Fatalf("source runtime requires exact %s image repository/tag/artifact-digest/config-digest/archive", prefix)
	}
	return image
}

func createControlPlaneKindStage(t *testing.T, state *deployedState) {
	t.Helper()
	manager := kindcluster.Manager{Executor: state.runner}
	cluster, err := manager.Create(state.ctx, "control-plane")
	if err != nil {
		t.Fatalf("create Kind cluster: %v", err)
	}
	state.cluster = cluster
	state.client = kube.Client{Executor: state.runner, Kubeconfig: cluster.Kubeconfig, Redactor: state.redactor}
}

func installLVMStorageSubstrateStage(t *testing.T, state *deployedState) {
	t.Helper()
	if err := state.cluster.ConfigureLVMStorage(state.ctx, state.lvmSubstrate.LocalPath, controlPlaneNamespace, controlPlaneRelease+"-lvm-storage", lvmStorageContract()); err != nil {
		t.Fatalf("configure exact Kind OpenEBS LVM storage substrate: %v", err)
	}
}

func importRuntimeImagesStage(t *testing.T, state *deployedState) {
	t.Helper()
	control := deployedImage{
		name: "control-plane", repository: state.imageRepo, tag: state.imageTag,
		digest: state.imageDigest, configDigest: state.imageConfigDigest,
		sourceSHA: os.Getenv("CONTROL_PLANE_IMAGE_SOURCE_SHA"), archive: state.imageArchive,
	}
	images := []*deployedImage{&control, &state.harnessImage, &state.toolRunnerImage, &state.inferenceImage, &state.runtimeImage}
	for _, image := range images {
		if image.repository == "" {
			continue
		}
		loadLocalImage(t, state, image)
	}
	state.imageDigest = control.digest
}

func loadLocalImage(t *testing.T, state *deployedState, image *deployedImage) {
	t.Helper()
	identity, err := state.cluster.ImportImageArchive(state.ctx, image.archive, image.reference(), image.configDigest)
	if err != nil {
		t.Fatalf("import supplied %s image archive into Kind: %v", image.name, err)
	}
	if image.sourceSHA != "" && identity.Labels["org.opencontainers.image.revision"] != image.sourceSHA {
		t.Fatalf("Kind %s image revision label=%q want=%q", image.name, identity.Labels["org.opencontainers.image.revision"], image.sourceSHA)
	}
	image.digest = identity.RuntimeDigest
}

func installCertificateSubstrateStage(t *testing.T, state *deployedState) {
	t.Helper()
	out, err := state.client.HelmUpgrade(state.ctx, kube.HelmOptions{
		Release: controlPlaneRelease + "-cert-manager", Namespace: controlPlaneNamespace,
		Chart: state.substrate, CreateNamespace: true, Wait: true, Timeout: 8 * time.Minute,
	})
	if err != nil {
		t.Fatalf("install certificate substrate: %v\n%s", err, out)
	}
}

func installControlPlanePlatformStage(t *testing.T, state *deployedState) {
	t.Helper()
	// CI supplies every image as an exact archive imported before install;
	// owner scenarios never build or pull images.
	pullPolicy := "Never"
	values := map[string]any{
		"global":            map[string]any{"internalTLS": map[string]any{"enabled": true}},
		"external-dns":      map[string]any{"enabled": false},
		"inference-gateway": map[string]any{"enabled": false},
		"redis":             map[string]any{"enabled": false},
		"minio":             map[string]any{"enabled": true, "artifactService": map[string]any{"enabled": true}},
		"ingress-nginx":     map[string]any{"enabled": false},
		"metallb":           map[string]any{"enabled": false},
		"metallb-config":    map[string]any{"enabled": false},
		"reloader":          map[string]any{"enabled": false},
		"control-plane": map[string]any{
			"image":   map[string]any{"repository": state.imageRepo, "tag": state.imageTag, "pullPolicy": pullPolicy},
			"gateway": map[string]any{"enabled": false},
			"dispatch": map[string]any{
				"enabled":      true,
				"defaultModel": map[string]any{"id": "deployed-e2e-unused", "api": "openai"},
			},
			"toolRunner": map[string]any{"enabled": false},
			"artifact":   map[string]any{"enabled": true},
			"ingress":    map[string]any{"enabled": false},
		},
	}
	valuesPath := state.writeJSON(t, "control-plane-platform-values.json", values)
	out, err := state.client.HelmUpgrade(state.ctx, kube.HelmOptions{
		Release: controlPlaneRelease, Namespace: controlPlaneNamespace, Chart: state.platform,
		ValueFiles: []string{valuesPath}, Wait: true, Timeout: 16 * time.Minute,
	})
	if err != nil {
		t.Fatalf("install control-plane platform fixture: %v\n%s", err, out)
	}
}

func (state *deployedState) installPlatformValues(t *testing.T, name string, values map[string]any, timeout time.Duration) {
	t.Helper()
	valuesPath := state.writeJSON(t, name, values)
	out, err := state.client.HelmUpgrade(state.ctx, kube.HelmOptions{
		Release: controlPlaneRelease, Namespace: controlPlaneNamespace, Chart: state.platform,
		ValueFiles: []string{valuesPath}, Wait: true, Timeout: timeout,
	})
	if err != nil {
		t.Fatalf("install platform fixture: %v\n%s", err, out)
	}
}

func assertDeploymentReadyStage(t *testing.T, state *deployedState) {
	t.Helper()
	state.kubectl(t, 6*time.Minute, "rollout", "status", "deployment/"+controlPlaneRelease+"-control-plane-api", "-n", controlPlaneNamespace, "--timeout=5m")
	state.kubectl(t, 6*time.Minute, "rollout", "status", "deployment/"+controlPlaneRelease+"-control-plane-manager", "-n", controlPlaneNamespace, "--timeout=5m")
	state.kubectl(t, 6*time.Minute, "rollout", "status", "deployment/"+controlPlaneRelease+"-control-plane-dispatch", "-n", controlPlaneNamespace, "--timeout=5m")
	state.kubectl(t, 6*time.Minute, "rollout", "status", "statefulset/"+controlPlaneRelease+"-postgresql", "-n", controlPlaneNamespace, "--timeout=5m")
	state.kubectl(t, 6*time.Minute, "rollout", "status", "statefulset/"+controlPlaneRelease+"-minio", "-n", controlPlaneNamespace, "--timeout=5m")
	state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Ready", "certificate/"+controlPlaneRelease+"-control-plane-api-tls", "-n", controlPlaneNamespace, "--timeout=3m")
	imageLines := state.kubectl(t, 30*time.Second, "get", "pods", "-n", controlPlaneNamespace,
		"-l", "app.kubernetes.io/name=control-plane", "-o",
		`jsonpath={range .items[*].status.containerStatuses[*]}{.name}={.imageID}{"\n"}{end}{range .items[*].status.initContainerStatuses[*]}{.name}={.imageID}{"\n"}{end}`)
	expectedImages := map[string]bool{"api": false, "manager": false, "dispatch": false, "migrate": false, "bootstrap": false}
	for _, line := range strings.Split(imageLines, "\n") {
		name, imageID, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if _, expected := expectedImages[name]; !expected {
			continue
		}
		if !strings.Contains(imageID, state.imageDigest) {
			t.Fatalf("deployed %s does not run exact image digest %s: %s", name, state.imageDigest, imageID)
		}
		expectedImages[name] = true
	}
	for name, found := range expectedImages {
		if !found {
			t.Fatalf("deployed control-plane image identity is missing for %s", name)
		}
	}
	migration := state.databaseQuery(t, "SELECT version::text || ':' || dirty::text FROM schema_migrations")
	if strings.TrimSpace(migration) == "" || strings.HasSuffix(strings.TrimSpace(migration), ":true") {
		t.Fatalf("database migration state is not clean: %q", migration)
	}
	state.openAPI(t)
	status, body := state.request(t, http.MethodGet, "/readyz", "", nil, nil)
	requireStatus(t, status, http.StatusOK, body)
}

func (state *deployedState) captureBootstrapKeys(t *testing.T) {
	t.Helper()
	state.captureBootstrapKeysExcluding(t, nil)
}

func (state *deployedState) captureBootstrapKeysExcluding(t *testing.T, excludedUIDs map[string]struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(state.ctx, 30*time.Second)
	defer cancel()
	keys, pod, err := state.readBootstrapKeys(ctx, excludedUIDs)
	if err != nil {
		t.Fatal(err)
	}
	state.redactor.Add(keys["admin"], keys["token"])
	state.adminKey = keys["admin"]
	state.tokenKey = keys["token"]
	t.Logf("captured bootstrap credential scopes from %s", pod.safeSummary())
}

var (
	bootstrapKeyPattern                = regexp.MustCompile(`API key \(scope=([^)]+)\): (\S+)`)
	kubectlUnknownStreamWarningPattern = regexp.MustCompile(
		`^E\d{4} \d{2}:\d{2}:\d{2}\.\d+\s+\d+ websocket\.go:\d+\] Unknown stream id \d+, discarding message$`,
	)
)

func parseBootstrapKeys(output string) map[string]string {
	keys := make(map[string]string)
	for _, match := range bootstrapKeyPattern.FindAllStringSubmatch(output, -1) {
		keys[match[1]] = match[2]
	}
	return keys
}

func parseRequiredBootstrapKeys(output string) (map[string]string, error) {
	keys := parseBootstrapKeys(output)
	if keys["admin"] == "" || keys["token"] == "" {
		return keys, fmt.Errorf("bootstrap did not emit the required admin and token credentials")
	}
	return keys, nil
}

func (state *deployedState) createWorkIdentity(t *testing.T, email string) {
	t.Helper()
	status, body := state.requestJSON(t, http.MethodPost, "/v1/users", state.adminKey, map[string]any{"email": email, "role": "user"})
	requireStatus(t, status, http.StatusCreated, body)
	var user struct {
		ID string `json:"id"`
	}
	mustDecode(t, body, &user)
	if user.ID == "" {
		t.Fatal("created work identity has no ID")
	}
	status, body = state.requestJSON(t, http.MethodPost, "/v1/api-keys", state.adminKey, map[string]any{
		"identityID": user.ID, "name": "deployed-e2e", "scope": "work",
	})
	requireStatus(t, status, http.StatusCreated, body)
	var key struct {
		FullKey string `json:"fullKey"`
	}
	mustDecode(t, body, &key)
	if key.FullKey == "" {
		t.Fatal("created work API key is empty")
	}
	state.redactor.Add(key.FullKey)
	state.workIdentityID = user.ID
	state.workKey = key.FullKey
}

func (state *deployedState) applyWorkFixture(t *testing.T) {
	t.Helper()
	manifest := `apiVersion: v1
kind: Secret
metadata:
  name: e2e-placeholder-ca
  namespace: iterabase-system
type: Opaque
stringData:
  ca.crt: synthetic-not-a-private-key
---
apiVersion: platform.iterabase.com/v1alpha1
kind: AgentPool
metadata:
  name: paused-e2e
  namespace: iterabase-system
spec:
  replicas: 0
  workerImage: invalid.local/unused:fixed
  podSecurity: baseline
  identity:
    trustDomain: iterabase.local
    caSecretRef: {name: e2e-placeholder-ca}
  sandbox:
    storageClassName: iterabase-agentpool-lvm-xfs
    accessMode: ReadWriteOnce
    size: 1Gi
  gateways:
    controlPlane:
      url: https://iterabase-control-plane-dispatch.iterabase-system.svc:8091
      serverName: iterabase-control-plane-dispatch.iterabase-system.svc
      selector: {podSelector: {matchLabels: {app.kubernetes.io/component: dispatch}}}
    toolGateway:
      url: https://iterabase-control-plane-gateway.iterabase-system.svc:8090
      serverName: iterabase-control-plane-gateway.iterabase-system.svc
      selector: {podSelector: {matchLabels: {app.kubernetes.io/component: gateway}}}
    inferenceGateway:
      url: https://iterabase-gateway.iterabase-system.svc:8443
      serverName: iterabase-gateway.iterabase-system.svc
      selector: {podSelector: {matchLabels: {app.kubernetes.io/name: inference-gateway}}}
  networkPolicy: {egress: denied}
  workspaceTools: false
---
apiVersion: platform.iterabase.com/v1alpha1
kind: Workflow
metadata:
  name: deployed-e2e
  namespace: iterabase-system
spec:
  key: e2e/manual-review
  version: "1"
  poolRef: paused-e2e
  source: {type: manual_api}
  graph:
    entryNode: review
    maxTransitions: 4
    nodes:
      - key: review
        label: {en: Review supplied case, pt: Rever caso fornecido}
        kind: human_gate
        outcomes: [accepted]
        resultPresentation:
          outcomes:
            - outcome: accepted
              summary: {en: Review completed, pt: Revisão concluída}
          fields:
            - path: [approved]
              label: {en: Approved, pt: Aprovado}
        humanGate:
          type: approval
          title: {en: Review required, pt: Revisão necessária}
          description: {en: Confirm the supplied case., pt: Confirme o caso fornecido.}
          responseSchema:
            type: object
            additionalProperties: false
            required: [approved]
            properties: {approved: {type: boolean}}
          presentation:
            outcomes: [{en: Accepted, pt: Aceite}]
            fields:
              - key: approved
                label: {en: Approved, pt: Aprovado}
    terminalOutcomes: [{node: review, outcome: accepted}]
  presentation:
    workflowTitle: Deployed review
    personaName: Operations reviewer
    locale: en
`
	path := state.writeManifest(t, "work-fixture.yaml", manifest)
	state.kubectl(t, 30*time.Second, "apply", "-f", path)
	state.kubectl(t, 3*time.Minute, "wait", "--for=jsonpath={.status.ready}=true", "agentpool/paused-e2e", "-n", controlPlaneNamespace, "--timeout=2m")
	state.kubectl(t, 3*time.Minute, "wait", "--for=jsonpath={.status.ready}=true", "workflow/deployed-e2e", "-n", controlPlaneNamespace, "--timeout=2m")
}

func (state *deployedState) openAPI(t *testing.T) {
	t.Helper()
	if state.apiForward != nil {
		state.stopForward(t, state.apiForward)
	}
	forward, err := state.client.PortForward(state.ctx, controlPlaneNamespace, "svc/"+controlPlaneRelease+"-control-plane-api", 8080, "https")
	if err != nil {
		t.Fatalf("port-forward control-plane API: %v", err)
	}
	state.forwards = append(state.forwards, forward)
	state.apiForward = forward
	ca := state.decodeSecret(t, controlPlaneRelease+"-internal-ca-root", "ca.crt")
	client, err := httpx.TLSClient(httpx.TLSOptions{
		Timeout: 20 * time.Second, RootCAPEM: ca,
		ServerName: controlPlaneRelease + "-control-plane-api." + controlPlaneNamespace + ".svc",
	})
	if err != nil {
		t.Fatalf("create verified API client: %v", err)
	}
	state.apiClient = client
	if state.browserProxy != nil {
		state.browserProxy.setTarget(forward.URL, client.Transport)
	}
}

func (state *deployedState) restartAPI(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(state.ctx, 30*time.Second)
	pods, err := state.readBootstrapEvidencePods(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	previousUIDs := bootstrapEvidencePodUIDs(pods)
	if len(previousUIDs) == 0 {
		t.Fatal("API restart requires at least one pre-restart pod identity")
	}

	state.stopAPIForward(t)
	state.kubectl(t, 30*time.Second, "rollout", "restart", "deployment/"+controlPlaneRelease+"-control-plane-api", "-n", controlPlaneNamespace)
	state.kubectl(t, 7*time.Minute, "rollout", "status", "deployment/"+controlPlaneRelease+"-control-plane-api", "-n", controlPlaneNamespace, "--timeout=6m")
	state.captureBootstrapKeysExcluding(t, previousUIDs) // register the exact new pod's credentials before diagnostics can retain logs
	state.openAPI(t)
	status, body := state.request(t, http.MethodGet, "/readyz", "", nil, nil)
	requireStatus(t, status, http.StatusOK, body)
}

func (state *deployedState) restartMinIO(t *testing.T) {
	t.Helper()
	before := state.kubectl(t, 30*time.Second, "get", "pod", "-n", controlPlaneNamespace,
		"-l", "app.kubernetes.io/name=minio", "-o", "jsonpath={.items[0].metadata.uid}")
	state.kubectl(t, 30*time.Second, "rollout", "restart", "statefulset/"+controlPlaneRelease+"-minio", "-n", controlPlaneNamespace)
	state.kubectl(t, 7*time.Minute, "rollout", "status", "statefulset/"+controlPlaneRelease+"-minio", "-n", controlPlaneNamespace, "--timeout=6m")
	after := state.kubectl(t, 30*time.Second, "get", "pod", "-n", controlPlaneNamespace,
		"-l", "app.kubernetes.io/name=minio", "-o", "jsonpath={.items[0].metadata.uid}")
	if before == "" || after == "" || before == after {
		t.Fatalf("MinIO process did not restart: before=%q after=%q", before, after)
	}
}

func (state *deployedState) decodeSecret(t *testing.T, name, key string) []byte {
	t.Helper()
	encoded := state.kubectl(t, 30*time.Second, "get", "secret", name, "-n", controlPlaneNamespace,
		"-o", fmt.Sprintf("jsonpath={.data.%s}", strings.ReplaceAll(key, ".", `\.`)))
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode Secret %s/%s: %v", name, key, err)
	}
	return decoded
}

func (state *deployedState) databaseQuery(t *testing.T, query string) string {
	t.Helper()
	output := state.kubectl(t, 30*time.Second, "exec", "-n", controlPlaneNamespace,
		"statefulset/"+controlPlaneRelease+"-postgresql", "-c", "postgresql", "--",
		"psql", "-U", "controlplane", "-d", "controlplane", "-Atc", query)
	return cleanDatabaseQueryOutput(output)
}

func cleanDatabaseQueryOutput(output string) string {
	lines := strings.Split(output, "\n")
	clean := lines[:0]
	for _, line := range lines {
		if kubectlUnknownStreamWarningPattern.MatchString(line) {
			continue
		}
		clean = append(clean, line)
	}
	return strings.TrimSpace(strings.Join(clean, "\n"))
}

func (state *deployedState) firstPod(t *testing.T, selector string) string {
	t.Helper()
	pod := state.kubectl(t, 30*time.Second, "get", "pods", "-n", controlPlaneNamespace, "-l", selector,
		"-o", "jsonpath={.items[0].metadata.name}")
	if pod == "" {
		t.Fatalf("no pod for selector %q", selector)
	}
	return pod
}

func (state *deployedState) kubectl(t *testing.T, timeout time.Duration, args ...string) string {
	t.Helper()
	out, err := state.client.Kubectl(state.ctx, timeout, args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

func (state *deployedState) writeJSON(t *testing.T, name string, value any) string {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	return state.writeManifest(t, name, string(append(data, '\n')))
}

func (state *deployedState) writeManifest(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func (state *deployedState) recordRequest(t *testing.T, evidence requestEvidence) {
	t.Helper()
	evidence.At = time.Now().UTC()
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatalf("marshal request evidence: %v", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	file, err := os.OpenFile(state.requestLog, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open request evidence: %v", err)
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		t.Fatalf("write request evidence: %v", err)
	}
}

func (state *deployedState) registerBootstrapSecrets() error {
	if state.cluster == nil || state.redactor == nil {
		return fmt.Errorf("bootstrap secret registration requires an active cluster and redactor")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	keys, _, err := state.readBootstrapKeys(ctx, nil)
	if err != nil {
		return err
	}
	state.redactor.Add(keys["admin"], keys["token"])
	return nil
}

func controlPlaneDiagnostics(t *testing.T, state *deployedState) {
	t.Helper()
	if state.cluster == nil {
		return
	}
	// Bootstrap emits credentials from an init-container boundary. Resolve them
	// directly into memory before any generic log collector can retain evidence,
	// including failures that occurred before the scenario's identity setup.
	if err := state.registerBootstrapSecrets(); err != nil {
		t.Logf("skip deployed diagnostics because bootstrap credentials could not be registered: %v", err)
		return
	}
	// Add owner-specific database/object-store health without querying Secrets.
	health := []string{}
	if out, err := state.client.Kubectl(state.ctx, 45*time.Second, "exec", "-n", controlPlaneNamespace,
		"statefulset/"+controlPlaneRelease+"-postgresql", "-c", "postgresql", "--",
		"psql", "-U", "controlplane", "-d", "controlplane", "-Atc",
		"SELECT 'migration=' || version::text || ',dirty=' || dirty::text FROM schema_migrations"); err == nil {
		health = append(health, strings.TrimSpace(out))
	} else {
		health = append(health, "database-health unavailable: "+err.Error())
	}
	if out, err := state.client.Kubectl(state.ctx, 45*time.Second, "get", "statefulset", controlPlaneRelease+"-minio", "-n", controlPlaneNamespace,
		"-o", "jsonpath=minio-ready={.status.readyReplicas}/{.status.replicas}"); err == nil {
		health = append(health, strings.TrimSpace(out))
	} else {
		health = append(health, "object-store-health unavailable: "+err.Error())
	}
	healthPath := filepath.Join(state.outputDir, "service-health.txt")
	if err := os.WriteFile(healthPath, []byte(strings.Join(health, "\n")+"\n"), 0o600); err != nil {
		t.Logf("write owner health evidence: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	err := (diagnostics.Collector{
		Executor: state.runner, Kubeconfig: state.cluster.Kubeconfig,
		OutputDir: state.diagnosticsDir, Redactor: state.redactor,
		Artifacts: []artifacts.Entry{
			{Name: "customer-safe-requests.jsonl", Source: state.requestLog, Kind: artifacts.Text},
			{Name: "service-health.txt", Source: healthPath, Kind: artifacts.Text},
		},
	}).Collect(ctx)
	if err != nil {
		t.Logf("best-effort deployed diagnostics: %v", err)
	}
}

func cleanupControlPlaneForwards(t *testing.T, state *deployedState) {
	t.Helper()
	for i := len(state.forwards) - 1; i >= 0; i-- {
		if err := state.forwards[i].Stop(); err != nil {
			t.Errorf("stop port-forward: %v", err)
		}
	}
	state.forwards = nil
	state.apiForward = nil
	state.apiClient = nil
}

func (state *deployedState) stopAPIForward(t *testing.T) {
	t.Helper()
	if state.apiForward == nil {
		return
	}
	if state.browserProxy != nil {
		state.browserProxy.clearTarget()
	}
	state.stopForward(t, state.apiForward)
	state.apiForward = nil
	state.apiClient = nil
}

func (state *deployedState) stopForward(t *testing.T, forward *kube.Forward) {
	t.Helper()
	if err := forward.Stop(); err != nil {
		t.Fatalf("stop port-forward: %v", err)
	}
	for i, candidate := range state.forwards {
		if candidate == forward {
			state.forwards = append(state.forwards[:i], state.forwards[i+1:]...)
			break
		}
	}
}

func cleanupControlPlaneKind(t *testing.T, state *deployedState) {
	t.Helper()
	if state.cluster == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err := state.cluster.Delete(ctx); err != nil {
		t.Errorf("delete Kind cluster: %v", err)
	}
}

func deployedScenarioHooks() ([]sharede2e.Hook[*deployedState], []sharede2e.Hook[*deployedState]) {
	return []sharede2e.Hook[*deployedState]{{Name: "deployed-service-evidence", Run: controlPlaneDiagnostics}},
		[]sharede2e.Hook[*deployedState]{
			{Name: "stop-port-forwards", Run: cleanupControlPlaneForwards},
			{Name: "delete-kind-cluster", Run: cleanupControlPlaneKind},
		}
}
