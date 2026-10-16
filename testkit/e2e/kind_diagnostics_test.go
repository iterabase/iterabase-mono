package e2e

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/diagnostics"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
)

type lifecycleEvidenceExecutor struct{ directory string }

func (executor lifecycleEvidenceExecutor) Run(_ context.Context, command process.Command) (process.Result, error) {
	file, err := os.OpenFile(filepath.Join(executor.directory, "order"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return process.Result{}, err
	}
	_, writeErr := file.WriteString(command.Name + "\n")
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return process.Result{}, err
	}
	stdout := ""
	var runErr error
	switch command.Name {
	case "kind":
		stdout = "charts-control-plane\n"
	case "docker":
		if command.Args[2] == "journalctl" {
			stdout = "failed to read expected number of bytes: unexpected EOF\n"
			runErr = errors.New("synthetic unavailable journal; no retry")
		} else {
			stdout = "REF SIZE AGE\n"
		}
	case "helm":
		stdout = "[]"
	}
	return process.Result{Output: stdout, Stdout: stdout}, runErr
}

func TestKindEvidenceFailurePreservesPrimaryFailureAndCleanup(t *testing.T) {
	if os.Getenv("ITERABASE_E2E_KIND_DIAGNOSTIC_HELPER") == "1" {
		suite := helperSuite(
			[]Stage[*helperState]{{Name: "image-pull", Run: func(t *testing.T, _ *helperState) { t.Fatal("original image-pull failure") }}},
			[]Hook[*helperState]{{Name: "delete-kind", Run: func(t *testing.T, state *helperState) {
				// The index must exist before teardown begins, even when one
				// runtime source failed. This does not convert failure to pass.
				if _, err := os.Stat(filepath.Join(state.directory, "evidence", "kind-runtime", "index.json")); err != nil {
					t.Fatal(err)
				}
				writeMarker(t, state.directory, "cleanup")
			}}},
			[]Hook[*helperState]{{Name: "evidence", Run: func(t *testing.T, state *helperState) {
				if err := (diagnostics.Collector{
					Executor: lifecycleEvidenceExecutor{directory: state.directory}, Kubeconfig: "/tmp/kubeconfig",
					OutputDir: filepath.Join(state.directory, "evidence"), KindClusterName: "charts",
				}).Collect(context.Background()); err != nil {
					t.Logf("best-effort diagnostics: %v", err)
				}
			}}},
		)
		suite.Run(t)
		return
	}
	directory := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKindEvidenceFailurePreservesPrimaryFailureAndCleanup$")
	command.Env = append(os.Environ(), "ITERABASE_E2E_KIND_DIAGNOSTIC_HELPER=1", "ITERABASE_E2E_HELPER_DIR="+directory)
	output, err := command.CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(output), "original image-pull failure") {
		t.Fatalf("primary failure lost or validation stalled: %v\n%s", err, output)
	}
	order, err := os.ReadFile(filepath.Join(directory, "order"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(order), "kind\ndocker\ndocker\nkubectl\n") || strings.Count(string(order), "kind\n") != 1 {
		t.Fatalf("wrong ordering or repeated runtime observation: %s", order)
	}
	if _, err := os.Stat(filepath.Join(directory, "cleanup")); err != nil {
		t.Fatalf("collection failure suppressed cleanup: %v\n%s", err, output)
	}
}
