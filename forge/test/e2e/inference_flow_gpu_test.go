// Package e2e inference-flow GPU scenario: a full request→completion happy
// path on the real permanent GPU fixture — Forge bootstraps K3s + the GPU operator + the
// iterabase-platform chart, the control-plane deploys a real vLLM backend, and
// a curl to the gateway with an API key returns a real completion.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/nunocgoncalves/iterabase-mono/forge/test/e2e/internal/remotecluster"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/httpx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The GPU scenario's dependent-layer smoke uses the smallest public product
// path that proves the Forge-readied GPU can serve after exact chart/image
// handoff. Portable ModelBackend rendering, identity, catalogue, authorization,
// and inference correctness remain authoritative in control-plane E2E.
func applyInferencePlatformStage(t *testing.T, state *permanentGPUFixtureState) {
	prepareCandidateChart(t, state.host.IP, state.privKeyPath)
	state.runtimeImageDigests = prepareCandidateImages(t, state.host.IP, state.privKeyPath)
	plan := prepareCandidateOverlay(t, state.runID, state.host.IP, state.privKeyPath)
	// GPU readiness was already proven on this host. Reconcile the same config
	// with the platform chart while skipping a redundant GPU-operator upgrade.
	candidateConfig := writeForgeConfigInferenceGPU(
		t, state.runID, state.host.IP, state.privKeyPath, state.chartVersion, plan,
	)
	out := applyOnceArgs(t, state.forgeBin, state.forgeHome, candidateConfig, "--skip-gpu")
	state.bindKubeconfigTunnel(t)
	markers := []string{"action:     skip", "node ready: true", "data storage: iterabase-data", "LVM storage ready: true", "certificate substrate applied: true", "LVM storage substrate applied: true",
		"chart applied: true", "overlay applied: true", "flux installed: true", "gitrepository: ready=True"}
	assertApplyMarkers(t, out, markers...)
	t.Logf("apply output:\n%s", out)
	candidateCluster := remotecluster.Use(t, filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml"))
	assertCandidateImageDigests(t, candidateCluster, "iterabase-system", state.runtimeImageDigests,
		controlPlaneDigestEnv, inferenceGatewayDigestEnv, toolRunnerDigestEnv)
}

func runInferenceGPUStage(t *testing.T, state *permanentGPUFixtureState) {
	runID := state.runID
	forgeHome := state.forgeHome

	// Bind every Kubernetes operation to the kubeconfig fetched by Forge; this
	// helper never creates a Kind cluster or installs a chart directly.
	kcPath := filepath.Join(forgeHome, runID, "kubeconfig.yaml")
	c := remotecluster.Use(t, kcPath)
	namespace := "iterabase-system"
	const (
		alias       = "qwen35"
		mbName      = "qwen35-backend"
		release     = "itb"
		apiService  = "svc/itb-control-plane-api"
		apiSelector = "app.kubernetes.io/component=api"
		svcPort     = 8080
	)

	// 4. Create controller-owned managed claims while holding the serving
	// Deployment unschedulable, then copy the separately verified harness cache
	// into the general-class HF PVC. The host disk is seed evidence only: the
	// serving pod receives no hostPath and mounts only its managed PVCs.
	modelAuthority, err := loadModelCacheAuthority()
	require.NoError(t, err)
	seedManagedModelBackendCache(t, c, namespace, mbName, modelAuthority)
	catManifest := fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: ModelBackend
metadata:
  name: %s
  namespace: %s
spec:
  kind: vLLM
  model: %s
  extraArgs: ["--revision", "%s"]
  persistentVolumes:
    - {name: hf-cache, mountPath: /data/hf-cache, storageClassName: iterabase-lvm-xfs, size: 4Gi}
    - {name: generic-cache, mountPath: /cache, storageClassName: iterabase-lvm-xfs, size: 1Gi}
---
apiVersion: platform.iterabase.com/v1alpha1
kind: Model
metadata:
  name: %s
  namespace: %s
spec:
  modelID: %s
  displayName: Qwen3.5 0.8B
  backendRef: %s
  transforms:
    rewrite_model_name: true
`, mbName, namespace, modelAuthority.ModelID, modelAuthority.Revision, mbName, namespace, alias, mbName)
	catPath := filepath.Join(t.TempDir(), "catalog.yaml")
	require.NoError(t, os.WriteFile(catPath, []byte(catManifest), 0o600))
	c.Kubectl(t, "apply", "-f", catPath, "-n", namespace)
	assertModelBackendServingUsesManagedPVCs(t, c, namespace, mbName)

	// 5. capture the control-plane admin key (bootstrap init container) + apply
	//    an IdentityMapping CR (the identity — CRD path).
	apiPod := c.FirstPodName(t, namespace, apiSelector)
	logs := c.PodLogs(t, namespace, apiPod, "bootstrap")
	adminKey := mustFindKey(t, logs, "scope=admin")
	state.diagnostics.redactor.Add(adminKey)
	t.Logf("captured control-plane admin key (prefix=%s)", keyPrefix(adminKey))

	imManifest := fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: IdentityMapping
metadata:
  name: inference-user
  namespace: %s
spec:
  identity:
    kind: user
    displayName: Inference GPU E2E User
  bindings:
    - provider: teams
      type: user
      externalID: aad:inference-gpu-user
`, namespace)
	imPath := filepath.Join(t.TempDir(), "identitymapping.yaml")
	require.NoError(t, os.WriteFile(imPath, []byte(imManifest), 0o600))
	c.ApplyAndWait(t, imPath, namespace,
		"identitymapping.platform.iterabase.com/inference-user",
		"jsonpath={.status.ready}=true", 60*time.Second)
	identityID := strings.TrimSpace(c.Kubectl(t, "get", "-n", namespace,
		"identitymapping.platform.iterabase.com", "inference-user",
		"-o", "jsonpath={.status.identityID}"))
	require.NotEmpty(t, identityID, "IdentityMapping has no status.identityID")

	// 6. port-forward the control-plane API + issue a gateway-scoped API key.
	apiBase, _ := c.PortForward(t, namespace, apiService, svcPort, 18080)
	apiClient, err := httpx.Client(10 * time.Second)
	require.NoError(t, err)
	gatewayKey := createAPIKey(t, apiClient, apiBase+"/v1/api-keys", adminKey, identityID, "gateway")
	state.diagnostics.redactor.Add(gatewayKey)
	t.Logf("issued gateway-scoped API key (prefix=%s)", keyPrefix(gatewayKey))

	// 7. get the gateway's admin key + port-forward the gateway. Port-forward
	//    (not the fixture address / ingress) so the readiness poll + the completion
	//    request depend only on the gateway pod being up — not on ingress-nginx
	//    scheduling on the GPU node (which can lag or be tainted differently).
	gatewayAdminKey := getSecretKey(t, c, namespace, release+"-gateway-admin", "adminApiKey")
	state.diagnostics.redactor.Add(gatewayAdminKey)
	gwBase, _ := c.PortForward(t, namespace, "svc/"+release+"-gateway", svcPort, 18081)
	gwClient := &http.Client{Timeout: 300 * time.Second} // long timeout for inference

	// 8. wait for vLLM to be ready: the gateway snapshot marks the model
	//    available (the control-plane's ModelBackend reconciler sets healthy=true
	//    once the vLLM Deployment is Available — model downloaded + serving).
	entry, ok := waitForModelAvailable(t, c.Kubeconfig, namespace, mbName, gwClient, gwBase, gatewayAdminKey, alias, 25*time.Minute)
	if !ok {
		dumpVLLMDiagnostics(t, c.Kubeconfig, namespace, mbName)
		t.Fatalf("model %q never became available within 25m (vLLM pod not healthy; see diagnostics above)", alias)
	}
	t.Logf("model available: alias=%s backend=%s", entry.ModelID, entry.BackendURL)

	// One completion is dependent smoke proving usable infrastructure. Product
	// request, transform, and response correctness remain control-plane-owned.
	status, body := chatCompletionsStatus(t, gwClient, gwBase, gatewayKey, alias)
	if status != http.StatusOK {
		dumpVLLMDiagnostics(t, c.Kubeconfig, namespace, mbName)
		t.Fatalf("chat completions: status %d, want 200 (real completion)\n%s", status, body)
	}
	content := extractCompletion(body)
	if content == "" {
		t.Fatalf("completion response has no content:\n%s", body)
	}
	growManagedModelBackendClaims(t, c, namespace, mbName, modelAuthority, gwClient, gwBase, gatewayAdminKey, gatewayKey, alias)
	reapplyManagedModelBackendEvidence(t, state, c, namespace, mbName, modelAuthority)
	deleteManagedModelBackendClaimsEvidence(t, state, c, namespace, mbName)
	preview := content
	if len(preview) > 120 {
		preview = preview[:120] + "…"
	}
	t.Logf("real completion (%s): %q", alias, preview)
}

func seedManagedModelBackendCache(t *testing.T, cluster *remotecluster.Cluster, namespace, mbName string, authority modelCacheAuthority) {
	t.Helper()
	modelDir := strings.Split(authority.WeightPath, "/")[0]
	require.NotEmpty(t, modelDir)
	holdManifest := fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: ModelBackend
metadata: {name: %s, namespace: %s}
spec:
  kind: vLLM
  model: %s
  extraArgs: ["--revision", "%s"]
  nodeSelector: {iterabase.com/model-seed-hold: "true"}
  persistentVolumes:
    - {name: hf-cache, mountPath: /data/hf-cache, storageClassName: iterabase-lvm-xfs, size: 4Gi}
    - {name: generic-cache, mountPath: /cache, storageClassName: iterabase-lvm-xfs, size: 1Gi}
`, mbName, namespace, authority.ModelID, authority.Revision)
	holdPath := filepath.Join(t.TempDir(), "managed-modelbackend-hold.yaml")
	require.NoError(t, os.WriteFile(holdPath, []byte(holdManifest), 0o600))
	cluster.Kubectl(t, "apply", "-f", holdPath, "-n", namespace)
	for _, claim := range []string{mbName + "-hf-cache", mbName + "-generic-cache"} {
		cluster.Kubectl(t, "wait", "-n", namespace, "--for=create", "pvc/"+claim, "--timeout=3m")
	}

	seedManifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s-cache-seed, namespace: %s}
spec:
  restartPolicy: Never
  containers:
    - name: seed
      image: busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0
      command: [sh, -ceu]
      args:
        - |
          cp -a /seed/%s /managed/
          printf managed-generic-cache=seeded > /cache/seed-marker
          sync
          printf '%s  /managed/%s\n' | sha256sum -c -
      volumeMounts:
        - {name: seed, mountPath: /seed, readOnly: true}
        - {name: managed, mountPath: /managed}
        - {name: cache, mountPath: /cache}
  volumes:
    - name: seed
      hostPath: {path: /data/hf-cache, type: Directory}
    - name: managed
      persistentVolumeClaim: {claimName: %s-hf-cache}
    - name: cache
      persistentVolumeClaim: {claimName: %s-generic-cache}
`, mbName, namespace, modelDir, authority.SHA256, authority.WeightPath, mbName, mbName)
	seedPath := filepath.Join(t.TempDir(), "managed-modelbackend-seed.yaml")
	require.NoError(t, os.WriteFile(seedPath, []byte(seedManifest), 0o600))
	cluster.Kubectl(t, "apply", "-f", seedPath, "-n", namespace)
	cluster.Kubectl(t, "wait", "-n", namespace, "--for=jsonpath={.status.phase}=Succeeded", "pod/"+mbName+"-cache-seed", "--timeout=15m")
	cluster.Kubectl(t, "delete", "pod/"+mbName+"-cache-seed", "-n", namespace, "--wait=true", "--timeout=3m")
}

func assertModelBackendServingUsesManagedPVCs(t *testing.T, cluster *remotecluster.Cluster, namespace, mbName string) {
	t.Helper()
	cluster.Kubectl(t, "wait", "-n", namespace, "--for=create", "deployment/"+mbName, "--timeout=3m")
	var deployment appsv1.Deployment
	require.NoError(t, json.Unmarshal([]byte(cluster.Kubectl(t, "get", "deployment/"+mbName, "-n", namespace, "-o", "json")), &deployment))
	claims := map[string]string{}
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		assert.Nil(t, volume.HostPath, "serving deployment must never mount the harness seed disk")
		if volume.PersistentVolumeClaim != nil {
			claims[volume.Name] = volume.PersistentVolumeClaim.ClaimName
		}
	}
	require.Len(t, claims, 2)
	mounts := map[string]string{}
	for _, mount := range deployment.Spec.Template.Spec.Containers[0].VolumeMounts {
		mounts[mount.MountPath] = claims[mount.Name]
	}
	assert.Equal(t, mbName+"-hf-cache", mounts["/data/hf-cache"])
	assert.Equal(t, mbName+"-generic-cache", mounts["/cache"])
	for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "HF_HOME" {
			assert.Equal(t, "/data/hf-cache", env.Value)
			return
		}
	}
	t.Fatal("serving deployment omitted controller-managed HF_HOME")
}

func growManagedModelBackendClaims(
	t *testing.T,
	cluster *remotecluster.Cluster,
	namespace, mbName string,
	authority modelCacheAuthority,
	gatewayClient *http.Client,
	gatewayBase, gatewayAdminKey, gatewayKey, alias string,
) {
	t.Helper()
	pod, podUID, beforeHF, beforeCache, identities := captureManagedResizeBaseline(t, cluster, namespace, mbName)
	patchManagedModelBackendGrowth(t, cluster, namespace, mbName)
	waitForManagedResizeClosedState(t, cluster, namespace, mbName)
	assertServingPodIdentity(t, cluster, namespace, pod, podUID, "safe online ModelBackend resize replaced the serving pod")
	waitForManagedModelUnavailable(t, gatewayClient, gatewayBase, gatewayAdminKey, gatewayKey, alias)
	convergeGrownManagedClaims(t, cluster, namespace, mbName, identities)
	cluster.Kubectl(t, "wait", "modelbackend/"+mbName, "-n", namespace, "--for=jsonpath={.status.healthy}=true", "--timeout=15m")
	assertServingPodIdentity(t, cluster, namespace, pod, podUID, "converged online ModelBackend resize replaced the serving pod")
	assertManagedFilesystemsGrew(t, cluster, namespace, pod, beforeHF, beforeCache, authority)
	replacement := replaceManagedServingPod(t, cluster, namespace, mbName, pod, podUID)
	cluster.Kubectl(t, "wait", "pod/"+replacement, "-n", namespace, "--for=condition=Ready", "--timeout=15m")
	assertManagedCachePreserved(t, cluster, namespace, replacement, authority)
	if _, ok := waitForModelAvailable(t, cluster.Kubeconfig, namespace, mbName, gatewayClient, gatewayBase, gatewayAdminKey, alias, 3*time.Minute); !ok {
		t.Fatal("managed ModelBackend catalogue did not reopen after pod replacement")
	}
	status, body := chatCompletionsStatus(t, gatewayClient, gatewayBase, gatewayKey, alias)
	if status != http.StatusOK || extractCompletion(body) == "" {
		t.Fatalf("managed ModelBackend did not reopen after growth and pod replacement: status=%d body=%s", status, body)
	}
}

// captureManagedResizeBaseline records the serving pod, its mounted filesystem
// sizes, the growth marker, and both managed claim identities before the resize.
func captureManagedResizeBaseline(t *testing.T, cluster *remotecluster.Cluster, namespace, mbName string) (pod, podUID, beforeHF, beforeCache string, identities map[string]string) {
	t.Helper()
	pod = cluster.FirstPodName(t, namespace, "platform.iterabase.com/modelbackend="+mbName)
	podUID = strings.TrimSpace(cluster.Kubectl(t, "get", "pod/"+pod, "-n", namespace, "-o", "jsonpath={.metadata.uid}"))
	beforeHF = strings.TrimSpace(cluster.Kubectl(t, "exec", "-n", namespace, pod, "--", "df", "-B1", "--output=size", "/data/hf-cache"))
	beforeCache = strings.TrimSpace(cluster.Kubectl(t, "exec", "-n", namespace, pod, "--", "df", "-B1", "--output=size", "/cache"))
	cluster.Kubectl(t, "exec", "-n", namespace, pod, "--", "sh", "-ceu", "printf HOR-557-managed-cache > /cache/growth-marker; sync")
	identities = map[string]string{}
	for _, claim := range []string{mbName + "-hf-cache", mbName + "-generic-cache"} {
		identities[claim] = strings.TrimSpace(cluster.Kubectl(t, "get", "pvc/"+claim, "-n", namespace, "-o", `jsonpath={.metadata.uid}|{.spec.volumeName}`))
	}
	return pod, podUID, beforeHF, beforeCache, identities
}

// patchManagedModelBackendGrowth requests the in-place growth the stage proves.
func patchManagedModelBackendGrowth(t *testing.T, cluster *remotecluster.Cluster, namespace, mbName string) {
	t.Helper()
	patch := `{"spec":{"persistentVolumes":[{"name":"hf-cache","mountPath":"/data/hf-cache","storageClassName":"iterabase-lvm-xfs","size":"6Gi"},{"name":"generic-cache","mountPath":"/cache","storageClassName":"iterabase-lvm-xfs","size":"2Gi"}]}}`
	cluster.Kubectl(t, "patch", "modelbackend/"+mbName, "-n", namespace, "--type=merge", "-p", patch)
}

// waitForManagedResizeClosedState waits until the ModelBackend reports the
// non-routable managed resize state instead of serving silently.
func waitForManagedResizeClosedState(t *testing.T, cluster *remotecluster.Cluster, namespace, mbName string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := kubectlAllowFail(t, cluster.Kubeconfig, "get", "modelbackend/"+mbName, "-n", namespace, "-o", `jsonpath={.status.healthy}|{.status.message}`)
		if err == nil && !strings.HasPrefix(strings.TrimSpace(out), "true|") && strings.Contains(out, "managed PVC") {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("ModelBackend never exposed the non-routable managed resize state")
}

// assertServingPodIdentity fails when the resize replaced the serving pod.
func assertServingPodIdentity(t *testing.T, cluster *remotecluster.Cluster, namespace, pod, podUID, action string) {
	t.Helper()
	if current := strings.TrimSpace(cluster.Kubectl(t, "get", "pod/"+pod, "-n", namespace, "-o", "jsonpath={.metadata.uid}")); current != podUID {
		t.Fatalf("%s: before=%s after=%s", action, podUID, current)
	}
}

// waitForManagedModelUnavailable waits until the gateway catalogue reports the
// alias unavailable, and fails if the gateway still serves it, because a resize
// must not keep routing traffic to a non-converged backend.
func waitForManagedModelUnavailable(t *testing.T, gatewayClient *http.Client, gatewayBase, gatewayAdminKey, gatewayKey, alias string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		catalog, status, err := snapshotCatalog(gatewayClient, gatewayBase, gatewayAdminKey)
		if err == nil && status == http.StatusOK {
			for _, entry := range catalog {
				if entry.ModelID != alias || entry.Available {
					continue
				}
				if status, _ := chatCompletionsStatus(t, gatewayClient, gatewayBase, gatewayKey, alias); status == http.StatusOK {
					t.Fatal("gateway routed new traffic while managed storage resize was not converged")
				}
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("gateway catalogue did not close while managed storage was resizing")
}

// convergeGrownManagedClaims waits for both claims to reach their new capacity
// and proves their identity and LVM volume capacity survived the growth.
func convergeGrownManagedClaims(t *testing.T, cluster *remotecluster.Cluster, namespace, mbName string, identities map[string]string) {
	t.Helper()
	for claim, wanted := range map[string]string{mbName + "-hf-cache": "6Gi", mbName + "-generic-cache": "2Gi"} {
		cluster.Kubectl(t, "wait", "pvc/"+claim, "-n", namespace, "--for=jsonpath={.status.capacity.storage}="+wanted, "--timeout=15m")
		if after := strings.TrimSpace(cluster.Kubectl(t, "get", "pvc/"+claim, "-n", namespace, "-o", `jsonpath={.metadata.uid}|{.spec.volumeName}`)); after != identities[claim] {
			t.Fatalf("managed claim identity changed during growth: claim=%s before=%s after=%s", claim, identities[claim], after)
		}
		pv := strings.Split(identities[claim], "|")[1]
		handle := strings.TrimSpace(cluster.Kubectl(t, "get", "pv/"+pv, "-o", "jsonpath={.spec.csi.volumeHandle}"))
		capacity := strings.TrimSpace(cluster.Kubectl(t, "get", "lvmvolume.local.openebs.io/"+handle, "-n", namespace, "-o", "jsonpath={.spec.capacity}|{.status.state}"))
		assertLVMVolumeCapacity(t, capacity, wanted)
	}
}

// assertManagedFilesystemsGrew proves the mounted filesystems report growth and
// the cached model content survived it.
func assertManagedFilesystemsGrew(t *testing.T, cluster *remotecluster.Cluster, namespace, pod, beforeHF, beforeCache string, authority modelCacheAuthority) {
	t.Helper()
	afterHF := strings.TrimSpace(cluster.Kubectl(t, "exec", "-n", namespace, pod, "--", "df", "-B1", "--output=size", "/data/hf-cache"))
	afterCache := strings.TrimSpace(cluster.Kubectl(t, "exec", "-n", namespace, pod, "--", "df", "-B1", "--output=size", "/cache"))
	if beforeHF == afterHF || beforeCache == afterCache {
		t.Fatalf("mounted filesystems did not report growth: hf=%q->%q cache=%q->%q", beforeHF, afterHF, beforeCache, afterCache)
	}
	assertManagedCachePreserved(t, cluster, namespace, pod, authority)
}

// assertManagedCachePreserved verifies the cached weight hash and the growth
// marker are intact in a serving pod's mounted storage.
func assertManagedCachePreserved(t *testing.T, cluster *remotecluster.Cluster, namespace, pod string, authority modelCacheAuthority) {
	t.Helper()
	cluster.Kubectl(t, "exec", "-n", namespace, pod, "--", "sh", "-ceu", fmt.Sprintf("test \"$(sha256sum /data/hf-cache/%s | awk '{print $1}')\" = %s; test \"$(cat /cache/growth-marker)\" = HOR-557-managed-cache", authority.WeightPath, authority.SHA256))
}

// replaceManagedServingPod recreates the serving pod and returns the replacement
// name once it reports a new UID, proving the growth did not pin the workload.
func replaceManagedServingPod(t *testing.T, cluster *remotecluster.Cluster, namespace, mbName, pod, podUID string) string {
	t.Helper()
	cluster.Kubectl(t, "delete", "pod/"+pod, "-n", namespace, "--wait=true", "--timeout=5m")
	var replacement string
	for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline); {
		out, err := kubectlAllowFail(t, cluster.Kubeconfig, "get", "pods", "-n", namespace, "-l", "platform.iterabase.com/modelbackend="+mbName, "-o", `jsonpath={.items[0].metadata.name}|{.items[0].metadata.uid}`)
		parts := strings.Split(strings.TrimSpace(out), "|")
		if err == nil && len(parts) == 2 && parts[0] != "" && parts[1] != "" && parts[1] != podUID {
			replacement = parts[0]
			break
		}
		time.Sleep(2 * time.Second)
	}
	if replacement == "" {
		t.Fatal("serving pod replacement did not appear")
	}
	return replacement
}

func reapplyManagedModelBackendEvidence(t *testing.T, state *permanentGPUFixtureState, cluster *remotecluster.Cluster, namespace, mbName string, authority modelCacheAuthority) {
	t.Helper()
	claims := []string{mbName + "-hf-cache", mbName + "-generic-cache"}
	identities := map[string]string{}
	for _, claim := range claims {
		identities[claim] = strings.TrimSpace(cluster.Kubectl(t, "get", "pvc/"+claim, "-n", namespace, "-o", `jsonpath={.metadata.uid}|{.spec.volumeName}`))
	}
	pod := cluster.FirstPodName(t, namespace, "platform.iterabase.com/modelbackend="+mbName)
	podUID := strings.TrimSpace(cluster.Kubectl(t, "get", "pod/"+pod, "-n", namespace, "-o", "jsonpath={.metadata.uid}"))

	reapplyInferencePlatformStage(t, state)
	for _, claim := range claims {
		if after := strings.TrimSpace(cluster.Kubectl(t, "get", "pvc/"+claim, "-n", namespace, "-o", `jsonpath={.metadata.uid}|{.spec.volumeName}`)); after != identities[claim] {
			t.Fatalf("exact Forge/chart reapply replaced managed claim %s: before=%s after=%s", claim, identities[claim], after)
		}
	}
	if after := strings.TrimSpace(cluster.Kubectl(t, "get", "pod/"+pod, "-n", namespace, "-o", "jsonpath={.metadata.uid}")); after != podUID {
		t.Fatalf("exact Forge/chart reapply replaced healthy serving pod: before=%s after=%s", podUID, after)
	}
	cluster.Kubectl(t, "exec", "-n", namespace, pod, "--", "sh", "-ceu", fmt.Sprintf("test \"$(sha256sum /data/hf-cache/%s | awk '{print $1}')\" = %s; test \"$(cat /cache/growth-marker)\" = HOR-557-managed-cache", authority.WeightPath, authority.SHA256))
	assertModelBackendServingUsesManagedPVCs(t, cluster, namespace, mbName)
}

func reapplyInferencePlatformStage(t *testing.T, state *permanentGPUFixtureState) {
	t.Helper()
	prepareCandidateChart(t, state.host.IP, state.privKeyPath)
	plan := prepareCandidateOverlay(t, state.runID, state.host.IP, state.privKeyPath)
	candidateConfig := writeForgeConfigInferenceGPU(
		t, state.runID, state.host.IP, state.privKeyPath, state.chartVersion, plan,
	)
	out := applyOnceArgs(t, state.forgeBin, state.forgeHome, candidateConfig, "--skip-gpu")
	state.bindKubeconfigTunnel(t)
	markers := []string{"action:     skip", "node ready: true", "data storage: iterabase-data", "LVM storage ready: true", "certificate substrate applied: true", "LVM storage substrate applied: true",
		"chart applied: true", "overlay applied: true", "flux installed: true", "gitrepository: ready=True"}
	assertApplyMarkers(t, out, markers...)
	candidateCluster := remotecluster.Use(t, filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml"))
	assertCandidateImageDigests(t, candidateCluster, "iterabase-system", state.runtimeImageDigests,
		controlPlaneDigestEnv, inferenceGatewayDigestEnv, toolRunnerDigestEnv)
}

func deleteManagedModelBackendClaimsEvidence(t *testing.T, state *permanentGPUFixtureState, cluster *remotecluster.Cluster, namespace, mbName string) {
	t.Helper()
	type identity struct{ pv, handle string }
	identities := map[string]identity{}
	for _, claim := range []string{mbName + "-hf-cache", mbName + "-generic-cache"} {
		pv := strings.TrimSpace(cluster.Kubectl(t, "get", "pvc/"+claim, "-n", namespace, "-o", "jsonpath={.spec.volumeName}"))
		handle := strings.TrimSpace(cluster.Kubectl(t, "get", "pv/"+pv, "-o", "jsonpath={.spec.csi.volumeHandle}"))
		if pv == "" || handle == "" {
			t.Fatalf("managed claim %s has incomplete deletion identity: pv=%q handle=%q", claim, pv, handle)
		}
		identities[claim] = identity{pv: pv, handle: handle}
	}
	cluster.Kubectl(t, "delete", "model/"+mbName, "modelbackend/"+mbName, "-n", namespace, "--wait=true", "--timeout=5m")
	client, err := sshDial(state.host.IP, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh for managed ModelBackend deletion evidence: %v", err)
	}
	defer client.Close()
	for claim, identity := range identities {
		command := fmt.Sprintf(`for i in $(seq 1 300); do
  ! sudo k3s kubectl get pvc %s -n %s >/dev/null 2>&1 &&
  ! sudo k3s kubectl get pv %s >/dev/null 2>&1 &&
  ! sudo k3s kubectl get lvmvolume.local.openebs.io %s -n %s >/dev/null 2>&1 &&
  ! sudo lvs --noheadings -o lv_name iterabase-data | awk '{$1=$1;if(NF)print}' | grep -Fxq %s && exit 0
  sleep 2
done
exit 1`, candidateShellQuote(claim), candidateShellQuote(namespace), candidateShellQuote(identity.pv), candidateShellQuote(identity.handle), candidateShellQuote(namespace), candidateShellQuote(identity.handle))
		if output, err := sshOutput(client, command); err != nil {
			t.Fatalf("ModelBackend Delete lifecycle leaked claim identity %s/%s: %v\n%s", claim, identity.handle, err, output)
		}
	}
}

// writeForgeConfigInferenceGPU writes the current production-ordered GPU
// fixture: exact public Flux source, certificate substrate, then platform.
func writeForgeConfigInferenceGPU(
	t *testing.T, name, ip, keyPath, chartVersion string, plan candidateOverlayPlan,
) string {
	return writeForgeConfigSpec(t, forgeConfigSpec{
		Name: name, Address: ip, SSHKeyPath: keyPath, GPU: true,
		ChartVersion: chartVersion, ChartRepository: os.Getenv("FORGE_E2E_CHART_REPOSITORY"),
		ChartRelease: "itb", ChartNamespace: "iterabase-system",
		OverlayRepo: plan.repository, OverlayRef: plan.ref,
		Flux: plan.flux,
	})
}

// waitForModelAvailable polls the gateway's /admin/v1/snapshot until the given
// alias is present AND available=true (vLLM ready). The generous timeout covers
// the vLLM image pull + model load + startup on the permanent GPU fixture. Logs the
// last status + body periodically, and dumps vLLM pod diagnostics once at the
// 5m mark (so a crash/image-pull issue is visible without waiting the full
// timeout). Returns (entry, false) on timeout so the caller can dump final
// diagnostics before failing.
func waitForModelAvailable(t *testing.T, kubeconfig, namespace, mbName string, client *http.Client, baseURL, adminKey, alias string, timeout time.Duration) (catalogEntry, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastStatus int
	nextLog := time.Now().Add(2 * time.Minute)
	earlyDiag := time.Now().Add(5 * time.Minute)
	dumpedEarly := false
	for time.Now().Before(deadline) {
		catalog, status, err := snapshotCatalog(client, baseURL, adminKey)
		if err == nil {
			lastStatus = status
			if status == http.StatusOK {
				for _, e := range catalog {
					if e.ModelID == alias && e.Available {
						return e, true
					}
				}
			}
		}
		if time.Now().After(nextLog) {
			t.Logf("waiting for %s: last status=%d", alias, lastStatus)
			nextLog = time.Now().Add(2 * time.Minute)
		}
		// Early diagnostics at 5m: if the model is still unavailable, dump the
		// vLLM pod state + logs so the cause is visible without waiting 25m.
		if !dumpedEarly && time.Now().After(earlyDiag) {
			dumpedEarly = true
			t.Logf("model %s still unavailable after 5m; dumping vLLM diagnostics", alias)
			dumpVLLMDiagnostics(t, kubeconfig, namespace, mbName)
		}
		time.Sleep(15 * time.Second)
	}
	return catalogEntry{}, false
}

// kubectlAllowFail runs kubectl with the given kubeconfig + returns (output,
// error) without fataling — for best-effort diagnostics where a failing command
// shouldn't abort the rest (mirrors sshRun in this package).
func kubectlAllowFail(t *testing.T, kubeconfig string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	full := append([]string{"--kubeconfig", kubeconfig}, args...)
	out, err := exec.CommandContext(ctx, "kubectl", full...).CombinedOutput()
	return string(out), err
}

// dumpVLLMDiagnostics queries the vLLM backend state when the model never
// becomes available (or a completion fails), so the cause (unschedulable
// nodeSelector, image pull, crash, slow startup) is visible in the test log
// rather than just "not ready". Best-effort — mirrors dumpGPUDiagnostics: each
// command is logged with its error + a failure doesn't abort the rest.
func dumpVLLMDiagnostics(t *testing.T, kubeconfig, namespace, mbName string) {
	t.Helper()
	t.Log("=== vLLM backend diagnostics ===")
	label := "platform.iterabase.com/modelbackend=" + mbName
	type kCmd struct {
		desc string
		args []string
	}
	cmds := []kCmd{
		{"kubectl get modelbackend " + mbName + " -o yaml", []string{"get", "modelbackend", mbName, "-n", namespace, "-o", "yaml"}},
		{"kubectl get deployment " + mbName + " -o wide", []string{"get", "deployment", mbName, "-n", namespace, "-o", "wide"}},
		{"kubectl get pods -l " + label + " -o wide", []string{"get", "pods", "-n", namespace, "-l", label, "-o", "wide"}},
		{"kubectl describe pod -l " + label, []string{"describe", "pod", "-n", namespace, "-l", label}},
		{"kubectl get nodes --show-labels (nvidia labels?)", []string{"get", "nodes", "--show-labels"}},
		{"kubectl logs -l " + label + " --tail=100 --all-containers=true (current)", []string{"logs", "-n", namespace, "-l", label, "--tail=100", "--all-containers=true"}},
		{"kubectl logs -l " + label + " --previous --tail=100 (crash reason)", []string{"logs", "-n", namespace, "-l", label, "--previous", "--tail=100", "--all-containers=true"}},
		{"kubectl get events -n " + namespace + " --sort-by=.lastTimestamp", []string{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp"}},
	}
	for _, cm := range cmds {
		out, err := kubectlAllowFail(t, kubeconfig, cm.args...)
		t.Logf("$ %s\n%s(err=%v)", cm.desc, out, err)
	}
}

// extractCompletion pulls the text content from an OpenAI chat-completion
// response (choices[0].message.content).
func extractCompletion(body string) string {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return ""
	}
	if len(resp.Choices) == 0 {
		return ""
	}
	return resp.Choices[0].Message.Content
}
