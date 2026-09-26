package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/httpx"
)

func internalTLSScenario() sharede2e.Definition {
	diagnostics, cleanup := scenarioHooks()
	return sharede2e.Define(sharede2e.Scenario[*chartState]{
		Metadata: chartScenarioMetadata(
			"internal-tls",
			"Installs the minimal internal-TLS platform and proves a single internal CA root authority, issued identities chaining to that mounted root, distinct verified control-plane edge/backend TLS, gateway dependency readiness, real Redis/PostgreSQL transport enforcement, and root-key stability across a reconcile.",
			"test-e2e-internal-tls", 30,
			[]string{"HOR-371", "HOR-416", "HOR-475", "HOR-507", "HOR-528", "HOR-545", "DES-HOR-545-01"},
			[]string{"control-plane", "inference-gateway", "control-plane-chart", "inference-gateway-chart", "iterabase-platform-chart"},
		),
		NewState: newChartState,
		Stages: []sharede2e.Stage[*chartState]{
			{Name: "create-kind", Run: createKindStage},
			{Name: "import-runtime-images", DependsOn: []string{"create-kind"}, Run: importRuntimeImagesStage},
			{Name: "install-certificate-substrate", DependsOn: []string{"import-runtime-images"}, Run: installInternalTLSCertificateSubstrateStage},
			{Name: "install-lvm-storage-substrate", DependsOn: []string{"install-certificate-substrate"}, Run: installLVMStorageStage},
			{Name: "install-internal-tls-platform", DependsOn: []string{"install-lvm-storage-substrate"}, Run: installInternalTLSPlatformStage},
			{Name: "assert-internal-identities", DependsOn: []string{"install-internal-tls-platform"}, Run: assertInternalIdentitiesStage},
			{Name: "assert-gateway-dependencies", DependsOn: []string{"assert-internal-identities"}, Run: assertGatewayDependenciesStage},
			{Name: "assert-control-plane-verified-https", DependsOn: []string{"assert-internal-identities"}, Run: assertControlPlaneVerifiedHTTPSStage},
			{Name: "assert-control-plane-ingress-verified-tls", DependsOn: []string{"assert-control-plane-verified-https"}, Run: assertControlPlaneIngressVerifiedTLSStage},
			{Name: "assert-rendered-client-config", DependsOn: []string{"assert-gateway-dependencies"}, Run: assertRenderedTLSClientConfigStage},
			{Name: "assert-redis-transport", DependsOn: []string{"assert-internal-identities"}, Run: assertRedisTransportStage},
			{Name: "assert-postgresql-transport", DependsOn: []string{"assert-internal-identities"}, Run: assertPostgreSQLTransportStage},
			{Name: "reconcile-internal-tls-authority", DependsOn: []string{"assert-postgresql-transport"}, Run: reconcileInternalTLSAuthorityStage},
		},
		Diagnostics: diagnostics,
		Cleanup:     cleanup,
	})
}

func installInternalTLSCertificateSubstrateStage(t *testing.T, state *chartState) {
	t.Helper()
	state.installSubstrate(t, filepathFromCharts(state, "values-tls.yaml"))
	state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Ready", "clusterissuer/internal-ca", "--timeout=3m")
	state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Ready", "certificate/"+testRelease+"-internal-ca-root", "-n", testNamespace, "--timeout=3m")
	state.internalCARootUID = state.kubectl(t, 30*time.Second, "get", "certificate/"+testRelease+"-internal-ca-root", "-n", testNamespace,
		"-o", "jsonpath={.metadata.uid}")
	owner := state.kubectl(t, 30*time.Second, "get", "certificate/"+testRelease+"-internal-ca-root", "-n", testNamespace,
		"-o", "jsonpath={.metadata.annotations.meta\\.helm\\.sh/release-name}")
	if owner != testRelease {
		t.Fatalf("ordered internal CA owner=%q want future platform release %q", owner, testRelease)
	}
}

func internalTLSPlatformValues(t *testing.T) map[string]any {
	t.Helper()
	values := runtimePlatformValues(t)
	values["ingress-nginx"] = map[string]any{
		"enabled": true,
		"controller": map[string]any{
			"service": map[string]any{"type": "ClusterIP"},
		},
	}
	controlPlane := values["control-plane"].(map[string]any)
	controlPlane["ingress"] = map[string]any{
		"enabled":   true,
		"className": "nginx",
		"host":      "control-plane.iterabase.local",
		"tls": map[string]any{
			"enabled":       true,
			"clusterIssuer": "selfsigned",
		},
	}
	return values
}

func installInternalTLSPlatformStage(t *testing.T, state *chartState) {
	t.Helper()
	state.installPlatform(t, 16*time.Minute,
		filepathFromCharts(state, "values-tls.yaml"),
		state.writeValues(t, "internal-tls-runtime", internalTLSPlatformValues(t)),
	)
	assertCandidateImages(t, state)
}

func assertInternalIdentitiesStage(t *testing.T, state *chartState) {
	t.Helper()
	state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Ready", "clusterissuer/internal-ca", "--timeout=3m")
	currentRootUID := state.kubectl(t, 30*time.Second, "get", "certificate/"+testRelease+"-internal-ca-root", "-n", testNamespace,
		"-o", "jsonpath={.metadata.uid}")
	if state.internalCARootUID != "" && currentRootUID != state.internalCARootUID {
		t.Fatalf("platform did not adopt the ordered internal CA in place: before=%q after=%q", state.internalCARootUID, currentRootUID)
	}
	for _, certificate := range []string{
		testRelease + "-postgresql-tls", testRelease + "-redis-tls", testRelease + "-control-plane-api-tls",
	} {
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
	state.installPlatform(t, 16*time.Minute,
		filepathFromCharts(state, "values-tls.yaml"),
		state.writeValues(t, "internal-tls-runtime", internalTLSPlatformValues(t)),
	)
	state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Ready", "certificate/"+internalCARootSecretName(), "-n", testNamespace, "--timeout=3m")

	assertInternalCARootStable(t, state, uidBefore, fingerprintBefore)
	assertIssuedChainsMatchMountedRoot(t, state, coreInternalCALeafSecrets()...)
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

func assertControlPlaneVerifiedHTTPSStage(t *testing.T, state *chartState) {
	t.Helper()
	state.kubectl(t, 6*time.Minute, "rollout", "status", "deployment/"+testRelease+"-control-plane-api", "-n", testNamespace, "--timeout=5m")
	ca := decodeSecretValue(t, state, testRelease+"-internal-ca-root", "ca.crt")
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

func assertRenderedTLSClientConfigStage(t *testing.T, state *chartState) {
	t.Helper()
	pod := state.firstPod(t, "app.kubernetes.io/name=inference-gateway")
	out := state.kubectl(t, 30*time.Second, "exec", "-n", testNamespace, pod, "--", "printenv", "DATABASE_URL", "REDIS_URL", "REDIS_TLS_CA_FILE")
	if !strings.Contains(out, "sslmode=verify-full") || !strings.Contains(out, "sslrootcert=") {
		t.Fatalf("gateway DATABASE_URL does not require verified PostgreSQL TLS: %s", out)
	}
	if !strings.Contains(out, "rediss://") || !strings.Contains(out, "/etc/iterabase/internal-ca/ca.crt") {
		t.Fatalf("gateway Redis configuration does not require CA-backed rediss: %s", out)
	}
	assertMountedInternalCA(t, state, pod, "/etc/iterabase/internal-ca/ca.crt")
}

func assertRedisTransportStage(t *testing.T, state *chartState) {
	t.Helper()
	manifest := `apiVersion: v1
kind: Pod
metadata:
  name: redis-transport-probe
  namespace: iterabase-system
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: redis:7-alpine@sha256:ff02b58f971e7d7d156a1267e283fcbbeee91773b6aa36c49dac28ecfe28eadf
      env:
        - name: REDIS_PASSWORD
          valueFrom:
            secretKeyRef:
              name: iterabase-redis
              key: redis-password
      command: ["/bin/sh", "-c"]
      args:
        - |
          if redis-cli -h iterabase-redis -p 6379 -a "$REDIS_PASSWORD" PING 2>/dev/null | grep -q PONG; then
            echo "authenticated plaintext unexpectedly succeeded" >&2
            exit 1
          fi
          test "$(redis-cli --tls --cacert /ca/ca.crt -h iterabase-redis -p 6379 PING 2>/dev/null)" = "NOAUTH Authentication required."
          test "$(redis-cli --tls --cacert /ca/ca.crt -h iterabase-redis -p 6379 -a "$REDIS_PASSWORD" PING 2>/dev/null)" = PONG
      volumeMounts:
        - name: ca
          mountPath: /ca
          readOnly: true
  volumes:
    - name: ca
      secret:
        secretName: iterabase-internal-ca-root
        items:
          - key: ca.crt
            path: ca.crt
`
	state.kubectl(t, 30*time.Second, "apply", "-f", state.writeManifest(t, "redis-transport.yaml", manifest))
	state.kubectl(t, 3*time.Minute, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/redis-transport-probe", "-n", testNamespace, "--timeout=2m")
}

func assertPostgreSQLTransportStage(t *testing.T, state *chartState) {
	t.Helper()
	manifest := `apiVersion: v1
kind: Pod
metadata:
  name: postgresql-transport-probe
  namespace: iterabase-system
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: postgres:16-alpine@sha256:cf78e76683b9ca8c5733cbbdce6c9262b45b6767934dd0a95e671f9a0fc20685
      env:
        - name: PGPASSWORD
          valueFrom:
            secretKeyRef:
              name: iterabase-postgresql
              key: password
      command: ["/bin/sh", "-c"]
      args:
        - |
          if psql "host=iterabase-postgresql port=5432 user=controlplane dbname=controlplane sslmode=disable connect_timeout=5" -c "select 1" >/tmp/plain 2>&1; then
            echo "authenticated plaintext unexpectedly succeeded" >&2
            exit 1
          fi
          psql "host=iterabase-postgresql port=5432 user=controlplane dbname=controlplane sslmode=verify-full sslrootcert=/ca/ca.crt connect_timeout=5" -c "select 1" | grep -q "(1 row)"
      volumeMounts:
        - name: ca
          mountPath: /ca
          readOnly: true
  volumes:
    - name: ca
      secret:
        secretName: iterabase-internal-ca-root
        items:
          - key: ca.crt
            path: ca.crt
`
	state.kubectl(t, 30*time.Second, "apply", "-f", state.writeManifest(t, "postgresql-transport.yaml", manifest))
	state.kubectl(t, 3*time.Minute, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/postgresql-transport-probe", "-n", testNamespace, "--timeout=2m")
}
