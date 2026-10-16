package e2e_test

import (
	"context"
	"testing"

	kindcluster "github.com/nunocgoncalves/iterabase-mono/testkit/e2e/kind"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

type unusedDiagnosticsExecutor struct{}

func (*unusedDiagnosticsExecutor) Run(context.Context, process.Command) (process.Result, error) {
	panic("wiring test must not execute infrastructure")
}

func TestUnitChartDiagnosticsOptIntoRuntimeEvidenceWithoutChangingHooks(t *testing.T) {
	t.Parallel()
	executor := &unusedDiagnosticsExecutor{}
	state := &chartState{
		cluster:        &kindcluster.Cluster{Name: "charts-exact", Kubeconfig: "/tmp/private-kubeconfig"},
		diagnosticsDir: t.TempDir(), redactor: redact.New(),
	}
	collector := chartDiagnosticCollector(state, executor)
	if collector.KindClusterName != state.cluster.Name || collector.Kubeconfig != state.cluster.Kubeconfig || collector.OutputDir != state.diagnosticsDir || collector.Executor != executor || collector.Redactor != state.redactor {
		t.Fatalf("chart diagnostics lost exact cluster/evidence/redaction wiring: %+v", collector)
	}
	diagnostics, cleanup := scenarioHooks()
	if len(diagnostics) != 1 || diagnostics[0].Name != "shared-cluster-evidence" || len(cleanup) != 2 || cleanup[0].Name != "stop-port-forwards" || cleanup[1].Name != "delete-kind-cluster" {
		t.Fatalf("failure/cleanup hooks changed: diagnostics=%+v cleanup=%+v", diagnostics, cleanup)
	}
}
