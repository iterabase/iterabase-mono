package kube

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
)

type recordingExecutor struct {
	commands []process.Command
	result   process.Result
}

func (executor *recordingExecutor) Run(_ context.Context, command process.Command) (process.Result, error) {
	executor.commands = append(executor.commands, command)
	if executor.result == (process.Result{}) {
		return process.Result{Output: "ok"}, nil
	}
	return executor.result, nil
}

func TestHelmUpgradeUsesExactChartAndSortedValues(t *testing.T) {
	t.Parallel()
	executor := &recordingExecutor{}
	client := Client{Executor: executor, Kubeconfig: "/tmp/isolated-kubeconfig"}
	_, err := client.HelmUpgrade(context.Background(), HelmOptions{
		Release: "platform", Namespace: "iterabase-system",
		Chart:  Chart{Mode: e2e.FixtureSource, LocalPath: "/tmp/source/platform"},
		Values: map[string]string{"z.value": "last", "a.value": "first"},
		Wait:   true, CreateNamespace: true, Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	args := executor.commands[0].Args
	first := slices.Index(args, "a.value=first")
	last := slices.Index(args, "z.value=last")
	if first < 0 || last < 0 || first > last {
		t.Fatalf("Helm values are not sorted: %v", args)
	}
	if args[len(args)-1] != "/tmp/source/platform" {
		t.Fatalf("Helm did not use exact source path: %v", args)
	}
}

func TestKubectlEvidenceNameDoesNotContainArguments(t *testing.T) {
	t.Parallel()
	executor := &recordingExecutor{}
	client := Client{Executor: executor, Kubeconfig: "/tmp/isolated-kubeconfig"}
	if _, err := client.Kubectl(context.Background(), time.Minute, "create", "secret", "--from-literal=password=do-not-leak"); err != nil {
		t.Fatal(err)
	}
	if name := executor.commands[0].OutputName; name == "" || strings.Contains(name, "do-not-leak") {
		t.Fatalf("unsafe output name %q", name)
	}
}

func TestKubectlSeparatedKeepsStderrOutOfExactStdout(t *testing.T) {
	t.Parallel()
	const diagnostic = "E0915 18:57:37.879813 3454 websocket.go:296] Unknown stream id 1, discarding message"
	executor := &recordingExecutor{result: process.Result{
		Output: diagnostic + "\n10005\n",
		Stdout: "10005\n",
		Stderr: diagnostic + "\n",
	}}
	client := Client{Executor: executor, Kubeconfig: "/tmp/isolated-kubeconfig"}
	stdout, stderr, err := client.KubectlSeparated(context.Background(), time.Minute,
		"exec", "statefulset/postgresql", "--", "psql", "-Atc", "SELECT 10005;")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "10005\n" {
		t.Fatalf("separated kubectl stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "Unknown stream id 1, discarding message") {
		t.Fatalf("separated kubectl stderr = %q", stderr)
	}
	if executor.commands[0].Name != "kubectl" {
		t.Fatalf("separated kubectl command = %+v", executor.commands[0])
	}
}

func TestPortForwardDiscoversEphemeralPortAndStops(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "kubectl")
	script := "#!/bin/sh\necho 'Forwarding from 127.0.0.1:54321 -> 8080'\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	client := Client{Executor: &recordingExecutor{}, Kubeconfig: "/tmp/isolated-kubeconfig"}
	forward, err := client.PortForward(context.Background(), "default", "svc/api", 8080, "http")
	if err != nil {
		t.Fatal(err)
	}
	if forward.LocalPort != 54321 || forward.URL != "http://127.0.0.1:54321" {
		t.Fatalf("forward = %+v", forward)
	}
	if err := forward.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := forward.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestChartRejectsFloatingOrImplicitInputs(t *testing.T) {
	t.Parallel()
	charts := []Chart{
		{Mode: e2e.FixtureSource, LocalPath: "/tmp/charts/platform-latest"},
		{Mode: e2e.FixtureSource, LocalPath: "charts/platform"},
		{Mode: e2e.FixtureSource},
		{Mode: "published", LocalPath: "/tmp/charts/platform"},
	}
	for _, chart := range charts {
		if err := chart.Validate(); err == nil {
			t.Fatalf("chart unexpectedly valid: %+v", chart)
		}
	}
}
