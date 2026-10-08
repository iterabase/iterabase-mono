package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/httpx"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/poll"
)

// observabilityTLSPlatform is the platform values the observability-tls
// scenario installs: the observability and internal-TLS presets plus the
// verified control-plane edge. The certificate substrate installs with the same
// values-tls.yaml so the ordered internal CA is the one the platform adopts.
var observabilityTLSPlatform = platformValues{
	"values-observability.yaml", "values-tls.yaml", platformValuesBase, platformValuesRuntime, "test/e2e/values/observability-tls.yaml",
}

func observabilityTLSScenario() sharede2e.Definition {
	diagnostics, cleanup := scenarioHooks()
	return sharede2e.Define(sharede2e.Scenario[*chartState]{
		Metadata: chartScenarioMetadata(
			"observability-tls",
			"Installs the internal-TLS observability composition on the ordered internal CA and proves a single adopted root authority whose issued stack, datastore, and control-plane leaves chain to the mounted root; verified HTTPS for the stack, exporters, self-monitors, Grafana datasources/sidecars, Loki gateway, Promtail, and Alertmanager; distinct verified control-plane edge/backend TLS; gateway dependency readiness; rejected plaintext Redis/PostgreSQL transport; and root-key stability across a reconcile.",
			"test-e2e-observability-tls", 50,
			[]string{"HOR-371", "HOR-408", "HOR-414", "HOR-418", "HOR-420", "HOR-416", "HOR-475", "HOR-507", "HOR-528", "HOR-545", "HOR-590", "DES-HOR-545-01"},
			[]string{"control-plane-chart", "inference-gateway-chart", "iterabase-platform-chart"},
			append(substrateRenders("opo1", "values-tls.yaml"), observabilityCandidateValues(observabilityTLSPlatform, true, true).render()),
		),
		NewState: newChartState,
		Stages: []sharede2e.Stage[*chartState]{
			{Name: "create-kind", Run: createKindStage},
			{Name: "import-runtime-images", DependsOn: []string{"create-kind"}, Run: importRuntimeImagesStage},
			{Name: "install-certificate-substrate", DependsOn: []string{"import-runtime-images"}, Run: installInternalTLSCertificateSubstrateStage},
			{Name: "install-lvm-storage-substrate", DependsOn: []string{"install-certificate-substrate"}, Run: installLVMStorageStage},
			{Name: "install-tool-source", DependsOn: []string{"install-lvm-storage-substrate"}, Run: installObservabilityToolSourceStage},
			{Name: "install-observability-tls", DependsOn: []string{"install-tool-source"}, Run: installObservabilityTLSStage},
			{Name: "install-harness-worker", DependsOn: []string{"install-observability-tls"}, Run: installObservabilityHarnessStage},
			{Name: "assert-stack-readiness", DependsOn: []string{"install-harness-worker"}, Run: assertStackReadinessStage},
			{Name: "assert-internal-identities", DependsOn: []string{"install-observability-tls"}, Run: assertInternalIdentitiesStage},
			{Name: "assert-issued-identities", DependsOn: []string{"assert-stack-readiness", "assert-internal-identities"}, Run: assertObservabilityIdentitiesStage},
			{Name: "assert-verified-stack-https", DependsOn: []string{"assert-issued-identities"}, Run: assertVerifiedStackHTTPSStage},
			{Name: "assert-exporter-client-paths", DependsOn: []string{"assert-verified-stack-https"}, Run: assertTLSExporterPathsStage},
			{Name: "assert-verified-self-monitors", DependsOn: []string{"assert-verified-stack-https"}, Run: assertVerifiedSelfMonitorsStage},
			{Name: "assert-grafana-datasources-sidecars", DependsOn: []string{"assert-verified-stack-https"}, Run: assertGrafanaTLSPathsStage},
			{Name: "assert-loki-gateway", DependsOn: []string{"assert-verified-stack-https"}, Run: assertLokiGatewayTLSStage},
			{Name: "assert-promtail-loki", DependsOn: []string{"assert-verified-stack-https"}, Run: assertTLSPromtailPathStage},
			{Name: "assert-prometheus-alertmanager", DependsOn: []string{"assert-verified-stack-https"}, Run: assertTLSAlertmanagerPathStage},
			{Name: "assert-gateway-dependencies", DependsOn: []string{"assert-internal-identities"}, Run: assertGatewayDependenciesStage},
			{Name: "assert-gateway-mounted-ca", DependsOn: []string{"assert-gateway-dependencies"}, Run: assertGatewayMountedCAStage},
			{Name: "assert-control-plane-verified-https", DependsOn: []string{"assert-internal-identities"}, Run: assertControlPlaneVerifiedHTTPSStage},
			{Name: "assert-control-plane-ingress-verified-tls", DependsOn: []string{"assert-control-plane-verified-https"}, Run: assertControlPlaneIngressVerifiedTLSStage},
			{Name: "assert-redis-transport", DependsOn: []string{"assert-internal-identities"}, Run: assertRedisTransportStage},
			{Name: "assert-postgresql-transport", DependsOn: []string{"assert-internal-identities"}, Run: assertPostgreSQLTransportStage},
			{Name: "reconcile-internal-tls-authority", DependsOn: []string{
				"assert-exporter-client-paths", "assert-verified-self-monitors", "assert-grafana-datasources-sidecars",
				"assert-loki-gateway", "assert-promtail-loki", "assert-prometheus-alertmanager", "assert-gateway-mounted-ca",
				"assert-control-plane-ingress-verified-tls", "assert-redis-transport", "assert-postgresql-transport",
			}, Run: reconcileInternalTLSAuthorityStage},
		},
		Diagnostics: diagnostics,
		Cleanup:     cleanup,
	})
}

func installObservabilityTLSStage(t *testing.T, state *chartState) {
	t.Helper()
	state.installPlatform(t, 22*time.Minute, observabilityValueFiles(t, state, observabilityTLSPlatform)...)
	assertCandidateImages(t, state)
}

func installInternalTLSCertificateSubstrateStage(t *testing.T, state *chartState) {
	t.Helper()
	state.installSubstrate(t, filepathFromCharts(state, "values-tls.yaml"))
	state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Ready", "clusterissuer/internal-ca", "--timeout=3m")
	state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Ready", "certificate/"+internalCARootSecretName(), "-n", testNamespace, "--timeout=3m")
	state.internalCARootUID = state.kubectl(t, 30*time.Second, "get", "certificate/"+internalCARootSecretName(), "-n", testNamespace,
		"-o", "jsonpath={.metadata.uid}")
	owner := state.kubectl(t, 30*time.Second, "get", "certificate/"+internalCARootSecretName(), "-n", testNamespace,
		"-o", "jsonpath={.metadata.annotations.meta\\.helm\\.sh/release-name}")
	if owner != testRelease {
		t.Fatalf("ordered internal CA owner=%q want future platform release %q", owner, testRelease)
	}
}

func assertInternalIdentitiesStage(t *testing.T, state *chartState) {
	t.Helper()
	state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Ready", "clusterissuer/internal-ca", "--timeout=3m")
	currentRootUID := state.kubectl(t, 30*time.Second, "get", "certificate/"+internalCARootSecretName(), "-n", testNamespace,
		"-o", "jsonpath={.metadata.uid}")
	if state.internalCARootUID != "" && currentRootUID != state.internalCARootUID {
		t.Fatalf("platform did not adopt the ordered internal CA in place: before=%q after=%q", state.internalCARootUID, currentRootUID)
	}
	for _, certificate := range coreInternalCALeafSecrets() {
		state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Ready", "certificate/"+certificate, "-n", testNamespace, "--timeout=3m")
	}
	// HOR-528: one root authority, first revision, and every issued workload
	// leaf chaining to the exact root the clients mount.
	assertSingleInternalCARootAuthority(t, state)
	assertIssuedChainsMatchMountedRoot(t, state, coreInternalCALeafSecrets()...)
}

// reconcileInternalTLSAuthorityStage reapplies both the ordered certificate
// companion and the platform with unchanged values and proves the internal CA
// root was adopted, not re-issued: the exercised reconcile re-applies the one
// shared identity both writers render, so cert-manager has no reason to issue a
// new root and the leaves stay valid.
func reconcileInternalTLSAuthorityStage(t *testing.T, state *chartState) {
	t.Helper()
	uidBefore := state.kubectl(t, 30*time.Second, "get", "certificate/"+internalCARootSecretName(), "-n", testNamespace,
		"-o", "jsonpath={.metadata.uid}")
	fingerprintBefore := internalCARootFingerprint(t, state)

	state.installSubstrate(t, filepathFromCharts(state, "values-tls.yaml"))
	state.installPlatform(t, 22*time.Minute, observabilityValueFiles(t, state, observabilityTLSPlatform)...)
	state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Ready", "certificate/"+internalCARootSecretName(), "-n", testNamespace, "--timeout=3m")

	assertInternalCARootStable(t, state, uidBefore, fingerprintBefore)
	leaves := append(coreInternalCALeafSecrets(), observabilityInternalCALeafSecrets()...)
	assertIssuedChainsMatchMountedRoot(t, state, leaves...)
}

func assertGatewayDependenciesStage(t *testing.T, state *chartState) {
	t.Helper()
	state.kubectl(t, 6*time.Minute, "rollout", "status", "deployment/"+testRelease+"-gateway", "-n", testNamespace, "--timeout=5m")
	forward := state.forward(t, "svc/"+testRelease+"-gateway", 8080, "http")
	client, err := httpx.Client(15 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	body := requireHTTP(t, client, http.MethodGet, forward.URL+"/readyz", nil, http.StatusOK)
	if !strings.Contains(string(body), `"fresh":true`) {
		t.Fatalf("gateway snapshot is not fresh: %s", stateSafeBody(body))
	}
	state.stopForward(t, forward)
}

// assertGatewayMountedCAStage proves the inference gateway mounts the exact
// issued root at the CA path its rendered verify-full/rediss client
// configuration names (scripts/check-gateway-tls-client.sh proves that config).
func assertGatewayMountedCAStage(t *testing.T, state *chartState) {
	t.Helper()
	pod := state.firstPod(t, "app.kubernetes.io/name=inference-gateway")
	assertMountedInternalCA(t, state, pod, "/etc/iterabase/internal-ca/ca.crt")
}

func assertControlPlaneVerifiedHTTPSStage(t *testing.T, state *chartState) {
	t.Helper()
	state.kubectl(t, 6*time.Minute, "rollout", "status", "deployment/"+testRelease+"-control-plane-api", "-n", testNamespace, "--timeout=5m")
	ca := decodeSecretValue(t, state, internalCARootSecretName(), "ca.crt")
	forward := state.forward(t, "svc/"+testRelease+"-control-plane-api", 8080, "https")
	client := verifiedClient(t, ca, testRelease+"-control-plane-api."+testNamespace+".svc")
	requireHTTP(t, client, http.MethodGet, forward.URL+"/healthz", nil, http.StatusOK)
	state.stopForward(t, forward)
}

func assertControlPlaneIngressVerifiedTLSStage(t *testing.T, state *chartState) {
	t.Helper()
	const host = "control-plane.iterabase.local"
	ingress := testRelease + "-control-plane-api"
	internalSecret := testRelease + "-control-plane-api-tls"
	edgeSecret := testRelease + "-control-plane-api-ingress-tls"

	state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Ready", "certificate/"+edgeSecret, "-n", testNamespace, "--timeout=3m")
	if got := state.kubectl(t, 30*time.Second, "get", "ingress/"+ingress, "-n", testNamespace,
		"-o", "jsonpath={.spec.tls[0].secretName}"); got != edgeSecret {
		t.Fatalf("control-plane edge TLS Secret=%q want=%q", got, edgeSecret)
	}
	if edgeSecret == internalSecret {
		t.Fatal("control-plane edge and backend TLS Secrets must differ")
	}
	expectedAnnotations := map[string]string{
		"nginx.ingress.kubernetes.io/backend-protocol":      "HTTPS",
		"nginx.ingress.kubernetes.io/proxy-ssl-secret":      testNamespace + "/" + internalSecret,
		"nginx.ingress.kubernetes.io/proxy-ssl-verify":      "on",
		"nginx.ingress.kubernetes.io/proxy-ssl-server-name": "on",
		"nginx.ingress.kubernetes.io/proxy-ssl-name":        testRelease + "-control-plane-api." + testNamespace + ".svc",
	}
	for annotation, want := range expectedAnnotations {
		got := state.kubectl(t, 30*time.Second, "get", "ingress/"+ingress, "-n", testNamespace,
			"-o", fmt.Sprintf("jsonpath={.metadata.annotations.%s}", strings.ReplaceAll(annotation, ".", "\\.")))
		if got != want {
			t.Fatalf("control-plane ingress annotation %s=%q want=%q", annotation, got, want)
		}
	}

	edgeCertificate := decodeSecretValue(t, state, edgeSecret, "tls.crt")
	forward := state.forward(t, "svc/"+testRelease+"-ingress-nginx-controller", 443, "https")
	client := verifiedDialClient(t, edgeCertificate, host, fmt.Sprintf("127.0.0.1:%d", forward.LocalPort))
	if err := waitHTTPReady(state.ctx, client, "https://"+host+"/healthz", 2*time.Minute); err != nil {
		t.Fatalf("verified control-plane ingress did not become ready: %v", err)
	}
	requireHTTP(t, client, http.MethodGet, "https://"+host+"/healthz", nil, http.StatusOK)
	requireHTTP(t, client, http.MethodGet, "https://"+host+"/", nil, http.StatusOK)
	state.stopForward(t, forward)
}

// assertRedisTransportStage proves the Redis server rejects authenticated
// plaintext and accepts only CA-verified TLS with AUTH.
func assertRedisTransportStage(t *testing.T, state *chartState) {
	t.Helper()
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: redis-transport-probe
  namespace: %[1]s
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: redis:7-alpine@sha256:ff02b58f971e7d7d156a1267e283fcbbeee91773b6aa36c49dac28ecfe28eadf
      env:
        - name: REDIS_PASSWORD
          valueFrom:
            secretKeyRef:
              name: %[2]s-redis
              key: redis-password
      command: ["/bin/sh", "-c"]
      args:
        - |
          if redis-cli -h %[2]s-redis -p 6379 -a "$REDIS_PASSWORD" PING 2>/dev/null | grep -q PONG; then
            echo "authenticated plaintext unexpectedly succeeded" >&2
            exit 1
          fi
          test "$(redis-cli --tls --cacert /ca/ca.crt -h %[2]s-redis -p 6379 PING 2>/dev/null)" = "NOAUTH Authentication required."
          test "$(redis-cli --tls --cacert /ca/ca.crt -h %[2]s-redis -p 6379 -a "$REDIS_PASSWORD" PING 2>/dev/null)" = PONG
      volumeMounts:
        - name: ca
          mountPath: /ca
          readOnly: true
  volumes:
    - name: ca
      secret:
        secretName: %[3]s
        items:
          - key: ca.crt
            path: ca.crt
`, testNamespace, testRelease, internalCARootSecretName())
	state.kubectl(t, 30*time.Second, "apply", "-f", state.writeManifest(t, "redis-transport.yaml", manifest))
	state.kubectl(t, 3*time.Minute, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/redis-transport-probe", "-n", testNamespace, "--timeout=2m")
}

// assertPostgreSQLTransportStage proves PostgreSQL rejects authenticated
// plaintext and accepts verify-full TLS against the mounted root.
func assertPostgreSQLTransportStage(t *testing.T, state *chartState) {
	t.Helper()
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: postgresql-transport-probe
  namespace: %[1]s
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: postgres:16-alpine@sha256:cf78e76683b9ca8c5733cbbdce6c9262b45b6767934dd0a95e671f9a0fc20685
      env:
        - name: PGPASSWORD
          valueFrom:
            secretKeyRef:
              name: %[2]s-postgresql
              key: password
      command: ["/bin/sh", "-c"]
      args:
        - |
          if psql "host=%[2]s-postgresql port=5432 user=controlplane dbname=controlplane sslmode=disable connect_timeout=5" -c "select 1" >/tmp/plain 2>&1; then
            echo "authenticated plaintext unexpectedly succeeded" >&2
            exit 1
          fi
          psql "host=%[2]s-postgresql port=5432 user=controlplane dbname=controlplane sslmode=verify-full sslrootcert=/ca/ca.crt connect_timeout=5" -c "select 1" | grep -q "(1 row)"
      volumeMounts:
        - name: ca
          mountPath: /ca
          readOnly: true
  volumes:
    - name: ca
      secret:
        secretName: %[3]s
        items:
          - key: ca.crt
            path: ca.crt
`, testNamespace, testRelease, internalCARootSecretName())
	state.kubectl(t, 30*time.Second, "apply", "-f", state.writeManifest(t, "postgresql-transport.yaml", manifest))
	state.kubectl(t, 3*time.Minute, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/postgresql-transport-probe", "-n", testNamespace, "--timeout=2m")
}

func assertObservabilityIdentitiesStage(t *testing.T, state *chartState) {
	t.Helper()
	state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Ready", "clusterissuer/internal-ca", "--timeout=3m")
	for _, certificate := range []string{
		"observability-prometheus-tls", "observability-alertmanager-tls", "observability-grafana-tls", "observability-loki-tls",
	} {
		state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Ready", "certificate/"+certificate, "-n", testNamespace, "--timeout=3m")
	}
	// HOR-528: the same internal CA root signs the stack and the datastore/
	// control-plane leaves, and it is still the first and only authority.
	assertSingleInternalCARootAuthority(t, state)
	leaves := append(coreInternalCALeafSecrets(), observabilityInternalCALeafSecrets()...)
	assertIssuedChainsMatchMountedRoot(t, state, leaves...)
}

func assertVerifiedStackHTTPSStage(t *testing.T, state *chartState) {
	t.Helper()
	ca := decodeSecretValue(t, state, testRelease+"-internal-ca-root", "ca.crt")
	checks := []struct {
		service    string
		port       int
		serverName string
		path       string
	}{
		{kubePrometheusStackComponentName("prometheus"), 9090, kubePrometheusStackComponentName("prometheus") + "." + testNamespace + ".svc", "/-/healthy"},
		{kubePrometheusStackComponentName("alertmanager"), 9093, kubePrometheusStackComponentName("alertmanager") + "." + testNamespace + ".svc", "/-/healthy"},
		{testRelease + "-grafana", 80, testRelease + "-grafana." + testNamespace + ".svc", "/api/health"},
		{testRelease + "-loki", 3100, testRelease + "-loki." + testNamespace + ".svc", "/ready"},
	}
	for _, check := range checks {
		forward := state.forward(t, "svc/"+check.service, check.port, "https")
		client := verifiedClient(t, ca, check.serverName)
		if err := waitHTTPReady(state.ctx, client, forward.URL+check.path, 2*time.Minute); err != nil {
			t.Fatalf("verified HTTPS %s: %v", check.service, err)
		}
		requireHTTP(t, client, http.MethodGet, forward.URL+check.path, nil, http.StatusOK)
		state.stopForward(t, forward)
	}
}

func assertTLSExporterPathsStage(t *testing.T, state *chartState) {
	t.Helper()
	assertTLSExporterPaths(t, state,
		os.Getenv("HARNESS_IMAGE_REPO") != "" && os.Getenv("HARNESS_IMAGE_TAG") != "",
		os.Getenv("TOOL_RUNNER_IMAGE_REPO") != "" && os.Getenv("TOOL_RUNNER_IMAGE_TAG") != "",
	)
}

func assertTLSExporterPaths(t *testing.T, state *chartState, includeHarness, includeToolRunner bool) {
	t.Helper()
	ca := decodeSecretValue(t, state, testRelease+"-internal-ca-root", "ca.crt")
	prometheusService := kubePrometheusStackComponentName("prometheus")
	forward := state.forward(t, "svc/"+prometheusService, 9090, "https")
	client := verifiedClient(t, ca, prometheusService+"."+testNamespace+".svc")
	for _, metric := range []string{"pg_up", "redis_up"} {
		if err := waitPrometheusValue(state.ctx, client, forward.URL, metric, "1", 5*time.Minute); err != nil {
			t.Fatalf("%s did not become 1 over verified Prometheus HTTPS: %v", metric, err)
		}
	}
	assertPlatformMetrics(t, state, client, forward.URL, includeHarness, includeToolRunner)
	state.stopForward(t, forward)
}

func assertVerifiedSelfMonitorsStage(t *testing.T, state *chartState) {
	t.Helper()
	ca := decodeSecretValue(t, state, testRelease+"-internal-ca-root", "ca.crt")
	prometheusService := kubePrometheusStackComponentName("prometheus")
	forward := state.forward(t, "svc/"+prometheusService, 9090, "https")
	client := verifiedClient(t, ca, prometheusService+"."+testNamespace+".svc")
	var last []byte
	err := poll.Until(state.ctx, 5*time.Minute, 5*time.Second, func(context.Context) (bool, string, error) {
		req, requestErr := http.NewRequestWithContext(state.ctx, http.MethodGet, forward.URL+"/api/v1/targets?state=active", nil)
		if requestErr != nil {
			return false, "build request", requestErr
		}
		resp, requestErr := client.Do(req)
		if requestErr != nil {
			return false, "query targets", requestErr
		}
		defer func() { _ = resp.Body.Close() }()
		var payload json.RawMessage
		if decodeErr := json.NewDecoder(resp.Body).Decode(&payload); decodeErr != nil {
			return false, "decode targets", decodeErr
		}
		last = payload
		if assertErr := assertVerifiedSelfMonitorTargets(last); assertErr != nil {
			return false, assertErr.Error(), nil
		}
		return true, "all verified stack targets are up", nil
	})
	if err != nil {
		t.Fatalf("verified stack self-monitors did not converge: %v\n%s", err, stateSafeBody(last))
	}
	if err := assertVerifiedSelfMonitorTargets(last); err != nil {
		t.Fatal(err)
	}
	state.stopForward(t, forward)
}

func assertVerifiedSelfMonitorTargets(body []byte) error {
	for _, monitor := range []struct {
		name      string
		endpoints []serviceMonitorEndpointExpectation
	}{
		{
			name: testRelease + "-prometheus-internal-tls",
			endpoints: []serviceMonitorEndpointExpectation{
				{index: 0, name: "http-web", port: "9090"},
			},
		},
		{
			name: testRelease + "-prometheus-reloader-internal-tls",
			endpoints: []serviceMonitorEndpointExpectation{
				{index: 0, name: "reloader-web", port: "8080"},
			},
		},
		{
			name: testRelease + "-alertmanager-internal-tls",
			endpoints: []serviceMonitorEndpointExpectation{
				{index: 0, name: "http-web", port: "9093"},
			},
		},
		{
			name: testRelease + "-alertmanager-reloader-internal-tls",
			endpoints: []serviceMonitorEndpointExpectation{
				{index: 0, name: "reloader-web", port: "8080"},
			},
		},
	} {
		if err := assertServiceMonitorTargets(body, testNamespace, monitor.name, monitor.endpoints); err != nil {
			return err
		}
	}
	return assertDiscoveredTargets(body, []string{
		testRelease + "-grafana-internal-tls", testRelease + "-loki-internal-tls",
	}, true)
}

func assertGrafanaTLSPathsStage(t *testing.T, state *chartState) {
	t.Helper()
	ca := decodeSecretValue(t, state, testRelease+"-internal-ca-root", "ca.crt")
	username := string(decodeSecretValue(t, state, testRelease+"-grafana", "admin-user"))
	password := string(decodeSecretValue(t, state, testRelease+"-grafana", "admin-password"))
	state.redactor.Add(username, password)
	forward := state.forward(t, "svc/"+testRelease+"-grafana", 80, "https")
	client := verifiedClient(t, ca, testRelease+"-grafana."+testNamespace+".svc")
	for _, datasource := range []struct {
		uid, healthPath string
	}{
		{"prometheus", "/api/v1/status/buildinfo"},
		{"alertmanager", "/api/v2/status"},
		{"loki", "/ready"},
	} {
		// Exercise the actual upstream health API through Grafana's datasource
		// proxy. This proves URL, Service identity, CA trust, and connectivity;
		// not every built-in datasource plugin implements Grafana's optional
		// plugin-health endpoint.
		endpoint := forward.URL + "/api/datasources/proxy/uid/" + datasource.uid + datasource.healthPath
		err := poll.Until(state.ctx, 3*time.Minute, 4*time.Second, func(context.Context) (bool, string, error) {
			req, requestErr := http.NewRequestWithContext(state.ctx, http.MethodGet, endpoint, nil)
			if requestErr != nil {
				return false, "build request", requestErr
			}
			req.SetBasicAuth(username, password)
			resp, requestErr := client.Do(req)
			if requestErr != nil {
				return false, "datasource request", requestErr
			}
			_ = resp.Body.Close()
			return resp.StatusCode == http.StatusOK, fmt.Sprintf("status=%d", resp.StatusCode), nil
		})
		if err != nil {
			t.Fatalf("Grafana datasource %s did not pass its upstream health API: %v", datasource.uid, err)
		}
	}
	state.stopForward(t, forward)

	// Force both sidecars to process an update, then require a successful reload
	// and reject TLS verification/handshake errors in their retained logs.
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	for _, configMap := range []string{testRelease + "-iterabase-datasources-tls", testRelease + "-loki-datasource"} {
		state.kubectl(t, 30*time.Second, "annotate", "configmap/"+configMap, "-n", testNamespace, "e2e.iterabase.com/reload="+stamp, "--overwrite")
	}
	dashboards := strings.Fields(state.kubectl(t, 30*time.Second, "get", "configmap", "-n", testNamespace,
		"-l", "grafana_dashboard=1", "-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`))
	if len(dashboards) == 0 {
		t.Fatal("no dashboard ConfigMap available to trigger the dashboard sidecar")
	}
	state.kubectl(t, 30*time.Second, "annotate", "configmap/"+dashboards[0], "-n", testNamespace,
		"e2e.iterabase.com/reload="+stamp, "--overwrite")
	for _, container := range []string{"grafana-sc-datasources", "grafana-sc-dashboard"} {
		var logs string
		err := poll.Until(state.ctx, 2*time.Minute, 3*time.Second, func(context.Context) (bool, string, error) {
			out, observeErr := state.kubectlOutput(30*time.Second, "logs", "statefulset/"+testRelease+"-grafana", "-n", testNamespace, "-c", container, "--tail=200")
			if observeErr != nil {
				return false, "read sidecar logs", observeErr
			}
			logs = out
			if strings.Contains(logs, "Response: 200") {
				return true, "successful reload observed", nil
			}
			return false, "no successful reload response yet", nil
		})
		if err != nil {
			t.Fatalf("%s did not complete verified reload: %v\n%s", container, err, logs)
		}
		assertNoTLSFailure(t, container, logs)
	}
}

func assertLokiGatewayTLSStage(t *testing.T, state *chartState) {
	t.Helper()
	forward := state.forward(t, "svc/"+testRelease+"-loki-gateway", 80, "http")
	client := &http.Client{Timeout: 15 * time.Second}
	if err := waitHTTPReady(state.ctx, client, forward.URL+"/loki/api/v1/labels", 2*time.Minute); err != nil {
		t.Fatalf("Loki gateway did not reach TLS backend: %v", err)
	}
	state.stopForward(t, forward)
	logs := state.kubectl(t, 30*time.Second, "logs", "deployment/"+testRelease+"-loki-gateway", "-n", testNamespace, "--tail=300")
	assertNoTLSFailure(t, "Loki gateway", logs)
}

func assertTLSPromtailPathStage(t *testing.T, state *chartState) {
	t.Helper()
	ca := decodeSecretValue(t, state, testRelease+"-internal-ca-root", "ca.crt")
	marker := fmt.Sprintf("tlslogpath%d", time.Now().UnixNano())
	emitter := "tls-log-emitter-" + marker
	started := time.Now().Add(-time.Minute)
	state.kubectl(t, 30*time.Second, "run", emitter, "-n", testNamespace, "--image=busybox:1.37.0", "--restart=Never", "--",
		"sh", "-c", fmt.Sprintf("i=0; while [ $i -lt 15 ]; do echo %s; sleep 1; i=$((i+1)); done", marker))
	state.kubectl(t, 2*time.Minute, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/"+emitter, "-n", testNamespace, "--timeout=90s")
	forward := state.forward(t, "svc/"+testRelease+"-loki", 3100, "https")
	client := verifiedClient(t, ca, testRelease+"-loki."+testNamespace+".svc")
	if err := waitMarker(state.ctx, client, lokiMarkerURL(forward.URL, emitter, marker, started, time.Now().Add(time.Minute)), marker, 4*time.Minute); err != nil {
		t.Fatalf("Promtail did not deliver to verified HTTPS Loki: %v", err)
	}
	state.stopForward(t, forward)
	state.kubectl(t, 30*time.Second, "delete", "pod", emitter, "-n", testNamespace, "--wait=true")
	logs := state.kubectl(t, 30*time.Second, "logs", "daemonset/"+testRelease+"-promtail", "-n", testNamespace, "--tail=300")
	assertNoTLSFailure(t, "Promtail", logs)
}

func assertTLSAlertmanagerPathStage(t *testing.T, state *chartState) {
	t.Helper()
	manifest := `apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: iterabase-tls-path-probe
  namespace: iterabase-system
spec:
  groups:
    - name: iterabase-tls-path-probe
      interval: 5s
      rules:
        - alert: IterabaseTLSPathProbe
          expr: vector(1)
          labels:
            severity: none
          annotations:
            summary: TLS E2E path probe
`
	state.kubectl(t, 30*time.Second, "apply", "-f", state.writeManifest(t, "tls-alert.yaml", manifest))
	ca := decodeSecretValue(t, state, testRelease+"-internal-ca-root", "ca.crt")
	alertmanagerService := kubePrometheusStackComponentName("alertmanager")
	forward := state.forward(t, "svc/"+alertmanagerService, 9093, "https")
	client := verifiedClient(t, ca, alertmanagerService+"."+testNamespace+".svc")
	err := poll.Until(state.ctx, 5*time.Minute, 5*time.Second, func(context.Context) (bool, string, error) {
		resp, requestErr := client.Get(forward.URL + "/api/v2/alerts")
		if requestErr != nil {
			return false, "query alerts", requestErr
		}
		defer func() { _ = resp.Body.Close() }()
		var alerts []struct {
			Labels map[string]string `json:"labels"`
		}
		if decodeErr := json.NewDecoder(resp.Body).Decode(&alerts); decodeErr != nil {
			return false, "decode alerts", decodeErr
		}
		for _, alert := range alerts {
			if alert.Labels["alertname"] == "IterabaseTLSPathProbe" {
				return true, "probe delivered", nil
			}
		}
		return false, "probe not delivered", nil
	})
	if err != nil {
		t.Fatalf("Prometheus did not deliver to verified HTTPS Alertmanager: %v", err)
	}
	state.stopForward(t, forward)
}

func assertNoTLSFailure(t *testing.T, component, logs string) {
	t.Helper()
	lower := strings.ToLower(logs)
	for _, marker := range []string{"certificate verify failed", "tls handshake error", "server gave http response to https client", "client sent an http request to an https server"} {
		if strings.Contains(lower, marker) {
			t.Fatalf("%s logs contain %q:\n%s", component, marker, logs)
		}
	}
}
