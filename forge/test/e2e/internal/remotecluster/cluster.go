// Package remotecluster provides bounded owner-specific helpers around the
// kubeconfig fetched from a real host provisioned by Forge. It never creates a
// Kind cluster or installs charts directly; portable product and rollout
// behavior belongs to the control-plane and chart owner suites.
package remotecluster

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// Cluster binds every local Kubernetes operation to a Forge-fetched kubeconfig.
type Cluster struct {
	Kubeconfig string
}

// Use wraps a Forge-fetched kubeconfig without creating or deleting a cluster.
func Use(t *testing.T, kubeconfig string) *Cluster {
	t.Helper()
	return &Cluster{Kubeconfig: kubeconfig}
}

// Kubectl runs kubectl against the cluster and returns combined stdout.
func (c *Cluster) Kubectl(t *testing.T, args ...string) string {
	t.Helper()
	mustBin(t, "kubectl")
	full := append([]string{"--kubeconfig", c.Kubeconfig}, args...)
	return run(t, "kubectl", full...)
}

// KubectlSeparated runs kubectl without merging stderr into exact stdout.
// Successful stdout is returned unchanged; stderr is independently bounded.
// Failures return both bounded streams in the error. Existing Kubectl callers
// intentionally keep their historical combined-output behavior.
func (c *Cluster) KubectlSeparated(args ...string) (stdout, stderr string, err error) {
	full := append([]string{"--kubeconfig", c.Kubeconfig}, args...)
	return runSeparated("kubectl", full...)
}

// FirstPodName returns the name of the first pod matching a label selector
// (e.g. "app.kubernetes.io/component=api"), polling briefly until one exists.
func (c *Cluster) FirstPodName(t *testing.T, namespace, selector string) string {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		out := c.Kubectl(t, "get", "pods", "-n", namespace, "-l", selector,
			"-o", `jsonpath={.items[0].metadata.name}`)
		if strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no pod found in %s with selector %q", namespace, selector)
	return ""
}

// PodLogs returns the logs of a container in the named pod. Pass container==""
// for the default container; pass an init-container name to read bootstrap /
// migrate output.
func (c *Cluster) PodLogs(t *testing.T, namespace, pod, container string) string {
	t.Helper()
	args := []string{"logs", "-n", namespace, pod}
	if container != "" {
		args = append(args, "-c", container)
	}
	return c.Kubectl(t, args...)
}

// ApplyAndWait applies a manifest file and then waits for a condition on a
// resource. resource is a fully-qualified "kind.group/name" (or "kind/name");
// condition is a `kubectl wait` condition such as "jsonpath={.status.ready}=true".
func (c *Cluster) ApplyAndWait(t *testing.T, manifestPath, namespace, resource, condition string, timeout time.Duration) {
	t.Helper()
	c.Kubectl(t, "apply", "-f", manifestPath, "-n", namespace)
	c.Kubectl(t, "wait", "-n", namespace, resource, "--for", condition, "--timeout", timeout.String())
}

// PortForward port-forwards a service port to a local port and returns the local
// base URL (e.g. "http://127.0.0.1:18080"). The forward is torn down by the
// returned stop func and also on test cleanup. service is a kubectl target
// (e.g. "svc/cp-control-plane-api").
func (c *Cluster) PortForward(t *testing.T, namespace, service string, svcPort, localPort int) (baseURL string, stop func()) {
	t.Helper()
	mustBin(t, "kubectl")
	cmd := exec.Command("kubectl", "--kubeconfig", c.Kubeconfig,
		"port-forward", "-n", namespace, service, fmt.Sprintf("%d:%d", localPort, svcPort))
	if err := cmd.Start(); err != nil {
		t.Fatalf("port-forward start: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	var stopOnce sync.Once
	stopFn := func() {
		stopOnce.Do(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-done
		})
	}
	t.Cleanup(stopFn)

	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("port-forward exited early")
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return fmt.Sprintf("http://%s", addr), stopFn
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("port-forward to %s/%s:%d never became ready", namespace, service, svcPort)
	return "", stopFn
}

// mustBin fails the test if the named binary is not on PATH.
func mustBin(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Fatalf("required binary %q not on PATH: %v", name, err)
	}
}

// run executes a command with a generous timeout and fails the test on error,
// returning combined stdout. The 15m cap is a backstop; kubectl waits carry
// their own tighter bounds so this only guards against a truly hung operation.
func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

const maxSeparatedCommandStreamBytes = 16 << 10

func runSeparated(name string, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdoutBuffer, stderrBuffer bytes.Buffer
	cmd.Stdout = &stdoutBuffer
	cmd.Stderr = &stderrBuffer
	commandErr := cmd.Run()
	stdout = stdoutBuffer.String()
	stderr = boundedCommandStream(stderrBuffer.String(), maxSeparatedCommandStreamBytes)
	if commandErr == nil {
		return stdout, stderr, nil
	}
	stdout = boundedCommandStream(stdout, maxSeparatedCommandStreamBytes)
	if ctx.Err() != nil {
		commandErr = ctx.Err()
	}
	return stdout, stderr, fmt.Errorf("%s separated execution: %w\nstdout:\n%s\nstderr:\n%s", name, commandErr, stdout, stderr)
}

func boundedCommandStream(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	suffix := fmt.Sprintf("\n...[truncated; %d bytes total]", len(value))
	keep := limit - len(suffix)
	if keep < 0 {
		return suffix[len(suffix)-limit:]
	}
	for keep > 0 && !utf8.ValidString(value[:keep]) {
		keep--
	}
	return value[:keep] + suffix
}
