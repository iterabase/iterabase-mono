// Package e2e contains the composed CPU/GPU scenarios. The no-GPU preflight
// runs on a fresh CPU host; GPU readiness and real inference run on a fresh
// GPU host. Each run has its own EC2 instance, so there is no fixture lock.
package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// GPUFixtureHost identifies the fixed GPU fixture surface consumed by the scenario.
type GPUFixtureHost struct {
	IP                string
	PrivKeyPath       string
	DataStorageDevice string
}

type gpuFixtureState struct {
	fixture             *hostFixture
	runID               string
	privKeyPath         string
	host                *GPUFixtureHost
	forgeBin            string
	forgeHome           string
	chartVersion        string
	upgradeEvidence     *gpuUpgradeEvidence
	runtimeImageDigests map[string]importedRuntimeIdentity
	apiTunnel           *sshAPITunnel
	apiServerName       string
	diagnostics         forgeDiagnostics
}

func newGPUFixtureState(t *testing.T) *gpuFixtureState {
	fixture := requireHostFixture(t, "gpu")
	state := &gpuFixtureState{
		fixture:             fixture,
		runID:               fixture.installName(),
		privKeyPath:         fixture.sshKeyPath,
		host:                &GPUFixtureHost{IP: fixture.address, PrivKeyPath: fixture.sshKeyPath, DataStorageDevice: fixture.dataStorageDevice},
		forgeHome:           t.TempDir(),
		chartVersion:        platformChartVersion(t, ""),
		runtimeImageDigests: make(map[string]importedRuntimeIdentity),
		diagnostics:         newForgeDiagnostics(t, gpuScenarioName),
	}
	state.forgeBin = buildForge(t)
	return state
}

func prepareGPUFixtureStage(t *testing.T, state *gpuFixtureState) {
	require.NoError(t, state.fixture.prepare(t))
	rememberDataStorageDevice(state.host.IP, state.host.DataStorageDevice)
}

func applyGPUSubstrateStage(t *testing.T, state *gpuFixtureState) {
	cfgPath := writeForgeConfigGPUDriver(t, state.runID, state.host.IP, state.privKeyPath, gpuUpgradeBaselineDriver)
	// Bring up K3s first so the pinned-image cache is imported and verified
	// before the GPU operator (or any chart) can consult a public registry.
	bootstrap := applyOnceArgs(t, state.forgeBin, state.forgeHome, cfgPath,
		"--skip-gpu", "--skip-chart", "--skip-overlay", "--skip-secrets", "--skip-flux")
	assertApplyMarkers(t, bootstrap, "action:     install", "node ready: true", "data storage: iterabase-data")
	preparePinnedImageCache(t, &state.diagnostics, state.host.IP, state.privKeyPath, "gpu")
	out := applyOnce(t, state.forgeBin, state.forgeHome, cfgPath)
	assertApplyMarkers(t, out, "node ready: true", "data storage: iterabase-data", "gpu ready: true", "gpu driver: "+gpuUpgradeBaselineDriver)
	state.bindKubeconfigTunnel(t)
	assertPinnedNFDRender(t, filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml"), state.runID+"-gpu-operator")
	t.Logf("apply output:\n%s", out)
}

const (
	pinnedNFDImage        = "registry.k8s.io/nfd/node-feature-discovery:v0.19.0@sha256:2fa1c99ad09bdf2c8ad97706a4ad2fd548c84d5ecd70ba32a6152c667b96c4d2"
	pinnedNFDResyncPeriod = "-resync-period=30s"
)

// assertPinnedNFDRender verifies the resources actually rendered by the
// GPU Operator subchart. The lifecycle unit test protects the Helm value path;
// this live assertion protects the parent/subchart value wiring.
func assertPinnedNFDRender(t *testing.T, kcPath, release string) {
	t.Helper()
	restCfg, err := clientcmd.BuildConfigFromFlags("", kcPath)
	require.NoError(t, err)
	cs, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)

	ctx := context.Background()
	master, err := cs.AppsV1().Deployments("gpu-operator").Get(ctx, release+"-node-feature-discovery-master", metav1.GetOptions{})
	require.NoError(t, err)
	worker, err := cs.AppsV1().DaemonSets("gpu-operator").Get(ctx, release+"-node-feature-discovery-worker", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, validatePinnedNFDRender(master, worker))
}

func validatePinnedNFDRender(master *appsv1.Deployment, worker *appsv1.DaemonSet) error {
	if len(master.Spec.Template.Spec.Containers) != 1 {
		return fmt.Errorf("NFD master rendered %d containers, want exactly 1", len(master.Spec.Template.Spec.Containers))
	}
	masterContainer := master.Spec.Template.Spec.Containers[0]
	if masterContainer.Image != pinnedNFDImage {
		return fmt.Errorf("NFD master image %q, want %q", masterContainer.Image, pinnedNFDImage)
	}
	if !containsString(masterContainer.Args, pinnedNFDResyncPeriod) {
		return fmt.Errorf("NFD master args %q omit %q", masterContainer.Args, pinnedNFDResyncPeriod)
	}
	if len(worker.Spec.Template.Spec.Containers) != 1 {
		return fmt.Errorf("NFD worker rendered %d containers, want exactly 1", len(worker.Spec.Template.Spec.Containers))
	}
	if image := worker.Spec.Template.Spec.Containers[0].Image; image != pinnedNFDImage {
		return fmt.Errorf("NFD worker image %q, want %q", image, pinnedNFDImage)
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestValidatePinnedNFDRender(t *testing.T) {
	master := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "master", Image: pinnedNFDImage, Args: []string{"-enable-leader-election", pinnedNFDResyncPeriod},
	}}}}}}
	worker := &appsv1.DaemonSet{Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "worker", Image: pinnedNFDImage,
	}}}}}}
	require.NoError(t, validatePinnedNFDRender(master, worker))

	wrongImage := worker.DeepCopy()
	wrongImage.Spec.Template.Spec.Containers[0].Image = "registry.k8s.io/nfd/node-feature-discovery:" + "v0.18.3"
	require.ErrorContains(t, validatePinnedNFDRender(master, wrongImage), "worker image")

	missingResync := master.DeepCopy()
	missingResync.Spec.Template.Spec.Containers[0].Args = []string{"-enable-leader-election"}
	require.ErrorContains(t, validatePinnedNFDRender(missingResync, worker), "omit")
}

func assertGPUSmokeStage(t *testing.T, state *gpuFixtureState) {
	checkGPUSmoke(t, filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml"))
}

// checkGPUSmoke schedules a one-off pod requesting nvidia.com/gpu that runs
// nvidia-smi. Succeeding proves the full path the ModelBackend contract relies
// on: the resource is schedulable AND a container can actually use the GPU
// (CDI/runtime injection, not just advertisement).
func checkGPUSmoke(t *testing.T, kcPath string) {
	t.Helper()
	restCfg, err := clientcmd.BuildConfigFromFlags("", kcPath)
	require.NoError(t, err)
	cs, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)

	const name = "gpu-smoke"
	nvidiaRC := "nvidia"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			RuntimeClassName: &nvidiaRC,
			RestartPolicy:    corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "smoke",
				Image:   "nvidia/cuda:12.4.1-base-ubuntu22.04@sha256:0f6bfcbf267e65123bcc2287e2153dedfc0f24772fb5ce84afe16ac4b2fada95",
				Command: []string{"sh", "-c", "nvidia-smi 2>/dev/null || ls /dev/nvidia* 2>/dev/null"},
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
					},
				},
			}},
		},
	}
	_, err = cs.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	defer func() { _ = cs.CoreV1().Pods("default").Delete(context.Background(), name, metav1.DeleteOptions{}) }()

	deadline := time.Now().Add(8 * time.Minute) // image pull; driver is already up (gate passed)
	for time.Now().Before(deadline) {
		p, gerr := cs.CoreV1().Pods("default").Get(context.Background(), name, metav1.GetOptions{})
		if gerr == nil {
			switch p.Status.Phase {
			case corev1.PodSucceeded:
				return
			case corev1.PodFailed:
				t.Fatalf("gpu smoke pod failed: %+v", p.Status.ContainerStatuses)
			}
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("gpu smoke pod did not succeed within timeout")
}

// writeForgeConfigGPU writes a k3s + GPU (no platform chart) forge.yaml.
func writeForgeConfigGPU(t *testing.T, name, ip, keyPath string) string {
	return writeForgeConfigGPUDriver(t, name, ip, keyPath, "")
}

func writeForgeConfigGPUDriver(t *testing.T, name, ip, keyPath, driverVersion string) string {
	return writeForgeConfigSpec(t, forgeConfigSpec{
		Name: name, Address: ip, SSHKeyPath: keyPath, GPU: true, GPUDriverVersion: driverVersion,
	})
}

// sshRun runs a command on the fixture host host over SSH and returns combined output.
func sshRun(t *testing.T, ip, keyPath, cmd string) (string, error) {
	t.Helper()
	client, err := sshDial(ip, keyPath)
	if err != nil {
		return "", err
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

// dumpGPUDiagnostics queries the GPU operator state on the fixture host
// when the readiness gate fails, so the cause (driver/toolkit/device-plugin/validator)
// is visible in the test log rather than just "gpu not ready after 15m".
func dumpGPUDiagnostics(t *testing.T, ip, keyPath string) {
	t.Helper()
	t.Log("=== GPU diagnostics ===")
	cmds := []string{
		"sudo k3s kubectl get clusterpolicy -o yaml",
		"sudo k3s kubectl get nodes -o wide --show-labels",
		"sudo k3s kubectl describe nodes",
		"sudo k3s kubectl get daemonsets,pods -n gpu-operator -o wide",
		"for pod in $(sudo k3s kubectl get pods -n gpu-operator -o name); do echo --- $pod; sudo k3s kubectl logs -n gpu-operator $pod --tail=200 --all-containers=true --prefix=true 2>&1 || true; done",
		"sudo k3s kubectl get deployment,pods,pvc -n forge-gpu-upgrade -o wide 2>&1 || true",
		"sudo k3s kubectl logs -n forge-gpu-upgrade -l app.kubernetes.io/name=gpu-driver-upgrade-workload --tail=200 --prefix=true 2>&1 || true",
		"sudo k3s kubectl get events -A --sort-by=.lastTimestamp | tail -100",
		"echo '--- k3s containerd config: nvidia/cdi entries? ---'; sudo grep -iE 'nvidia|cdi|runtime' /var/lib/rancher/k3s/agent/etc/containerd/config.toml 2>/dev/null || echo 'no k3s containerd config or no nvidia/cdi entries'",
		"echo '--- /etc/containerd ---'; sudo ls /etc/containerd/ 2>/dev/null || echo 'no /etc/containerd'",
	}
	for _, c := range cmds {
		out, err := sshRun(t, ip, keyPath, c)
		t.Logf("$ %s\n%s(err=%v)", c, out, err)
	}
}
