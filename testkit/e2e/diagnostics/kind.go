package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

const (
	kindRuntimeTimeout = 2 * time.Minute
	kindCommandTimeout = 20 * time.Second
	kindCommandBytes   = 1 << 20
	kindEvidenceBytes  = 8 << 20
	kindIndexBytes     = 64 << 10
	kindMaxNodes       = 16
)

var kindIdentity = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,126}[a-z0-9])?$`)

// KindRuntimeCollector samples read-only node/runtime evidence. It never pulls,
// fetches content bodies, changes runtime configuration, or repairs ingest state.
// Collection is opt-in and best-effort across sources; every omission is explicit.
type KindRuntimeCollector struct {
	Executor    process.Executor
	ClusterName string
	OutputDir   string
	Redactor    *redact.Redactor
}

type kindSource struct {
	Name       string    `json:"name"`
	Node       string    `json:"node,omitempty"`
	Command    []string  `json:"command"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	State      string    `json:"state"`
	ExitCode   int       `json:"exit_code"`
	StdoutFile string    `json:"stdout_file,omitempty"`
	StderrFile string    `json:"stderr_file,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type kindReport struct {
	ClusterName        string       `json:"cluster_name"`
	StartedAt          time.Time    `json:"started_at"`
	FinishedAt         time.Time    `json:"finished_at"`
	CollectionComplete bool         `json:"collection_complete"`
	NodesOmitted       int          `json:"nodes_omitted,omitempty"`
	Error              string       `json:"error,omitempty"`
	Precision          string       `json:"precision"`
	Sources            []kindSource `json:"sources"`
}

// Collect writes private, sanitized evidence and index.json in OutputDir. The
// 8 MiB budget includes the index; command capture is bounded before redaction.
func (collector KindRuntimeCollector) Collect(ctx context.Context) error {
	if collector.Executor == nil || collector.OutputDir == "" || !kindIdentity.MatchString(collector.ClusterName) {
		return fmt.Errorf("kind diagnostics require executor, output directory and valid cluster identity")
	}
	if err := os.MkdirAll(collector.OutputDir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(collector.OutputDir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("kind runtime evidence directory is not empty; refusing to overwrite or repeat collection")
	}
	bounded, cancel := context.WithTimeout(ctx, kindRuntimeTimeout)
	defer cancel()
	report := kindReport{
		ClusterName: collector.ClusterName, StartedAt: time.Now().UTC(),
		Precision: "Read-only observation, not historical transfer proof. ctr 1.7.18 active SIZE is a humanized current offset; exact expected bytes are unavailable. Empty means no entries at this sample, not a successful historical pull.",
	}
	remaining := kindEvidenceBytes - kindIndexBytes
	var collected []error
	probe := func(name, node, command string, args ...string) (process.Result, bool) {
		source := kindSource{
			Name: name, Node: node, Command: append([]string{command}, args...),
			StartedAt: time.Now().UTC(), ExitCode: -1,
		}
		finish := func() {
			source.FinishedAt = time.Now().UTC()
			report.Sources = append(report.Sources, source)
		}
		fail := func(state, message string) {
			message = collector.Redactor.DiagnosticString(message)
			if len(message) > 1024 {
				message = "collection error exceeded metadata limit; entire message withheld"
			}
			source.State, source.Error = state, message
			collected = append(collected, fmt.Errorf("kind %s: %s", name, message))
		}
		if err := bounded.Err(); err != nil {
			fail("unavailable", err.Error())
			finish()
			return process.Result{}, false
		}
		if remaining == 0 {
			fail("budget-exhausted", "runtime evidence byte budget exhausted")
			finish()
			return process.Result{}, false
		}
		limit := min(kindCommandBytes, remaining)
		result, err := collector.Executor.Run(bounded, process.Command{
			Name: command, Args: args, Timeout: kindCommandTimeout,
			MaxOutputBytes: limit, RedactURLs: true,
			// No automatic process evidence file: only the sanitized, budgeted
			// separate streams below may be persisted in the upload directory.
		})
		source.ExitCode = result.ExitCode
		// Defend the Executor seam too: a fake/custom executor must not bypass
		// capture bounds, sanitization, or stream separation.
		if result.OutputTruncated || len(result.Output) > limit || len(result.Stdout)+len(result.Stderr) > limit {
			fail("truncated", "command output limit exceeded; entire payload withheld")
			finish()
			return process.Result{OutputTruncated: true}, false
		}
		result.Stdout = collector.Redactor.DiagnosticString(result.Stdout)
		result.Stderr = collector.Redactor.DiagnosticString(result.Stderr)
		if name == "journals" {
			result.Stdout = withoutRuntimeConfiguration(result.Stdout)
			result.Stderr = withoutRuntimeConfiguration(result.Stderr)
		}
		if len(result.Stdout)+len(result.Stderr) > limit {
			fail("truncated", "redacted output limit exceeded; entire payload withheld")
			finish()
			return process.Result{OutputTruncated: true}, false
		}
		source.State = "collected"
		if strings.TrimSpace(result.Stdout) == "" {
			source.State = "empty"
		}
		if err != nil {
			fail("unavailable", err.Error())
		} else if result.ExitCode != 0 {
			fail("unavailable", fmt.Sprintf("command exited with status %d", result.ExitCode))
		}
		for _, stream := range []struct{ suffix, value string }{
			{"stdout", result.Stdout}, {"stderr", result.Stderr},
		} {
			filename := fmt.Sprintf("%02d-%s.%s.log", len(report.Sources), name, stream.suffix)
			// Charge the full attempted write even if a filesystem failure leaves
			// only a partial file, so later sources cannot exceed the budget.
			remaining -= len(stream.value)
			if writeErr := os.WriteFile(filepath.Join(collector.OutputDir, filename), []byte(stream.value), 0o600); writeErr != nil {
				fail("unavailable", "write sanitized evidence: "+writeErr.Error())
				continue
			}
			if stream.suffix == "stdout" {
				source.StdoutFile = filename
			} else {
				source.StderrFile = filename
			}
		}
		finish()
		return result, source.State != "unavailable"
	}

	nodes, ok := probe("nodes", "", "kind", "get", "nodes", "--name", collector.ClusterName)
	if ok {
		identities := strings.Fields(nodes.Stdout) // Never parse combined/stderr.
		valid := len(identities) > 0
		seen := make(map[string]bool)
		for _, node := range identities {
			if !kindIdentity.MatchString(node) || seen[node] {
				valid = false
			}
			seen[node] = true
		}
		if !valid {
			collected = append(collected, errors.New("kind node enumeration returned empty, duplicate or invalid identities"))
			report.Sources[0].State = "unavailable"
			report.Sources[0].Error = "empty, duplicate or invalid node identities; no runtime commands issued"
		} else {
			// Bound index cardinality as well as runtime command/data budgets.
			if len(identities) > kindMaxNodes {
				report.NodesOmitted = len(identities) - kindMaxNodes
				identities = identities[:kindMaxNodes]
				collected = append(collected, errors.New("kind node limit reached; additional nodes omitted"))
			}
			for index, node := range identities {
				if bounded.Err() != nil || remaining == 0 {
					report.NodesOmitted += len(identities) - index
					collected = append(collected, errors.New("kind runtime collection deadline or evidence budget exhausted; remaining nodes omitted"))
					break
				}
				journal, journalOK := probe("journals", node, "docker", "exec", node,
					"journalctl", "--no-pager", "--utc", "--output=short-iso-precise", "--lines=500",
					"--unit=containerd.service", "--unit=kubelet.service")
				if journalOK && strings.TrimSpace(journal.Stdout) == "-- No entries --" {
					report.Sources[len(report.Sources)-1].State = "empty"
				}
				active, activeOK := probe("active-ingests", node, "docker", "exec", node,
					"ctr", "--namespace", "k8s.io", "content", "active")
				if activeOK {
					lines := strings.Split(strings.TrimSpace(active.Stdout), "\n")
					if strings.Join(strings.Fields(lines[0]), " ") != "REF SIZE AGE" {
						report.Sources[len(report.Sources)-1].State = "unavailable"
						report.Sources[len(report.Sources)-1].Error = "unrecognized active-ingest table; no counts inferred"
						collected = append(collected, errors.New("kind active-ingest table has no recognized header"))
					} else if len(lines) == 1 {
						report.Sources[len(report.Sources)-1].State = "empty"
					}
				}
			}
		}
	}
	report.FinishedAt = time.Now().UTC()
	report.CollectionComplete = len(collected) == 0
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return errors.Join(append(collected, err)...)
	}
	if len(data)+1 > kindIndexBytes {
		// Do not truncate arbitrary metadata/error text across a secret boundary.
		report.Sources = nil
		report.CollectionComplete = false
		report.Error = "index exceeded metadata limit; source metadata withheld"
		data, err = json.MarshalIndent(report, "", "  ")
		collected = append(collected, errors.New(report.Error), err)
	}
	if err := os.WriteFile(filepath.Join(collector.OutputDir, "index.json"), append(data, '\n'), 0o600); err != nil {
		collected = append(collected, err)
	}
	return errors.Join(collected...)
}

// Containerd 1.7 can log its CRI configuration at startup. Do not retain that
// configuration dump even after ordinary field/URL redaction; it is not needed
// to identify an image-content failure. No configuration file is queried.
func withoutRuntimeConfiguration(value string) string {
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		if strings.Contains(strings.ToLower(line), "start cri plugin with config") {
			lines[index] = "<runtime configuration record withheld>"
		}
	}
	return strings.Join(lines, "\n")
}
