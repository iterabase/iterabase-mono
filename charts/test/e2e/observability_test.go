package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/httpx"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/poll"
)

const (
	observabilityToolSourceName      = "observability-e2e"
	observabilityToolServerNamespace = "flux-system"
	observabilityHarnessStorageClass = "iterabase-agentpool-lvm-xfs"
	gpuOperatorFixtureNamespace      = "gpu-operator"
)

// observabilityPlatform is the platform values the observability scenario
// installs. The harness and tool-runner layers apply when their composed images
// are present, which is always true in CI, so both are declared renders.
var observabilityPlatform = platformValues{"values-observability.yaml", platformValuesBase, platformValuesRuntime}

const (
	observabilityHarnessValues    = "test/e2e/values/observability-harness.yaml"
	observabilityToolRunnerValues = "test/e2e/values/observability-tool-runner.yaml"
)

func observabilityScenario() sharede2e.Definition {
	diagnostics, cleanup := scenarioHooks()
	return sharede2e.Define(sharede2e.Scenario[*chartState]{
		Metadata: chartScenarioMetadata(
			"observability",
			"Installs the pinned LVM substrate and chart-owned observability composition, then proves exact thick XFS persistence, per-pool and aggregate VG monitor discovery, stack readiness, shipped dashboard queries, and client paths.",
			"test-e2e-observability", 40,
			[]string{"HOR-408", "HOR-414", "HOR-418", "HOR-416", "HOR-505", "HOR-545", "HOR-590", "DES-HOR-545-01"},
			[]string{"control-plane-chart", "inference-gateway-chart", "iterabase-platform-chart"},
			append(substrateRenders("opo1"), observabilityCandidateValues(observabilityPlatform, true, true).render()),
		),
		NewState: newChartState,
		Stages: []sharede2e.Stage[*chartState]{
			{Name: "create-kind", Run: createKindStage},
			{Name: "import-runtime-images", DependsOn: []string{"create-kind"}, Run: importRuntimeImagesStage},
			{Name: "install-certificate-substrate", DependsOn: []string{"import-runtime-images"}, Run: installCertificateSubstrateStage},
			{Name: "install-lvm-storage-substrate", DependsOn: []string{"install-certificate-substrate"}, Run: installLVMStorageStage},
			{Name: "install-tool-source", DependsOn: []string{"install-lvm-storage-substrate"}, Run: installObservabilityToolSourceStage},
			{Name: "install-dcgm-exporter-fixture", DependsOn: []string{"install-tool-source"}, Run: installDCGMExporterFixtureStage},
			{Name: "install-observability", DependsOn: []string{"install-dcgm-exporter-fixture"}, Run: installObservabilityStage},
			{Name: "install-harness-worker", DependsOn: []string{"install-observability"}, Run: installObservabilityHarnessStage},
			{Name: "assert-stack-readiness", DependsOn: []string{"install-harness-worker"}, Run: assertStackReadinessStage},
			{Name: "assert-monitor-discovery", DependsOn: []string{"assert-stack-readiness"}, Run: assertMonitorDiscoveryStage},
			{Name: "assert-prometheus-persistence", DependsOn: []string{"assert-monitor-discovery"}, Run: assertPrometheusPersistenceStage},
			{Name: "assert-loki-persistence", DependsOn: []string{"assert-stack-readiness"}, Run: assertLokiPersistenceStage},
		},
		Diagnostics: diagnostics,
		Cleanup:     cleanup,
	})
}

func installObservabilityStage(t *testing.T, state *chartState) {
	t.Helper()
	state.installPlatform(t, 20*time.Minute, observabilityValueFiles(t, state, observabilityPlatform)...)
	assertCandidateImages(t, state)
}

func installDCGMExporterFixtureStage(t *testing.T, state *chartState) {
	t.Helper()
	manifest := `apiVersion: v1
kind: Namespace
metadata:
  name: gpu-operator
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: nvidia-dcgm-exporter-metrics
  namespace: gpu-operator
data:
  metrics: |
    # HELP DCGM_FI_DEV_GPU_UTIL GPU utilization (in %).
    # TYPE DCGM_FI_DEV_GPU_UTIL gauge
    DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-e2e",pci_bus_id="00000000:01:00.0",device="nvidia0",modelName="NVIDIA E2E GPU",Hostname="dcgm-e2e"} 42
    # HELP DCGM_FI_DEV_FB_FREE Frame buffer memory free (in MiB).
    # TYPE DCGM_FI_DEV_FB_FREE gauge
    DCGM_FI_DEV_FB_FREE{gpu="0",UUID="GPU-e2e",pci_bus_id="00000000:01:00.0",device="nvidia0",modelName="NVIDIA E2E GPU",Hostname="dcgm-e2e"} 81920
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nvidia-dcgm-exporter
  namespace: gpu-operator
spec:
  replicas: 1
  selector:
    matchLabels:
      app: nvidia-dcgm-exporter
  template:
    metadata:
      labels:
        app: nvidia-dcgm-exporter
    spec:
      automountServiceAccountToken: false
      containers:
        - name: exporter
          image: busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0
          command: ["sh", "-c"]
          args:
            - |
              body="$(cat /www/metrics)"
              while true; do
                {
                  printf 'HTTP/1.1 200 OK\r\n'
                  printf 'Content-Type: text/plain; version=0.0.4; charset=utf-8\r\n'
                  printf 'Content-Length: %s\r\n' "${#body}"
                  printf 'Connection: close\r\n\r\n'
                  printf '%s' "$body"
                } | nc -l -p 9400
              done
          ports:
            - name: gpu-metrics
              containerPort: 9400
          readinessProbe:
            httpGet:
              path: /metrics
              port: gpu-metrics
          resources:
            requests: {cpu: 5m, memory: 4Mi}
            limits: {cpu: 50m, memory: 16Mi}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: ["ALL"]}
            readOnlyRootFilesystem: true
            runAsNonRoot: true
            runAsUser: 65534
            seccompProfile: {type: RuntimeDefault}
          volumeMounts:
            - name: metrics
              mountPath: /www
              readOnly: true
      volumes:
        - name: metrics
          configMap:
            name: nvidia-dcgm-exporter-metrics
---
apiVersion: v1
kind: Service
metadata:
  name: nvidia-dcgm-exporter
  namespace: gpu-operator
  labels:
    app: nvidia-dcgm-exporter
spec:
  selector:
    app: nvidia-dcgm-exporter
  ports:
    - name: gpu-metrics
      port: 9400
      targetPort: gpu-metrics
`
	state.kubectl(t, 30*time.Second, "apply", "-f", state.writeManifest(t, "dcgm-exporter-fixture.yaml", manifest))
	state.kubectl(t, 3*time.Minute, "rollout", "status", "deployment/nvidia-dcgm-exporter", "-n", gpuOperatorFixtureNamespace, "--timeout=2m")
}

func harnessImagePresent() bool {
	return os.Getenv("HARNESS_IMAGE_REPO") != "" && os.Getenv("HARNESS_IMAGE_TAG") != ""
}

func toolRunnerImagePresent() bool {
	return os.Getenv("TOOL_RUNNER_IMAGE_REPO") != "" && os.Getenv("TOOL_RUNNER_IMAGE_TAG") != ""
}

// observabilityCandidateValues adds the harness and tool-runner layers that a
// composed candidate with those images installs.
func observabilityCandidateValues(base platformValues, harness, toolRunner bool) platformValues {
	values := append(platformValues{}, base...)
	if harness {
		values = append(values, observabilityHarnessValues)
	}
	if toolRunner {
		values = append(values, observabilityToolRunnerValues)
	}
	return values
}

// observabilityValueFiles returns the declared values files for the composed
// images actually present plus the runtime image overlay.
func observabilityValueFiles(t *testing.T, state *chartState, base platformValues) []string {
	t.Helper()
	values := runtimeImageValues(t)
	if repository, tag := os.Getenv("TOOL_RUNNER_IMAGE_REPO"), os.Getenv("TOOL_RUNNER_IMAGE_TAG"); repository != "" && tag != "" {
		controlPlane, _ := values["control-plane"].(map[string]any)
		if controlPlane == nil {
			controlPlane = map[string]any{}
			values["control-plane"] = controlPlane
		}
		controlPlane["toolRunner"] = map[string]any{
			"image": map[string]any{"repository": repository, "tag": tag, "pullPolicy": "Never"},
		}
	}
	files := observabilityCandidateValues(base, harnessImagePresent(), toolRunnerImagePresent()).files(state)
	return append(files, state.writeValues(t, "observability-runtime", values))
}

func installObservabilityHarnessStage(t *testing.T, state *chartState) {
	t.Helper()
	repository, tag := os.Getenv("HARNESS_IMAGE_REPO"), os.Getenv("HARNESS_IMAGE_TAG")
	if repository == "" || tag == "" {
		if os.Getenv(sharede2e.RequiredEnv) == "true" {
			t.Fatal("required observability runtime is missing the composed harness image")
		}
		t.Log("HARNESS_IMAGE_REPO/TAG absent in the optional local fixture")
		return
	}
	manifest := fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: AgentPool
metadata:
  name: observability-e2e
  namespace: %s
spec:
  replicas: 1
  workerImage: %s:%s
  podSecurity: baseline
  identity:
    trustDomain: iterabase.local
    caSecretRef: {name: %s-control-plane-gateway-ca}
    certMountPath: /etc/harness/tls
  sandbox:
    storageClassName: %s
    accessMode: ReadWriteOnce
    size: 1Gi
    mountPath: /data/sandboxes
  gateways:
    controlPlane:
      url: https://%s-control-plane-dispatch:8091
      serverName: %s-control-plane-dispatch.%s.svc
      selector: {podSelector: {matchLabels: {app.kubernetes.io/name: control-plane, app.kubernetes.io/component: dispatch}}}
    toolGateway:
      url: https://%s-control-plane-gateway:8090
      serverName: %s-control-plane-gateway.%s.svc
      selector: {podSelector: {matchLabels: {app.kubernetes.io/name: control-plane, app.kubernetes.io/component: gateway}}}
    inferenceGateway:
      url: https://%s-inference-gateway:8443
      serverName: %s-inference-gateway.%s.svc
      selector: {podSelector: {matchLabels: {app.kubernetes.io/name: inference-gateway}}}
  networkPolicy: {egress: denied}
  workspaceTools: false
  piDirs: [/pi/product, /pi/client]
  walDir: /var/harness/wal
  probe: {port: 8081}
`, testNamespace, repository, tag, testRelease, observabilityHarnessStorageClass, testRelease, testRelease, testNamespace,
		testRelease, testRelease, testNamespace, testRelease, testRelease, testNamespace)
	state.kubectl(t, 30*time.Second, "apply", "-f", state.writeManifest(t, "observability-agentpool.yaml", manifest))
	state.waitForPods(t, "app.kubernetes.io/name=control-plane,app.kubernetes.io/component=harness", 7*time.Minute)
}

func filepathFromCharts(state *chartState, name string) string {
	return filepath.Join(state.chartsRoot, name)
}

func assertStackReadinessStage(t *testing.T, state *chartState) {
	t.Helper()
	assertStackReadiness(t, state,
		os.Getenv("HARNESS_IMAGE_REPO") != "" && os.Getenv("HARNESS_IMAGE_TAG") != "",
		os.Getenv("TOOL_RUNNER_IMAGE_REPO") != "" && os.Getenv("TOOL_RUNNER_IMAGE_TAG") != "",
	)
}

func assertStackReadiness(t *testing.T, state *chartState, includeHarness, includeToolRunner bool) {
	t.Helper()
	for _, selector := range stackReadinessSelectors(includeHarness, includeToolRunner) {
		state.waitForPods(t, selector, 7*time.Minute)
	}
}

func stackReadinessSelectors(includeHarness, includeToolRunner bool) []string {
	selectors := []string{
		"app.kubernetes.io/name=prometheus",
		"app.kubernetes.io/name=grafana",
		"app.kubernetes.io/name=loki,app.kubernetes.io/component=single-binary",
		"app.kubernetes.io/name=alertmanager",
		"app.kubernetes.io/name=promtail",
		"app.kubernetes.io/name=postgresql,app.kubernetes.io/component=exporter",
		"app.kubernetes.io/name=redis,app.kubernetes.io/component=exporter",
		"app.kubernetes.io/name=control-plane,app.kubernetes.io/component=api",
		"app.kubernetes.io/name=control-plane,app.kubernetes.io/component=manager",
		"app.kubernetes.io/name=control-plane,app.kubernetes.io/component=gateway",
		"app.kubernetes.io/name=inference-gateway",
	}
	if includeHarness {
		selectors = append(selectors,
			"app.kubernetes.io/name=control-plane,app.kubernetes.io/component=dispatch",
			"app.kubernetes.io/name=control-plane,app.kubernetes.io/component=harness",
		)
	}
	if includeToolRunner {
		selectors = append(selectors, "app.kubernetes.io/name=control-plane,app.kubernetes.io/component=tool-runner")
	}
	return selectors
}

func grafanaInferenceDashboardQueries(t *testing.T, client *http.Client, baseURL, username, password string) (string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/dashboards/uid/iterabase-inference-model-serving", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(username, password)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Grafana inference dashboard status=%d", resp.StatusCode)
	}
	var payload struct {
		Dashboard struct {
			Panels []struct {
				Title   string `json:"title"`
				Targets []struct {
					Expression string `json:"expr"`
				} `json:"targets"`
			} `json:"panels"`
			Templating struct {
				List []struct {
					Name  string `json:"name"`
					Query struct {
						Expression string `json:"query"`
					} `json:"query"`
				} `json:"list"`
			} `json:"templating"`
		} `json:"dashboard"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	var variableQuery, panelQuery string
	for _, variable := range payload.Dashboard.Templating.List {
		if variable.Name == "namespace" {
			variableQuery = variable.Query.Expression
			break
		}
	}
	for _, panel := range payload.Dashboard.Panels {
		if panel.Title == "GPU utilization" && len(panel.Targets) == 1 {
			panelQuery = panel.Targets[0].Expression
			break
		}
	}
	const variablePrefix = "label_values("
	const variableSuffix = ", namespace)"
	if !strings.HasPrefix(variableQuery, variablePrefix) || !strings.HasSuffix(variableQuery, variableSuffix) {
		t.Fatalf("Grafana inference dashboard namespace variable has unsupported query %q", variableQuery)
	}
	if !strings.Contains(panelQuery, "$namespace") {
		t.Fatalf("Grafana inference dashboard GPU utilization query does not use the namespace variable: %q", panelQuery)
	}
	return strings.TrimSuffix(strings.TrimPrefix(variableQuery, variablePrefix), variableSuffix), panelQuery
}

func assertMonitorDiscoveryStage(t *testing.T, state *chartState) {
	t.Helper()
	forward := state.forward(t, "svc/"+kubePrometheusStackComponentName("prometheus"), 9090, "http")
	client, err := httpx.Client(15 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range []string{"pg_up", "redis_up"} {
		if err := waitPrometheusValue(state.ctx, client, forward.URL, metric, "1", 5*time.Minute); err != nil {
			t.Fatalf("%s did not become 1: %v", metric, err)
		}
	}
	for _, metric := range []struct {
		query string
		want  string
	}{
		{`count(up{namespace="gpu-operator",service="nvidia-dcgm-exporter",endpoint="gpu-metrics"} == 1)`, "1"},
		{`DCGM_FI_DEV_GPU_UTIL{namespace="gpu-operator",UUID="GPU-e2e",device="nvidia0",modelName="NVIDIA E2E GPU"}`, "42"},
		{`DCGM_FI_DEV_FB_FREE{namespace="gpu-operator",UUID="GPU-e2e",device="nvidia0",modelName="NVIDIA E2E GPU"}`, "81920"},
		{`count(lvm_vg_total_size_bytes{namespace="iterabase-system",name="iterabase-data"} > 0)`, "1"},
		{`count(lvm_vg_free_size_bytes{namespace="iterabase-system",name="iterabase-data"} > 0)`, "1"},
	} {
		if err := waitPrometheusValue(state.ctx, client, forward.URL, metric.query, metric.want, 5*time.Minute); err != nil {
			t.Fatalf("representative DCGM metric %s did not become %s: %v", metric.query, metric.want, err)
		}
	}

	username := string(decodeSecretValue(t, state, testRelease+"-grafana", "admin-user"))
	password := string(decodeSecretValue(t, state, testRelease+"-grafana", "admin-password"))
	state.redactor.Add(username, password)
	grafanaForward := state.forward(t, "svc/"+testRelease+"-grafana", 80, "http")
	namespaceSelector, panelQuery := grafanaInferenceDashboardQueries(t, &http.Client{Timeout: 15 * time.Second}, grafanaForward.URL, username, password)
	state.stopForward(t, grafanaForward)
	if err := requirePrometheusSeriesLabelValue(client, forward.URL, namespaceSelector, "namespace", gpuOperatorFixtureNamespace); err != nil {
		t.Fatalf("Grafana inference dashboard namespace variable does not select the GPU Operator namespace: %v", err)
	}
	panelQuery = strings.ReplaceAll(panelQuery, "$namespace", gpuOperatorFixtureNamespace)
	if err := waitPrometheusValue(state.ctx, client, forward.URL, panelQuery, "42", 5*time.Minute); err != nil {
		t.Fatalf("shipped Grafana inference GPU panel query %s did not become 42: %v", panelQuery, err)
	}
	assertPlatformMetrics(t, state, client, forward.URL,
		os.Getenv("HARNESS_IMAGE_REPO") != "" && os.Getenv("HARNESS_IMAGE_TAG") != "",
		os.Getenv("TOOL_RUNNER_IMAGE_REPO") != "" && os.Getenv("TOOL_RUNNER_IMAGE_TAG") != "",
	)
	body := requireHTTP(t, client, http.MethodGet, forward.URL+"/api/v1/targets?state=active", nil, http.StatusOK)
	targets := []string{"postgresql", "redis", "nvidia-dcgm-exporter"}
	if os.Getenv("CONTROL_PLANE_IMAGE_REPO") != "" && os.Getenv("CONTROL_PLANE_IMAGE_TAG") != "" {
		targets = append(targets, "control-plane")
	}
	if os.Getenv("INFERENCE_GATEWAY_IMAGE_REPO") != "" && os.Getenv("INFERENCE_GATEWAY_IMAGE_TAG") != "" {
		targets = append(targets, "inference-gateway")
	}
	if err := assertDiscoveredTargets(body, targets, false); err != nil {
		t.Fatal(err)
	}
	rules := string(requireHTTP(t, client, http.MethodGet, forward.URL+"/api/v1/rules", nil, http.StatusOK))
	for _, alert := range []string{"IterabasePlatformTargetDown", "IterabaseGatewayOutcomeUnknown", "IterabaseDispatchWithoutWorkers", "IterabaseInferenceGatewayHighErrorRate", "IterabaseDataVGCapacityWarning", "IterabaseLVMClaimPending"} {
		if !strings.Contains(rules, alert) {
			t.Fatalf("Prometheus did not load shipped alert %s", alert)
		}
	}
	state.stopForward(t, forward)
}

func assertPrometheusPersistenceStage(t *testing.T, state *chartState) {
	t.Helper()
	client, err := httpx.Client(15 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	prometheusService := kubePrometheusStackComponentName("prometheus")
	forward := state.forward(t, "svc/"+prometheusService, 9090, "http")
	timestamp, value, err := prometheusInstantSample(client, forward.URL, `up{job="postgresql"}`)
	if err != nil || value != "1" {
		t.Fatalf("capture pre-restart PostgreSQL sample: timestamp=%v value=%q err=%v", timestamp, value, err)
	}
	// Stop the producer before the restart. The bounded historical interval ends
	// at the captured timestamp, so a fresh post-restart scrape cannot satisfy it.
	state.kubectl(t, 2*time.Minute, "scale", "deployment/"+testRelease+"-postgresql-exporter", "-n", testNamespace, "--replicas=0")
	state.kubectl(t, 3*time.Minute, "rollout", "status", "deployment/"+testRelease+"-postgresql-exporter", "-n", testNamespace, "--timeout=2m")
	state.stopForward(t, forward)
	pod := state.firstPod(t, "app.kubernetes.io/name=prometheus")
	state.kubectl(t, 2*time.Minute, "delete", "pod", pod, "-n", testNamespace, "--wait=true")
	state.waitForPods(t, "app.kubernetes.io/name=prometheus", 6*time.Minute)
	forward = state.forward(t, "svc/"+prometheusService, 9090, "http")
	query := url.Values{
		"query": {`up{job="postgresql"}`},
		"start": {strconv.FormatFloat(timestamp-30, 'f', 3, 64)},
		"end":   {strconv.FormatFloat(timestamp+0.001, 'f', 3, 64)},
		"step":  {"1"},
	}
	body := requireHTTP(t, client, http.MethodGet, forward.URL+"/api/v1/query_range?"+query.Encode(), nil, http.StatusOK)
	if err := assertHistoricalPrometheusSample(body, timestamp+0.001, "1"); err != nil {
		t.Fatalf("pre-restart Prometheus sample did not survive: %v", err)
	}
	state.stopForward(t, forward)
}

func assertLokiPersistenceStage(t *testing.T, state *chartState) {
	t.Helper()
	client, err := httpx.Client(15 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("lokipersist%d", time.Now().UnixNano())
	emitter := "log-emitter-" + marker
	started := time.Now().Add(-time.Minute)
	state.kubectl(t, 30*time.Second, "run", emitter, "-n", testNamespace, "--image=busybox:1.37.0", "--restart=Never", "--",
		"sh", "-c", fmt.Sprintf("i=0; while [ $i -lt 20 ]; do echo %s; sleep 1; i=$((i+1)); done", marker))
	state.kubectl(t, 2*time.Minute, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/"+emitter, "-n", testNamespace, "--timeout=90s")
	ended := time.Now().Add(time.Minute)
	forward := state.forward(t, "svc/"+testRelease+"-loki", 3100, "http")
	queryURL := lokiMarkerURL(forward.URL, emitter, marker, started, ended)
	if err := waitMarker(state.ctx, client, queryURL, marker, 4*time.Minute); err != nil {
		t.Fatalf("Promtail did not ingest stopped producer marker: %v", err)
	}
	state.kubectl(t, 30*time.Second, "delete", "pod", emitter, "-n", testNamespace, "--wait=true")
	state.stopForward(t, forward)
	pod := state.firstPod(t, "app.kubernetes.io/name=loki,app.kubernetes.io/component=single-binary")
	state.kubectl(t, 2*time.Minute, "delete", "pod", pod, "-n", testNamespace, "--wait=true")
	state.waitForPods(t, "app.kubernetes.io/name=loki,app.kubernetes.io/component=single-binary", 6*time.Minute)
	forward = state.forward(t, "svc/"+testRelease+"-loki", 3100, "http")
	body := requireHTTP(t, client, http.MethodGet, lokiMarkerURL(forward.URL, emitter, marker, started, ended), nil, http.StatusOK)
	if !strings.Contains(string(body), marker) {
		t.Fatalf("pre-restart Loki marker missing from fixed historical interval: %s", stateSafeBody(body))
	}
	state.stopForward(t, forward)
}

func assertPlatformMetrics(t *testing.T, state *chartState, client *http.Client, prometheusURL string, includeHarness, includeToolRunner bool) {
	t.Helper()
	for _, query := range platformMetricQueries(includeHarness, includeToolRunner) {
		if err := waitPrometheusValue(state.ctx, client, prometheusURL, query, "1", 5*time.Minute); err != nil {
			t.Fatalf("representative platform metric %s did not become queryable: %v", query, err)
		}
	}
}

func platformMetricQueries(includeHarness, includeToolRunner bool) []string {
	queries := []string{}
	if os.Getenv("CONTROL_PLANE_IMAGE_REPO") != "" && os.Getenv("CONTROL_PLANE_IMAGE_TAG") != "" {
		queries = append(queries,
			`count(up{job="control-plane",component="api"} == 1)`,
			`count(up{job="control-plane",component="manager"} == 1)`,
			`count(up{job="control-plane",component="gateway"} == 1)`,
			`count(control_plane_build_info{component="api"})`,
			`count(control_plane_build_info{component="gateway"})`,
			`count(go_goroutines{job="control-plane",component="manager"})`,
		)
	}
	if os.Getenv("INFERENCE_GATEWAY_IMAGE_REPO") != "" && os.Getenv("INFERENCE_GATEWAY_IMAGE_TAG") != "" {
		queries = append(queries,
			`count(up{job="inference-gateway",component="inference-gateway"} == 1)`,
			`count(inference_gateway_build_info)`,
		)
	}
	if includeHarness {
		queries = append(queries,
			`count(up{job="control-plane",component="dispatch"} == 1)`,
			`count(up{job="control-plane",component="harness"} == 1)`,
			`count(control_plane_harness_build_info)`,
		)
	}
	if includeToolRunner {
		queries = append(queries,
			`count(up{job="control-plane",component="tool-runner"} == 1)`,
			`count(up{job="control-plane",component="tool-materializer"} == 1)`,
		)
	}
	return queries
}

func TestUnitObservabilityHarnessUsesDedicatedAgentPoolStorageClass(t *testing.T) {
	if observabilityHarnessStorageClass != "iterabase-agentpool-lvm-xfs" {
		t.Fatalf("observability harness storage class=%q", observabilityHarnessStorageClass)
	}
}

func TestUnitFeatureRuntimeAssertionsMatchDisabledComponents(t *testing.T) {
	t.Setenv("HARNESS_IMAGE_REPO", "example.invalid/harness")
	t.Setenv("HARNESS_IMAGE_TAG", "candidate")
	t.Setenv("TOOL_RUNNER_IMAGE_REPO", "example.invalid/tool-runner")
	t.Setenv("TOOL_RUNNER_IMAGE_TAG", "candidate")

	selectors := strings.Join(stackReadinessSelectors(false, false), "\n")
	for _, component := range []string{"component=dispatch", "component=harness", "component=tool-runner"} {
		if strings.Contains(selectors, component) {
			t.Fatalf("disabled component %q remained in readiness selectors: %s", component, selectors)
		}
	}
	queries := strings.Join(platformMetricQueries(false, false), "\n")
	for _, component := range []string{`component="dispatch"`, `component="harness"`, `component="tool-runner"`, `component="tool-materializer"`} {
		if strings.Contains(queries, component) {
			t.Fatalf("disabled component %q remained in metric queries: %s", component, queries)
		}
	}

	candidateSelectors := strings.Join(stackReadinessSelectors(true, true), "\n")
	for _, component := range []string{"component=dispatch", "component=harness", "component=tool-runner"} {
		if !strings.Contains(candidateSelectors, component) {
			t.Fatalf("enabled component %q missing from readiness selectors: %s", component, candidateSelectors)
		}
	}
	candidateQueries := strings.Join(platformMetricQueries(true, true), "\n")
	for _, component := range []string{`component="dispatch"`, `component="harness"`, `component="tool-runner"`, `component="tool-materializer"`} {
		if !strings.Contains(candidateQueries, component) {
			t.Fatalf("enabled component %q missing from metric queries: %s", component, candidateQueries)
		}
	}
}

func requirePrometheusSeriesLabelValue(client *http.Client, baseURL, selector, label, want string) error {
	endpoint := baseURL + "/api/v1/series?" + url.Values{"match[]": {selector}}.Encode()
	resp, err := client.Get(endpoint)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	var payload struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	if payload.Status != "success" {
		return fmt.Errorf("series status=%q", payload.Status)
	}
	for _, series := range payload.Data {
		if series[label] == want {
			return nil
		}
	}
	return fmt.Errorf("selector %q returned no series with %s=%q", selector, label, want)
}

func waitPrometheusValue(ctx context.Context, client *http.Client, baseURL, query, want string, timeout time.Duration) error {
	return poll.Until(ctx, timeout, 5*time.Second, func(context.Context) (bool, string, error) {
		return observePrometheusValue(client, baseURL, query, want)
	})
}

func observePrometheusValue(client *http.Client, baseURL, query, want string) (bool, string, error) {
	_, value, err := prometheusInstantSample(client, baseURL, query)
	if errors.Is(err, errPrometheusNoSample) {
		return false, "query returned no sample", nil
	}
	if err != nil {
		return false, "query failed", err
	}
	return value == want, "value=" + value, nil
}

var errPrometheusNoSample = errors.New("query returned no sample")

func prometheusInstantSample(client *http.Client, baseURL, query string) (float64, string, error) {
	endpoint := baseURL + "/api/v1/query?" + url.Values{"query": {query}}.Encode()
	resp, err := client.Get(endpoint)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var payload struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, "", err
	}
	if payload.Status != "success" {
		return 0, "", fmt.Errorf("query status=%q", payload.Status)
	}
	if len(payload.Data) == 0 {
		return 0, "", fmt.Errorf("query response missing data")
	}
	var data struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(payload.Data, &data); err != nil {
		return 0, "", fmt.Errorf("decode query data: %w", err)
	}
	if data.ResultType != "vector" {
		return 0, "", fmt.Errorf("query resultType=%q, want %q", data.ResultType, "vector")
	}
	if len(data.Result) == 0 {
		return 0, "", fmt.Errorf("query response missing result")
	}
	var result []struct {
		Value []json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(data.Result, &result); err != nil {
		return 0, "", fmt.Errorf("decode query result: %w", err)
	}
	if result == nil {
		return 0, "", fmt.Errorf("query result must be an array")
	}
	if len(result) == 0 {
		return 0, "", errPrometheusNoSample
	}
	if len(result[0].Value) != 2 {
		return 0, "", fmt.Errorf("query returned malformed sample")
	}
	var timestamp *float64
	var value *string
	if err := json.Unmarshal(result[0].Value[0], &timestamp); err != nil {
		return 0, "", fmt.Errorf("decode query timestamp: %w", err)
	}
	if timestamp == nil {
		return 0, "", fmt.Errorf("query timestamp must be a number")
	}
	if err := json.Unmarshal(result[0].Value[1], &value); err != nil {
		return 0, "", fmt.Errorf("decode query value: %w", err)
	}
	if value == nil {
		return 0, "", fmt.Errorf("query value must be a string")
	}
	return *timestamp, *value, nil
}

func lokiMarkerURL(baseURL, pod, marker string, start, end time.Time) string {
	query := fmt.Sprintf(`{pod=%q} |= %q`, pod, marker)
	values := url.Values{
		"query":     {query},
		"start":     {strconv.FormatInt(start.UnixNano(), 10)},
		"end":       {strconv.FormatInt(end.UnixNano(), 10)},
		"direction": {"forward"},
		"limit":     {"20"},
	}
	return baseURL + "/loki/api/v1/query_range?" + values.Encode()
}

func waitMarker(ctx context.Context, client *http.Client, endpoint, marker string, timeout time.Duration) error {
	return poll.Until(ctx, timeout, 5*time.Second, func(context.Context) (bool, string, error) {
		resp, err := client.Get(endpoint)
		if err != nil {
			return false, "query failed", err
		}
		defer func() { _ = resp.Body.Close() }()
		var payload json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return false, "decode failed", err
		}
		found := strings.Contains(string(payload), marker)
		return found, fmt.Sprintf("marker found=%t", found), nil
	})
}
