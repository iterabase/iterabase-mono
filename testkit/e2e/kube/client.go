// Package kube provides bounded kubectl, Helm, and port-forward operations.
package kube

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

// Client binds all cluster operations to one isolated kubeconfig.
type Client struct {
	Executor   process.Executor
	Kubeconfig string
	Redactor   *redact.Redactor
}

// Chart is an explicit source Helm input: an exact local chart directory or
// packaged archive.
type Chart struct {
	Mode      e2e.FixtureMode
	LocalPath string
}

// HelmOptions controls one deterministic upgrade/install operation.
type HelmOptions struct {
	Release         string
	Namespace       string
	Chart           Chart
	Values          map[string]string
	ValueFiles      []string
	CreateNamespace bool
	Wait            bool
	Timeout         time.Duration
}

// Kubectl executes one bounded command against only this kubeconfig.
func (client Client) Kubectl(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	if err := client.validate(); err != nil {
		return "", err
	}
	result, err := client.kubectlCommand(ctx, timeout, args)
	return result.Output, err
}

// KubectlSeparated executes one bounded command against only this kubeconfig and
// keeps exact stdout separate from stderr diagnostics. Existing Kubectl callers
// intentionally keep their historical combined-output behavior.
func (client Client) KubectlSeparated(ctx context.Context, timeout time.Duration, args ...string) (stdout, stderr string, err error) {
	if err := client.validate(); err != nil {
		return "", "", err
	}
	result, err := client.kubectlCommand(ctx, timeout, args)
	return result.Stdout, result.Stderr, err
}

// kubectlCommand owns the isolated kubeconfig prefix and evidence name shared by
// both kubectl entry points so the command contract stays in one place.
func (client Client) kubectlCommand(ctx context.Context, timeout time.Duration, args []string) (process.Result, error) {
	full := append([]string{"--kubeconfig", client.Kubeconfig}, args...)
	return client.Executor.Run(ctx, process.Command{
		Name: "kubectl", Args: full, Timeout: timeout,
		OutputName: "kubectl-" + commandFileName(args) + ".log",
	})
}

// HelmUpgrade executes exactly one upgrade --install with sorted values.
func (client Client) HelmUpgrade(ctx context.Context, options HelmOptions) (string, error) {
	if err := client.validate(); err != nil {
		return "", err
	}
	if err := options.Chart.Validate(); err != nil {
		return "", err
	}
	if options.Release == "" || options.Namespace == "" || options.Timeout <= 0 {
		return "", fmt.Errorf("helm release, namespace, and positive timeout are required")
	}
	args := []string{"upgrade", "--install", options.Release, "--namespace", options.Namespace, "--kubeconfig", client.Kubeconfig}
	if options.CreateNamespace {
		args = append(args, "--create-namespace")
	}
	if options.Wait {
		args = append(args, "--wait")
	}
	args = append(args, "--timeout", options.Timeout.String())
	for _, values := range options.ValueFiles {
		args = append(args, "--values", values)
	}
	keys := make([]string, 0, len(options.Values))
	for key := range options.Values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--set-string", key+"="+options.Values[key])
	}
	args = append(args, options.Chart.LocalPath)
	result, err := client.Executor.Run(ctx, process.Command{
		Name: "helm", Args: args, Timeout: options.Timeout + 2*time.Minute,
		OutputName: "helm-" + options.Release + ".log",
	})
	return result.Output, err
}

// Validate rejects floating chart selection and mismatched fixture shapes.
func (chart Chart) Validate() error {
	if strings.Contains(strings.ToLower(chart.LocalPath), "latest") {
		return fmt.Errorf("helm chart input must not use latest")
	}
	if chart.Mode != e2e.FixtureSource {
		return fmt.Errorf("unsupported Helm fixture mode %q", chart.Mode)
	}
	if chart.LocalPath == "" {
		return fmt.Errorf("%s Helm chart requires an exact local path", chart.Mode)
	}
	if !filepath.IsAbs(chart.LocalPath) {
		return fmt.Errorf("helm chart local path must be absolute")
	}
	return nil
}

func (client Client) validate() error {
	if client.Executor == nil {
		return fmt.Errorf("kubernetes client has no process executor")
	}
	if !filepath.IsAbs(client.Kubeconfig) {
		return fmt.Errorf("kubernetes client requires an absolute isolated kubeconfig")
	}
	return nil
}

func commandFileName(args []string) string {
	verb := "command"
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			verb = regexp.MustCompile(`[^A-Za-z0-9_.-]+`).ReplaceAllString(arg, "-")
			break
		}
	}
	digest := sha256.Sum256([]byte(strings.Join(args, "\x00")))
	return fmt.Sprintf("%s-%x", strings.Trim(verb, "-"), digest[:6])
}

// Forward is one live kubectl port-forward.
type Forward struct {
	LocalPort int
	URL       string

	cancel context.CancelFunc
	done   chan error
	output *lockedBuffer
	once   sync.Once
	err    error
}

var forwardingLine = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:([0-9]+) ->`)

// PortForward starts a loopback-only forward using a kernel-selected free port.
func (client Client) PortForward(ctx context.Context, namespace, resource string, remotePort int, scheme string) (*Forward, error) {
	if err := client.validate(); err != nil {
		return nil, err
	}
	if namespace == "" || resource == "" || remotePort <= 0 {
		return nil, fmt.Errorf("port-forward requires namespace, resource, and remote port")
	}
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("port-forward scheme must be http or https")
	}
	lifetime, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(lifetime, "kubectl", "--kubeconfig", client.Kubeconfig,
		"port-forward", "--address", "127.0.0.1", "-n", namespace, resource, "0:"+strconv.Itoa(remotePort))
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	forward := &Forward{cancel: cancel, done: make(chan error, 1), output: &lockedBuffer{}}
	ready := make(chan int, 1)
	go func() {
		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			line := scanner.Text()
			forward.output.WriteString(line + "\n")
			if match := forwardingLine.FindStringSubmatch(line); match != nil {
				port, _ := strconv.Atoi(match[1])
				select {
				case ready <- port:
				default:
				}
			}
		}
	}()
	go func() { forward.done <- cmd.Wait() }()
	select {
	case port := <-ready:
		forward.LocalPort = port
		forward.URL = fmt.Sprintf("%s://127.0.0.1:%d", scheme, port)
		return forward, nil
	case err := <-forward.done:
		cancel()
		return nil, fmt.Errorf("port-forward exited before readiness: %v\n%s", err, client.Redactor.String(forward.output.String()))
	case <-time.After(60 * time.Second):
		cancel()
		return nil, fmt.Errorf("port-forward did not become ready\n%s", client.Redactor.String(forward.output.String()))
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

// Stop terminates the forward and waits for process exit. It is idempotent.
func (forward *Forward) Stop() error {
	forward.once.Do(func() {
		forward.cancel()
		forward.err = <-forward.done
		if forward.err != nil && !strings.Contains(forward.err.Error(), "signal: killed") && !strings.Contains(forward.err.Error(), "context canceled") {
			forward.err = fmt.Errorf("stop port-forward: %w", forward.err)
		} else {
			forward.err = nil
		}
	})
	return forward.err
}

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (buffer *lockedBuffer) WriteString(value string) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	_, _ = buffer.Buffer.WriteString(value)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.String()
}
