package e2e_test

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/httpx"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/kube"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
	"gopkg.in/yaml.v3"
)

const (
	platformPredecessorName         = "supported-platform-predecessor"
	substratePredecessorName        = "supported-substrate-predecessor"
	metalLBPlatformPredecessorName  = "metallb-platform-predecessor"
	metalLBSubstratePredecessorName = "metallb-substrate-predecessor"
	transitionFieldManager          = "iterabase-chart-e2e"
	transitionMarker                = "hor-475-persisted-state"
	singleNodeGatewayGenerationKey  = "iterabase.com/e2e-gateway-generation"
	singleNodeGatewayPredecessor    = "predecessor"
	singleNodeGatewayCurrent        = "corrected-current"
)

var operatorCRDs = []string{
	"alertmanagers.monitoring.coreos.com",
	"podmonitors.monitoring.coreos.com",
	"prometheuses.monitoring.coreos.com",
	"prometheusrules.monitoring.coreos.com",
	"servicemonitors.monitoring.coreos.com",
}

type transitionBaseline struct {
	Name       string
	Chart      string
	Repository string
	Version    string
	Checksum   string
	Archive    string
}

type bundledCRDHeader struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

type bundledCRD struct {
	header   bundledCRDHeader
	manifest string
}

type lifecycleSnapshot struct {
	Secrets                map[string]string
	PVCs                   map[string]string
	Pods                   map[string]string
	ArtifactProvisionerJob artifactProvisionerJobIdentity
}

type artifactProvisionerJobIdentity struct {
	Name string
	UID  string
}

type helmHistoryEntry struct {
	Revision int    `json:"revision"`
	Status   string `json:"status"`
	Chart    string `json:"chart"`
}

func nMinusOneUpgradeScenario() sharede2e.Definition {
	diagnostics, cleanup := scenarioHooks()
	return sharede2e.Define(sharede2e.Scenario[*chartState]{
		Metadata: transitionScenarioMetadata(
			"n-minus-one-upgrade",
			"Upgrades the checksum-pinned supported predecessor to the exact current chart pair and proves schema ownership, persistent state, immutable Secrets, PVCs, Jobs, hooks, and rollout health.",
			"test-e2e-upgrade", 40,
			[]string{"HOR-415", "HOR-418", "HOR-475", "HOR-530"},
			[]string{"control-plane-chart", "inference-gateway-chart", "iterabase-platform-chart"},
		),
		NewState: newChartState,
		Stages: []sharede2e.Stage[*chartState]{
			{Name: "create-kind", Run: createKindStage},
			{Name: "import-runtime-images", DependsOn: []string{"create-kind"}, Run: importRuntimeImagesStage},
			{Name: "resolve-supported-predecessor", DependsOn: []string{"import-runtime-images"}, Run: resolveTransitionBaselinesStage},
			{Name: "install-predecessor-substrate", DependsOn: []string{"resolve-supported-predecessor"}, Run: installPredecessorSubstrateStage},
			{Name: "install-predecessor-platform", DependsOn: []string{"install-predecessor-substrate"}, Run: installPredecessorLifecyclePlatformStage},
			{Name: "seed-persisted-state", DependsOn: []string{"install-predecessor-platform"}, Run: seedPersistedStateStage},
			{Name: "capture-predecessor-state", DependsOn: []string{"seed-persisted-state"}, Run: capturePredecessorStateStage},
			{Name: "upgrade-current-pair", DependsOn: []string{"capture-predecessor-state"}, Run: upgradeCurrentLifecyclePairStage},
			{Name: "assert-upgrade-contract", DependsOn: []string{"upgrade-current-pair"}, Run: assertUpgradeContractStage},
		},
		Diagnostics: diagnostics,
		Cleanup:     cleanup,
	})
}

func featureEnableUpgradeScenario() sharede2e.Definition {
	diagnostics, cleanup := scenarioHooks()
	return sharede2e.Define(sharede2e.Scenario[*chartState]{
		Metadata: transitionScenarioMetadata(
			"feature-enable-upgrade",
			"Starts from the supported internal-TLS predecessor without observability, pre-applies exact current operator CRDs, then enables the verified-HTTPS observability composition.",
			"test-e2e-feature-enable", 50,
			[]string{"HOR-415", "HOR-418", "HOR-420", "HOR-475"},
			[]string{"control-plane-chart", "inference-gateway-chart", "iterabase-platform-chart"},
		),
		NewState: newChartState,
		Stages: []sharede2e.Stage[*chartState]{
			{Name: "create-kind", Run: createKindStage},
			{Name: "import-runtime-images", DependsOn: []string{"create-kind"}, Run: importRuntimeImagesStage},
			{Name: "resolve-supported-predecessor", DependsOn: []string{"import-runtime-images"}, Run: resolveTransitionBaselinesStage},
			{Name: "install-predecessor-substrate", DependsOn: []string{"resolve-supported-predecessor"}, Run: installPredecessorSubstrateStage},
			{Name: "install-predecessor-with-feature-disabled", DependsOn: []string{"install-predecessor-substrate"}, Run: installPredecessorFeatureDisabledStage},
			{Name: "assert-operator-crds-absent", DependsOn: []string{"install-predecessor-with-feature-disabled"}, Run: assertOperatorCRDsAbsentStage},
			{Name: "preapply-current-operator-crds", DependsOn: []string{"assert-operator-crds-absent"}, Run: preapplyCurrentOperatorCRDsStage},
			{Name: "enable-observability-tls", DependsOn: []string{"preapply-current-operator-crds"}, Run: enableObservabilityTLSStage},
			{Name: "assert-feature-stack-readiness", DependsOn: []string{"enable-observability-tls"}, Run: assertFeatureStackReadinessStage},
			{Name: "assert-feature-identities", DependsOn: []string{"assert-feature-stack-readiness"}, Run: assertObservabilityIdentitiesStage},
			{Name: "assert-feature-endpoint-separation", DependsOn: []string{"assert-feature-stack-readiness"}, Run: assertEndpointSeparationStage},
			{Name: "assert-feature-verified-stack", DependsOn: []string{"assert-feature-identities"}, Run: assertVerifiedStackHTTPSStage},
			{Name: "assert-feature-client-paths", DependsOn: []string{"assert-feature-verified-stack", "assert-feature-endpoint-separation"}, Run: assertFeatureEnableClientPathsStage},
		},
		Diagnostics: diagnostics,
		Cleanup:     cleanup,
	})
}

func singleNodeObservabilityIngressRecoveryScenario() sharede2e.Definition {
	diagnostics, cleanup := scenarioHooks()
	return sharede2e.Define(sharede2e.Scenario[*chartState]{
		Metadata: transitionScenarioMetadata(
			"single-node-observability-ingress-recovery",
			"Upgrades the exact 0.3.12 single-node observability baseline after proving an explicit RollingUpdate override bypasses migration, injects a failure before admission post-hooks, then proves fail-closed reapply, explicit rollback, and forward recovery.",
			"test-e2e-observability-ingress-recovery", 60,
			[]string{"HOR-414", "HOR-506"},
			[]string{"iterabase-platform-chart"},
		),
		NewState: newChartState,
		Stages: []sharede2e.Stage[*chartState]{
			{Name: "create-kind", Run: createKindStage},
			{Name: "import-runtime-images", DependsOn: []string{"create-kind"}, Run: importRuntimeImagesStage},
			{Name: "resolve-supported-predecessor", DependsOn: []string{"import-runtime-images"}, Run: resolveTransitionBaselinesStage},
			{Name: "install-predecessor-substrate", DependsOn: []string{"resolve-supported-predecessor"}, Run: installPredecessorSubstrateStage},
			{Name: "install-predecessor-observability", DependsOn: []string{"install-predecessor-substrate"}, Run: installPredecessorSingleNodeObservabilityStage},
			{Name: "assert-rolling-update-override-bypasses-recovery", DependsOn: []string{"install-predecessor-observability"}, Run: assertRollingUpdateOverrideBypassesRecoveryStage},
			{Name: "preapply-current-operator-crds", DependsOn: []string{"assert-rolling-update-override-bypasses-recovery"}, Run: preapplyCurrentOperatorCRDsStage},
			{Name: "inject-failed-current-upgrade", DependsOn: []string{"preapply-current-operator-crds"}, Run: injectFailedSingleNodeIngressUpgradeStage},
			{Name: "assert-interrupted-boundary", DependsOn: []string{"inject-failed-current-upgrade"}, Run: assertInterruptedSingleNodeIngressUpgradeStage},
			{Name: "reapply-current-intent", DependsOn: []string{"assert-interrupted-boundary"}, Run: reapplySingleNodeIngressUpgradeStage},
			{Name: "assert-recovered-upgrade", DependsOn: []string{"reapply-current-intent"}, Run: assertRecoveredSingleNodeIngressUpgradeStage},
			{Name: "prepare-explicit-legacy-rollback", DependsOn: []string{"assert-recovered-upgrade"}, Run: prepareSingleNodeLegacyRollbackStage},
			{Name: "rollback-predecessor-pair", DependsOn: []string{"prepare-explicit-legacy-rollback"}, Run: rollbackSingleNodePredecessorStage},
			{Name: "assert-rollback-boundary", DependsOn: []string{"rollback-predecessor-pair"}, Run: assertSingleNodeRollbackStage},
			{Name: "forward-recovery", DependsOn: []string{"assert-rollback-boundary"}, Run: forwardSingleNodeRecoveryStage},
			{Name: "assert-forward-recovery", DependsOn: []string{"forward-recovery"}, Run: assertForwardSingleNodeRecoveryStage},
		},
		Diagnostics: diagnostics,
		Cleanup:     cleanup,
	})
}

func reapplyRollbackRecoveryScenario() sharede2e.Definition {
	diagnostics, cleanup := scenarioHooks()
	return sharede2e.Define(sharede2e.Scenario[*chartState]{
		Metadata: transitionScenarioMetadata(
			"reapply-rollback-recovery",
			"Reapplies current intent with --wait without rolling stable workloads or replacing the completed Helm-owned artifact provisioner, then exercises the supported inverse rollback and forward recovery while retaining state.",
			"test-e2e-reapply-rollback", 45,
			[]string{"HOR-415", "HOR-418", "HOR-475", "HOR-530"},
			[]string{"control-plane-chart", "inference-gateway-chart", "iterabase-platform-chart"},
		),
		NewState: newChartState,
		Stages: []sharede2e.Stage[*chartState]{
			{Name: "create-kind", Run: createKindStage},
			{Name: "import-runtime-images", DependsOn: []string{"create-kind"}, Run: importRuntimeImagesStage},
			{Name: "resolve-supported-predecessor", DependsOn: []string{"import-runtime-images"}, Run: resolveTransitionBaselinesStage},
			{Name: "install-predecessor-substrate", DependsOn: []string{"resolve-supported-predecessor"}, Run: installPredecessorSubstrateStage},
			{Name: "install-predecessor-platform", DependsOn: []string{"install-predecessor-substrate"}, Run: installPredecessorLifecyclePlatformStage},
			{Name: "seed-persisted-state", DependsOn: []string{"install-predecessor-platform"}, Run: seedPersistedStateStage},
			{Name: "upgrade-current-pair", DependsOn: []string{"seed-persisted-state"}, Run: upgradeCurrentLifecyclePairStage},
			{Name: "capture-current-state", DependsOn: []string{"upgrade-current-pair"}, Run: captureCurrentStateStage},
			{Name: "reapply-current-intent", DependsOn: []string{"capture-current-state"}, Run: reapplyCurrentPairStage},
			{Name: "assert-idempotent-reapply", DependsOn: []string{"reapply-current-intent"}, Run: assertIdempotentReapplyStage},
			{Name: "inverse-rollback", DependsOn: []string{"assert-idempotent-reapply"}, Run: inverseRollbackStage},
			{Name: "assert-rollback-boundary", DependsOn: []string{"inverse-rollback"}, Run: assertRollbackBoundaryStage},
			{Name: "forward-recovery", DependsOn: []string{"assert-rollback-boundary"}, Run: forwardRecoveryStage},
			{Name: "assert-forward-recovery", DependsOn: []string{"forward-recovery"}, Run: assertForwardRecoveryStage},
		},
		Diagnostics: diagnostics,
		Cleanup:     cleanup,
	})
}

func transitionBaselineFixturePath() string {
	if path := os.Getenv("ITERABASE_E2E_TRANSITION_BASELINES"); path != "" {
		return path
	}
	return "transition-baselines.json"
}

func loadTransitionBaselineFixture(t *testing.T) sharede2e.Fixture {
	t.Helper()
	data, err := os.ReadFile(transitionBaselineFixturePath())
	if err != nil {
		t.Fatalf("read transition baseline fixture: %v", err)
	}
	fixture, err := decodeTransitionBaselineFixture(data)
	if err != nil {
		t.Fatalf("decode transition baseline fixture: %v", err)
	}
	return fixture
}

func decodeTransitionBaselineFixture(data []byte) (sharede2e.Fixture, error) {
	var fixture sharede2e.Fixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		return sharede2e.Fixture{}, err
	}
	if fixture.Mode != sharede2e.FixturePublished {
		return sharede2e.Fixture{}, fmt.Errorf("transition baselines must use published mode")
	}
	if err := fixture.Validate(); err != nil {
		return sharede2e.Fixture{}, err
	}
	if len(fixture.Inputs) != 4 {
		return sharede2e.Fixture{}, fmt.Errorf("transition baseline fixture must contain exactly four charts (supported + MetalLB predecessor pairs)")
	}
	expected := map[string]string{
		platformPredecessorName:         "iterabase-platform",
		substratePredecessorName:        "cert-manager-substrate",
		metalLBPlatformPredecessorName:  "iterabase-platform",
		metalLBSubstratePredecessorName: "cert-manager-substrate",
	}
	versions := make(map[string]string)
	for _, input := range fixture.Inputs {
		chart, _, version, err := parsePublishedChartReference(input.Reference)
		if err != nil {
			return sharede2e.Fixture{}, fmt.Errorf("%s: %w", input.Name, err)
		}
		if input.Kind != "published-chart" || expected[input.Name] != chart {
			return sharede2e.Fixture{}, fmt.Errorf("unexpected transition baseline %s=%s", input.Name, input.Reference)
		}
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(input.Checksum) {
			return sharede2e.Fixture{}, fmt.Errorf("transition baseline %s requires a canonical archive checksum", input.Name)
		}
		versions[input.Name] = version
	}
	pairs := [][2]string{
		{platformPredecessorName, substratePredecessorName},
		{metalLBPlatformPredecessorName, metalLBSubstratePredecessorName},
	}
	for _, pair := range pairs {
		if versions[pair[0]] != versions[pair[1]] {
			return sharede2e.Fixture{}, fmt.Errorf("platform and substrate predecessors %s/%s must use the same version", pair[0], pair[1])
		}
	}
	return fixture, nil
}

func parsePublishedChartReference(reference string) (chart, repository, version string, err error) {
	separator := strings.LastIndexByte(reference, ':')
	if separator <= len("oci://") || separator == len(reference)-1 {
		return "", "", "", fmt.Errorf("published chart reference has no exact version: %q", reference)
	}
	repository, version = reference[:separator], reference[separator+1:]
	chart = filepath.Base(repository)
	if chart == "." || chart == "/" || strings.Contains(strings.ToLower(version), "latest") {
		return "", "", "", fmt.Errorf("invalid published chart reference %q", reference)
	}
	return chart, repository, version, nil
}

func transitionBaselines(fixture sharede2e.Fixture) (map[string]transitionBaseline, error) {
	baselines := make(map[string]transitionBaseline, len(fixture.Inputs))
	for _, input := range fixture.Inputs {
		chart, repository, version, err := parsePublishedChartReference(input.Reference)
		if err != nil {
			return nil, err
		}
		baselines[input.Name] = transitionBaseline{
			Name: input.Name, Chart: chart, Repository: repository, Version: version, Checksum: input.Checksum,
		}
	}
	return baselines, nil
}

func resolveTransitionBaselinesStage(t *testing.T, state *chartState) {
	t.Helper()
	baselines, err := transitionBaselines(loadTransitionBaselineFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentPairNewerThanPredecessor(t, state, baselines)
	directory := filepath.Join(state.outputDir, "transition-baselines")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create transition baseline directory: %v", err)
	}
	environment := map[string]string{
		platformPredecessorName:         "ITERABASE_E2E_PREDECESSOR_PLATFORM_ARCHIVE",
		substratePredecessorName:        "ITERABASE_E2E_PREDECESSOR_SUBSTRATE_ARCHIVE",
		metalLBPlatformPredecessorName:  "ITERABASE_E2E_METALLB_PREDECESSOR_PLATFORM_ARCHIVE",
		metalLBSubstratePredecessorName: "ITERABASE_E2E_METALLB_PREDECESSOR_SUBSTRATE_ARCHIVE",
	}
	for name, baseline := range baselines {
		archive := os.Getenv(environment[name])
		if archive == "" {
			state.process(t, 4*time.Minute, "helm", "pull", baseline.Repository, "--version", baseline.Version, "--destination", directory)
			archive = filepath.Join(directory, baseline.Chart+"-"+baseline.Version+".tgz")
		}
		archive, err = filepath.Abs(archive)
		if err != nil {
			t.Fatalf("resolve %s archive: %v", name, err)
		}
		if err := verifyArchiveChecksum(archive, baseline.Checksum); err != nil {
			t.Fatalf("verify %s: %v", name, err)
		}
		baseline.Archive = archive
		baselines[name] = baseline
	}
	state.transitionBaselines = baselines
}

func assertCurrentPairNewerThanPredecessor(t *testing.T, state *chartState, baselines map[string]transitionBaseline) {
	t.Helper()
	for _, pair := range []struct {
		name  string
		chart kube.Chart
	}{
		{name: platformPredecessorName, chart: state.platform},
		{name: substratePredecessorName, chart: state.substrate},
	} {
		baseline, ok := baselines[pair.name]
		if !ok {
			t.Fatalf("transition baseline %s is missing", pair.name)
		}
		current := currentChartVersion(t, state, pair.chart)
		if err := newerChartVersionError(current, baseline.Version); err != nil {
			t.Fatalf("%s: %v", pair.name, err)
		}
	}
}

func newerChartVersionError(current, predecessor string) error {
	comparison, err := compareNumericVersions(current, predecessor)
	if err != nil {
		return fmt.Errorf("compare current chart %q with predecessor %q: %w", current, predecessor, err)
	}
	if comparison <= 0 {
		return fmt.Errorf("current chart version %s is not newer than supported predecessor %s", current, predecessor)
	}
	return nil
}

func verifyArchiveChecksum(path, expected string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	actual := hex.EncodeToString(digest[:])
	if actual != expected {
		return fmt.Errorf("archive checksum %s != expected %s", actual, expected)
	}
	return nil
}

func installPredecessorSubstrateStage(t *testing.T, state *chartState) {
	baseline := requireTransitionBaseline(t, state, substratePredecessorName)
	args := []string{"upgrade", "--install", testRelease + "-cert-manager", "--namespace", testNamespace,
		"--create-namespace", "--kubeconfig", state.cluster.Kubeconfig, "--wait", "--timeout", "8m", baseline.Archive}
	state.process(t, 10*time.Minute, "helm", args...)
	assertReleaseChartVersion(t, state, testRelease+"-cert-manager", baseline.Chart, baseline.Version)
}

func installPredecessorLifecyclePlatformStage(t *testing.T, state *chartState) {
	installPredecessorPlatform(t, state, lifecyclePlatformValueFiles(t, state)...)
	assertLifecycleHealth(t, state)
	assertReleaseMechanics(t, state)
}

func installPredecessorFeatureDisabledStage(t *testing.T, state *chartState) {
	installPredecessorPlatform(t, state,
		filepathFromCharts(state, "values-tls.yaml"),
		state.writeValues(t, "feature-disabled", runtimePlatformValues(t)),
	)
	assertInternalIdentitiesStage(t, state)
	assertGatewayDependenciesStage(t, state)
	assertControlPlaneVerifiedHTTPSStage(t, state)
}

func installPredecessorPlatform(t *testing.T, state *chartState, valueFiles ...string) {
	t.Helper()
	baseline := requireTransitionBaseline(t, state, platformPredecessorName)
	args := []string{"upgrade", "--install", testRelease, "--namespace", testNamespace,
		"--kubeconfig", state.cluster.Kubeconfig, "--wait", "--timeout", "18m"}
	for _, values := range valueFiles {
		args = append(args, "--values", values)
	}
	args = append(args, baseline.Archive)
	state.process(t, 20*time.Minute, "helm", args...)
	assertReleaseChartVersion(t, state, testRelease, baseline.Chart, baseline.Version)
}

func requireTransitionBaseline(t *testing.T, state *chartState, name string) transitionBaseline {
	t.Helper()
	baseline, ok := state.transitionBaselines[name]
	if !ok || baseline.Archive == "" {
		t.Fatalf("transition baseline %s is unresolved", name)
	}
	return baseline
}

func lifecyclePlatformValueFiles(t *testing.T, state *chartState) []string {
	t.Helper()
	values := runtimePlatformValues(t)
	values["minio"] = map[string]any{
		"enabled":         true,
		"artifactService": map[string]any{"enabled": true, "bucket": "iterabase-artifacts"},
	}
	controlPlane := values["control-plane"].(map[string]any)
	controlPlane["artifact"] = map[string]any{"enabled": true}
	return []string{state.writeValues(t, "lifecycle-runtime", values)}
}

func upgradeCurrentLifecyclePairStage(t *testing.T, state *chartState) {
	state.installSubstrate(t)
	state.installPlatform(t, 18*time.Minute, lifecyclePlatformValueFiles(t, state)...)
	assertCandidateImages(t, state)
}

func enableObservabilityTLSStage(t *testing.T, state *chartState) {
	state.installSubstrate(t)
	state.installPlatform(t, 22*time.Minute,
		filepathFromCharts(state, "values-observability.yaml"),
		filepathFromCharts(state, "values-tls.yaml"),
		state.writeValues(t, "feature-enabled", runtimePlatformValues(t)),
	)
	assertCandidateImages(t, state)
}

func singleNodeObservabilityValueFiles(t *testing.T, state *chartState, generation string, enablePrivate, injectFailure bool) []string {
	t.Helper()
	values := runtimePlatformValues(t)
	// This transition isolates the chart-owned observability and ingress
	// lifecycle. Runtime services have their own exact N-1 journeys and are not
	// needed to reproduce either single-node failure boundary here.
	values["control-plane"] = map[string]any{"enabled": false}
	values["inference-gateway"] = map[string]any{"enabled": false}
	values["redis"] = map[string]any{"enabled": false}
	values["minio"] = map[string]any{"enabled": false}
	values["ingress-nginx"] = map[string]any{
		"enabled": true,
		"controller": map[string]any{
			"service": map[string]any{"type": "ClusterIP"},
		},
	}
	observability := map[string]any{
		"loki": map[string]any{
			"gateway": map[string]any{
				"podAnnotations": map[string]any{singleNodeGatewayGenerationKey: generation},
			},
		},
	}
	if injectFailure {
		// Keep both admission controllers available while an unrelated workload
		// deliberately cannot become Ready before the bounded Helm timeout. With
		// --wait, post-upgrade admission patch hooks therefore do not run, matching
		// the observed failed-gateway boundary without manufacturing a webhook
		// endpoint outage outside HOR-506's recovery contract.
		observability["kube-prometheus-stack"] = map[string]any{
			"prometheusOperator": map[string]any{
				"readinessProbe": map[string]any{"initialDelaySeconds": 600},
			},
		}
	}
	values["observability"] = observability
	if enablePrivate {
		values["internal-ingress-nginx"] = map[string]any{
			"enabled": true,
			"controller": map[string]any{
				"service": map[string]any{"type": "ClusterIP"},
			},
		}
	}
	files := []string{
		filepathFromCharts(state, "values-observability.yaml"),
		filepathFromCharts(state, "values-tls.yaml"),
	}
	if enablePrivate {
		files = append(files, filepath.Join(state.chartsRoot, "test", "e2e", "values-single-node-observability-ingress.yaml"))
	}
	return append(files, state.writeValues(t, "single-node-observability-"+generation, values))
}

func installPredecessorSingleNodeObservabilityStage(t *testing.T, state *chartState) {
	t.Helper()
	baseline := requireTransitionBaseline(t, state, platformPredecessorName)
	args := []string{"upgrade", "--install", testRelease, "--namespace", testNamespace,
		"--kubeconfig", state.cluster.Kubeconfig, "--wait", "--timeout", "22m"}
	for _, values := range singleNodeObservabilityValueFiles(t, state, singleNodeGatewayPredecessor, false, false) {
		args = append(args, "--values", values)
	}
	args = append(args, baseline.Archive)
	state.process(t, 24*time.Minute, "helm", args...)
	assertReleaseChartVersion(t, state, testRelease, baseline.Chart, baseline.Version)
	assertSingleNodeGateway(t, state, "RollingUpdate", singleNodeGatewayPredecessor)
	assertIngressAdmissionCA(t, state, "ingress-nginx")
}

func assertRollingUpdateOverrideBypassesRecoveryStage(t *testing.T, state *chartState) {
	t.Helper()
	values := singleNodeObservabilityValueFiles(t, state, singleNodeGatewayPredecessor, false, false)
	values = append(values, state.writeValues(t, "rolling-update-override", map[string]any{
		"observability": map[string]any{
			"loki": map[string]any{
				"gateway": map[string]any{
					"deploymentStrategy": map[string]any{
						"type": "RollingUpdate",
						"rollingUpdate": map[string]any{
							"maxSurge":       1,
							"maxUnavailable": 0,
						},
					},
				},
			},
		},
	}))
	args := []string{"upgrade", testRelease}
	args = append(args, helmChartArgs(state.platform)...)
	args = append(args, "--namespace", testNamespace, "--kubeconfig", state.cluster.Kubeconfig,
		"--dry-run=server", "--hide-secret")
	for _, valuesFile := range values {
		args = append(args, "--values", valuesFile)
	}
	out := state.process(t, 4*time.Minute, "helm", args...)
	hookName := testRelease + "-loki-gateway-rollout-recovery"
	if strings.Contains(out, hookName) {
		t.Fatalf("explicit RollingUpdate override rendered migration hook %q", hookName)
	}
	assertSingleNodeGateway(t, state, "RollingUpdate", singleNodeGatewayPredecessor)
}

func injectFailedSingleNodeIngressUpgradeStage(t *testing.T, state *chartState) {
	t.Helper()
	state.installSubstrate(t)
	out, err := state.client.HelmUpgrade(state.ctx, kube.HelmOptions{
		Release: testRelease, Namespace: testNamespace, Chart: state.platform,
		ValueFiles: singleNodeObservabilityValueFiles(t, state, singleNodeGatewayCurrent, true, true),
		Wait:       true, Timeout: 2 * time.Minute,
	})
	if err == nil {
		t.Fatalf("injected pre-post-hook upgrade unexpectedly succeeded: %s", stateSafeBody([]byte(out)))
	}
	t.Logf("injected upgrade failed as intended: %s", stateSafeBody([]byte(out)))
	entry, historyErr := currentHelmHistoryEntry([]byte(state.process(t, 60*time.Second, "helm", "history", testRelease,
		"--namespace", testNamespace, "--kubeconfig", state.cluster.Kubeconfig, "--output", "json")))
	if historyErr != nil {
		t.Fatal(historyErr)
	}
	if entry.Revision != 2 || entry.Status != "failed" {
		t.Fatalf("interrupted platform revision=%d status=%q want revision=2 failed", entry.Revision, entry.Status)
	}
}

func assertInterruptedSingleNodeIngressUpgradeStage(t *testing.T, state *chartState) {
	t.Helper()
	assertSingleNodeGateway(t, state, "Recreate", singleNodeGatewayCurrent)
	admissionName := testRelease + "-internal-ingress-nginx-admission"
	caBundle := state.kubectl(t, 30*time.Second, "get", "validatingwebhookconfiguration/"+admissionName,
		"-o", "jsonpath={.webhooks[0].clientConfig.caBundle}")
	if caBundle != "" {
		t.Fatalf("interrupted internal ingress webhook CA was already patched: %q", caBundle)
	}
	secretCA := state.kubectl(t, 30*time.Second, "get", "secret/"+admissionName, "-n", testNamespace,
		"-o", "jsonpath={.data.ca}")
	if secretCA == "" {
		t.Fatal("interrupted upgrade did not retain the generated internal admission serving Secret")
	}
	if got := state.kubectl(t, 30*time.Second, "get", "validatingwebhookconfiguration/"+admissionName,
		"-o", "jsonpath={.webhooks[0].failurePolicy}"); got != "Fail" {
		t.Fatalf("interrupted internal webhook failurePolicy=%q want Fail", got)
	}
}

func reapplySingleNodeIngressUpgradeStage(t *testing.T, state *chartState) {
	state.installSubstrate(t)
	state.installPlatform(t, 25*time.Minute,
		singleNodeObservabilityValueFiles(t, state, singleNodeGatewayCurrent, true, false)...)
}

func assertRecoveredSingleNodeIngressUpgradeStage(t *testing.T, state *chartState) {
	t.Helper()
	assertReleaseRevision(t, state, testRelease, 3)
	assertReleaseRevision(t, state, testRelease+"-cert-manager", 3)
	assertSingleNodeGateway(t, state, "Recreate", singleNodeGatewayCurrent)
	assertSingleNodeObservabilityReadiness(t, state)
	assertIngressAdmissionCA(t, state, "ingress-nginx")
	assertIngressAdmissionCA(t, state, "internal-ingress-nginx")
	assertIngressAdmissionClasses(t, state)
	assertPrivateObservabilityIngresses(t, state, true)
	hooks := state.process(t, 60*time.Second, "helm", "get", "hooks", testRelease, "--namespace", testNamespace,
		"--kubeconfig", state.cluster.Kubeconfig)
	if !strings.Contains(hooks, testRelease+"-internal-ingress-nginx-admission-recovery") ||
		!strings.Contains(hooks, "helm.sh/hook-weight: \"1\"") {
		t.Fatalf("successful reapply did not record the pre-upgrade admission recovery hook: %s", stateSafeBody([]byte(hooks)))
	}
}

func prepareSingleNodeLegacyRollbackStage(t *testing.T, state *chartState) {
	t.Helper()
	// Revision 1 predates DES-HOR-506-01 and stores RollingUpdate plus hard
	// anti-affinity. Scale the one-replica gateway to zero before Helm restores
	// that legacy manifest, making the inverse boundary explicit and bounded.
	gateway := testRelease + "-loki-gateway"
	state.kubectl(t, 30*time.Second, "scale", "deployment/"+gateway, "-n", testNamespace, "--replicas=0")
	state.kubectl(t, 3*time.Minute, "wait", "--for=delete", "pod", "-n", testNamespace,
		"-l", "app.kubernetes.io/name=loki,app.kubernetes.io/component=gateway", "--timeout=2m")
}

func rollbackSingleNodePredecessorStage(t *testing.T, state *chartState) {
	rollbackRelease(t, state, testRelease, 1)
	rollbackRelease(t, state, testRelease+"-cert-manager", 1)
}

func assertSingleNodeRollbackStage(t *testing.T, state *chartState) {
	t.Helper()
	platform := requireTransitionBaseline(t, state, platformPredecessorName)
	substrate := requireTransitionBaseline(t, state, substratePredecessorName)
	assertRollbackReleaseHistory(t, state, testRelease, platform.Chart, platform.Version, 4)
	assertRollbackReleaseHistory(t, state, testRelease+"-cert-manager", substrate.Chart, substrate.Version, 4)
	assertSingleNodeGateway(t, state, "RollingUpdate", singleNodeGatewayPredecessor)
	assertIngressAdmissionCA(t, state, "ingress-nginx")
	assertPrivateObservabilityIngresses(t, state, false)
	if got := state.kubectl(t, 30*time.Second, "get", "validatingwebhookconfiguration/"+testRelease+"-internal-ingress-nginx-admission",
		"--ignore-not-found", "-o", "name"); got != "" {
		t.Fatalf("legacy rollback retained internal admission webhook %q", got)
	}
}

func forwardSingleNodeRecoveryStage(t *testing.T, state *chartState) {
	state.installSubstrate(t)
	state.installPlatform(t, 25*time.Minute,
		singleNodeObservabilityValueFiles(t, state, singleNodeGatewayCurrent, true, false)...)
}

func assertForwardSingleNodeRecoveryStage(t *testing.T, state *chartState) {
	t.Helper()
	assertReleaseRevision(t, state, testRelease, 5)
	assertReleaseRevision(t, state, testRelease+"-cert-manager", 5)
	assertSingleNodeGateway(t, state, "Recreate", singleNodeGatewayCurrent)
	assertIngressAdmissionCA(t, state, "ingress-nginx")
	assertIngressAdmissionCA(t, state, "internal-ingress-nginx")
	assertIngressAdmissionClasses(t, state)
	assertPrivateObservabilityIngresses(t, state, true)
}

func assertSingleNodeObservabilityReadiness(t *testing.T, state *chartState) {
	t.Helper()
	for _, selector := range []string{
		"app.kubernetes.io/name=prometheus",
		"app.kubernetes.io/name=grafana",
		"app.kubernetes.io/name=loki,app.kubernetes.io/component=single-binary",
		"app.kubernetes.io/name=alertmanager",
		"app.kubernetes.io/name=promtail",
		"app.kubernetes.io/name=ingress-nginx,app.kubernetes.io/component=controller",
		"app.kubernetes.io/name=internal-ingress-nginx,app.kubernetes.io/component=controller",
	} {
		state.waitForPods(t, selector, 7*time.Minute)
	}
}

func assertSingleNodeGateway(t *testing.T, state *chartState, strategy, generation string) {
	t.Helper()
	gateway := testRelease + "-loki-gateway"
	state.kubectl(t, 6*time.Minute, "rollout", "status", "deployment/"+gateway, "-n", testNamespace, "--timeout=5m")
	if got := state.kubectl(t, 30*time.Second, "get", "deployment/"+gateway, "-n", testNamespace,
		"-o", "jsonpath={.spec.strategy.type}"); got != strategy {
		t.Fatalf("Loki gateway strategy=%q want %q", got, strategy)
	}
	if got := state.kubectl(t, 30*time.Second, "get", "deployment/"+gateway, "-n", testNamespace,
		"-o", `go-template={{index .spec.template.metadata.annotations "iterabase.com/e2e-gateway-generation"}}`); got != generation {
		t.Fatalf("Loki gateway generation=%q want %q", got, generation)
	}
	if got := state.kubectl(t, 30*time.Second, "get", "deployment/"+gateway, "-n", testNamespace,
		"-o", "jsonpath={.status.readyReplicas}"); got != "1" {
		t.Fatalf("Loki gateway ready replicas=%q want 1", got)
	}
}

func assertIngressAdmissionCA(t *testing.T, state *chartState, plane string) {
	t.Helper()
	admissionName := testRelease + "-" + plane + "-admission"
	secretCA := state.kubectl(t, 30*time.Second, "get", "secret/"+admissionName, "-n", testNamespace,
		"-o", "jsonpath={.data.ca}")
	bundle := state.kubectl(t, 30*time.Second, "get", "validatingwebhookconfiguration/"+admissionName,
		"-o", "jsonpath={.webhooks[0].clientConfig.caBundle}")
	if secretCA == "" || bundle != secretCA {
		t.Fatalf("%s admission CA bundle does not match serving Secret: bundle=%q secret-present=%t", plane, bundle, secretCA != "")
	}
	if got := state.kubectl(t, 30*time.Second, "get", "validatingwebhookconfiguration/"+admissionName,
		"-o", "jsonpath={.webhooks[0].failurePolicy}"); got != "Fail" {
		t.Fatalf("%s admission failurePolicy=%q want Fail", plane, got)
	}
	certificateData := state.kubectl(t, 30*time.Second, "get", "secret/"+admissionName, "-n", testNamespace,
		"-o", "jsonpath={.data.cert}")
	caPEM, err := base64.StdEncoding.DecodeString(secretCA)
	if err != nil {
		t.Fatalf("decode %s admission CA: %v", plane, err)
	}
	certificatePEM, err := base64.StdEncoding.DecodeString(certificateData)
	if err != nil {
		t.Fatalf("decode %s admission certificate: %v", plane, err)
	}
	caBlock, _ := pem.Decode(caPEM)
	certificateBlock, _ := pem.Decode(certificatePEM)
	if caBlock == nil || certificateBlock == nil {
		t.Fatalf("%s admission Secret does not contain PEM CA/certificate", plane)
	}
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("parse %s admission CA: %v", plane, err)
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		t.Fatalf("parse %s admission certificate: %v", plane, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	controllerService := testRelease + "-" + plane + "-controller-admission"
	if _, err := certificate.Verify(x509.VerifyOptions{
		Roots: roots, DNSName: controllerService + "." + testNamespace + ".svc",
	}); err != nil {
		t.Fatalf("%s admission serving certificate does not verify against its Secret CA: %v", plane, err)
	}
}

func assertIngressAdmissionClasses(t *testing.T, state *chartState) {
	t.Helper()
	for _, class := range []string{"nginx", "nginx-internal"} {
		name := strings.ReplaceAll(class, "nginx", "admission")
		manifest := fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: %s-valid
  namespace: %s
spec:
  ingressClassName: %s
  rules:
    - host: %s.recovery.iterabase.local
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: %s-gateway
                port:
                  number: 8080
`, name, testNamespace, class, name, testRelease)
		path := state.writeManifest(t, name+"-valid-ingress.yaml", manifest)
		state.kubectl(t, 30*time.Second, "apply", "--dry-run=server", "-f", path)

		invalid := strings.Replace(manifest, "metadata:\n", "metadata:\n  annotations:\n    nginx.ingress.kubernetes.io/server-snippet: return 200;\n", 1)
		invalid = strings.Replace(invalid, name+"-valid", name+"-invalid", 1)
		path = state.writeManifest(t, name+"-invalid-ingress.yaml", invalid)
		out, err := state.kubectlOutput(30*time.Second, "apply", "--dry-run=server", "-f", path)
		if err == nil {
			t.Fatalf("%s admission accepted disabled server-snippet annotation", class)
		}
		lower := strings.ToLower(out + " " + err.Error())
		if !strings.Contains(lower, "admission webhook") && !strings.Contains(lower, "snippet") {
			t.Fatalf("%s invalid Ingress did not fail through admission validation: %v: %s", class, err, stateSafeBody([]byte(out)))
		}
	}
}

func assertPrivateObservabilityIngresses(t *testing.T, state *chartState, present bool) {
	t.Helper()
	for _, name := range []string{
		testRelease + "-grafana",
		kubePrometheusStackComponentName("prometheus"),
		kubePrometheusStackComponentName("alertmanager"),
		testRelease + "-loki-gateway",
	} {
		got := state.kubectl(t, 30*time.Second, "get", "ingress/"+name, "-n", testNamespace,
			"--ignore-not-found", "-o", "jsonpath={.spec.ingressClassName}")
		if present && got != "nginx-internal" {
			t.Fatalf("private observability Ingress %s class=%q want nginx-internal", name, got)
		}
		if !present && got != "" {
			t.Fatalf("legacy rollback retained private observability Ingress %s with class %q", name, got)
		}
	}
}

func seedPersistedStateStage(t *testing.T, state *chartState) {
	t.Helper()
	state.kubectl(t, 2*time.Minute, "exec", "-n", testNamespace, "statefulset/"+testRelease+"-postgresql", "--",
		"psql", "-U", "controlplane", "-d", "controlplane", "-v", "ON_ERROR_STOP=1", "-c",
		"CREATE TABLE IF NOT EXISTS e2e_chart_transition (marker text PRIMARY KEY); INSERT INTO e2e_chart_transition(marker) VALUES ('"+transitionMarker+"') ON CONFLICT DO NOTHING;")
	state.kubectl(t, 30*time.Second, "exec", "-n", testNamespace, "statefulset/"+testRelease+"-minio", "--",
		"sh", "-c", "printf '%s' '"+transitionMarker+"' > /data/"+transitionMarker)
	assertPersistedState(t, state)
}

func assertPersistedState(t *testing.T, state *chartState) {
	t.Helper()
	postgres := state.kubectl(t, 90*time.Second, "exec", "-n", testNamespace, "statefulset/"+testRelease+"-postgresql", "--",
		"psql", "-At", "-U", "controlplane", "-d", "controlplane", "-c",
		"SELECT marker FROM e2e_chart_transition WHERE marker='"+transitionMarker+"';")
	if postgres != transitionMarker {
		t.Fatalf("PostgreSQL persisted marker=%q want=%q", postgres, transitionMarker)
	}
	minio := state.kubectl(t, 30*time.Second, "exec", "-n", testNamespace, "statefulset/"+testRelease+"-minio", "--",
		"cat", "/data/"+transitionMarker)
	if minio != transitionMarker {
		t.Fatalf("MinIO persisted marker=%q want=%q", minio, transitionMarker)
	}
}

func capturePredecessorStateStage(t *testing.T, state *chartState) {
	state.snapshots["predecessor"] = captureLifecycleSnapshot(t, state)
}

func captureCurrentStateStage(t *testing.T, state *chartState) {
	assertLifecycleHealth(t, state)
	assertPersistedState(t, state)
	snapshot := captureLifecycleSnapshot(t, state)
	snapshot.ArtifactProvisionerJob = currentArtifactProvisionerJobIdentity(t, state)
	state.snapshots["current"] = snapshot
}

func captureLifecycleSnapshot(t *testing.T, state *chartState) lifecycleSnapshot {
	t.Helper()
	snapshot := lifecycleSnapshot{Secrets: map[string]string{}, PVCs: map[string]string{}, Pods: map[string]string{}}
	for _, name := range []string{
		testRelease + "-postgresql", testRelease + "-minio", testRelease + "-minio-artifacts",
		testRelease + "-control-plane-jwt", testRelease + "-gateway-admin",
	} {
		snapshot.Secrets[name] = secretDigest(t, state, name)
	}
	for _, name := range []string{"data-" + testRelease + "-postgresql-0", "data-" + testRelease + "-minio-0"} {
		snapshot.PVCs[name] = state.kubectl(t, 30*time.Second, "get", "pvc/"+name, "-n", testNamespace,
			"-o", "jsonpath={.metadata.uid}/{.spec.volumeName}")
	}
	for _, release := range []string{testRelease, testRelease + "-cert-manager"} {
		releaseSelector := "app.kubernetes.io/instance=" + release
		workloadData := state.kubectl(t, 30*time.Second, "get", "deployments,statefulsets,daemonsets", "-n", testNamespace,
			"-l", releaseSelector, "-o", "json")
		workloads, err := stableWorkloadSelectorsJSON([]byte(workloadData))
		if err != nil {
			t.Fatalf("discover stable workloads for release %s: %v", release, err)
		}
		for _, workload := range workloads {
			identities := strings.Fields(state.kubectl(t, 30*time.Second, "get", "pods", "-n", testNamespace,
				"-l", workload.Selector, "-o", `go-template={{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}={{.metadata.uid}}{{"\n"}}{{end}}{{end}}`))
			if len(identities) == 0 {
				t.Fatalf("%s/%s has no stable pod identity", release, workload.Key)
			}
			for _, identity := range identities {
				name, uid, ok := strings.Cut(identity, "=")
				if !ok || name == "" || uid == "" {
					t.Fatalf("%s/%s returned invalid pod identity %q", release, workload.Key, identity)
				}
				snapshot.Pods[release+"/"+workload.Key+"/"+name] = uid
			}
		}
	}
	return snapshot
}

type stableWorkloadSelector struct {
	Key      string
	Selector string
}

func stableWorkloadSelectorsJSON(workloadData []byte) ([]stableWorkloadSelector, error) {
	var workloads struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Selector struct {
					MatchLabels      map[string]string `json:"matchLabels"`
					MatchExpressions []json.RawMessage `json:"matchExpressions"`
				} `json:"selector"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(workloadData, &workloads); err != nil {
		return nil, fmt.Errorf("decode stable workloads: %w", err)
	}
	if len(workloads.Items) == 0 {
		return nil, errors.New("release has no stable workloads")
	}
	selectors := make([]stableWorkloadSelector, 0, len(workloads.Items))
	for _, workload := range workloads.Items {
		if workload.Metadata.Name == "" || len(workload.Spec.Selector.MatchLabels) == 0 || len(workload.Spec.Selector.MatchExpressions) != 0 {
			return nil, fmt.Errorf("%s/%s requires an exact matchLabels selector", workload.Kind, workload.Metadata.Name)
		}
		labels := make([]string, 0, len(workload.Spec.Selector.MatchLabels))
		for key, value := range workload.Spec.Selector.MatchLabels {
			labels = append(labels, key+"="+value)
		}
		sort.Strings(labels)
		selectors = append(selectors, stableWorkloadSelector{
			Key: strings.ToLower(workload.Kind) + "/" + workload.Metadata.Name, Selector: strings.Join(labels, ","),
		})
	}
	sort.Slice(selectors, func(i, j int) bool { return selectors[i].Key < selectors[j].Key })
	return selectors, nil
}

func secretDigest(t *testing.T, state *chartState, name string) string {
	t.Helper()
	result, err := state.runner.Run(state.ctx, process.Command{
		Name: "bash", Args: []string{"-o", "pipefail", "-c", `kubectl --kubeconfig "$KUBECONFIG_PATH" get secret "$SECRET_NAME" -n "$SECRET_NAMESPACE" -o go-template='{{range $key, $value := .data}}{{$key}}={{$value}}{{"\n"}}{{end}}' | sha256sum | awk '{print $1}'`},
		Env:     map[string]string{"KUBECONFIG_PATH": state.cluster.Kubeconfig, "SECRET_NAME": name, "SECRET_NAMESPACE": testNamespace},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("hash Secret %s without retaining its values: %v", name, err)
	}
	digest := strings.TrimSpace(result.Stdout)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
		t.Fatalf("Secret %s returned invalid digest %q", name, digest)
	}
	return digest
}

func assertUpgradeContractStage(t *testing.T, state *chartState) {
	currentPlatform := currentChartVersion(t, state, state.platform)
	currentSubstrate := currentChartVersion(t, state, state.substrate)
	assertReleaseChartVersion(t, state, testRelease, "iterabase-platform", currentPlatform)
	assertReleaseChartVersion(t, state, testRelease+"-cert-manager", "cert-manager-substrate", currentSubstrate)
	assertSchemaOwnership(t, state)
	assertLifecycleHealth(t, state)
	assertReleaseMechanics(t, state)
	assertPersistedState(t, state)
	assertRetainedState(t, state.snapshots["predecessor"], captureLifecycleSnapshot(t, state), false)
}

func assertRetainedState(t *testing.T, before, after lifecycleSnapshot, includePods bool) {
	t.Helper()
	if err := retainedStateError(before, after, includePods); err != nil {
		t.Fatal(err)
	}
}

func retainedStateError(before, after lifecycleSnapshot, includePods bool) error {
	if !mapsEqual(before.Secrets, after.Secrets) {
		return fmt.Errorf("generated Secret digests changed: before=%v after=%v", before.Secrets, after.Secrets)
	}
	if !mapsEqual(before.PVCs, after.PVCs) {
		return fmt.Errorf("PVC identities changed: before=%v after=%v", before.PVCs, after.PVCs)
	}
	if includePods && !mapsEqual(before.Pods, after.Pods) {
		return fmt.Errorf("idempotent reapply rolled workloads: before=%v after=%v", before.Pods, after.Pods)
	}
	if includePods && before.ArtifactProvisionerJob != after.ArtifactProvisionerJob {
		return fmt.Errorf("idempotent reapply replaced the completed artifact-provisioner Job: before=%+v after=%+v", before.ArtifactProvisionerJob, after.ArtifactProvisionerJob)
	}
	return nil
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func assertSchemaOwnership(t *testing.T, state *chartState) {
	t.Helper()
	manifest := state.process(t, 90*time.Second, "helm", "get", "manifest", testRelease, "-n", testNamespace,
		"--kubeconfig", state.cluster.Kubeconfig)
	for _, crd := range []string{"agentpools.platform.iterabase.com", "workflows.platform.iterabase.com"} {
		if !strings.Contains(manifest, "name: "+crd) {
			t.Fatalf("Helm release does not retain declarative ownership of %s", crd)
		}
		managedBy := state.kubectl(t, 30*time.Second, "get", "crd/"+crd,
			"-o", "jsonpath={.metadata.labels.app\\.kubernetes\\.io/managed-by}")
		instance := state.kubectl(t, 30*time.Second, "get", "crd/"+crd,
			"-o", "jsonpath={.metadata.labels.app\\.kubernetes\\.io/instance}")
		storedVersion := state.kubectl(t, 30*time.Second, "get", "crd/"+crd, "-o", "jsonpath={.status.storedVersions[0]}")
		if managedBy != "Helm" || instance != testRelease || storedVersion != "v1alpha1" {
			t.Fatalf("%s ownership/schema managed-by=%q instance=%q storedVersion=%q", crd, managedBy, instance, storedVersion)
		}
	}
}

func artifactProvisionerJobName(manifest []byte, release string) (string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(string(manifest)))
	var names []string
	for {
		var document struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name        string            `yaml:"name"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
		}
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("decode Helm release manifest: %w", err)
		}
		if document.Kind != "Job" || document.Metadata.Labels["app.kubernetes.io/component"] != "artifact-provisioner" {
			continue
		}
		if document.Metadata.Name == "" {
			return "", errors.New("artifact-provisioner Job has no name")
		}
		if managedBy := document.Metadata.Labels["app.kubernetes.io/managed-by"]; managedBy != "Helm" {
			return "", fmt.Errorf("artifact-provisioner Job managed-by=%q want Helm", managedBy)
		}
		if instance := document.Metadata.Labels["app.kubernetes.io/instance"]; instance != release {
			return "", fmt.Errorf("artifact-provisioner Job instance=%q want %q", instance, release)
		}
		if hook := document.Metadata.Annotations["helm.sh/hook"]; hook != "" {
			return "", fmt.Errorf("artifact-provisioner Job unexpectedly renders as Helm hook %q", hook)
		}
		names = append(names, document.Metadata.Name)
	}
	if len(names) != 1 {
		return "", fmt.Errorf("Helm release manifest has %d artifact-provisioner Jobs, want 1: %v", len(names), names)
	}
	return names[0], nil
}

func currentArtifactProvisionerJobIdentity(t *testing.T, state *chartState) artifactProvisionerJobIdentity {
	t.Helper()
	// Helm release manifests contain Secret data whose key names can look
	// credential-shaped to text redaction. Keep the exact manifest in a private
	// temporary file so redaction cannot make its YAML invalid before parsing.
	manifestPath := filepath.Join(t.TempDir(), "platform-manifest.yaml")
	if err := os.WriteFile(manifestPath, nil, 0o600); err != nil {
		t.Fatalf("create private Helm manifest file: %v", err)
	}
	if _, err := state.runner.Run(state.ctx, process.Command{
		Name: "bash", Args: []string{"-o", "pipefail", "-c", `helm get manifest "$RELEASE" -n "$NAMESPACE" --kubeconfig "$KUBECONFIG_PATH" > "$MANIFEST_OUTPUT"`},
		Env: map[string]string{
			"RELEASE": testRelease, "NAMESPACE": testNamespace, "KUBECONFIG_PATH": state.cluster.Kubeconfig, "MANIFEST_OUTPUT": manifestPath,
		},
		Timeout: 60 * time.Second,
	}); err != nil {
		t.Fatalf("read exact Helm release manifest: %v", err)
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read private Helm manifest file: %v", err)
	}
	name, err := artifactProvisionerJobName(manifest, testRelease)
	if err != nil {
		t.Fatal(err)
	}
	state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Complete", "job/"+name, "-n", testNamespace, "--timeout=3m")
	identity, err := readArtifactProvisionerJobIdentity(state, name)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func readArtifactProvisionerJobIdentity(state *chartState, name string) (artifactProvisionerJobIdentity, error) {
	raw, err := state.kubectlOutput(30*time.Second, "get", "job/"+name, "-n", testNamespace, "-o", "json")
	if err != nil {
		return artifactProvisionerJobIdentity{}, fmt.Errorf("read artifact-provisioner Job %s: %w", name, err)
	}
	var job struct {
		Metadata struct {
			Name   string            `json:"name"`
			UID    string            `json:"uid"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		return artifactProvisionerJobIdentity{}, fmt.Errorf("decode artifact-provisioner Job %s: %w", name, err)
	}
	if job.Metadata.Name != name || job.Metadata.UID == "" {
		return artifactProvisionerJobIdentity{}, fmt.Errorf("artifact-provisioner Job identity name=%q uid=%q want name=%q and a UID", job.Metadata.Name, job.Metadata.UID, name)
	}
	if managedBy := job.Metadata.Labels["app.kubernetes.io/managed-by"]; managedBy != "Helm" {
		return artifactProvisionerJobIdentity{}, fmt.Errorf("live artifact-provisioner Job managed-by=%q want Helm", managedBy)
	}
	if instance := job.Metadata.Labels["app.kubernetes.io/instance"]; instance != testRelease {
		return artifactProvisionerJobIdentity{}, fmt.Errorf("live artifact-provisioner Job instance=%q want %q", instance, testRelease)
	}
	complete := false
	for _, condition := range job.Status.Conditions {
		if condition.Type == "Complete" && condition.Status == "True" {
			complete = true
			break
		}
	}
	if !complete {
		return artifactProvisionerJobIdentity{}, fmt.Errorf("artifact-provisioner Job %s is not Complete", name)
	}
	return artifactProvisionerJobIdentity{Name: name, UID: job.Metadata.UID}, nil
}

func assertReleaseMechanics(t *testing.T, state *chartState) {
	t.Helper()
	hooks := state.process(t, 60*time.Second, "helm", "get", "hooks", testRelease+"-cert-manager", "-n", testNamespace,
		"--kubeconfig", state.cluster.Kubeconfig)
	if !strings.Contains(hooks, "startupapicheck") || !strings.Contains(hooks, "helm.sh/hook: post-install") {
		t.Fatalf("certificate substrate does not retain its startup API hook: %s", stateSafeBody([]byte(hooks)))
	}
	_ = currentArtifactProvisionerJobIdentity(t, state)
}

func assertLifecycleHealth(t *testing.T, state *chartState) {
	t.Helper()
	for _, workload := range []string{
		"statefulset/" + testRelease + "-postgresql",
		"statefulset/" + testRelease + "-minio",
		"deployment/" + testRelease + "-redis",
		"deployment/" + testRelease + "-control-plane-api",
		"deployment/" + testRelease + "-control-plane-manager",
		"deployment/" + testRelease + "-gateway",
	} {
		state.kubectl(t, 6*time.Minute, "rollout", "status", workload, "-n", testNamespace, "--timeout=5m")
	}
	client, err := httpx.Client(15 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	controlPlane := state.forward(t, "svc/"+testRelease+"-control-plane-api", 8080, "http")
	requireHTTP(t, client, http.MethodGet, controlPlane.URL+"/healthz", nil, http.StatusOK)
	state.stopForward(t, controlPlane)
	gateway := state.forward(t, "svc/"+testRelease+"-gateway", 8080, "http")
	if err := waitHTTPReady(state.ctx, client, gateway.URL+"/readyz", 2*time.Minute); err != nil {
		t.Fatalf("gateway snapshot did not recover after the database rollout: %v", err)
	}
	body := requireHTTP(t, client, http.MethodGet, gateway.URL+"/readyz", nil, http.StatusOK)
	if !strings.Contains(string(body), `"fresh":true`) {
		t.Fatalf("gateway snapshot is not fresh: %s", stateSafeBody(body))
	}
	state.stopForward(t, gateway)
}

func assertOperatorCRDsAbsentStage(t *testing.T, state *chartState) {
	for _, crd := range operatorCRDs {
		name := state.kubectl(t, 30*time.Second, "get", "crd/"+crd, "--ignore-not-found", "-o", "name")
		if name != "" {
			t.Fatalf("operator CRD %s unexpectedly existed before feature enablement: %s", crd, name)
		}
	}
}

func preapplyCurrentOperatorCRDsStage(t *testing.T, state *chartState) {
	t.Helper()
	// Do not pass a CRD schema through text redaction before applying it: schema
	// property names can look credential-shaped even though this exact chart
	// manifest contains no Secret values. Redirect stdout to a private file and
	// retain only command stderr through the shared process runner.
	path := filepath.Join(t.TempDir(), "current-platform-crds.yaml")
	args := []string{"-o", "pipefail", "-c", `helm show crds "$@" > "$CRD_OUTPUT"`, "--"}
	args = append(args, helmChartArgs(state.platform)...)
	if _, err := state.runner.Run(state.ctx, process.Command{
		Name: "bash", Args: args, Env: map[string]string{"CRD_OUTPUT": path}, Timeout: 2 * time.Minute,
	}); err != nil {
		t.Fatalf("extract exact current platform CRDs: %v", err)
	}
	manifest, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read exact current platform CRDs: %v", err)
	}
	selected, err := selectBundledCRDs(string(manifest))
	if err != nil {
		t.Fatalf("select authoritative current platform CRDs: %v", err)
	}
	for _, crd := range operatorCRDs {
		if !strings.Contains(selected, "name: "+crd) {
			t.Fatalf("exact current platform archive does not bundle %s", crd)
		}
	}
	if err := os.WriteFile(path, []byte(selected), 0o600); err != nil {
		t.Fatalf("write selected current platform CRDs: %v", err)
	}
	state.kubectl(t, 3*time.Minute, "apply", "--server-side", "--force-conflicts", "--field-manager="+transitionFieldManager, "-f", path)
	for _, crd := range operatorCRDs {
		state.kubectl(t, 3*time.Minute, "wait", "--for=condition=Established", "crd/"+crd, "--timeout=2m")
	}
	// CRD schemas contain credential-shaped property names that text redaction
	// can make invalid JSON. Keep the exact kubectl payload in a private
	// temporary file and expose only command stderr to the shared runner.
	statusPath := filepath.Join(t.TempDir(), "operator-crd-statuses.json")
	if err := os.WriteFile(statusPath, nil, 0o600); err != nil {
		t.Fatalf("create operator CRD status file: %v", err)
	}
	statusArgs := []string{"-o", "pipefail", "-c", `kubectl --kubeconfig "$KUBECONFIG_PATH" get "$@" -o json > "$CRD_STATUS_OUTPUT"`, "--"}
	for _, crd := range operatorCRDs {
		statusArgs = append(statusArgs, "crd/"+crd)
	}
	if _, err := state.runner.Run(state.ctx, process.Command{
		Name: "bash", Args: statusArgs,
		Env: map[string]string{"KUBECONFIG_PATH": state.cluster.Kubeconfig, "CRD_STATUS_OUTPUT": statusPath}, Timeout: 30 * time.Second,
	}); err != nil {
		t.Fatalf("read operator CRD statuses: %v", err)
	}
	statuses, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatalf("read operator CRD status file: %v", err)
	}
	if err := assertEstablishedCRDsJSON(statuses, operatorCRDs); err != nil {
		t.Fatal(err)
	}
	for _, crd := range operatorCRDs {
		managers := state.kubectl(t, 30*time.Second, "get", "crd/"+crd, "-o", "jsonpath={.metadata.managedFields[*].manager}")
		if !slices.Contains(strings.Fields(managers), transitionFieldManager) {
			t.Fatalf("%s was not server-side applied by %s: %s", crd, transitionFieldManager, managers)
		}
	}
}

func selectBundledCRDs(raw string) (string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	selected := make(map[string]bundledCRD)
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("decode bundled CRDs: %w", err)
		}
		var header bundledCRDHeader
		if err := document.Decode(&header); err != nil {
			return "", fmt.Errorf("decode bundled CRD header: %w", err)
		}
		if header.APIVersion != "apiextensions.k8s.io/v1" || header.Kind != "CustomResourceDefinition" {
			continue
		}
		if header.Metadata.Name == "" {
			return "", errors.New("bundled CRD is missing metadata.name")
		}
		var resource any
		if err := document.Decode(&resource); err != nil {
			return "", fmt.Errorf("decode bundled CRD %s: %w", header.Metadata.Name, err)
		}
		encoded, err := yaml.Marshal(resource)
		if err != nil {
			return "", fmt.Errorf("encode bundled CRD %s: %w", header.Metadata.Name, err)
		}
		candidate := bundledCRD{header: header, manifest: strings.TrimSpace(string(encoded))}
		if existing, duplicate := selected[header.Metadata.Name]; duplicate {
			candidate, err = selectBundledCRD(existing, candidate)
			if err != nil {
				return "", err
			}
		}
		selected[header.Metadata.Name] = candidate
	}
	if len(selected) == 0 {
		return "", errors.New("exact current platform archive contains no CRDs")
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	manifests := make([]string, 0, len(names))
	for _, name := range names {
		manifests = append(manifests, selected[name].manifest)
	}
	return strings.Join(manifests, "\n---\n") + "\n", nil
}

func selectMetalLBCRDs(raw string) (string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	var crds []string
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("decode MetalLB bundled CRDs: %w", err)
		}
		var header bundledCRDHeader
		if err := document.Decode(&header); err != nil {
			return "", fmt.Errorf("decode MetalLB bundled CRD header: %w", err)
		}
		if header.APIVersion != "apiextensions.k8s.io/v1" || header.Kind != "CustomResourceDefinition" {
			continue
		}
		if !strings.HasSuffix(header.Metadata.Name, ".metallb.io") {
			continue
		}
		var resource any
		if err := document.Decode(&resource); err != nil {
			return "", fmt.Errorf("decode MetalLB bundled CRD: %w", err)
		}
		encoded, err := yaml.Marshal(resource)
		if err != nil {
			return "", fmt.Errorf("encode MetalLB bundled CRD %s: %w", header.Metadata.Name, err)
		}
		crds = append(crds, strings.TrimSpace(string(encoded)))
	}
	// Filter only the MetalLB CRDs; every other CRD the chart ships in `crds/`
	// directories or renders as an ordinary template is left entirely to Helm's
	// own install path, which owns it correctly on a fresh install. Pre-applying
	// those without Helm ownership makes a fresh `helm install` fail to import
	// them ("invalid ownership metadata"), so only the MetalLB set is established
	// before Helm (mirrors Forge's selectMetalLBCRDs, DES-HOR-511-03/04).
	if len(crds) == 0 {
		return "", nil
	}
	return strings.Join(crds, "\n---\n") + "\n", nil
}

func selectBundledCRD(existing, candidate bundledCRD) (bundledCRD, error) {
	if existing.manifest == candidate.manifest {
		return existing, nil
	}
	const versionAnnotation = "operator.prometheus.io/version"
	existingVersion := existing.header.Metadata.Annotations[versionAnnotation]
	candidateVersion := candidate.header.Metadata.Annotations[versionAnnotation]
	if existingVersion == "" && candidateVersion == "" {
		return bundledCRD{}, fmt.Errorf("conflicting duplicate bundled CRD %q has no authoritative version annotation", existing.header.Metadata.Name)
	}
	if existingVersion == "" {
		return candidate, nil
	}
	if candidateVersion == "" {
		return existing, nil
	}
	comparison, err := compareNumericVersions(existingVersion, candidateVersion)
	if err != nil {
		return bundledCRD{}, fmt.Errorf("compare duplicate bundled CRD %q versions: %w", existing.header.Metadata.Name, err)
	}
	if comparison < 0 {
		return candidate, nil
	}
	if comparison > 0 {
		return existing, nil
	}
	return bundledCRD{}, fmt.Errorf("conflicting duplicate bundled CRD %q has equal authoritative version %q", existing.header.Metadata.Name, existingVersion)
}

func compareNumericVersions(left, right string) (int, error) {
	parse := func(version string) ([]int, error) {
		parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid numeric version %q", version)
		}
		values := make([]int, len(parts))
		for index, part := range parts {
			value, err := strconv.Atoi(part)
			if err != nil || value < 0 {
				return nil, fmt.Errorf("invalid numeric version %q", version)
			}
			values[index] = value
		}
		return values, nil
	}
	leftParts, err := parse(left)
	if err != nil {
		return 0, err
	}
	rightParts, err := parse(right)
	if err != nil {
		return 0, err
	}
	for index := 0; index < max(len(leftParts), len(rightParts)); index++ {
		var leftPart, rightPart int
		if index < len(leftParts) {
			leftPart = leftParts[index]
		}
		if index < len(rightParts) {
			rightPart = rightParts[index]
		}
		if leftPart < rightPart {
			return -1, nil
		}
		if leftPart > rightPart {
			return 1, nil
		}
	}
	return 0, nil
}

// bundledCRDNames returns the sorted, de-duplicated CRD names in raw YAML
// (selecting the authoritative candidate on duplicate names), mirroring
// selectBundledCRDs but returning names rather than manifests.
func bundledCRDNames(raw string) ([]string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	seen := make(map[string]struct{})
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode CRD names: %w", err)
		}
		var header bundledCRDHeader
		if err := document.Decode(&header); err != nil {
			return nil, fmt.Errorf("decode CRD header: %w", err)
		}
		if header.APIVersion != "apiextensions.k8s.io/v1" || header.Kind != "CustomResourceDefinition" || header.Metadata.Name == "" {
			continue
		}
		seen[header.Metadata.Name] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// markRenderedCRDsOwned injects the incoming release's Helm ownership metadata
// into every rendered (template) CustomResourceDefinition so a fresh `helm
// install` can adopt them (mirrors Forge's markHelmAdoptableCRDs, DES-HOR-511-04).
// Scope is strict: only the rendered template MetalLB CRDs are marked for the
// incoming release/namespace; crds/-directory CRDs install via Helm's own path.
// Idempotent; preserves existing metadata.
func markRenderedCRDsOwned(rendered, release, namespace string) (string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(rendered))
	var manifests []string
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("decode rendered CRD for helm adoption: %w", err)
		}
		var header bundledCRDHeader
		if err := document.Decode(&header); err != nil {
			return "", fmt.Errorf("decode rendered CRD header: %w", err)
		}
		if header.APIVersion != "apiextensions.k8s.io/v1" || header.Kind != "CustomResourceDefinition" {
			continue
		}
		// Strict founder scope (DES-HOR-511-04): only the nine MetalLB CRDs are
		// marked Helm-adoptable; other rendered template CRDs are still included in
		// the pre-apply set (established before Helm) but left unmarked.
		if strings.HasSuffix(header.Metadata.Name, ".metallb.io") {
			injectHelmOwnership(document.Content[0], release, namespace)
		}
		var resource any
		if err := document.Decode(&resource); err != nil {
			return "", fmt.Errorf("decode rendered CRD for helm adoption: %w", err)
		}
		manifest, err := yaml.Marshal(resource)
		if err != nil {
			return "", fmt.Errorf("encode rendered CRD for helm adoption: %w", err)
		}
		manifests = append(manifests, strings.TrimSpace(string(manifest)))
	}
	return strings.Join(manifests, "\n---\n") + "\n", nil
}

// injectHelmOwnership sets release-ownership annotations + managed-by label on a
// CRD's metadata node, preserving existing metadata (idempotent).
func injectHelmOwnership(root *yaml.Node, release, namespace string) {
	var meta *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "metadata" {
			meta = root.Content[i+1]
			break
		}
	}
	if meta == nil {
		meta = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "metadata"}, meta)
	}
	ensureYAMLMapKeys(meta, "annotations", map[string]string{
		"meta.helm.sh/release-name":      release,
		"meta.helm.sh/release-namespace": namespace,
	})
	ensureYAMLMapKeys(meta, "labels", map[string]string{
		"app.kubernetes.io/managed-by": "Helm",
	})
}

// ensureYAMLMapKeys sets scalar key/value pairs on a mapping node, preserving
// the existing node structure and other entries.
func ensureYAMLMapKeys(mapNode *yaml.Node, key string, values map[string]string) {
	var sub *yaml.Node
	for i := 0; i+1 < len(mapNode.Content); i += 2 {
		if mapNode.Content[i].Value == key {
			sub = mapNode.Content[i+1]
		}
	}
	if sub == nil {
		sub = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mapNode.Content = append(mapNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, sub)
	}
	if sub.Kind != yaml.MappingNode {
		sub = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	for k, v := range values {
		var set bool
		for i := 0; i+1 < len(sub.Content); i += 2 {
			if sub.Content[i].Value == k {
				sub.Content[i+1].Value = v
				if sub.Content[i+1].Kind != yaml.ScalarNode {
					sub.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
				}
				set = true
				break
			}
		}
		if !set {
			sub.Content = append(sub.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
		}
	}
}

func assertEstablishedCRDsJSON(data []byte, required []string) error {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("decode operator CRDs: %w", err)
	}
	established := make(map[string]bool, len(list.Items))
	for _, item := range list.Items {
		for _, condition := range item.Status.Conditions {
			if condition.Type == "Established" && condition.Status == "True" {
				established[item.Metadata.Name] = true
			}
		}
	}
	for _, name := range required {
		if !established[name] {
			return fmt.Errorf("operator CRD %s is not Established", name)
		}
	}
	return nil
}

func assertFeatureEnableClientPathsStage(t *testing.T, state *chartState) {
	assertTLSExporterPaths(t, state, false, false)
	assertVerifiedSelfMonitorsStage(t, state)
	assertGrafanaTLSPathsStage(t, state)
	assertLokiGatewayTLSStage(t, state)
	assertTLSPromtailPathStage(t, state)
	assertTLSAlertmanagerPathStage(t, state)
	assertGatewayDependenciesStage(t, state)
	assertRenderedTLSClientConfigStage(t, state)
	assertRedisTransportStage(t, state)
	assertPostgreSQLTransportStage(t, state)
}

func reapplyCurrentPairStage(t *testing.T, state *chartState) {
	state.installSubstrate(t)
	state.installPlatform(t, 18*time.Minute, lifecyclePlatformValueFiles(t, state)...)
}

func assertIdempotentReapplyStage(t *testing.T, state *chartState) {
	before := state.snapshots["current"]
	after := captureLifecycleSnapshot(t, state)
	after.ArtifactProvisionerJob = currentArtifactProvisionerJobIdentity(t, state)
	assertRetainedState(t, before, after, true)
	assertPersistedState(t, state)
	assertLifecycleHealth(t, state)
	assertReleaseRevision(t, state, testRelease, 3)
	assertReleaseRevision(t, state, testRelease+"-cert-manager", 3)
}

func inverseRollbackStage(t *testing.T, state *chartState) {
	// The supported post-0.3 inverse boundary restores the platform first, then
	// its same-version substrate. CRDs, generated Secrets, and PVCs are retained.
	rollbackRelease(t, state, testRelease, 1)
	rollbackRelease(t, state, testRelease+"-cert-manager", 1)
}

func rollbackRelease(t *testing.T, state *chartState, release string, revision int) {
	t.Helper()
	state.process(t, 12*time.Minute, "helm", "rollback", release, strconv.Itoa(revision), "--namespace", testNamespace,
		"--kubeconfig", state.cluster.Kubeconfig, "--wait", "--timeout", "10m")
}

func assertRollbackBoundaryStage(t *testing.T, state *chartState) {
	platform := requireTransitionBaseline(t, state, platformPredecessorName)
	substrate := requireTransitionBaseline(t, state, substratePredecessorName)
	assertRollbackReleaseHistory(t, state, testRelease, platform.Chart, platform.Version, 4)
	assertRollbackReleaseHistory(t, state, testRelease+"-cert-manager", substrate.Chart, substrate.Version, 4)
	assertPersistedState(t, state)
	assertRetainedState(t, state.snapshots["current"], captureLifecycleSnapshot(t, state), false)
	assertLifecycleHealth(t, state)
}

func forwardRecoveryStage(t *testing.T, state *chartState) {
	state.installSubstrate(t)
	state.installPlatform(t, 18*time.Minute, lifecyclePlatformValueFiles(t, state)...)
	assertCandidateImages(t, state)
}

func assertForwardRecoveryStage(t *testing.T, state *chartState) {
	assertReleaseChartVersion(t, state, testRelease, "iterabase-platform", currentChartVersion(t, state, state.platform))
	assertReleaseChartVersion(t, state, testRelease+"-cert-manager", "cert-manager-substrate", currentChartVersion(t, state, state.substrate))
	assertPersistedState(t, state)
	assertRetainedState(t, state.snapshots["current"], captureLifecycleSnapshot(t, state), false)
	assertLifecycleHealth(t, state)
	assertSchemaOwnership(t, state)
}

func currentChartVersion(t *testing.T, state *chartState, chart kube.Chart) string {
	t.Helper()
	// Keep the Helm input selection identical to installation rather than
	// trusting a parallel version constant.
	args := []string{"show", "chart"}
	if chart.LocalPath != "" {
		args = append(args, chart.LocalPath)
	} else {
		args = append(args, chart.Reference, "--version", chart.Version)
	}
	metadata := state.process(t, 60*time.Second, "helm", args...)
	for _, line := range strings.Split(metadata, "\n") {
		if version, ok := strings.CutPrefix(line, "version:"); ok {
			return strings.Trim(strings.TrimSpace(version), `"'`)
		}
	}
	t.Fatalf("chart metadata has no version: %s", metadata)
	return ""
}

func assertReleaseChartVersion(t *testing.T, state *chartState, release, chart, version string) {
	t.Helper()
	entry, err := currentHelmHistoryEntry([]byte(state.process(t, 60*time.Second, "helm", "history", release,
		"--namespace", testNamespace, "--kubeconfig", state.cluster.Kubeconfig, "--output", "json")))
	if err != nil {
		t.Fatal(err)
	}
	want := chart + "-" + version
	if entry.Chart != want || entry.Status != "deployed" {
		t.Fatalf("%s current history chart=%q status=%q want chart=%q deployed", release, entry.Chart, entry.Status, want)
	}
}

func assertReleaseRevision(t *testing.T, state *chartState, release string, want int) {
	t.Helper()
	entry, err := currentHelmHistoryEntry([]byte(state.process(t, 60*time.Second, "helm", "history", release,
		"--namespace", testNamespace, "--kubeconfig", state.cluster.Kubeconfig, "--output", "json")))
	if err != nil {
		t.Fatal(err)
	}
	if entry.Revision != want || entry.Status != "deployed" {
		t.Fatalf("%s revision=%d status=%q want revision=%d deployed", release, entry.Revision, entry.Status, want)
	}
}

func assertRollbackReleaseHistory(t *testing.T, state *chartState, release, chart, version string, revision int) {
	t.Helper()
	entry, err := currentHelmHistoryEntry([]byte(state.process(t, 60*time.Second, "helm", "history", release,
		"--namespace", testNamespace, "--kubeconfig", state.cluster.Kubeconfig, "--output", "json")))
	if err != nil {
		t.Fatal(err)
	}
	if err := rollbackHistoryError(entry, chart, version, revision); err != nil {
		t.Fatalf("%s rollback boundary: %v", release, err)
	}
}

func rollbackHistoryError(entry helmHistoryEntry, chart, version string, revision int) error {
	wantChart := chart + "-" + version
	if entry.Revision != revision || entry.Status != "deployed" || entry.Chart != wantChart {
		return fmt.Errorf("revision=%d status=%q chart=%q want revision=%d status=deployed chart=%q",
			entry.Revision, entry.Status, entry.Chart, revision, wantChart)
	}
	return nil
}

func currentHelmHistoryEntry(data []byte) (helmHistoryEntry, error) {
	var history []helmHistoryEntry
	if err := json.Unmarshal(data, &history); err != nil {
		return helmHistoryEntry{}, fmt.Errorf("decode Helm history: %w", err)
	}
	if len(history) == 0 {
		return helmHistoryEntry{}, fmt.Errorf("Helm history is empty")
	}
	sort.Slice(history, func(i, j int) bool { return history[i].Revision < history[j].Revision })
	return history[len(history)-1], nil
}

func TestUnitTransitionBaselineFixtureRejectsMutableOrMismatchedInputs(t *testing.T) {
	valid, err := os.ReadFile("transition-baselines.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeTransitionBaselineFixture(valid); err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}
	for name, mutate := range map[string]func(string) string{
		"latest":       func(value string) string { return strings.Replace(value, ":0.3.12", ":latest", 1) },
		"bad checksum": func(value string) string { return strings.Replace(value, "86b0f230", "notahash", 1) },
		"bad metallb checksum": func(value string) string {
			return strings.Replace(value, "252e5fea", "notahash", 1)
		},
		"version mismatch": func(value string) string {
			return strings.Replace(value, "cert-manager-substrate:0.3.12", "cert-manager-substrate:0.3.11", 1)
		},
		"metallb version mismatch": func(value string) string {
			return strings.Replace(value, "cert-manager-substrate:0.3.19", "cert-manager-substrate:0.3.18", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeTransitionBaselineFixture([]byte(mutate(string(valid)))); err == nil {
				t.Fatal("intentional transition baseline break passed")
			}
		})
	}
}

func TestUnitBundledCRDsSelectAuthoritativeOperatorSchema(t *testing.T) {
	authoritative := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: servicemonitors.monitoring.coreos.com
  annotations:
    operator.prometheus.io/version: 0.93.0
spec:
  group: monitoring.coreos.com`
	stale := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: servicemonitors.monitoring.coreos.com
spec:
  group: stale.example.com`
	selected, err := selectBundledCRDs(stale + "\n---\n" + authoritative)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(selected, "group: monitoring.coreos.com") || strings.Contains(selected, "stale.example.com") {
		t.Fatalf("authoritative operator schema was not selected:\n%s", selected)
	}
	ambiguous := strings.ReplaceAll(stale, "stale.example.com", "first.example.com") + "\n---\n" + strings.ReplaceAll(stale, "stale.example.com", "second.example.com")
	if _, err := selectBundledCRDs(ambiguous); err == nil {
		t.Fatal("ambiguous duplicate bundled CRDs passed")
	}
}

func TestUnitOperatorCRDsMustBeEstablishedBeforeFeatureEnable(t *testing.T) {
	fixture := []byte(`{"items":[{"metadata":{"name":"servicemonitors.monitoring.coreos.com"},"status":{"conditions":[{"type":"Established","status":"False"}]}}]}`)
	if err := assertEstablishedCRDsJSON(fixture, []string{"servicemonitors.monitoring.coreos.com"}); err == nil {
		t.Fatal("unestablished operator CRD passed")
	}
}

func TestUnitArtifactProvisionerManifestRequiresOrdinaryHelmOwnership(t *testing.T) {
	valid := `apiVersion: batch/v1
kind: Job
metadata:
  name: iterabase-minio-artifact-provisioner-0-2-3
  labels:
    app.kubernetes.io/component: artifact-provisioner
    app.kubernetes.io/instance: iterabase
    app.kubernetes.io/managed-by: Helm
`
	if name, err := artifactProvisionerJobName([]byte(valid), "iterabase"); err != nil || name != "iterabase-minio-artifact-provisioner-0-2-3" {
		t.Fatalf("valid ordinary provisioner rejected: name=%q err=%v", name, err)
	}
	for name, changed := range map[string]string{
		"wrong owner": strings.Replace(valid, "managed-by: Helm", "managed-by: controller", 1),
		"hook":        strings.Replace(valid, "  labels:\n", "  annotations:\n    helm.sh/hook: post-install\n  labels:\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := artifactProvisionerJobName([]byte(changed), "iterabase"); err == nil {
				t.Fatal("invalid artifact-provisioner ownership passed")
			}
		})
	}
}

func TestUnitRetainedStateRejectsSecretPVCReapplyRolloutAndProvisionerChanges(t *testing.T) {
	baseline := lifecycleSnapshot{
		Secrets: map[string]string{"secret": "a"}, PVCs: map[string]string{"pvc": "b"}, Pods: map[string]string{"pods": "c"},
		ArtifactProvisionerJob: artifactProvisionerJobIdentity{Name: "provisioner", UID: "job-uid"},
	}
	changes := []lifecycleSnapshot{
		{Secrets: map[string]string{"secret": "changed"}, PVCs: baseline.PVCs, Pods: baseline.Pods, ArtifactProvisionerJob: baseline.ArtifactProvisionerJob},
		{Secrets: baseline.Secrets, PVCs: map[string]string{"pvc": "changed"}, Pods: baseline.Pods, ArtifactProvisionerJob: baseline.ArtifactProvisionerJob},
		{Secrets: baseline.Secrets, PVCs: baseline.PVCs, Pods: map[string]string{"pods": "changed"}, ArtifactProvisionerJob: baseline.ArtifactProvisionerJob},
		{Secrets: baseline.Secrets, PVCs: baseline.PVCs, Pods: baseline.Pods, ArtifactProvisionerJob: artifactProvisionerJobIdentity{Name: "provisioner", UID: "replacement-uid"}},
	}
	for index, changed := range changes {
		if err := retainedStateError(baseline, changed, true); err == nil {
			t.Fatalf("intentional retained-state break %d passed", index)
		}
	}
}

func TestUnitRollbackBoundaryRejectsIncorrectHelmHistory(t *testing.T) {
	entry, err := currentHelmHistoryEntry([]byte(`[{"revision":3,"status":"superseded","chart":"iterabase-platform-0.3.15"},{"revision":4,"status":"deployed","chart":"iterabase-platform-0.3.12"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := rollbackHistoryError(entry, "iterabase-platform", "0.3.12", 4); err != nil {
		t.Fatalf("valid rollback history rejected: %v", err)
	}
	for name, changed := range map[string]helmHistoryEntry{
		"wrong revision": {Revision: 3, Status: "deployed", Chart: "iterabase-platform-0.3.12"},
		"wrong status":   {Revision: 4, Status: "failed", Chart: "iterabase-platform-0.3.12"},
		"wrong chart":    {Revision: 4, Status: "deployed", Chart: "iterabase-platform-0.3.15"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := rollbackHistoryError(changed, "iterabase-platform", "0.3.12", 4); err == nil {
				t.Fatal("intentional rollback history break passed")
			}
		})
	}
}

func TestUnitTransitionCurrentMustBeNewerThanPredecessor(t *testing.T) {
	metadata := transitionScenarioMetadata("fixture-mode-contract", "fixture mode contract", "", 0, nil, nil)
	if !slices.Equal(metadata.FixtureModes, []sharede2e.FixtureMode{sharede2e.FixtureSource, sharede2e.FixtureCandidate}) {
		t.Fatalf("transition fixture modes=%v", metadata.FixtureModes)
	}
	if err := newerChartVersionError("0.3.15", "0.3.12"); err != nil {
		t.Fatalf("newer current chart rejected: %v", err)
	}
	for _, current := range []string{"0.3.12", "0.3.11", "0.3.1", "0.3.15-rc.1"} {
		if err := newerChartVersionError(current, "0.3.12"); err == nil {
			t.Fatalf("non-newer or unsupported current chart version %q passed", current)
		}
	}
}

func TestUnitStableWorkloadSnapshotIncludesEveryController(t *testing.T) {
	workloads := []byte(`{"items":[
		{"kind":"Deployment","metadata":{"name":"manager"},"spec":{"selector":{"matchLabels":{"component":"manager","app":"control-plane"}}}},
		{"kind":"DaemonSet","metadata":{"name":"csi"},"spec":{"selector":{"matchLabels":{"app":"csi"}}}}
	]}`)
	selectors, err := stableWorkloadSelectorsJSON(workloads)
	if err != nil {
		t.Fatal(err)
	}
	want := []stableWorkloadSelector{
		{Key: "daemonset/csi", Selector: "app=csi"},
		{Key: "deployment/manager", Selector: "app=control-plane,component=manager"},
	}
	if !slices.Equal(selectors, want) {
		t.Fatalf("stable workload selectors=%v want=%v", selectors, want)
	}
}

func TestUnitTransitionBaselineOrderIsStable(t *testing.T) {
	fixture := loadTransitionBaselineFixture(t)
	names := make([]string, 0, len(fixture.Inputs))
	for _, input := range fixture.Inputs {
		names = append(names, input.Name)
	}
	if !slices.Equal(names, []string{platformPredecessorName, substratePredecessorName, metalLBPlatformPredecessorName, metalLBSubstratePredecessorName}) {
		t.Fatalf("transition baseline order=%v", names)
	}
}
