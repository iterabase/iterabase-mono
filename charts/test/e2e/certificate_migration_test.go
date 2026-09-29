package e2e_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/kube"
)

func certificateMigrationScenario() sharede2e.Definition {
	diagnostics, cleanup := scenarioHooks()
	return sharede2e.Define(sharede2e.Scenario[*chartState]{
		Metadata: chartScenarioMetadata(
			"certificate-ownership-migration",
			"Migrates certificate substrate ownership from released platform 0.2.2 to the same-version companion release, reconciles current intent, and proves the inverse rollback handoff.",
			"test-e2e-certificate-migration", 35,
			[]string{"HOR-416", "HOR-475"}, []string{"iterabase-platform-chart"},
		),
		NewState: newChartState,
		Stages: []sharede2e.Stage[*chartState]{
			{Name: "create-kind", Run: createKindStage},
			{Name: "import-runtime-images", DependsOn: []string{"create-kind"}, Run: importRuntimeImagesStage},
			{Name: "install-released-owner", DependsOn: []string{"import-runtime-images"}, Run: installReleasedCertificateOwnerStage},
			{Name: "retire-bundled-substrate", DependsOn: []string{"install-released-owner"}, Run: retireBundledSubstrateStage},
			{Name: "transfer-crds-to-companion", DependsOn: []string{"retire-bundled-substrate"}, Run: transferCRDsToCompanionStage},
			{Name: "install-companion-owner", DependsOn: []string{"transfer-crds-to-companion"}, Run: installCompanionOwnerStage},
			{Name: "reconcile-current-platform", DependsOn: []string{"install-companion-owner"}, Run: reconcileCurrentPlatformStage},
			{Name: "inverse-rollback-handoff", DependsOn: []string{"reconcile-current-platform"}, Run: inverseCertificateRollbackStage},
		},
		Diagnostics: diagnostics,
		Cleanup:     cleanup,
	})
}

func certificateMigrationValues() map[string]any {
	return map[string]any{
		"redis":             map[string]any{"enabled": false},
		"minio":             map[string]any{"enabled": false},
		"inference-gateway": map[string]any{"enabled": false},
		"control-plane":     map[string]any{"enabled": false, "toolRunner": map[string]any{"enabled": false}},
		"ingress-nginx":     map[string]any{"enabled": false},
		"metallb":           map[string]any{"enabled": false},
		"metallb-config":    map[string]any{"enabled": false},
		"cert-issuers":      map[string]any{"enabled": false},
		"external-dns":      map[string]any{"enabled": false},
		"reloader":          map[string]any{"enabled": false},
		"observability":     map[string]any{"enabled": false},
	}
}

func installReleasedCertificateOwnerStage(t *testing.T, state *chartState) {
	t.Helper()
	values := state.writeValues(t, "certificate-migration", certificateMigrationValues())
	archive := os.Getenv("ITERABASE_E2E_CERTIFICATE_MIGRATION_ARCHIVE")
	if archive == "" {
		t.Fatal("composed runtime is missing the certificate migration archive")
	}
	args := []string{
		"install", testRelease, archive,
		"--namespace", testNamespace, "--create-namespace", "--kubeconfig", state.cluster.Kubeconfig,
		"--wait", "--timeout", "8m", "--values", values,
	}
	state.process(t, 10*time.Minute, "helm", args...)
	assertCertificateSubstrateOwner(t, state, testRelease)
}

func retireBundledSubstrateStage(t *testing.T, state *chartState) {
	t.Helper()
	helmUpgradeCurrentPlatform(t, state)
	if _, err := state.kubectlOutput(30*time.Second, "get", "deployment/"+testRelease+"-cert-manager", "-n", testNamespace); err == nil {
		t.Fatal("platform upgrade retained its old cert-manager Deployment")
	}
	state.kubectl(t, 3*time.Minute, "wait", "--for=condition=Established", "crd/certificates.cert-manager.io", "--timeout=2m")
}

func transferCRDsToCompanionStage(t *testing.T, state *chartState) {
	transferCertificateCRDs(t, state, testRelease+"-cert-manager")
}

func installCompanionOwnerStage(t *testing.T, state *chartState) {
	t.Helper()
	args := []string{"install", testRelease + "-cert-manager"}
	args = append(args, helmChartArgs(state.substrate)...)
	args = append(args, "--namespace", testNamespace, "--kubeconfig", state.cluster.Kubeconfig, "--wait", "--timeout", "8m")
	state.process(t, 10*time.Minute, "helm", args...)
	assertCertificateSubstrateOwner(t, state, testRelease+"-cert-manager")
}

func reconcileCurrentPlatformStage(t *testing.T, state *chartState) {
	helmUpgradeCurrentPlatform(t, state)
	assertCertificateSubstrateOwner(t, state, testRelease+"-cert-manager")
}

func inverseCertificateRollbackStage(t *testing.T, state *chartState) {
	t.Helper()
	state.process(t, 7*time.Minute, "helm", "uninstall", testRelease+"-cert-manager", "--namespace", testNamespace,
		"--kubeconfig", state.cluster.Kubeconfig, "--wait", "--timeout", "5m")
	transferCertificateCRDs(t, state, testRelease)
	state.process(t, 10*time.Minute, "helm", "rollback", testRelease, "1", "--namespace", testNamespace,
		"--kubeconfig", state.cluster.Kubeconfig, "--wait", "--timeout", "8m")
	assertCertificateSubstrateOwner(t, state, testRelease)
}

func helmUpgradeCurrentPlatform(t *testing.T, state *chartState) {
	t.Helper()
	args := []string{"upgrade", testRelease}
	args = append(args, helmChartArgs(state.platform)...)
	args = append(args, "--namespace", testNamespace, "--kubeconfig", state.cluster.Kubeconfig,
		"--wait", "--timeout", "8m", "--values", state.writeValues(t, "certificate-migration-current", certificateMigrationValues()))
	state.process(t, 10*time.Minute, "helm", args...)
}

func helmChartArgs(chart kube.Chart) []string {
	if chart.LocalPath != "" {
		return []string{chart.LocalPath}
	}
	return []string{chart.Reference, "--version", chart.Version}
}

func transferCertificateCRDs(t *testing.T, state *chartState, owner string) {
	t.Helper()
	out := state.kubectl(t, 30*time.Second, "get", "crd", "-l", "app.kubernetes.io/name=cert-manager", "-o", "name")
	crds := strings.Fields(out)
	if len(crds) < 6 {
		t.Fatalf("expected at least six retained cert-manager CRDs, found %d: %s", len(crds), out)
	}
	args := []string{"annotate", "--overwrite"}
	args = append(args, crds...)
	args = append(args, "meta.helm.sh/release-name="+owner, "meta.helm.sh/release-namespace="+testNamespace)
	state.kubectl(t, 2*time.Minute, args...)
}

func assertCertificateSubstrateOwner(t *testing.T, state *chartState, owner string) {
	t.Helper()
	state.kubectl(t, 4*time.Minute, "rollout", "status", "deployment/"+testRelease+"-cert-manager", "-n", testNamespace, "--timeout=3m")
	state.kubectl(t, 4*time.Minute, "rollout", "status", "deployment/"+testRelease+"-cert-manager-webhook", "-n", testNamespace, "--timeout=3m")
	state.kubectl(t, 4*time.Minute, "rollout", "status", "daemonset/cert-manager-csi-driver", "-n", testNamespace, "--timeout=3m")
	for _, resource := range []string{
		"deployment/" + testRelease + "-cert-manager",
		"clusterrole/" + testRelease + "-cert-manager-cainjector",
		"validatingwebhookconfiguration/" + testRelease + "-cert-manager-webhook",
		"csidriver/csi.cert-manager.io",
	} {
		actual := state.kubectl(t, 30*time.Second, "get", resource, "-n", testNamespace,
			"-o", "jsonpath={.metadata.annotations.meta\\.helm\\.sh/release-name}")
		if actual != owner {
			t.Fatalf("%s owner=%q want=%q", resource, actual, owner)
		}
	}
	state.kubectl(t, 30*time.Second, "get", "crd", "certificates.cert-manager.io")
	t.Logf("certificate substrate owner %s is Ready", fmt.Sprintf("%q", owner))
}
