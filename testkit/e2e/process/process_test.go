package process

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

func TestRunnerBoundsAndRedactsProcessOutput(t *testing.T) {
	t.Parallel()
	outputDir := t.TempDir()
	runner := Runner{Redactor: redact.New("process-secret"), OutputDir: outputDir}
	result, err := runner.Run(context.Background(), Command{
		Name: "sh", Args: []string{"-c", "printf 'token=process-secret\\n'; exit 7"},
		Timeout: time.Second, OutputName: "process.log",
	})
	if err == nil || result.ExitCode != 7 {
		t.Fatalf("process result = %+v, error = %v", result, err)
	}
	if strings.Contains(result.Output, "process-secret") {
		t.Fatalf("process output was not redacted: %s", result.Output)
	}
	data, readErr := os.ReadFile(filepath.Join(outputDir, "process.log"))
	if readErr != nil || strings.Contains(string(data), "process-secret") {
		t.Fatalf("persisted process evidence = %q, error = %v", data, readErr)
	}
}

func TestRunnerSeparatesStdoutFromStderrDiagnostics(t *testing.T) {
	t.Parallel()
	result, err := (Runner{}).Run(context.Background(), Command{
		Name: "sh", Args: []string{"-c", `printf '10005\n'; printf '%s\n' 'E0915 18:57:37.879813 3454 websocket.go:296] Unknown stream id 1, discarding message' >&2`},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "10005\n" {
		t.Fatalf("separated stdout = %q", result.Stdout)
	}
	if !strings.Contains(result.Stderr, "Unknown stream id 1, discarding message") {
		t.Fatalf("separated stderr = %q", result.Stderr)
	}
	if !strings.Contains(result.Output, "10005") || !strings.Contains(result.Output, "Unknown stream id 1") {
		t.Fatalf("combined output = %q", result.Output)
	}
}

func TestRunnerRetainsBothStreamsForFailingCommands(t *testing.T) {
	t.Parallel()
	result, err := (Runner{}).Run(context.Background(), Command{
		Name: "sh", Args: []string{"-c", `printf 'partial stdout\n'; printf '%s\n' 'actionable failure evidence' >&2; exit 3`},
		Timeout: time.Second,
	})
	if err == nil || result.ExitCode != 3 {
		t.Fatalf("failing process result = %+v, error = %v", result, err)
	}
	if result.Stdout != "partial stdout\n" || !strings.Contains(result.Stderr, "actionable failure evidence") {
		t.Fatalf("failing process streams = %+v", result)
	}
}

func TestRunnerTerminatesAtTimeout(t *testing.T) {
	t.Parallel()
	started := time.Now()
	_, err := (Runner{}).Run(context.Background(), Command{
		Name: "sh", Args: []string{"-c", "sleep 2"}, Timeout: 20 * time.Millisecond,
	})
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("timed process error = %v", err)
	}
}

func TestRunnerRequiresTimeout(t *testing.T) {
	t.Parallel()
	_, err := (Runner{}).Run(context.Background(), Command{Name: "true"})
	if err == nil {
		t.Fatal("unbounded process unexpectedly accepted")
	}
}
