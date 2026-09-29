package e2e_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/diagnostics"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/httpx"
	kindcluster "github.com/nunocgoncalves/iterabase-mono/testkit/e2e/kind"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/kube"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/poll"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

const testNamespace = "iterabase-system"

// Chart-owned DES-HOR-545-01 storage expectations. The shared testkit helper
// validates these through an LVMStorageContract that this owning suite
// constructs; the exact product names and identities stay here (testkit/AGENTS.md).
const (
	PlatformDataStorageClass       = "iterabase-lvm-xfs"
	AgentPoolWorkspaceStorageClass = "iterabase-agentpool-lvm-xfs"
	lvmProvisioner                 = "local.csi.openebs.io"
	lvmDataVolumeGroupName         = "iterabase-data"
	lvmNodeTopologyKey             = "openebs.io/nodename"
)

func lvmStorageContract() kindcluster.LVMStorageContract {
	return kindcluster.LVMStorageContract{
		DataVolumeGroupName: lvmDataVolumeGroupName,
		Provisioner:         lvmProvisioner,
		NodeTopologyKey:     lvmNodeTopologyKey,
		StorageClasses: []kindcluster.StorageClassExpectation{
			{Name: PlatformDataStorageClass, Shared: false},
			{Name: AgentPoolWorkspaceStorageClass, Shared: true},
		},
	}
}

const (
	metalLBValidationPolicyValue = "metallb.crds.validationFailurePolicy"
	metalLBPolicyFail            = "Fail"
	metalLBPolicyIgnore          = "Ignore"
	metalLBWebhookConfigName     = "metallb-webhook-configuration"
)

var testRelease = func() string {
	if configured := strings.TrimSpace(os.Getenv("ITERABASE_E2E_RELEASE")); configured != "" {
		return configured
	}
	return "iterabase"
}()

func kubePrometheusStackComponentName(component string) string {
	return kubePrometheusStackComponentNameForRelease(testRelease, component)
}

func kubePrometheusStackComponentNameForRelease(release, component string) string {
	const chartName = "kube-prometheus-stack"
	fullname := release
	if !strings.Contains(release, chartName) {
		fullname += "-" + chartName
	}
	if len(fullname) > 26 {
		fullname = fullname[:26]
	}
	return strings.TrimSuffix(fullname, "-") + "-" + component
}

type chartState struct {
	ctx                  context.Context
	chartsRoot           string
	outputDir            string
	diagnosticsDir       string
	redactor             *redact.Redactor
	runner               process.Runner
	cluster              *kindcluster.Cluster
	client               kube.Client
	forwards             []*kube.Forward
	platform             kube.Chart
	substrate            kube.Chart
	lvmSubstrate         kube.Chart
	lvmStorageReady      bool
	transitionBaselines  map[string]transitionBaseline
	runtimeImageDigests  map[string]string
	snapshots            map[string]lifecycleSnapshot
	internalIngressIP    string
	internalPool         string
	internalPoolInternal string
	metalLB              *metalLBSnapshot
	internalCARootUID    string
}

func newChartState(t *testing.T) *chartState {
	t.Helper()
	chartsRoot := os.Getenv("ITERABASE_CHARTS_ROOT")
	if chartsRoot == "" {
		var err error
		chartsRoot, err = filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatalf("resolve charts root: %v", err)
		}
	}
	chartsRoot, err := filepath.Abs(chartsRoot)
	if err != nil {
		t.Fatalf("resolve charts root: %v", err)
	}
	outputDir := filepath.Join(t.TempDir(), "evidence")
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		t.Fatalf("create evidence directory: %v", err)
	}
	diagnosticsDir := filepath.Join(outputDir, "diagnostics")
	if configured := os.Getenv("ITERABASE_E2E_DIAGNOSTICS"); configured != "" {
		diagnosticsDir = configured
		if err := os.MkdirAll(diagnosticsDir, 0o700); err != nil {
			t.Fatalf("create persistent diagnostics directory: %v", err)
		}
	}
	redactor := redact.New()
	state := &chartState{
		ctx: context.Background(), chartsRoot: chartsRoot, outputDir: outputDir, diagnosticsDir: diagnosticsDir, redactor: redactor,
		runner:              process.Runner{Redactor: redactor, OutputDir: outputDir},
		runtimeImageDigests: make(map[string]string), snapshots: make(map[string]lifecycleSnapshot),
	}
	state.platform, state.substrate, state.lvmSubstrate = resolveCharts(t, chartsRoot)
	return state
}

func resolveCharts(t *testing.T, _ string) (kube.Chart, kube.Chart, kube.Chart) {
	t.Helper()
	mode := sharede2e.FixtureMode(os.Getenv("ITERABASE_E2E_FIXTURE_MODE"))
	switch mode {
	case sharede2e.FixtureSource, sharede2e.FixtureCandidate:
		platform := os.Getenv("ITERABASE_PLATFORM_LOCAL_CHART")
		if platform == "" {
			t.Fatal("composed runtime requires ITERABASE_PLATFORM_LOCAL_CHART")
		}
		platform, err := filepath.Abs(platform)
		if err != nil {
			t.Fatalf("resolve composed platform chart: %v", err)
		}
		substrate := filepath.Join(filepath.Dir(platform), "cert-manager-substrate")
		lvmSubstrate := filepath.Join(filepath.Dir(platform), "lvm-storage-substrate")
		return kube.Chart{Mode: mode, LocalPath: platform}, kube.Chart{Mode: mode, LocalPath: substrate}, kube.Chart{Mode: mode, LocalPath: lvmSubstrate}
	case sharede2e.FixturePublished:
		t.Fatal("current chart scenarios do not advertise published mode until the same-version LVM storage companion is published")
		return kube.Chart{}, kube.Chart{}, kube.Chart{}
	default:
		t.Fatalf("unsupported charts fixture mode %q", mode)
		return kube.Chart{}, kube.Chart{}, kube.Chart{}
	}
}

func publishedPlatformVersion(t *testing.T) string {
	t.Helper()
	path := os.Getenv("ITERABASE_E2E_PUBLISHED_FIXTURE")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read published fixture: %v", err)
	}
	var fixture sharede2e.Fixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode published fixture: %v", err)
	}
	for _, input := range fixture.Inputs {
		if input.Name != "iterabase-platform" {
			continue
		}
		separator := strings.LastIndexByte(input.Reference, ':')
		if separator < 0 || separator == len(input.Reference)-1 {
			t.Fatalf("published platform reference has no version: %q", input.Reference)
		}
		return input.Reference[separator+1:]
	}
	t.Fatal("published fixture has no iterabase-platform input")
	return ""
}

func createKindStage(t *testing.T, state *chartState) {
	t.Helper()
	manager := kindcluster.Manager{Executor: state.runner}
	cluster, err := manager.Create(state.ctx, "charts")
	if err != nil {
		t.Fatalf("create Kind cluster: %v", err)
	}
	state.cluster = cluster
	state.client = kube.Client{Executor: state.runner, Kubeconfig: cluster.Kubeconfig, Redactor: state.redactor}
}

func installLVMStorageStage(t *testing.T, state *chartState) {
	t.Helper()
	state.installLVMStorage(t)
}

func importRuntimeImagesStage(t *testing.T, state *chartState) {
	t.Helper()
	images := []struct {
		name       string
		artifact   string
		prefix     string
		archiveEnv string
	}{
		{name: "control-plane", artifact: "control-plane-image", prefix: "CONTROL_PLANE", archiveEnv: "FORGE_E2E_CONTROL_PLANE_IMAGE_ARCHIVE"},
		{name: "inference-gateway", artifact: "inference-gateway-image", prefix: "INFERENCE_GATEWAY", archiveEnv: "FORGE_E2E_INFERENCE_IMAGE_ARCHIVE"},
		{name: "harness", artifact: "harness-image", prefix: "HARNESS", archiveEnv: "FORGE_E2E_HARNESS_IMAGE_ARCHIVE"},
		{name: "tool-runner", artifact: "tool-runner-image", prefix: "TOOL_RUNNER", archiveEnv: "FORGE_E2E_TOOL_RUNNER_IMAGE_ARCHIVE"},
		{name: "runtime-fixture", artifact: "runtime-fixture-image", prefix: "FORGE_E2E_RUNTIME", archiveEnv: "FORGE_E2E_RUNTIME_IMAGE_ARCHIVE"},
	}
	imported := 0
	for _, image := range images {
		repository := os.Getenv(image.prefix + "_IMAGE_REPO")
		tag := os.Getenv(image.prefix + "_IMAGE_TAG")
		digest := os.Getenv(image.prefix + "_IMAGE_DIGEST")
		configDigest := os.Getenv(image.prefix + "_IMAGE_CONFIG_DIGEST")
		archive := os.Getenv(image.archiveEnv)
		if repository == "" && tag == "" && digest == "" && configDigest == "" && archive == "" {
			continue
		}
		if repository == "" || tag == "" || archive == "" ||
			!regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(digest) ||
			!regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(configDigest) {
			t.Fatalf("composed %s runtime image has incomplete repository/tag/artifact-digest/config-digest/archive identity", image.name)
		}
		reference := repository + ":" + tag
		identity, err := state.cluster.ImportImageArchive(state.ctx, archive, reference, configDigest)
		if err != nil {
			t.Fatalf("import composed %s image before chart install: %v", image.name, err)
		}
		if sourceSHA := os.Getenv(image.prefix + "_IMAGE_SOURCE_SHA"); sourceSHA != "" &&
			identity.Labels["org.opencontainers.image.revision"] != sourceSHA {
			t.Fatalf("imported %s image revision label=%q want=%q", image.name, identity.Labels["org.opencontainers.image.revision"], sourceSHA)
		}
		state.runtimeImageDigests[image.prefix] = identity.RuntimeDigest
		if err := sharede2e.RecordRuntimeImageIdentity(image.artifact, identity.RuntimeDigest); err != nil {
			t.Fatalf("record imported %s runtime identity: %v", image.name, err)
		}
		imported++
	}
	if imported == 0 && os.Getenv(sharede2e.RequiredEnv) == "true" {
		t.Fatal("required composed chart runtime has no image archives to import")
	}
}

func chartDiagnostics(t *testing.T, state *chartState) {
	t.Helper()
	if state.cluster == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	err := (diagnostics.Collector{
		Executor: state.runner, Kubeconfig: state.cluster.Kubeconfig,
		OutputDir: state.diagnosticsDir, Redactor: state.redactor,
	}).Collect(ctx)
	if err != nil {
		t.Logf("best-effort diagnostics: %v", err)
	}
}

func cleanupForwards(t *testing.T, state *chartState) {
	t.Helper()
	for i := len(state.forwards) - 1; i >= 0; i-- {
		if err := state.forwards[i].Stop(); err != nil {
			t.Errorf("stop port-forward: %v", err)
		}
	}
	state.forwards = nil
}

func cleanupKind(t *testing.T, state *chartState) {
	t.Helper()
	if state.cluster == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err := state.cluster.Delete(ctx); err != nil {
		t.Errorf("delete Kind cluster: %v", err)
	}
}

func scenarioHooks() ([]sharede2e.Hook[*chartState], []sharede2e.Hook[*chartState]) {
	return []sharede2e.Hook[*chartState]{{Name: "shared-cluster-evidence", Run: chartDiagnostics}},
		[]sharede2e.Hook[*chartState]{
			{Name: "stop-port-forwards", Run: cleanupForwards},
			{Name: "delete-kind-cluster", Run: cleanupKind},
		}
}

func (state *chartState) kubectl(t *testing.T, timeout time.Duration, args ...string) string {
	t.Helper()
	stdout, stderr, err := state.client.KubectlSeparated(state.ctx, timeout, args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout, stderr)
	}
	if diagnostics := strings.TrimSpace(stderr); diagnostics != "" {
		t.Logf("kubectl %s diagnostics: %s", strings.Join(args, " "), diagnostics)
	}
	return strings.TrimSpace(stdout)
}

// kubectlOutput returns exact trimmed stdout for assertions while keeping stderr
// diagnostics in the error so failures stay actionable.
func (state *chartState) kubectlOutput(timeout time.Duration, args ...string) (string, error) {
	stdout, stderr, err := state.client.KubectlSeparated(state.ctx, timeout, args...)
	if err != nil {
		return strings.TrimSpace(stdout), fmt.Errorf("kubectl %s: %w\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout, stderr)
	}
	return strings.TrimSpace(stdout), nil
}

func (state *chartState) process(t *testing.T, timeout time.Duration, name string, args ...string) string {
	t.Helper()
	result, err := state.runner.Run(state.ctx, process.Command{Name: name, Args: args, Timeout: timeout})
	if err != nil {
		t.Fatalf("%s %s: %v\nstdout:\n%s\nstderr:\n%s", name, strings.Join(args, " "), err, result.Stdout, result.Stderr)
	}
	return strings.TrimSpace(result.Stdout)
}

func (state *chartState) writeValues(t *testing.T, name string, values map[string]any) string {
	t.Helper()
	data, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s values: %v", name, err)
	}
	return state.writeManifest(t, name+".json", string(append(data, '\n')))
}

func (state *chartState) writeManifest(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func assertCandidateImages(t *testing.T, state *chartState) {
	t.Helper()
	for _, image := range []struct {
		selector string
		prefix   string
	}{
		{selector: "app.kubernetes.io/name=control-plane,app.kubernetes.io/component=api", prefix: "CONTROL_PLANE"},
		{selector: "app.kubernetes.io/name=inference-gateway", prefix: "INFERENCE_GATEWAY"},
	} {
		repository := os.Getenv(image.prefix + "_IMAGE_REPO")
		tag := os.Getenv(image.prefix + "_IMAGE_TAG")
		runtimeDigest := state.runtimeImageDigests[image.prefix]
		if repository == "" && tag == "" && runtimeDigest == "" {
			continue
		}
		if repository == "" || tag == "" || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(runtimeDigest) {
			t.Fatalf("%s imported runtime request/digest identity is incomplete", image.prefix)
		}
		reference := repository + ":" + tag
		requested := state.kubectl(t, 30*time.Second, "get", "pods", "-n", testNamespace, "-l", image.selector,
			"-o", "jsonpath={.items[*].spec.containers[*].image} {.items[*].spec.initContainers[*].image}")
		if !slices.Contains(strings.Fields(requested), reference) {
			t.Fatalf("%s pods did not request exact composed reference %s: %s", image.prefix, reference, requested)
		}
		imageIDs := state.kubectl(t, 30*time.Second, "get", "pods", "-n", testNamespace, "-l", image.selector,
			"-o", "jsonpath={.items[*].status.containerStatuses[*].imageID} {.items[*].status.initContainerStatuses[*].imageID}")
		if !strings.Contains(imageIDs, runtimeDigest) {
			t.Fatalf("%s pods do not run imported runtime digest %s: %s", image.prefix, runtimeDigest, imageIDs)
		}
	}
}

func basePlatformValues() map[string]any {
	return map[string]any{
		"external-dns": map[string]any{"enabled": false},
		"minio":        map[string]any{"enabled": false},
		"control-plane": map[string]any{
			"artifact":   map[string]any{"enabled": false},
			"dispatch":   map[string]any{"enabled": false},
			"toolRunner": map[string]any{"enabled": false},
		},
	}
}

func runtimePlatformValues(t *testing.T) map[string]any {
	t.Helper()
	values := basePlatformValues()
	values["ingress-nginx"] = map[string]any{"enabled": false}
	values["metallb"] = map[string]any{"enabled": false}
	values["metallb-config"] = map[string]any{"enabled": false}
	applyRuntimeImages(t, values)
	return values
}

func applyRuntimeImages(t *testing.T, values map[string]any) {
	t.Helper()
	// Every workflow mode consumes the same composer-produced image identities;
	// chart stages never substitute owner-local or stale published fixture bytes.
	applyCandidateImages(values)
	if os.Getenv(sharede2e.RequiredEnv) == "true" {
		for _, prefix := range []string{"CONTROL_PLANE", "INFERENCE_GATEWAY"} {
			if os.Getenv(prefix+"_IMAGE_REPO") == "" || os.Getenv(prefix+"_IMAGE_TAG") == "" {
				t.Fatalf("composed runtime is missing %s image identity", prefix)
			}
		}
	}
}

func setPlatformImage(values map[string]any, component, repository, tag string) {
	componentValues, _ := values[component].(map[string]any)
	if componentValues == nil {
		componentValues = map[string]any{}
		values[component] = componentValues
	}
	image, _ := componentValues["image"].(map[string]any)
	if image == nil {
		image = map[string]any{}
		componentValues["image"] = image
	}
	if repository != "" {
		image["repository"] = repository
	}
	if tag != "" {
		image["tag"] = tag
	}
	image["pullPolicy"] = "Never"
}

func applyCandidateImages(values map[string]any) {
	for component, prefix := range map[string]string{
		"control-plane":     "CONTROL_PLANE",
		"inference-gateway": "INFERENCE_GATEWAY",
	} {
		repository, tag := os.Getenv(prefix+"_IMAGE_REPO"), os.Getenv(prefix+"_IMAGE_TAG")
		if repository != "" || tag != "" {
			setPlatformImage(values, component, repository, tag)
		}
	}
}

func TestUnitComposedRuntimeImagesRemainConstantAcrossChartTransitions(t *testing.T) {
	t.Setenv("ITERABASE_E2E_FIXTURE_MODE", string(sharede2e.FixtureSource))
	t.Setenv("CONTROL_PLANE_IMAGE_REPO", "registry.example/control-plane")
	t.Setenv("CONTROL_PLANE_IMAGE_TAG", "0.0.30")
	t.Setenv("INFERENCE_GATEWAY_IMAGE_REPO", "registry.example/inference-gateway")
	t.Setenv("INFERENCE_GATEWAY_IMAGE_TAG", "0.2.7")

	values := runtimePlatformValues(t)
	for component, want := range map[string]string{
		"control-plane":     "0.0.30",
		"inference-gateway": "0.2.7",
	} {
		componentValues := values[component].(map[string]any)
		image := componentValues["image"].(map[string]any)
		if got := image["tag"]; got != want {
			t.Fatalf("%s image tag = %v, want %s", component, got, want)
		}
	}
}

func (state *chartState) installSubstrate(t *testing.T, valueFiles ...string) {
	t.Helper()
	out, err := state.client.HelmUpgrade(state.ctx, kube.HelmOptions{
		Release: testRelease + "-cert-manager", Namespace: testNamespace, Chart: state.substrate,
		CreateNamespace: true, Wait: true, Timeout: 8 * time.Minute, ValueFiles: valueFiles,
	})
	if err != nil {
		t.Fatalf("install certificate substrate: %v\n%s", err, out)
	}
}

func (state *chartState) installLVMStorage(t *testing.T) {
	t.Helper()
	if state.lvmStorageReady {
		return
	}
	if err := state.cluster.ConfigureLVMStorage(state.ctx, state.lvmSubstrate.LocalPath, testNamespace, testRelease+"-lvm-storage", lvmStorageContract()); err != nil {
		t.Fatalf("install exact Kind OpenEBS LVM storage substrate: %v", err)
	}
	state.assertLVMSnapshotDependencyRBAC(t)
	state.lvmStorageReady = true
}

func (state *chartState) assertLVMSnapshotDependencyRBAC(t *testing.T) {
	t.Helper()
	checkCanI := func(subject, verb, resource string, allNamespaces bool, want string) {
		t.Helper()
		scope := "--namespace=" + testNamespace
		if allNamespaces {
			scope = "--all-namespaces"
		}
		state.process(t, 30*time.Second, "bash", "-ceu", `
set +e
out=$(kubectl --kubeconfig "$1" auth can-i "$2" "$3" "$4" "$5" 2>/dev/null)
rc=$?
set -e
test "$out" = "$6"
if test "$6" = yes; then test "$rc" = 0; else test "$rc" = 1; fi
`, "bounded-lvmsnapshot-rbac-check", state.cluster.Kubeconfig, verb, resource, "--as="+subject, scope, want)
	}

	controller := "system:serviceaccount:" + testNamespace + ":openebs-lvm-controller-sa"
	for _, check := range []struct {
		verb, resource string
		allNamespaces  bool
		want           string
	}{
		{verb: "list", resource: "secrets", allNamespaces: true, want: "no"},
		{verb: "get", resource: "secrets", want: "no"},
		{verb: "create", resource: "customresourcedefinitions.apiextensions.k8s.io", want: "no"},
		{verb: "delete", resource: "customresourcedefinitions.apiextensions.k8s.io", want: "no"},
		{verb: "get", resource: "lvmvolumes.local.openebs.io", want: "yes"},
		{verb: "get", resource: "volumesnapshotclasses.snapshot.storage.k8s.io", want: "no"},
	} {
		checkCanI(controller, check.verb, check.resource, check.allNamespaces, check.want)
	}
	for _, subject := range []string{
		controller,
		"system:serviceaccount:" + testNamespace + ":openebs-lvm-node-sa",
	} {
		for _, verb := range []string{"list", "watch"} {
			checkCanI(subject, verb, "lvmsnapshots.local.openebs.io", true, "yes")
		}
		for _, verb := range []string{"get", "create", "update", "patch", "delete"} {
			checkCanI(subject, verb, "lvmsnapshots.local.openebs.io", true, "no")
		}
	}
}

func (state *chartState) installPlatform(t *testing.T, timeout time.Duration, valueFiles ...string) {
	t.Helper()
	// Mirror Forge's pre-apply (DES-HOR-511-03): establish the exact chart's CRDs
	// (from its `crds/` directories AND CRDs rendered as ordinary template
	// resources, e.g. the MetalLB CRDs) and wait for Established before Helm, so
	// ordinary custom resources can be mapped. Idempotent. Returns whether MetalLB
	// is enabled (its rendered template CRDs are present).
	metallb := state.preapplyAllCRDs(t, valueFiles...)
	// Mirror Forge's DES-HOR-511 pre-apply: adopt any legacy hook-created MetalLB
	// pools/advertisements into the release before Helm upgrades, so the transition
	// from a hook-based predecessor preserves object UIDs instead of failing to
	// adopt them. Idempotent and a no-op when none exist.
	state.adoptMetalLBHookObjects(t)

	// Mirror Forge's bounded bootstrap (DES-HOR-511-04): when MetalLB is enabled
	// and this is a fresh install or an interrupted bootstrap still at the Ignore
	// policy, install with a bootstrap-only validationFailurePolicy=Ignore, wait for
	// the MetalLB controller + webhook backend to become ready, then converge the
	// release back to the steady-state Fail policy and assert it.
	if metallb {
		installed, _ := state.releaseInstalled(t)
		policy := state.metalLBValidationPolicy(t)
		if !installed || policy == metalLBPolicyIgnore {
			state.helmUpgrade(t, timeout, valueFiles, map[string]string{
				metalLBValidationPolicyValue: metalLBPolicyIgnore,
			})
			state.waitMetalLBAdmissionBackend(t, timeout)
		}
	}
	state.helmUpgrade(t, timeout, valueFiles, nil)
	if metallb {
		if final := state.metalLBValidationPolicy(t); final != metalLBPolicyFail {
			t.Fatalf("metallb validation failurePolicy not converged to %s: got %q", metalLBPolicyFail, final)
		}
	}
}

// helmUpgrade is a thin helper wrapping client.HelmUpgrade with the platform
// release/namespace and the given value files and --set-string overrides.
func (state *chartState) helmUpgrade(t *testing.T, timeout time.Duration, valueFiles []string, values map[string]string) {
	t.Helper()
	state.installLVMStorage(t)
	out, err := state.client.HelmUpgrade(state.ctx, kube.HelmOptions{
		Release: testRelease, Namespace: testNamespace, Chart: state.platform,
		ValueFiles: valueFiles, Values: values, Wait: true, Timeout: timeout,
	})
	if err != nil {
		t.Fatalf("install platform: %v\n%s", err, out)
	}
}

// releaseInstalled reports whether the platform Helm release exists.
func (state *chartState) releaseInstalled(t *testing.T) (bool, error) {
	out, err := state.runner.Run(state.ctx, process.Command{
		Name: "helm", Args: []string{"status", testRelease, "-n", testNamespace, "--kubeconfig", state.client.Kubeconfig},
		Timeout: 30 * time.Second, OutputName: "helm-status.log",
	})
	if err != nil {
		return false, nil // release not found
	}
	return strings.Contains(out.Stdout, "STATUS: deployed"), nil
}

// metalLBValidationPolicy reads the failurePolicy of the MetalLB admission webhook
// configuration ("" when absent, e.g. MetalLB disabled).
func (state *chartState) metalLBValidationPolicy(t *testing.T) string {
	out, err := state.kubectlOutput(30*time.Second, "get", "validatingwebhookconfiguration",
		metalLBWebhookConfigName, "-o", "jsonpath={.webhooks[0].failurePolicy}")
	if err != nil {
		return "" // absent => MetalLB disabled
	}
	return out
}

// waitMetalLBAdmissionBackend polls until the metallb controller deployment is
// Available and the webhook service has ready endpoints (or the timeout elapses).
func (state *chartState) waitMetalLBAdmissionBackend(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		replicas, _ := state.kubectlOutput(30*time.Second, "get", "deployment", "-n", testNamespace,
			"-l", "app.kubernetes.io/instance="+testRelease+",app.kubernetes.io/name=metallb,app.kubernetes.io/component=controller",
			"-o", "jsonpath={.items[0].status.readyReplicas}")
		endpoints, _ := state.kubectlOutput(30*time.Second, "get", "endpoints", "-n", testNamespace,
			"metallb-webhook-service", "-o", "jsonpath={.subsets[*].addresses[*].ip}")
		if replicas != "" && replicas != "0" && endpoints != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("metallb admission backend not ready after %s", timeout)
		}
		time.Sleep(3 * time.Second)
	}
}

// preapplyAllCRDs establishes the exact platform chart's CRDs before Helm by
// unioning `helm show crds` (CRDs in `crds/` directories) with the CRDs rendered
// as ordinary template resources (DES-HOR-511-03: the MetalLB CRDs), applying
// them server-side and waiting for Established. The rendered (template) CRDs are
// marked Helm-adoptable for the incoming release (DES-HOR-511-04) so a fresh
// `helm install` can adopt them. Returns whether MetalLB is enabled (rendered
// template CRDs present). CRD schemas contain credential-shaped property names
// that text redaction can corrupt, so the exact payload is written to a private
// temp file and applied with `-f`.
func (state *chartState) preapplyAllCRDs(t *testing.T, valueFiles ...string) bool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "platform-crds.yaml")
	showArgs := []string{"-o", "pipefail", "-c", `helm show crds "$@" > "$CRD_OUTPUT"`, "--"}
	showArgs = append(showArgs, helmChartArgs(state.platform)...)
	if _, err := state.runner.Run(state.ctx, process.Command{
		Name: "bash", Args: showArgs, Env: map[string]string{"CRD_OUTPUT": path}, Timeout: 2 * time.Minute,
	}); err != nil {
		t.Fatalf("extract chart CRDs (helm show crds): %v", err)
	}
	showCRDs, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chart CRDs (helm show crds): %v", err)
	}

	// Render CRDs owned as ordinary template resources (gated by their values, so
	// e.g. MetalLB CRDs appear only when MetalLB is enabled). Render with the
	// release namespace so `.Release.Namespace`-templated fields (e.g. the
	// bgppeers conversion webhook's clientConfig.service.namespace) resolve
	// identically to the subsequent `helm install`, avoiding an SSA conflict when
	// Helm adopts the pre-applied CRD.
	renderedPath := filepath.Join(t.TempDir(), "platform-rendered-crds.yaml")
	tmplArgs := []string{"-o", "pipefail", "-c", `helm template "$@" > "$TMPL_OUTPUT"`, "--"}
	tmplArgs = append(tmplArgs, helmChartArgs(state.platform)...)
	for _, f := range valueFiles {
		tmplArgs = append(tmplArgs, "-f", f)
	}
	tmplArgs = append(tmplArgs, "-n", testNamespace)
	if _, err := state.runner.Run(state.ctx, process.Command{
		Name: "bash", Args: tmplArgs, Env: map[string]string{"TMPL_OUTPUT": renderedPath}, Timeout: 3 * time.Minute,
	}); err != nil {
		t.Fatalf("render chart CRDs (helm template): %v", err)
	}
	rendered, err := os.ReadFile(renderedPath)
	if err != nil {
		t.Fatalf("read rendered chart CRDs: %v", err)
	}
	// DES-HOR-511-03/04 pre-apply (mirrors Forge's applyChartCRDs):
	//  - Every crds/-directory CRD (surfaced by `helm show crds`, e.g.
	//    observability/Prometheus and external-dns) is preserved in the pre-apply
	//    set; Helm only installs crds/-dir CRDs on an initial install, so this is
	//    what makes an operator-feature-enable upgrade deterministic.
	//  - Only the MetalLB rendered template CRDs receive Helm ownership and are
	//    added to the pre-apply set. Other rendered template CRDs (e.g. control-
	//    plane's agentpools) are left to Helm: pre-applying them without Helm
	//    ownership makes a fresh `helm install` fail to import them.
	metallbRendered, err := selectMetalLBCRDs(string(rendered))
	if err != nil {
		t.Fatalf("select MetalLB rendered CRDs: %v", err)
	}
	renderedOwned, err := markRenderedCRDsOwned(metallbRendered, testRelease, testNamespace)
	if err != nil {
		t.Fatalf("mark rendered MetalLB CRDs Helm-adoptable: %v", err)
	}

	combined := string(showCRDs)
	if renderedOwned != "" {
		combined += "\n---\n" + renderedOwned
	}
	selected, err := selectBundledCRDs(combined)
	if err != nil {
		t.Fatalf("select authoritative platform CRDs: %v", err)
	}
	names, err := bundledCRDNames(combined)
	if err != nil {
		t.Fatalf("collect platform CRD names: %v", err)
	}
	if err := os.WriteFile(path, []byte(selected), 0o600); err != nil {
		t.Fatalf("write selected platform CRDs: %v", err)
	}
	state.kubectl(t, 3*time.Minute, "apply", "--server-side", "--force-conflicts", "--field-manager="+transitionFieldManager, "-f", path)
	for _, name := range names {
		state.kubectl(t, 3*time.Minute, "wait", "--for=condition=Established", "crd/"+name, "--timeout=2m")
	}
	return metallbRendered != ""
}

// adoptMetalLBHookObjects transfers ownership of any MetalLB IPAddressPool /
// L2Advertisement created by a hook-era (pre-DES-HOR-511) chart into the current
// release before Helm renders them as ordinary resources. Only this release's
// objects (matching the instance label) are touched, only ownership/hook metadata
// changes, and the step is best-effort (a no-op when the kinds or objects are
// absent, e.g. cloud installs).
func (state *chartState) adoptMetalLBHookObjects(t *testing.T) {
	t.Helper()
	sel := "app.kubernetes.io/instance=" + testRelease
	for _, kind := range []string{"ipaddresspool", "l2advertisement"} {
		out, err := state.kubectlOutput(30*time.Second, "get", kind, "-n", testNamespace, "-l", sel, "-o", "name")
		if err != nil {
			continue // kind absent (cloud/older chart) => nothing to adopt
		}
		resources := strings.Fields(out)
		if len(resources) == 0 {
			continue
		}
		args := append([]string{"annotate", "--overwrite", "-n", testNamespace}, resources...)
		args = append(args,
			"meta.helm.sh/release-name="+testRelease,
			"meta.helm.sh/release-namespace="+testNamespace,
			"helm.sh/hook-",
			"helm.sh/hook-weight-",
		)
		if _, err := state.client.Kubectl(state.ctx, 30*time.Second, args...); err != nil {
			t.Fatalf("adopt MetalLB %s ownership: %v", kind, err)
		}
	}
}

func (state *chartState) waitForPods(t *testing.T, selector string, timeout time.Duration) {
	t.Helper()
	err := poll.Until(state.ctx, timeout, 3*time.Second, func(context.Context) (bool, string, error) {
		out, err := state.kubectlOutput(30*time.Second, "get", "pods", "-n", testNamespace, "-l", selector, "-o", "name")
		if err != nil {
			return false, "list pods", err
		}
		return strings.TrimSpace(out) != "", strings.TrimSpace(out), nil
	})
	if err != nil {
		t.Fatalf("pods for %q did not appear: %v", selector, err)
	}
	state.kubectl(t, timeout+time.Minute, "wait", "--for=condition=Ready", "pod", "-n", testNamespace, "-l", selector, "--timeout", timeout.String())
}

func (state *chartState) firstPod(t *testing.T, selector string) string {
	t.Helper()
	var pod string
	err := poll.Until(state.ctx, 2*time.Minute, 2*time.Second, func(context.Context) (bool, string, error) {
		out, err := state.kubectlOutput(30*time.Second, "get", "pods", "-n", testNamespace, "-l", selector, "-o", "jsonpath={.items[0].metadata.name}")
		if err != nil {
			return false, "get first pod", err
		}
		pod = strings.TrimSpace(out)
		return pod != "", pod, nil
	})
	if err != nil {
		t.Fatalf("find pod for %q: %v", selector, err)
	}
	return pod
}

func (state *chartState) forward(t *testing.T, resource string, port int, scheme string) *kube.Forward {
	t.Helper()
	forward, err := state.client.PortForward(state.ctx, testNamespace, resource, port, scheme)
	if err != nil {
		t.Fatalf("port-forward %s: %v", resource, err)
	}
	state.forwards = append(state.forwards, forward)
	return forward
}

func (state *chartState) stopForward(t *testing.T, forward *kube.Forward) {
	t.Helper()
	if err := forward.Stop(); err != nil {
		t.Fatalf("stop port-forward: %v", err)
	}
	for i, candidate := range state.forwards {
		if candidate == forward {
			state.forwards = append(state.forwards[:i], state.forwards[i+1:]...)
			return
		}
	}
}

func decodeSecretValue(t *testing.T, state *chartState, name, key string) []byte {
	t.Helper()
	jsonPath := fmt.Sprintf("jsonpath={.data.%s}", strings.ReplaceAll(key, ".", `\.`))
	encoded := state.kubectl(t, 30*time.Second, "get", "secret", name, "-n", testNamespace, "-o", jsonPath)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode %s/%s: %v", name, key, err)
	}
	return decoded
}

func verifiedClient(t *testing.T, ca []byte, serverName string) *http.Client {
	t.Helper()
	client, err := httpx.TLSClient(httpx.TLSOptions{Timeout: 15 * time.Second, RootCAPEM: ca, ServerName: serverName})
	if err != nil {
		t.Fatalf("create verified TLS client: %v", err)
	}
	return client
}

func verifiedDialClient(t *testing.T, ca []byte, serverName, address string) *http.Client {
	t.Helper()
	client := verifiedClient(t, ca, serverName)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("verified client transport has unexpected type")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, address)
	}
	return client
}

func requireHTTP(t *testing.T, client *http.Client, method, url string, configure func(*http.Request), want int) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", url, err)
	}
	if configure != nil {
		configure(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if readErr != nil {
		t.Fatalf("read %s: %v", url, readErr)
	}
	if len(body) > 2<<20 {
		t.Fatalf("read %s: response exceeds 2 MiB", url)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s status=%d want=%d body=%s", url, resp.StatusCode, want, stateSafeBody(body))
	}
	return body
}

func stateSafeBody(body []byte) string {
	const limit = 1000
	value := string(body)
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}

func waitHTTPReady(ctx context.Context, client *http.Client, url string, timeout time.Duration) error {
	return poll.Until(ctx, timeout, 2*time.Second, func(context.Context) (bool, string, error) {
		resp, err := client.Get(url)
		if err != nil {
			return false, err.Error(), nil
		}
		_ = resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300, fmt.Sprintf("status %d", resp.StatusCode), nil
	})
}
