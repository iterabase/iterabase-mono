package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	gpuUpgradeBaselineDriver        = "580.126.20"
	gpuUpgradeBaselineDriverSHA256  = "16b9830998d2f2795fe72e760169b51bd4185e3b412ea3d0c4e46f453af2cc30"
	gpuUpgradeCandidateDriver       = "595.71.05"
	gpuUpgradeCandidateDriverSHA256 = "d8c38c473375d7262e36ad100dc0732859592a5ff65b238e41057e1dc0763098"
	gpuUpgradeNamespace             = "forge-gpu-upgrade"
	gpuUpgradeWorkloadName          = "gpu-driver-upgrade-workload"
	gpuUpgradeReadyPrefix           = "gpu-upgrade-ready "
)

func e2eGPUDriverSHA256(t *testing.T, version string) string {
	t.Helper()
	switch version {
	case gpuUpgradeBaselineDriver:
		return gpuUpgradeBaselineDriverSHA256
	case gpuUpgradeCandidateDriver:
		return gpuUpgradeCandidateDriverSHA256
	default:
		t.Fatalf("GPU driver %q has no reviewed Ubuntu 24.04 image digest", version)
		return ""
	}
}

type gpuUpgradeEvidence struct {
	PodUID          types.UID
	PersistentOwner string
	EmptyDirOwner   string
	DriverVersion   string
}

// gpuDriverUpgradeStage is selected by the affected selector when GPU driver
// inputs change, and always in full validation (C2 rule 5, C5). Its name must
// match DRIVER_UPGRADE_STAGE in .github/scripts/affected.py.
const gpuDriverUpgradeStage = "driver-upgrade"

// gpuDriverUpgradeStageRun proves the emptyDir-safe transition from the
// installed baseline driver to the candidate driver as one optional stage.
func gpuDriverUpgradeStageRun(t *testing.T, state *gpuFixtureState) {
	t.Helper()
	recordGPUUpgradeInputsStage(t, state)
	startGPUUpgradeWorkloadStage(t, state)
	applyGPUDriverUpgradeStage(t, state)
	assertGPUDriverUpgradeStage(t, state)
}

func recordGPUUpgradeInputsStage(t *testing.T, state *gpuFixtureState) {
	t.Helper()
	identity, err := runForgeE(state.forgeBin, state.forgeHome, "version")
	if err != nil {
		t.Fatalf("record Forge candidate identity: %v\n%s", err, identity)
	}
	t.Logf(
		"gpu driver transition fixture: forge=%q binary=%q fixture_mode=%q source_sha=%q baseline_driver=%q candidate_driver=%q",
		strings.TrimSpace(identity), state.forgeBin, os.Getenv("ITERABASE_E2E_FIXTURE_MODE"),
		os.Getenv("ITERABASE_E2E_SOURCE_SHA"), gpuUpgradeBaselineDriver, gpuUpgradeCandidateDriver,
	)
}

func startGPUUpgradeWorkloadStage(t *testing.T, state *gpuFixtureState) {
	t.Helper()
	clients := newGPUUpgradeClients(t, state)
	ctx := context.Background()
	if _, err := clients.typed.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: gpuUpgradeNamespace},
	}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create GPU upgrade namespace: %v", err)
	}
	if _, err := clients.typed.CoreV1().PersistentVolumeClaims(gpuUpgradeNamespace).Create(ctx, gpuUpgradePVC(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create GPU upgrade persistent cache: %v", err)
	}
	if _, err := clients.typed.AppsV1().Deployments(gpuUpgradeNamespace).Create(ctx, gpuUpgradeDeployment(state.runID), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create GPU upgrade workload: %v", err)
	}
	evidence := waitForGPUUpgradeWorkload(t, clients.typed, "", gpuUpgradeBaselineDriver, 10*time.Minute)
	if evidence.PersistentOwner != string(evidence.PodUID) || evidence.EmptyDirOwner != string(evidence.PodUID) {
		t.Fatalf("initial workload did not initialize both stores from pod %s: %+v", evidence.PodUID, evidence)
	}
	state.upgradeEvidence = &evidence
	t.Logf("baseline GPU workload ready: %+v", evidence)
}

func applyGPUDriverUpgradeStage(t *testing.T, state *gpuFixtureState) {
	t.Helper()
	cfgPath := writeForgeConfigGPUDriver(t, state.runID, state.host.IP, state.privKeyPath, gpuUpgradeCandidateDriver)
	t.Logf("reconciling exact GPU driver transition %s -> %s", gpuUpgradeBaselineDriver, gpuUpgradeCandidateDriver)
	out := applyOnce(t, state.forgeBin, state.forgeHome, cfgPath)
	assertApplyMarkers(t, out, "node ready: true", "gpu ready: true", "gpu driver: "+gpuUpgradeCandidateDriver)
	state.bindKubeconfigTunnel(t)
	t.Logf("driver upgrade apply output:\n%s", out)
}

func assertGPUDriverUpgradeStage(t *testing.T, state *gpuFixtureState) {
	t.Helper()
	if state.upgradeEvidence == nil {
		t.Fatal("baseline GPU workload evidence is missing")
	}
	before := *state.upgradeEvidence
	waitForGPUUpgradeNode(t, state, before.PodUID, 10*time.Minute)
	clients := newGPUUpgradeClients(t, state)
	assertGPUUpgradeClusterPolicy(t, clients.dynamic)

	after := waitForGPUUpgradeWorkload(t, clients.typed, before.PodUID, gpuUpgradeCandidateDriver, 10*time.Minute)
	if after.PodUID == before.PodUID {
		t.Fatalf("GPU workload pod was not recreated: uid=%s", after.PodUID)
	}
	if after.PersistentOwner != before.PersistentOwner {
		t.Fatalf("persistent cache owner changed across driver transition: before=%q after=%q", before.PersistentOwner, after.PersistentOwner)
	}
	if after.EmptyDirOwner != string(after.PodUID) || after.EmptyDirOwner == before.EmptyDirOwner {
		t.Fatalf("disposable emptyDir was not recreated for pod %s: before=%q after=%q", after.PodUID, before.EmptyDirOwner, after.EmptyDirOwner)
	}
	t.Logf("GPU driver transition converged with recreated workload: before=%+v after=%+v", before, after)
	releaseGPUUpgradeWorkload(t, clients.typed, 2*time.Minute)
}

func releaseGPUUpgradeWorkload(t *testing.T, client kubernetes.Interface, timeout time.Duration) {
	t.Helper()
	propagation := metav1.DeletePropagationForeground
	if err := client.AppsV1().Deployments(gpuUpgradeNamespace).Delete(context.Background(), gpuUpgradeWorkloadName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	}); err != nil {
		t.Fatalf("release GPU upgrade workload: %v", err)
	}
	deadline := time.Now().Add(timeout)
	selector := "app.kubernetes.io/name=" + gpuUpgradeWorkloadName
	for time.Now().Before(deadline) {
		pods, err := client.CoreV1().Pods(gpuUpgradeNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			t.Fatalf("observe GPU upgrade workload release: %v", err)
		}
		if len(pods.Items) == 0 {
			t.Log("released the fixture's GPU before the existing real-inference smoke")
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("GPU upgrade workload did not release the GPU within %s", timeout)
}

type gpuUpgradeClients struct {
	typed   kubernetes.Interface
	dynamic dynamic.Interface
}

func newGPUUpgradeClients(t *testing.T, state *gpuFixtureState) gpuUpgradeClients {
	t.Helper()
	kubeconfig := filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml")
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("build GPU upgrade kubeconfig: %v", err)
	}
	restConfig.Timeout = 30 * time.Second
	typed, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("create GPU upgrade Kubernetes client: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("create GPU upgrade dynamic client: %v", err)
	}
	return gpuUpgradeClients{typed: typed, dynamic: dynamicClient}
}

func gpuUpgradePVC() *corev1.PersistentVolumeClaim {
	storageClass := "iterabase-lvm-xfs"
	volumeMode := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: gpuUpgradeWorkloadName + "-cache", Namespace: gpuUpgradeNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClass,
			VolumeMode:       &volumeMode,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			}},
		},
	}
}

func gpuUpgradeDeployment(runID string) *appsv1.Deployment {
	labels := map[string]string{
		"app.kubernetes.io/name":       gpuUpgradeWorkloadName,
		"e2e.horizonshift.io/run":      runID,
		"e2e.horizonshift.io/contract": "gpu-driver-upgrade",
	}
	replicas := int32(1)
	gracePeriod := int64(0)
	sharedMemoryLimit := resource.MustParse("1Gi")
	nvidiaRuntime := "nvidia"
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: gpuUpgradeWorkloadName, Namespace: gpuUpgradeNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RuntimeClassName:              &nvidiaRuntime,
					TerminationGracePeriodSeconds: &gracePeriod,
					Containers: []corev1.Container{{
						Name:  "inference",
						Image: "nvidia/cuda:12.4.1-base-ubuntu22.04@sha256:0f6bfcbf267e65123bcc2287e2153dedfc0f24772fb5ce84afe16ac4b2fada95",
						Command: []string{"sh", "-ceu", `
if [ ! -s /cache/owner ]; then
  printf '%s' "$POD_UID" > /cache/owner
fi
if [ -e /dev/shm/owner ]; then
  echo "emptyDir unexpectedly retained a prior pod owner" >&2
  exit 1
fi
printf '%s' "$POD_UID" > /dev/shm/owner
driver="$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -n1 | tr -d '[:space:]')"
printf 'gpu-upgrade-ready pod_uid=%s persistent_owner=%s emptydir_owner=%s driver=%s\n' \
  "$POD_UID" "$(cat /cache/owner)" "$(cat /dev/shm/owner)" "$driver"
touch /tmp/ready
exec tail -f /dev/null
`},
						Env: []corev1.EnvVar{{
							Name: "POD_UID",
							ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
								APIVersion: "v1", FieldPath: "metadata.uid",
							}},
						}},
						Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
							corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
						}},
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
							Command: []string{"test", "-f", "/tmp/ready"},
						}}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "shared-memory", MountPath: "/dev/shm"},
							{Name: "persistent-cache", MountPath: "/cache"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "shared-memory", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium: corev1.StorageMediumMemory, SizeLimit: &sharedMemoryLimit,
						}}},
						{Name: "persistent-cache", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: gpuUpgradeWorkloadName + "-cache",
						}}},
					},
				},
			},
		},
	}
}

func waitForGPUUpgradeWorkload(t *testing.T, client kubernetes.Interface, previousUID types.UID, wantDriver string, timeout time.Duration) gpuUpgradeEvidence {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	selector := "app.kubernetes.io/name=" + gpuUpgradeWorkloadName
	for time.Now().Before(deadline) {
		pods, err := client.CoreV1().Pods(gpuUpgradeNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			t.Fatalf("observe GPU upgrade workload: %v", err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.UID == previousUID || !podReady(pod) {
				continue
			}
			logs, err := client.CoreV1().Pods(gpuUpgradeNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "inference"}).DoRaw(ctx)
			if err != nil {
				t.Fatalf("read ready GPU upgrade workload logs: %v", err)
			}
			evidence, err := parseGPUUpgradeEvidence(string(logs))
			if err != nil {
				t.Fatalf("parse ready GPU upgrade workload logs: %v\n%s", err, logs)
			}
			if evidence.PodUID != pod.UID {
				t.Fatalf("workload reported pod uid %s, Kubernetes reports %s", evidence.PodUID, pod.UID)
			}
			if evidence.DriverVersion != wantDriver {
				t.Fatalf("workload driver = %q, want %q", evidence.DriverVersion, wantDriver)
			}
			return evidence
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("GPU upgrade workload did not become ready with driver %s within %s", wantDriver, timeout)
	return gpuUpgradeEvidence{}
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func parseGPUUpgradeEvidence(logs string) (gpuUpgradeEvidence, error) {
	for _, line := range strings.Split(logs, "\n") {
		if !strings.HasPrefix(line, gpuUpgradeReadyPrefix) {
			continue
		}
		values := make(map[string]string)
		for _, field := range strings.Fields(strings.TrimPrefix(line, gpuUpgradeReadyPrefix)) {
			key, value, ok := strings.Cut(field, "=")
			if ok {
				values[key] = value
			}
		}
		for _, key := range []string{"pod_uid", "persistent_owner", "emptydir_owner", "driver"} {
			if values[key] == "" {
				return gpuUpgradeEvidence{}, fmt.Errorf("GPU upgrade evidence lacks %s", key)
			}
		}
		return gpuUpgradeEvidence{
			PodUID:          types.UID(values["pod_uid"]),
			PersistentOwner: values["persistent_owner"],
			EmptyDirOwner:   values["emptydir_owner"],
			DriverVersion:   values["driver"],
		}, nil
	}
	return gpuUpgradeEvidence{}, fmt.Errorf("GPU upgrade readiness record not found")
}

func waitForGPUUpgradeNode(t *testing.T, state *gpuFixtureState, previousUID types.UID, timeout time.Duration) {
	t.Helper()
	// A containerized driver replacement briefly restarts k3s on this single
	// node. Observe that expected unavailable state through SSH, then parse one
	// coherent node/workload/operator snapshot only after the API is serving again. The
	// replacement pod UID prevents the baseline upgrade-done label from being
	// mistaken for completion before the upgrade controller starts its cycle.
	const observe = `
if ! sudo systemctl is-active --quiet k3s; then
  echo __K3S_NOT_READY__
  exit 0
fi
nodes="$(sudo k3s kubectl --request-timeout=15s get nodes -o json 2>/dev/null)" || {
  echo __K3S_NOT_READY__
  exit 0
}
pods="$(sudo k3s kubectl --request-timeout=15s get pods -n forge-gpu-upgrade -l app.kubernetes.io/name=gpu-driver-upgrade-workload -o json 2>/dev/null)" || {
  echo __K3S_NOT_READY__
  exit 0
}
policies="$(sudo k3s kubectl --request-timeout=15s get clusterpolicy -o json 2>/dev/null)" || {
  echo __K3S_NOT_READY__
  exit 0
}
echo __K3S_READY__
printf '%s' "$nodes" | base64 -w0
printf '\n'
printf '%s' "$pods" | base64 -w0
printf '\n'
printf '%s' "$policies" | base64 -w0
printf '\n'
`
	deadline := time.Now().Add(timeout)
	lastObservation := "no coherent API observation"
	for time.Now().Before(deadline) {
		out, err := sshRun(t, state.host.IP, state.privKeyPath, observe)
		if err != nil {
			t.Fatalf("observe k3s during GPU driver upgrade: %v\n%s", err, out)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) == 1 && lines[0] == "__K3S_NOT_READY__" {
			time.Sleep(5 * time.Second)
			continue
		}
		if len(lines) != 4 || lines[0] != "__K3S_READY__" {
			t.Fatalf("unexpected GPU upgrade observation response: %q", out)
		}
		var nodes corev1.NodeList
		decodeGPUUpgradeSnapshot(t, lines[1], &nodes)
		var pods corev1.PodList
		decodeGPUUpgradeSnapshot(t, lines[2], &pods)
		var policies unstructured.UnstructuredList
		decodeGPUUpgradeSnapshot(t, lines[3], &policies)
		if len(nodes.Items) != 1 {
			t.Fatalf("GPU upgrade fixture has %d nodes, want 1", len(nodes.Items))
		}
		node := &nodes.Items[0]
		upgradeState := node.Labels["nvidia.com/gpu-driver-upgrade-state"]
		if upgradeState == "upgrade-failed" {
			t.Fatalf("GPU driver upgrade entered upgrade-failed on node %s", node.Name)
		}
		recreatedReady := false
		for i := range pods.Items {
			if pods.Items[i].UID != previousUID && podReady(&pods.Items[i]) {
				recreatedReady = true
				break
			}
		}
		policyReady := false
		policyEvidence := gpuUpgradePolicyReadiness{PolicyCount: len(policies.Items), ExpectedDriverVersion: gpuUpgradeCandidateDriver}
		if len(policies.Items) == 1 {
			var readinessErr error
			policyEvidence, readinessErr = readGPUUpgradePolicyReadiness(&policies.Items[0], gpuUpgradeCandidateDriver)
			if readinessErr != nil {
				t.Fatalf("read ClusterPolicy readiness during GPU upgrade: %v", readinessErr)
			}
			policyReady = policyEvidence.Ready()
		}
		nodeDriver := node.Labels["nvidia.com/cuda.driver-version.full"]
		lastObservation = fmt.Sprintf(
			"policy={%s} node=%q ready=%t schedulable=%t upgradeState=%q driver=%q recreatedWorkloadReady=%t",
			policyEvidence, node.Name, nodeReady(node), !node.Spec.Unschedulable,
			upgradeState, nodeDriver, recreatedReady,
		)
		nodeConverged := upgradeState == "upgrade-done" &&
			nodeDriver == gpuUpgradeCandidateDriver &&
			!node.Spec.Unschedulable && nodeReady(node)
		if nodeConverged && recreatedReady && policyReady {
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("GPU node, operator, and recreated workload did not converge on driver %s within %s; last observation: %s", gpuUpgradeCandidateDriver, timeout, lastObservation)
}

func decodeGPUUpgradeSnapshot(t *testing.T, encoded string, target any) {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode GPU upgrade observation: %v", err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("parse GPU upgrade observation: %v", err)
	}
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

type gpuUpgradePolicyReadiness struct {
	PolicyCount           int
	State                 string
	ReadyCondition        string
	ErrorCondition        string
	DriverVersion         string
	ExpectedDriverVersion string
}

func (r gpuUpgradePolicyReadiness) Ready() bool {
	stateAccepted := r.State == "ready" || r.State == "notReady"
	return r.PolicyCount == 1 && stateAccepted && r.ReadyCondition == "True" &&
		r.ErrorCondition == "False" && r.DriverVersion == r.ExpectedDriverVersion
}

func (r gpuUpgradePolicyReadiness) String() string {
	return fmt.Sprintf("count=%d state=%q Ready=%q Error=%q driver=%q expectedDriver=%q", r.PolicyCount, r.State, r.ReadyCondition, r.ErrorCondition, r.DriverVersion, r.ExpectedDriverVersion)
}

func readGPUUpgradePolicyReadiness(policy *unstructured.Unstructured, expectedDriver string) (gpuUpgradePolicyReadiness, error) {
	readiness := gpuUpgradePolicyReadiness{PolicyCount: 1, ExpectedDriverVersion: expectedDriver}
	var found bool
	var err error
	readiness.State, _, err = unstructured.NestedString(policy.Object, "status", "state")
	if err != nil {
		return readiness, fmt.Errorf("read status.state: %w", err)
	}
	readiness.DriverVersion, _, err = unstructured.NestedString(policy.Object, "spec", "driver", "version")
	if err != nil {
		return readiness, fmt.Errorf("read spec.driver.version: %w", err)
	}
	readiness.DriverVersion, _, _ = strings.Cut(readiness.DriverVersion, "@sha256:")
	conditions, found, err := unstructured.NestedSlice(policy.Object, "status", "conditions")
	if err != nil {
		return readiness, fmt.Errorf("read status.conditions: %w", err)
	}
	if !found {
		return readiness, nil
	}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			return readiness, fmt.Errorf("status.conditions contains %T, want object", raw)
		}
		conditionType, _ := condition["type"].(string)
		conditionStatus, _ := condition["status"].(string)
		switch conditionType {
		case "Ready":
			readiness.ReadyCondition = conditionStatus
		case "Error":
			readiness.ErrorCondition = conditionStatus
		}
	}
	return readiness, nil
}

func assertGPUUpgradeClusterPolicy(t *testing.T, client dynamic.Interface) {
	t.Helper()
	policies, err := client.Resource(schema.GroupVersionResource{
		Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies",
	}).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("read ClusterPolicy after GPU driver upgrade: %v", err)
	}
	if len(policies.Items) != 1 {
		t.Fatalf("GPU upgrade fixture has %d ClusterPolicies, want 1", len(policies.Items))
	}
	policy := &policies.Items[0]
	readiness, err := readGPUUpgradePolicyReadiness(policy, gpuUpgradeCandidateDriver)
	if err != nil {
		t.Fatalf("read ClusterPolicy after GPU driver upgrade: %v", err)
	}
	if !readiness.Ready() {
		t.Fatalf("ClusterPolicy did not expose accepted candidate readiness: %s", readiness)
	}
	if readiness.State == "notReady" {
		t.Logf("accepted documented GPU Operator legacy-state conflict with coherent current evidence: %s", readiness)
	}
	assertNestedBool(t, policy, true, "spec", "driver", "upgradePolicy", "podDeletion", "deleteEmptyDir")
	assertNestedBool(t, policy, false, "spec", "driver", "upgradePolicy", "drain", "enable")
}

func assertNestedBool(t *testing.T, object *unstructured.Unstructured, want bool, fields ...string) {
	t.Helper()
	got, found, err := unstructured.NestedBool(object.Object, fields...)
	if err != nil || !found || got != want {
		t.Fatalf("ClusterPolicy %s = %t (found=%v err=%v), want %t", strings.Join(fields, "."), got, found, err, want)
	}
}

func TestGPUUpgradePolicyReadinessAuthority(t *testing.T) {
	tests := []struct {
		name           string
		state          string
		readyCondition string
		errorCondition string
		driver         string
		wantReady      bool
	}{
		{name: "normal ready state", state: "ready", readyCondition: "True", errorCondition: "False", driver: gpuUpgradeCandidateDriver, wantReady: true},
		{name: "documented legacy state conflict", state: "notReady", readyCondition: "True", errorCondition: "False", driver: gpuUpgradeCandidateDriver, wantReady: true},
		{name: "operator reports error", state: "notReady", readyCondition: "False", errorCondition: "True", driver: gpuUpgradeCandidateDriver},
		{name: "candidate conditions with stale driver", state: "ready", readyCondition: "True", errorCondition: "False", driver: gpuUpgradeBaselineDriver},
		{name: "unsupported legacy state", state: "ignored", readyCondition: "True", errorCondition: "False", driver: gpuUpgradeCandidateDriver},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := &unstructured.Unstructured{Object: map[string]any{
				"spec": map[string]any{"driver": map[string]any{"version": tt.driver}},
				"status": map[string]any{
					"state": tt.state,
					"conditions": []any{
						map[string]any{"type": "Ready", "status": tt.readyCondition},
						map[string]any{"type": "Error", "status": tt.errorCondition},
					},
				},
			}}
			readiness, err := readGPUUpgradePolicyReadiness(policy, gpuUpgradeCandidateDriver)
			require.NoError(t, err)
			assert.Equal(t, tt.wantReady, readiness.Ready(), readiness.String())
		})
	}
}

func TestGPUUpgradeDeploymentSeparatesDisposableAndPersistentState(t *testing.T) {
	pvc := gpuUpgradePVC()
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "iterabase-lvm-xfs" ||
		pvc.Spec.VolumeMode == nil || *pvc.Spec.VolumeMode != corev1.PersistentVolumeFilesystem ||
		len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("GPU transition cache does not use the exact general LVM claim contract: %+v", pvc.Spec)
	}
	deployment := gpuUpgradeDeployment("test-run")
	pod := deployment.Spec.Template.Spec
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != "nvidia" {
		t.Fatalf("runtimeClassName = %v, want nvidia", pod.RuntimeClassName)
	}
	if len(pod.Volumes) != 2 || pod.Volumes[0].EmptyDir == nil || pod.Volumes[0].EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Fatalf("shared memory volume is not a memory-backed emptyDir: %+v", pod.Volumes)
	}
	if pod.Volumes[1].PersistentVolumeClaim == nil {
		t.Fatalf("persistent cache is not PVC-backed: %+v", pod.Volumes)
	}
	container := pod.Containers[0]
	gpuLimit := container.Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]
	if gpuLimit.Cmp(resource.MustParse("1")) != 0 {
		t.Fatalf("workload does not request exactly one GPU: %+v", container.Resources.Limits)
	}
	command := strings.Join(container.Command, " ")
	for _, contract := range []string{"/dev/shm/owner", "/cache/owner", "nvidia-smi"} {
		if !strings.Contains(command, contract) {
			t.Fatalf("workload command does not assert %s: %s", contract, command)
		}
	}
}

func TestParseGPUUpgradeEvidence(t *testing.T) {
	logs := "startup\n" + gpuUpgradeReadyPrefix + "pod_uid=new persistent_owner=old emptydir_owner=new driver=" + gpuUpgradeCandidateDriver + "\n"
	got, err := parseGPUUpgradeEvidence(logs)
	if err != nil {
		t.Fatal(err)
	}
	if got.PodUID != "new" || got.PersistentOwner != "old" || got.EmptyDirOwner != "new" || got.DriverVersion != gpuUpgradeCandidateDriver {
		t.Fatalf("unexpected evidence: %+v", got)
	}
}
