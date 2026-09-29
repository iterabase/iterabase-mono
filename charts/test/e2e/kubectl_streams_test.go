package e2e_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/kube"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
)

// kubectlUnknownStreamDiagnostic is the exact transport warning observed while
// validating HOR-527 (PR #65 E2E run 33249607490, job 99092948826, source
// ff7d511b). It must never reach an asserted stdout value.
const kubectlUnknownStreamDiagnostic = "E0816 13:00:19.754266   42934 websocket.go:296] Unknown stream id 1, discarding message"

// writeNoisyCommand writes a PATH-resolved command that emits one stderr
// diagnostic line before its stdout payload, reproducing transport noise.
func writeNoisyCommand(t *testing.T, name, stdout, stderr string, exitCode int) {
	t.Helper()
	directory := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '" + stderr + "' >&2\n" +
		"printf '%s\\n' '" + stdout + "'\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func newUnitChartState(t *testing.T) *chartState {
	t.Helper()
	outputDir := t.TempDir()
	runner := process.Runner{OutputDir: outputDir}
	return &chartState{
		ctx:    context.Background(),
		runner: runner,
		client: kube.Client{Executor: runner, Kubeconfig: filepath.Join(outputDir, "kubeconfig")},
	}
}

func TestUnitPersistedStateAssertionIgnoresKubectlProtocolStderr(t *testing.T) {
	writeNoisyCommand(t, "kubectl", transitionMarker, kubectlUnknownStreamDiagnostic, 0)
	assertPersistedState(t, newUnitChartState(t))
}

func TestUnitKubectlFailureRetainsStderrDiagnostics(t *testing.T) {
	writeNoisyCommand(t, "kubectl", "", kubectlUnknownStreamDiagnostic, 1)
	state := newUnitChartState(t)
	if _, err := state.kubectlResult(10*time.Second, "get", "pods"); err == nil ||
		!strings.Contains(err.Error(), "Unknown stream id 1, discarding message") {
		t.Fatalf("kubectl failure error = %v", err)
	}
}

func TestUnitProcessHelperKeepsAssertedStdoutExact(t *testing.T) {
	writeNoisyCommand(t, "noisy-tool", "result-data", "protocol-warning", 0)
	if got := newUnitChartState(t).process(t, 10*time.Second, "noisy-tool"); got != "result-data" {
		t.Fatalf("process helper stdout = %q", got)
	}
}
