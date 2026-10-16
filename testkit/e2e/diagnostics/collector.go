// Package diagnostics collects redacted Kubernetes, Helm, process, and component evidence.
package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/artifacts"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

// PodLogTailLines is the maximum log range retained for each container. Owners
// that register runtime credentials before collection must inspect at least this
// range (or the complete log) so the redactor cannot miss an older literal.
const PodLogTailLines = 500

// Collector gathers actionable failure evidence without querying Kubernetes Secrets.
type Collector struct {
	Executor   process.Executor
	Kubeconfig string
	OutputDir  string
	Redactor   *redact.Redactor
	Artifacts  []artifacts.Entry
	// KindClusterName opts chart-owned diagnostics into runtime evidence before
	// API/Helm collection, plus URL-safe diagnostic capture. Empty keeps defaults.
	KindClusterName string
}

// Collect is best-effort across evidence sources and returns all collection errors.
func (collector Collector) Collect(ctx context.Context) error {
	if collector.Executor == nil || collector.Kubeconfig == "" || collector.OutputDir == "" {
		return fmt.Errorf("diagnostics require executor, kubeconfig, and output directory")
	}
	if err := os.MkdirAll(collector.OutputDir, 0o700); err != nil {
		return err
	}
	var collected []error
	sanitize := collector.Redactor.String
	if collector.KindClusterName != "" {
		sanitize = collector.Redactor.DiagnosticString
		if err := (KindRuntimeCollector{
			Executor: collector.Executor, ClusterName: collector.KindClusterName,
			OutputDir: filepath.Join(collector.OutputDir, "kind-runtime"), Redactor: collector.Redactor,
		}).Collect(ctx); err != nil {
			collected = append(collected, err)
		}
	}
	collect := func(name, command string, args []string, transform func(string) string) string {
		result, err := collector.Executor.Run(ctx, process.Command{
			Name: command, Args: args, Timeout: 2 * time.Minute, RedactURLs: collector.KindClusterName != "",
		})
		output := result.Output
		if transform != nil {
			output = transform(output)
		}
		output = sanitize(output)
		if writeErr := os.WriteFile(filepath.Join(collector.OutputDir, name+".log"), []byte(output), 0o600); writeErr != nil {
			collected = append(collected, writeErr)
		}
		if err != nil {
			if collector.KindClusterName != "" {
				collected = append(collected, fmt.Errorf("%s: %s", name, sanitize(err.Error())))
			} else {
				collected = append(collected, fmt.Errorf("%s: %w", name, err))
			}
		}
		if collector.KindClusterName != "" {
			return sanitize(result.Stdout) // Structured identities never include stderr.
		}
		return output
	}
	kube := func(name string, args ...string) string {
		return collect(name, "kubectl", append([]string{"--kubeconfig", collector.Kubeconfig}, args...), nil)
	}
	helm := func(name string, args ...string) string {
		return collect(name, "helm", append([]string{"--kubeconfig", collector.Kubeconfig}, args...), redactHelmSecretData)
	}

	kube("kubernetes-resources", "get",
		"namespaces,nodes,pods,deployments,statefulsets,daemonsets,jobs,services,endpointslices,ingresses,persistentvolumeclaims,persistentvolumes,storageclasses,csidrivers,csinodes,csistoragecapacities.storage.k8s.io",
		"-A", "-o", "yaml")
	kube("kubernetes-events", "get", "events", "-A", "--sort-by=.lastTimestamp", "-o", "yaml")
	// Parse only namespace/name pairs. Redacting a full Pod JSON document before
	// parsing can intentionally rewrite credential-shaped fields inside pod specs
	// and make otherwise valid JSON malformed. Names are sufficient to drive the
	// subsequent describe/log collection and cannot contain secret values.
	podNames := kube("kubernetes-pods", "get", "pods", "-A", "-o", `jsonpath={range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\n"}{end}`)
	helmJSON := helm("helm-list", "list", "-A", "-o", "json")

	for _, line := range strings.Split(strings.TrimSpace(podNames), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			collected = append(collected, fmt.Errorf("decode pod identity %q", line))
			continue
		}
		namespace, name := fields[0], fields[1]
		base := safeName(namespace + "-" + name)
		kube("describe-"+base, "describe", "pod", name, "-n", namespace)
		tail := fmt.Sprintf("--tail=%d", PodLogTailLines)
		kube("logs-"+base, "logs", name, "-n", namespace, "--all-containers", "--prefix", tail)
		kube("logs-previous-"+base, "logs", name, "-n", namespace, "--all-containers", "--prefix", "--previous", tail)
	}

	var releases []struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	}
	if err := json.Unmarshal([]byte(helmJSON), &releases); err != nil {
		collected = append(collected, fmt.Errorf("decode Helm releases for diagnostics: %w", err))
	} else {
		for _, release := range releases {
			base := safeName(release.Namespace + "-" + release.Name)
			helm("helm-get-"+base, "get", "all", release.Name, "-n", release.Namespace)
			helm("helm-history-"+base, "history", release.Name, "-n", release.Namespace, "--output", "yaml")
			helm("helm-values-"+base, "get", "values", release.Name, "-n", release.Namespace, "--all")
			helm("helm-hooks-"+base, "get", "hooks", release.Name, "-n", release.Namespace)
			helm("helm-status-"+base, "status", release.Name, "-n", release.Namespace)
		}
	}

	if err := artifacts.Collect(collector.Artifacts, filepath.Join(collector.OutputDir, "component"), collector.Redactor); err != nil {
		collected = append(collected, err)
	}
	return errors.Join(collected...)
}

func safeName(value string) string {
	value = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(value)
	if len(value) > 150 {
		value = value[:150]
	}
	return value
}

// redactHelmSecretData removes generated and user-supplied Kubernetes Secret
// payloads before Helm evidence is persisted. Exact literals cannot cover
// generated base64 values such as tls.key, and "helm get all" includes rendered
// Secret manifests even though generic Kubernetes collection excludes Secrets.
func redactHelmSecretData(value string) string {
	lines := strings.Split(value, "\n")
	start := 0
	for index := 0; index <= len(lines); index++ {
		if index != len(lines) && strings.TrimSpace(lines[index]) != "---" {
			continue
		}
		redactSecretDocument(lines[start:index])
		start = index + 1
	}
	return strings.Join(lines, "\n")
}

func redactSecretDocument(lines []string) {
	secret := false
	for _, line := range lines {
		if kind, found := topLevelYAMLField(line, "kind"); found && kind == "Secret" {
			secret = true
			break
		}
	}
	if !secret {
		return
	}
	for index := 0; index < len(lines); index++ {
		_, data := topLevelYAMLField(lines[index], "data")
		_, stringData := topLevelYAMLField(lines[index], "stringData")
		if !data && !stringData {
			continue
		}
		key, _, _ := strings.Cut(strings.TrimSpace(lines[index]), ":")
		lines[index] = key + ": <redacted>"
		for nested := index + 1; nested < len(lines); nested++ {
			if strings.TrimSpace(lines[nested]) == "" {
				continue
			}
			if strings.TrimLeft(lines[nested], " \t") == lines[nested] {
				break
			}
			lines[nested] = ""
			index = nested
		}
	}
}

func topLevelYAMLField(line, want string) (string, bool) {
	if strings.TrimLeft(line, " \t") != line {
		return "", false
	}
	key, value, found := strings.Cut(line, ":")
	if !found || strings.TrimSpace(key) != want {
		return "", false
	}
	return strings.TrimSpace(value), true
}
