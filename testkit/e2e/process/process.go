// Package process runs bounded, one-shot external test operations without retries.
package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

// Command describes one bounded process invocation.
type Command struct {
	Name       string
	Args       []string
	Dir        string
	Env        map[string]string
	Timeout    time.Duration
	OutputName string

	// MaxOutputBytes bounds the combined raw stdout/stderr capture. Zero keeps
	// existing capture behavior. Overflow drains but withholds the entire payload
	// so a partial secret cannot escape redaction.
	MaxOutputBytes int
	// RedactURLs opts diagnostic evidence into URL sanitization before structural
	// redaction. Protocol/assertion callers retain their existing default.
	RedactURLs bool
}

// Result contains already-redacted command output. Output remains the combined
// stdout+stderr stream for existing callers; Stdout and Stderr keep the exact
// channels so assertions can read result data without protocol diagnostics.
type Result struct {
	Output   string
	Stdout   string
	Stderr   string
	ExitCode int
	// OutputTruncated means all captured payload was withheld, not a valid prefix.
	OutputTruncated bool
}

// ErrOutputLimit marks an incomplete capture; the subprocess is never retried.
var ErrOutputLimit = errors.New("process output capture limit exceeded")

// Executor is the seam used by Kind, Kubernetes, diagnostics, and Playwright helpers.
type Executor interface {
	Run(context.Context, Command) (Result, error)
}

// Runner executes real child processes.
type Runner struct {
	Redactor  *redact.Redactor
	OutputDir string
}

// Run executes exactly once. Scenario retry belongs nowhere in this helper;
// bounded condition observation is provided separately by poll.
func (runner Runner) Run(ctx context.Context, command Command) (Result, error) {
	if command.Name == "" {
		return Result{}, fmt.Errorf("process command name is empty")
	}
	if command.Timeout <= 0 {
		return Result{}, fmt.Errorf("process %s requires a positive timeout", command.Name)
	}
	if command.OutputName != "" && runner.OutputDir == "" {
		return Result{}, fmt.Errorf("process output %q requires an output directory", command.OutputName)
	}
	if command.MaxOutputBytes < 0 {
		return Result{}, fmt.Errorf("process %s has a negative capture limit", command.Name)
	}
	bounded, cancel := context.WithTimeout(ctx, command.Timeout)
	defer cancel()

	cmd := exec.CommandContext(bounded, command.Name, command.Args...)
	configureProcessTree(cmd)
	cmd.Dir = command.Dir
	cmd.Env = mergedEnv(command.Env)
	output := capturedOutput{limit: command.MaxOutputBytes}
	cmd.Stdout = captureWriter{capture: &output}
	cmd.Stderr = captureWriter{capture: &output, stderr: true}
	err := cmd.Run()
	combined, stdout, stderr, truncated := output.snapshot()
	sanitize := runner.Redactor.String
	if command.RedactURLs {
		sanitize = runner.Redactor.DiagnosticString
	}
	result := Result{
		Output: sanitize(combined), Stdout: sanitize(stdout), Stderr: sanitize(stderr),
		OutputTruncated: truncated,
	}
	// Redaction itself can expand short literals. Apply the same fail-closed
	// bound to the final evidence, before writeOutput can persist anything.
	if command.MaxOutputBytes > 0 && (len(result.Output) > command.MaxOutputBytes || len(result.Stdout)+len(result.Stderr) > command.MaxOutputBytes) {
		result.Output, result.Stdout, result.Stderr = "", "", ""
		result.OutputTruncated = true
	}
	var captureErr error
	if result.OutputTruncated {
		captureErr = fmt.Errorf("%s: %w (%d bytes; payload withheld)", command.Name, ErrOutputLimit, command.MaxOutputBytes)
	}
	if command.MaxOutputBytes > 0 || command.RedactURLs {
		result.ExitCode = -1 // Diagnostic capture must not invent exit 0 before start.
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if command.OutputName != "" {
		if err := runner.writeOutput(command.OutputName, []byte(result.Output)); err != nil {
			return result, err
		}
	}
	if bounded.Err() != nil {
		return result, errors.Join(fmt.Errorf("%s exceeded %s: %w", command.Name, command.Timeout, bounded.Err()), captureErr)
	}
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return result, errors.Join(fmt.Errorf("%s exited with status %d", command.Name, exitError.ExitCode()), captureErr)
		}
		return result, errors.Join(fmt.Errorf("run %s: %w", command.Name, err), captureErr)
	}
	return result, captureErr
}

func (runner Runner) writeOutput(name string, data []byte) error {
	if runner.OutputDir == "" {
		return fmt.Errorf("process output %q requested without an output directory", name)
	}
	clean := filepath.Clean(name)
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid process output name %q", name)
	}
	path := filepath.Join(runner.OutputDir, clean)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// capturedOutput serializes both channels and accounts for their combined size.
// Once overflowed it drains without retaining even a prefix of either channel.
type capturedOutput struct {
	mu                       sync.Mutex
	combined, stdout, stderr bytes.Buffer
	limit                    int
	truncated                bool
}

type captureWriter struct {
	capture *capturedOutput
	stderr  bool
}

var _ io.Writer = captureWriter{}

func (writer captureWriter) Write(data []byte) (int, error) {
	capture := writer.capture
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.truncated {
		return len(data), nil
	}
	if capture.limit > 0 && len(data) > capture.limit-capture.combined.Len() {
		capture.truncated = true
		capture.combined.Reset()
		capture.stdout.Reset()
		capture.stderr.Reset()
		return len(data), nil
	}
	capture.combined.Write(data)
	if writer.stderr {
		capture.stderr.Write(data)
	} else {
		capture.stdout.Write(data)
	}
	return len(data), nil
}

func (capture *capturedOutput) snapshot() (string, string, string, bool) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.combined.String(), capture.stdout.String(), capture.stderr.String(), capture.truncated
}

func mergedEnv(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, value := range os.Environ() {
		key, current, found := strings.Cut(value, "=")
		if found {
			values[key] = current
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}
