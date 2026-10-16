package e2e

import (
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func gpuUpgradeObservationTestSnapshot() string {
	return "__K3S_READY__\n" + strings.Join([]string{
		base64.StdEncoding.EncodeToString([]byte(`{"items":[{"metadata":{"name":"fixture-node"}}]}`)),
		base64.StdEncoding.EncodeToString([]byte(`{"items":[{"metadata":{"name":"replacement-workload","uid":"replacement-uid"}}]}`)),
		base64.StdEncoding.EncodeToString([]byte(`{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"nvidia.com/v1","kind":"ClusterPolicy","metadata":{"name":"fixture-policy"}}]}`)),
	}, "\n") + "\n"
}

func TestGPUUpgradeObservationParsesStdoutDespiteSSHStderrWarning(t *testing.T) {
	snapshot := gpuUpgradeObservationTestSnapshot()
	client := sshOutputTestClient(t, func(command string, channel ssh.Channel, _ <-chan struct{}) uint32 {
		if !strings.Contains(command, "sudo systemctl is-active --quiet k3s") ||
			!strings.Contains(command, "get clusterpolicy -o json") {
			t.Errorf("not the GPU upgrade observation script: %q", command)
		}
		_, _ = io.WriteString(channel.Stderr(), "sudo: unable to resolve host fixture: Temporary failure in name resolution\n")
		_, _ = io.WriteString(channel, snapshot)
		return 0
	})
	lines, err := gpuUpgradeNodeObservation(t, client)
	if err != nil || len(lines) != 4 || strings.Join(lines, "\n")+"\n" != snapshot {
		t.Fatalf("valid GPU upgrade stdout was contaminated: lines=%q err=%v", lines, err)
	}
	var nodes corev1.NodeList
	decodeGPUUpgradeSnapshot(t, lines[1], &nodes)
	var pods corev1.PodList
	decodeGPUUpgradeSnapshot(t, lines[2], &pods)
	var policies unstructured.UnstructuredList
	decodeGPUUpgradeSnapshot(t, lines[3], &policies)
	if len(nodes.Items) != 1 || nodes.Items[0].Name != "fixture-node" ||
		len(pods.Items) != 1 || pods.Items[0].UID != "replacement-uid" ||
		len(policies.Items) != 1 || policies.Items[0].GetName() != "fixture-policy" {
		t.Fatalf("lost coherent node/workload/operator snapshot: nodes=%v pods=%v policies=%v", nodes, pods, policies)
	}
}

func TestGPUUpgradeObservationPreservesNotReadyMarkerWithStderrWarning(t *testing.T) {
	client := sshOutputTestClient(t, func(_ string, channel ssh.Channel, _ <-chan struct{}) uint32 {
		_, _ = io.WriteString(channel.Stderr(), "sudo: unable to resolve host fixture\n")
		_, _ = io.WriteString(channel, "__K3S_NOT_READY__\n")
		return 0
	})
	lines, err := gpuUpgradeNodeObservation(t, client)
	if err != nil || len(lines) != 1 || lines[0] != "__K3S_NOT_READY__" {
		t.Fatalf("expected readiness state was contaminated: lines=%q err=%v", lines, err)
	}
}

func TestGPUUpgradeObservationRejectsMalformedStdout(t *testing.T) {
	for name, stdout := range map[string]string{
		"warning on stdout": "sudo: unable to resolve host fixture\n" + gpuUpgradeObservationTestSnapshot(),
		"missing snapshot":  "__K3S_READY__\n",
		"unknown marker":    "__UNEXPECTED__\n",
		"empty response":    "",
	} {
		t.Run(name, func(t *testing.T) {
			client := sshOutputTestClient(t, func(_ string, channel ssh.Channel, _ <-chan struct{}) uint32 {
				_, _ = io.WriteString(channel, stdout)
				return 0
			})
			if _, err := gpuUpgradeNodeObservation(t, client); err == nil {
				t.Fatalf("malformed GPU snapshot became success: %q", stdout)
			}
		})
	}
}

func TestGPUUpgradeObservationPreservesFailureAndRedactedStderr(t *testing.T) {
	t.Setenv("GH_TOKEN", "gpu-observation-private-token")
	for _, stdout := range []string{gpuUpgradeObservationTestSnapshot(), "__K3S_NOT_READY__\n"} {
		client := sshOutputTestClient(t, func(_ string, channel ssh.Channel, _ <-chan struct{}) uint32 {
			_, _ = io.WriteString(channel.Stderr(), "failure warning gpu-observation-private-token\n")
			_, _ = io.WriteString(channel, stdout)
			return 42
		})
		_, err := gpuUpgradeNodeObservation(t, client)
		var exit *ssh.ExitError
		if !errors.As(err, &exit) || exit.ExitStatus() != 42 ||
			!strings.Contains(err.Error(), "failure warning") || strings.Contains(err.Error(), "gpu-observation-private-token") {
			t.Fatalf("SSH error/stderr was lost, leaked or treated as readiness: %v", err)
		}
	}
}
