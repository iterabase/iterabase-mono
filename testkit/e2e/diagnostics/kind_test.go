package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

type kindFixture struct {
	commands []process.Command
	run      func(context.Context, process.Command) (process.Result, error)
}

func (fixture *kindFixture) Run(ctx context.Context, command process.Command) (process.Result, error) {
	fixture.commands = append(fixture.commands, command)
	if fixture.run != nil {
		return fixture.run(ctx, command)
	}
	return kindFixtureResult(command), nil
}

func kindFixtureResult(command process.Command) process.Result {
	stdout, stderr := "safe evidence\n", ""
	switch {
	case command.Name == "kind":
		stdout, stderr = "charts-control-plane\n", "hostname warning: not a node\n"
	case command.Name == "docker" && command.Args[2] == "journalctl":
		stdout = "failed to read expected number of bytes: unexpected EOF ref=layer-sha256:" + strings.Repeat("a", 64) + "\nhttps://user:pass@cdn01.quay.io/blob?token=first&X-Amz-Signature=second#fragment\nAuthorization: Basic base64-secret\nCookie: session=cookie-secret\nStart cri plugin with config {unrecognized:raw-config-secret}\n"
	case command.Name == "docker":
		stdout = "REF SIZE AGE\nlayer-sha256:" + strings.Repeat("a", 64) + " 14.6MB 3s\n"
	case command.Name == "kubectl" && strings.Contains(strings.Join(command.Args, " "), "get pods -A -o jsonpath="):
		stdout = ""
	case command.Name == "helm" && strings.Contains(strings.Join(command.Args, " "), "list -A -o json"):
		stdout, stderr = "[]", "warning: not JSON\n"
	}
	return process.Result{Output: stdout + stderr, Stdout: stdout, Stderr: stderr}
}

func readKindReport(t *testing.T, directory string) kindReport {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report kindReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func assertKindEvidenceSafe(t *testing.T, directory string) int64 {
	t.Helper()
	var size int64
	if err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		size += info.Size()
		if info.Mode().Perm() != 0o600 {
			t.Errorf("evidence mode %s: %o", path, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range []string{"user:pass", "first", "second", "X-Amz-Signature", "#fragment", "base64-secret", "cookie-secret", "raw-config-secret"} {
			if strings.Contains(string(data), secret) {
				t.Errorf("secret %q in %s", secret, path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if size > kindEvidenceBytes {
		t.Fatalf("runtime evidence exceeded total bound: %d", size)
	}
	return size
}

func TestKindRuntimeCollectsSeparatedReadOnlyEvidenceBeforeAPI(t *testing.T) {
	t.Parallel()
	fixture := &kindFixture{}
	directory := t.TempDir()
	if err := (Collector{
		Executor: fixture, Kubeconfig: "/tmp/kubeconfig", OutputDir: directory,
		KindClusterName: "charts", Redactor: redact.New(),
	}).Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	report := readKindReport(t, filepath.Join(directory, "kind-runtime"))
	if !report.CollectionComplete || len(report.Sources) != 3 || !strings.Contains(report.Precision, "humanized") || !strings.Contains(report.Precision, "expected bytes are unavailable") {
		t.Fatalf("missing provenance/precision: %+v", report)
	}
	if report.StartedAt.IsZero() || report.FinishedAt.Before(report.StartedAt) {
		t.Fatalf("invalid capture timing: %+v", report)
	}
	for index, want := range []string{"kind", "docker", "docker", "kubectl"} {
		if fixture.commands[index].Name != want {
			t.Fatalf("wrong evidence order: %+v", fixture.commands)
		}
	}
	for _, command := range fixture.commands[:3] {
		if command.Timeout != kindCommandTimeout || command.MaxOutputBytes <= 0 || command.MaxOutputBytes > kindCommandBytes || !command.RedactURLs || command.OutputName != "" {
			t.Fatalf("unsafe runtime capture policy: %+v", command)
		}
		if command.Name == "docker" && command.Args[1] != "charts-control-plane" {
			t.Fatalf("stderr drove a node command: %+v", command)
		}
	}
	if !strings.Contains(strings.Join(fixture.commands[1].Args, " "), "--lines=500 --unit=containerd.service --unit=kubelet.service") {
		t.Fatalf("unbounded or incomplete journal capture: %+v", fixture.commands[1])
	}
	assertKindEvidenceSafe(t, filepath.Join(directory, "kind-runtime"))
}

func TestKindFailuresRemainVisibleAndDoNotSuppressOtherDiagnostics(t *testing.T) {
	t.Parallel()
	fixture := &kindFixture{run: func(_ context.Context, command process.Command) (process.Result, error) {
		result := kindFixtureResult(command)
		if command.Name == "docker" && command.Args[2] == "journalctl" {
			result.ExitCode = 7
			return result, errors.New("read failure https://user:pass@cdn01.quay.io/blob?token=first&X-Amz-Signature=second#fragment")
		}
		return result, nil
	}}
	directory := t.TempDir()
	err := (Collector{Executor: fixture, Kubeconfig: "/tmp/kubeconfig", OutputDir: directory, KindClusterName: "charts"}).Collect(context.Background())
	if err == nil || strings.Contains(err.Error(), "second") || strings.Contains(err.Error(), "user:pass") {
		t.Fatalf("missing/unsafe collection failure: %v", err)
	}
	report := readKindReport(t, filepath.Join(directory, "kind-runtime"))
	if report.CollectionComplete || report.Sources[1].State != "unavailable" || report.Sources[1].ExitCode != 7 || report.Sources[2].State != "collected" {
		t.Fatalf("source error was hidden or suppressed later source: %+v", report)
	}
	if _, err := os.Stat(filepath.Join(directory, "kubernetes-resources.log")); err != nil {
		t.Fatalf("runtime failure suppressed Kubernetes evidence: %v", err)
	}
	if len(fixture.commands) < 4 || fixture.commands[3].Name != "kubectl" {
		t.Fatalf("unexpected retry or missing API evidence: %+v", fixture.commands)
	}
	assertKindEvidenceSafe(t, filepath.Join(directory, "kind-runtime"))
}

func TestKindEmptyAndUnavailableIngestStatesAreDistinct(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"REF SIZE AGE\n", "", "unexpected-format\n"} {
		fixture := &kindFixture{run: func(_ context.Context, command process.Command) (process.Result, error) {
			result := kindFixtureResult(command)
			if command.Name == "docker" && command.Args[2] == "ctr" {
				result.Stdout, result.Output = table, table
			}
			return result, nil
		}}
		directory := t.TempDir()
		err := (KindRuntimeCollector{Executor: fixture, ClusterName: "charts", OutputDir: directory}).Collect(context.Background())
		report := readKindReport(t, directory)
		want := "unavailable"
		if table == "REF SIZE AGE\n" {
			want = "empty"
		}
		if report.Sources[2].State != want || (err == nil) != (want == "empty") {
			t.Fatalf("table %q misrepresented: %+v err=%v", table, report, err)
		}
	}
}

func TestKindCaptureWithholdsTruncatedPayloadsAndBoundsWholeArtifact(t *testing.T) {
	t.Parallel()
	for _, oversized := range []bool{true, false} {
		fixture := &kindFixture{run: func(_ context.Context, command process.Command) (process.Result, error) {
			if command.Name == "kind" {
				var names []string
				for index := range 16 {
					names = append(names, fmt.Sprintf("charts-worker-%d", index))
				}
				value := strings.Join(names, "\n")
				return process.Result{Stdout: value, Output: value}, nil
			}
			length := command.MaxOutputBytes
			if oversized {
				length++ // A custom executor must not bypass the process policy.
			}
			value := "REF SIZE AGE\n" + strings.Repeat("a", length-len("REF SIZE AGE\n"))
			return process.Result{Stdout: value, Output: value}, nil
		}}
		directory := t.TempDir()
		err := (KindRuntimeCollector{Executor: fixture, ClusterName: "charts", OutputDir: directory}).Collect(context.Background())
		if err == nil {
			t.Fatal("incomplete capture was accepted")
		}
		report := readKindReport(t, directory)
		if report.CollectionComplete || (oversized && report.Sources[1].State != "truncated") || (!oversized && report.NodesOmitted == 0) {
			t.Fatalf("budget/truncation not explicit: %+v", report)
		}
		if oversized && (report.Sources[1].StdoutFile != "" || report.Sources[1].StderrFile != "") {
			t.Fatal("overflow retained a payload prefix")
		}
		assertKindEvidenceSafe(t, directory)
	}
}

func TestKindCollectionHonorsParentCancellationWithoutRetry(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture := &kindFixture{run: func(ctx context.Context, command process.Command) (process.Result, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > kindRuntimeTimeout || command.Timeout != kindCommandTimeout {
			t.Error("missing total/per-command time bound")
		}
		cancel()
		<-ctx.Done()
		return process.Result{ExitCode: -1}, ctx.Err()
	}}
	directory := t.TempDir()
	if err := (KindRuntimeCollector{Executor: fixture, ClusterName: "charts", OutputDir: directory}).Collect(ctx); err == nil {
		t.Fatal("cancellation failure swallowed")
	}
	if len(fixture.commands) != 1 {
		t.Fatalf("cancellation retried: %+v", fixture.commands)
	}
	if report := readKindReport(t, directory); report.CollectionComplete || report.Sources[0].State != "unavailable" {
		t.Fatalf("cancellation misrepresented: %+v", report)
	}
	// An already expired parent must not launch even the enumeration command.
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	fixture = &kindFixture{}
	if err := (KindRuntimeCollector{Executor: fixture, ClusterName: "charts", OutputDir: t.TempDir()}).Collect(expired); err == nil || len(fixture.commands) != 0 {
		t.Fatalf("expired parent launched a process: %+v err=%v", fixture.commands, err)
	}
}

func TestKindRejectsInvalidOrCombinedNodeIdentityAndRepeatCollection(t *testing.T) {
	t.Parallel()
	for _, identity := range []string{"", "--privileged", "node\nnode", "warning: not a node", "node/../../bad"} {
		fixture := &kindFixture{run: func(_ context.Context, _ process.Command) (process.Result, error) {
			return process.Result{Stdout: identity, Output: "charts-control-plane\n"}, nil
		}}
		directory := t.TempDir()
		collector := KindRuntimeCollector{Executor: fixture, ClusterName: "charts", OutputDir: directory}
		if err := collector.Collect(context.Background()); err == nil || len(fixture.commands) != 1 {
			t.Fatalf("invalid node caused runtime commands: %q commands=%+v err=%v", identity, fixture.commands, err)
		}
		if err := collector.Collect(context.Background()); err == nil || len(fixture.commands) != 1 {
			t.Fatal("existing evidence was overwritten or collection repeated")
		}
	}
}
