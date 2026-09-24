package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/forge/test/e2e/internal/remotecluster"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	controlPlaneDigestEnv     = "CONTROL_PLANE_IMAGE_DIGEST"
	inferenceGatewayDigestEnv = "INFERENCE_GATEWAY_IMAGE_DIGEST"
	toolRunnerDigestEnv       = "TOOL_RUNNER_IMAGE_DIGEST"
)

type candidateContainerSpec struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type candidateContainerStatus struct {
	Name    string `json:"name"`
	ImageID string `json:"imageID"`
}

const candidateControlPlaneReadyTimeout = 10 * time.Minute

// assertCandidateImageDigests verifies both sides of the unified runtime
// contract. Pods request the exact composer reference and K3s CRI binds their
// imageID to the config digest proven at import. The same import separately
// binds that tag to its single-platform manifest digest, which remains the
// runtime identity retained in scenario evidence.
func assertCandidateImageDigests(t *testing.T, cluster *remotecluster.Cluster, namespace string, runtimeDigests map[string]importedRuntimeIdentity, digestEnvs ...string) {
	t.Helper()
	waitForCandidateControlPlaneReady(t, cluster, namespace, candidateControlPlaneReadyTimeout)
	var pods struct {
		Items []struct {
			Spec struct {
				Containers     []candidateContainerSpec `json:"containers"`
				InitContainers []candidateContainerSpec `json:"initContainers"`
			} `json:"spec"`
			Status struct {
				ContainerStatuses     []candidateContainerStatus `json:"containerStatuses"`
				InitContainerStatuses []candidateContainerStatus `json:"initContainerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	raw := cluster.Kubectl(t, "get", "pods", "-n", namespace, "-o", "json")
	if err := json.Unmarshal([]byte(raw), &pods); err != nil {
		t.Fatalf("decode candidate pod image identities: %v", err)
	}
	for _, envName := range digestEnvs {
		var prefix string
		switch envName {
		case controlPlaneDigestEnv:
			prefix = "CONTROL_PLANE"
		case inferenceGatewayDigestEnv:
			prefix = "INFERENCE_GATEWAY"
		case toolRunnerDigestEnv:
			prefix = "TOOL_RUNNER"
		default:
			t.Fatalf("image digest environment %s has no repository/tag authority", envName)
		}
		repository, tag := os.Getenv(prefix+"_IMAGE_REPO"), os.Getenv(prefix+"_IMAGE_TAG")
		identity := runtimeDigests[prefix]
		if repository == "" && tag == "" && identity == (importedRuntimeIdentity{}) {
			continue
		}
		if repository == "" || tag == "" || !isCanonicalSHA256Digest(identity.ConfigDigest) ||
			!isCanonicalSHA256Digest(identity.ManifestDigest) {
			t.Fatalf("%s composed request/config/manifest digest identity is incomplete", prefix)
		}
		expectedReference := repository + ":" + tag
		found := false
		for _, pod := range pods.Items {
			specs := slices.Concat(pod.Spec.Containers, pod.Spec.InitContainers)
			statuses := slices.Concat(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses)
			for _, spec := range specs {
				if spec.Image != expectedReference {
					continue
				}
				for _, status := range statuses {
					if status.Name == spec.Name && imageIDMatchesConfigDigest(status.ImageID, identity.ConfigDigest) {
						found = true
						break
					}
				}
			}
		}
		if !found {
			t.Fatalf("no running container in %s requested %s and reported imported CRI config digest %s", namespace, expectedReference, identity.ConfigDigest)
		}
		t.Logf("verified composed request %s, CRI config digest %s, and imported runtime manifest %s", expectedReference, identity.ConfigDigest, identity.ManifestDigest)
	}
}

func imageIDMatchesConfigDigest(imageID, configDigest string) bool {
	return imageID == configDigest || strings.HasSuffix(imageID, "@"+configDigest)
}

func TestImageIDMatchesOnlyImportedConfigDigest(t *testing.T) {
	configDigest := "sha256:" + strings.Repeat("a", 64)
	manifestDigest := "sha256:" + strings.Repeat("b", 64)
	if !imageIDMatchesConfigDigest(configDigest, configDigest) ||
		!imageIDMatchesConfigDigest("docker.io/iterabase/control-plane@"+configDigest, configDigest) {
		t.Fatal("valid CRI config identity was rejected")
	}
	if imageIDMatchesConfigDigest(manifestDigest, configDigest) ||
		imageIDMatchesConfigDigest("sha256:prefix"+configDigest, configDigest) {
		t.Fatal("non-config or substring image identity unexpectedly passed")
	}
}

func waitForCandidateControlPlaneReady(t *testing.T, cluster *remotecluster.Cluster, namespace string, timeout time.Duration) {
	t.Helper()
	restConfig, err := clientcmd.BuildConfigFromFlags("", cluster.Kubeconfig)
	if err != nil {
		t.Fatalf("build candidate kubeconfig: %v", err)
	}
	restConfig.Timeout = 30 * time.Second
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("create candidate Kubernetes client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var last string
	for {
		deployments, listErr := clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=control-plane",
		})
		if listErr != nil {
			last = "list control-plane Deployments: " + listErr.Error()
		} else if ready, state := candidateControlPlaneDeploymentsReady(deployments.Items); ready {
			t.Logf("candidate control-plane workload Ready: %s", state)
			return
		} else {
			last = state
		}

		select {
		case <-ctx.Done():
			t.Fatalf("candidate control-plane workload did not become Ready within %s: %s", timeout, last)
		case <-time.After(2 * time.Second):
		}
	}
}

func candidateControlPlaneDeploymentsReady(deployments []appsv1.Deployment) (bool, string) {
	if len(deployments) == 0 {
		return false, "no control-plane Deployment observed"
	}
	states := make([]string, 0, len(deployments))
	allReady := true
	for i := range deployments {
		deployment := &deployments[i]
		desired := int32(0)
		if deployment.Spec.Replicas != nil {
			desired = *deployment.Spec.Replicas
		}
		available := false
		for _, condition := range deployment.Status.Conditions {
			if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
				available = true
				break
			}
		}
		ready := desired > 0 && deployment.Status.ObservedGeneration >= deployment.Generation &&
			deployment.Status.UpdatedReplicas == desired && deployment.Status.Replicas == desired &&
			deployment.Status.AvailableReplicas >= desired && available
		allReady = allReady && ready
		states = append(states, fmt.Sprintf("%s ready=%t desired=%d total=%d updated=%d available=%d observed=%d generation=%d",
			deployment.Name, ready, desired, deployment.Status.Replicas, deployment.Status.UpdatedReplicas,
			deployment.Status.AvailableReplicas, deployment.Status.ObservedGeneration, deployment.Generation))
	}
	return allReady, strings.Join(states, "; ")
}

func TestCandidateControlPlaneDeploymentsReady(t *testing.T) {
	replicas := int32(1)
	ready := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "control-plane-api", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2,
			Replicas:           1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
			Conditions: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue,
			}},
		},
	}

	ok, state := candidateControlPlaneDeploymentsReady(nil)
	if ok || !strings.Contains(state, "no control-plane Deployment") {
		t.Fatalf("empty deployment state = %t %q", ok, state)
	}
	ok, state = candidateControlPlaneDeploymentsReady([]appsv1.Deployment{ready})
	if !ok || !strings.Contains(state, "ready=true") {
		t.Fatalf("ready deployment state = %t %q", ok, state)
	}
	oldReplicaSetAvailable := ready.DeepCopy()
	oldReplicaSetAvailable.Status.UpdatedReplicas = 0
	ok, state = candidateControlPlaneDeploymentsReady([]appsv1.Deployment{*oldReplicaSetAvailable})
	if ok || !strings.Contains(state, "updated=0") {
		t.Fatalf("old ReplicaSet availability state = %t %q", ok, state)
	}
	rolling := ready.DeepCopy()
	rolling.Status.Replicas = 2
	ok, state = candidateControlPlaneDeploymentsReady([]appsv1.Deployment{*rolling})
	if ok || !strings.Contains(state, "total=2") {
		t.Fatalf("rolling Deployment state = %t %q", ok, state)
	}
	stale := ready.DeepCopy()
	stale.Status.ObservedGeneration = 1
	ok, state = candidateControlPlaneDeploymentsReady([]appsv1.Deployment{ready, *stale})
	if ok || !strings.Contains(state, "ready=false") {
		t.Fatalf("mixed deployment state = %t %q", ok, state)
	}
}
