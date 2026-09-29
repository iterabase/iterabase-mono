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
}

// Result contains already-redacted command output. Output remains the combined
// stdout+stderr stream for existing callers; Stdout and Stderr keep the exact
// channels so assertions can read result data without protocol diagnostics.
type Result struct {
	Output   string
	Stdout   string
	Stderr   string
	ExitCode int
}

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
	bounded, cancel := context.WithTimeout(ctx, command.Timeout)
	defer cancel()

	cmd := exec.CommandContext(bounded, command.Name, command.Args...)
	configureProcessTree(cmd)
	cmd.Dir = command.Dir
	cmd.Env = mergedEnv(command.Env)
	var output lockedBuffer
	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&output, &stdout)
	cmd.Stderr = io.MultiWriter(&output, &stderr)
	err := cmd.Run()
	redacted := runner.Redactor.String(output.String())
	result := Result{
		Output: redacted,
		Stdout: runner.Redactor.String(stdout.String()),
		Stderr: runner.Redactor.String(stderr.String()),
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if command.OutputName != "" {
		if err := runner.writeOutput(command.OutputName, []byte(redacted)); err != nil {
			return result, err
		}
	}
	if bounded.Err() != nil {
		return result, fmt.Errorf("%s exceeded %s: %w", command.Name, command.Timeout, bounded.Err())
	}
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return result, fmt.Errorf("%s exited with status %d", command.Name, exitError.ExitCode())
		}
		return result, fmt.Errorf("run %s: %w", command.Name, err)
	}
	return result, nil
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

// lockedBuffer serializes the stdout and stderr copiers that share the combined
// evidence stream while each channel is captured independently.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
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
