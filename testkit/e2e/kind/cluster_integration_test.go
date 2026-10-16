//go:build e2e_kind

package kind

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/diagnostics"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

func TestRealKindLifecycleUsesPrivateKubeconfigAndDeletesCluster(t *testing.T) {
	output := t.TempDir()
	runner := process.Runner{Redactor: redact.New(), OutputDir: output}
	cluster, err := (Manager{Executor: runner}).Create(context.Background(), "testkit-lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := cluster.Delete(ctx); err != nil {
			t.Errorf("delete cluster: %v", err)
		}
	})
	if _, err := os.Stat(cluster.Kubeconfig); err != nil {
		t.Fatalf("private kubeconfig does not exist: %v", err)
	}
	if _, err := runner.Run(context.Background(), process.Command{
		Name: "kubectl", Args: []string{"--kubeconfig", cluster.Kubeconfig, "get", "nodes"},
		Timeout: 2 * time.Minute, OutputName: "nodes.log",
	}); err != nil {
		t.Fatal(err)
	}
	// Exercise the actual read-only runtime commands while the same fixture is
	// still alive. No failing registry pull or content mutation is introduced.
	evidence := filepath.Join(output, "runtime-diagnostics")
	if err := (diagnostics.KindRuntimeCollector{
		Executor: runner, ClusterName: cluster.Name, OutputDir: evidence, Redactor: runner.Redactor,
	}).Collect(context.Background()); err != nil {
		t.Fatalf("collect Kind runtime evidence before teardown: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(evidence, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Complete bool `json:"collection_complete"`
		Sources  []struct {
			State string `json:"state"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if !report.Complete || len(report.Sources) != 3 {
		t.Fatalf("real Kind evidence incomplete: %s", data)
	}
	t.Logf("collected real Kind nodes, journals and ingest metadata before independent teardown: %+v", report.Sources)
}
