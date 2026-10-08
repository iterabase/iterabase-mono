package e2e_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/kube"
)

// nMinusOnePlatform is the platform values the N-1 upgrade installs on both the
// published N-1 chart and the head chart, layered before the runtime images.
var nMinusOnePlatform = platformValues{platformValuesBase, platformValuesRuntime, "test/e2e/values/n-1-upgrade.yaml"}

// nMinusOneChart is one published N-1 chart: an exact OCI reference, the
// SHA-256 of its pulled archive, and the verified local archive once resolved.
type nMinusOneChart struct {
	Chart      string
	Reference  string
	Repository string
	Version    string
	Checksum   string
	Archive    string
}

// nMinusOneBaseline is the newest published LVM-era chart trio. The workflow
// resolves it and passes it in; the scenario never discovers or floats it.
type nMinusOneBaseline struct {
	Version     string
	CertManager nMinusOneChart
	LVMStorage  nMinusOneChart
	Platform    nMinusOneChart
}

// nMinusOneEnvironment names the environment contract for each baseline chart:
// ITERABASE_E2E_N1_<STEM>_REFERENCE (oci://…/<chart>:<version>) and
// ITERABASE_E2E_N1_<STEM>_SHA256 (the pulled archive checksum) are required;
// ITERABASE_E2E_N1_<STEM>_ARCHIVE optionally supplies an already-pulled archive.
var nMinusOneEnvironment = []struct {
	stem  string
	chart string
}{
	{stem: "CERT_MANAGER", chart: "cert-manager-substrate"},
	{stem: "LVM_STORAGE", chart: "lvm-storage-substrate"},
	{stem: "PLATFORM", chart: "iterabase-platform"},
}

var (
	exactChartVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	archiveChecksum   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func nMinusOneEnv(stem, field string) string {
	return "ITERABASE_E2E_N1_" + stem + "_" + field
}

// parseNMinusOneBaseline validates the complete N-1 environment contract and
// fails closed on any missing, mutable, mismatched, or non-canonical input.
func parseNMinusOneBaseline(lookup func(string) string) (nMinusOneBaseline, error) {
	var baseline nMinusOneBaseline
	var missing []string
	charts := make([]nMinusOneChart, 0, len(nMinusOneEnvironment))
	for _, entry := range nMinusOneEnvironment {
		referenceKey, checksumKey := nMinusOneEnv(entry.stem, "REFERENCE"), nMinusOneEnv(entry.stem, "SHA256")
		reference, checksum := strings.TrimSpace(lookup(referenceKey)), strings.TrimSpace(lookup(checksumKey))
		if reference == "" {
			missing = append(missing, referenceKey)
		}
		if checksum == "" {
			missing = append(missing, checksumKey)
		}
		if reference == "" || checksum == "" {
			continue
		}
		if !strings.HasPrefix(reference, "oci://") {
			return nMinusOneBaseline{}, fmt.Errorf("%s=%q must be an exact oci:// chart reference", referenceKey, reference)
		}
		chart, repository, version, err := parsePublishedChartReference(reference)
		if err != nil {
			return nMinusOneBaseline{}, fmt.Errorf("%s: %w", referenceKey, err)
		}
		if chart != entry.chart {
			return nMinusOneBaseline{}, fmt.Errorf("%s names chart %q, want %q", referenceKey, chart, entry.chart)
		}
		if !exactChartVersion.MatchString(version) {
			return nMinusOneBaseline{}, fmt.Errorf("%s version %q is not an exact published semantic version", referenceKey, version)
		}
		if !archiveChecksum.MatchString(checksum) {
			return nMinusOneBaseline{}, fmt.Errorf("%s must be a lowercase 64-hex SHA-256 archive checksum", checksumKey)
		}
		charts = append(charts, nMinusOneChart{
			Chart: chart, Reference: reference, Repository: repository, Version: version, Checksum: checksum,
			Archive: strings.TrimSpace(lookup(nMinusOneEnv(entry.stem, "ARCHIVE"))),
		})
	}
	if len(missing) != 0 {
		return nMinusOneBaseline{}, fmt.Errorf("N-1 baseline is unresolved; the workflow must supply %s", strings.Join(missing, ", "))
	}
	for _, chart := range charts[1:] {
		if chart.Version != charts[0].Version {
			return nMinusOneBaseline{}, fmt.Errorf("N-1 charts must share one published version: %s=%s %s=%s",
				charts[0].Chart, charts[0].Version, chart.Chart, chart.Version)
		}
	}
	baseline.Version = charts[0].Version
	baseline.CertManager, baseline.LVMStorage, baseline.Platform = charts[0], charts[1], charts[2]
	return baseline, nil
}

// headNotOlderError rejects a head chart older than its N-1 baseline. A source
// head may still carry the published version before its release bump.
func headNotOlderError(head, baseline string) error {
	comparison, err := compareNumericVersions(head, baseline)
	if err != nil {
		return fmt.Errorf("compare head chart %q with N-1 %q: %w", head, baseline, err)
	}
	if comparison < 0 {
		return fmt.Errorf("head chart version %s is older than N-1 baseline %s", head, baseline)
	}
	return nil
}

func nMinusOneUpgradeScenario() sharede2e.Definition {
	diagnostics, cleanup := scenarioHooks()
	return sharede2e.Define(sharede2e.Scenario[*chartState]{
		Metadata: chartScenarioMetadata(
			"n-1-upgrade",
			"Installs the newest published N-1 certificate, LVM storage, and platform charts on fresh Kind, seeds persisted state, upgrades to the head chart set, proves persisted state, immutable Secrets, PVCs, the Helm-owned provisioner Job, schema ownership, and rollout health, reapplies head without rolling workloads, applies N-1 back with state preserved, and recovers forward to head.",
			"test-e2e-n-1-upgrade", 45,
			[]string{"HOR-415", "HOR-418", "HOR-475", "HOR-530", "HOR-590"},
			[]string{"control-plane-chart", "inference-gateway-chart", "iterabase-platform-chart"},
			append(substrateRenders("iterabase"), nMinusOnePlatform.render()),
		),
		NewState: newChartState,
		Stages: []sharede2e.Stage[*chartState]{
			{Name: "create-kind", Run: createKindStage},
			{Name: "import-runtime-images", DependsOn: []string{"create-kind"}, Run: importRuntimeImagesStage},
			{Name: "resolve-n-1-baseline", DependsOn: []string{"import-runtime-images"}, Run: resolveNMinusOneBaselineStage},
			{Name: "install-n-1-charts", DependsOn: []string{"resolve-n-1-baseline"}, Run: installNMinusOneChartsStage},
			{Name: "seed-persisted-state", DependsOn: []string{"install-n-1-charts"}, Run: seedPersistedStateStage},
			{Name: "capture-n-1-state", DependsOn: []string{"seed-persisted-state"}, Run: captureNMinusOneStateStage},
			{Name: "upgrade-head-charts", DependsOn: []string{"capture-n-1-state"}, Run: applyHeadChartsStage},
			{Name: "assert-upgrade-contract", DependsOn: []string{"upgrade-head-charts"}, Run: assertUpgradeContractStage},
			{Name: "capture-head-state", DependsOn: []string{"assert-upgrade-contract"}, Run: captureHeadStateStage},
			{Name: "reapply-head-charts", DependsOn: []string{"capture-head-state"}, Run: applyHeadChartsStage},
			{Name: "assert-idempotent-reapply", DependsOn: []string{"reapply-head-charts"}, Run: assertIdempotentReapplyStage},
			{Name: "roll-back-n-1-charts", DependsOn: []string{"assert-idempotent-reapply"}, Run: applyNMinusOneChartsStage},
			{Name: "assert-rollback-boundary", DependsOn: []string{"roll-back-n-1-charts"}, Run: assertRollbackBoundaryStage},
			{Name: "forward-head-charts", DependsOn: []string{"assert-rollback-boundary"}, Run: applyHeadChartsStage},
			{Name: "assert-forward-recovery", DependsOn: []string{"forward-head-charts"}, Run: assertForwardRecoveryStage},
		},
		Diagnostics: diagnostics,
		Cleanup:     cleanup,
	})
}

func resolveNMinusOneBaselineStage(t *testing.T, state *chartState) {
	t.Helper()
	baseline, err := parseNMinusOneBaseline(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := headNotOlderError(currentChartVersion(t, state, state.platform), baseline.Version); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(state.outputDir, "n-1-baseline")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create N-1 baseline directory: %v", err)
	}
	for _, chart := range []*nMinusOneChart{&baseline.CertManager, &baseline.LVMStorage, &baseline.Platform} {
		if chart.Archive == "" {
			// Retrieve exactly the pinned reference; the checksum below is the
			// identity, so a moved tag fails closed instead of being accepted.
			state.process(t, 4*time.Minute, "helm", "pull", chart.Repository, "--version", chart.Version, "--destination", directory)
			chart.Archive = filepath.Join(directory, chart.Chart+"-"+chart.Version+".tgz")
		}
		archive, err := filepath.Abs(chart.Archive)
		if err != nil {
			t.Fatalf("resolve N-1 %s archive: %v", chart.Chart, err)
		}
		if err := verifyArchiveChecksum(archive, chart.Checksum); err != nil {
			t.Fatalf("N-1 %s (%s) failed checksum verification: %v", chart.Chart, chart.Reference, err)
		}
		chart.Archive = archive
	}
	state.baseline = &baseline
}

// nMinusOneCharts returns the verified N-1 archives as exact local charts in the
// scenario's fixture mode, so they apply through the same Forge-equivalent path.
func nMinusOneCharts(t *testing.T, state *chartState) (certificate, lvmStorage, platform kube.Chart) {
	t.Helper()
	if state.baseline == nil {
		t.Fatal("N-1 baseline is unresolved")
	}
	chart := func(archive string) kube.Chart { return kube.Chart{Mode: state.platform.Mode, LocalPath: archive} }
	return chart(state.baseline.CertManager.Archive), chart(state.baseline.LVMStorage.Archive), chart(state.baseline.Platform.Archive)
}

func nMinusOneValueFiles(t *testing.T, state *chartState) []string {
	t.Helper()
	return append(nMinusOnePlatform.files(state), state.writeValues(t, "n-1-runtime", runtimeImageValues(t)))
}

// applyChartSet applies one exact chart trio in Forge's order: certificate
// substrate, LVM storage substrate, then the platform with CRD pre-apply.
func applyChartSet(t *testing.T, state *chartState, certificate, lvmStorage, platform kube.Chart) {
	t.Helper()
	state.installSubstrateChart(t, certificate)
	state.applyLVMStorage(t, lvmStorage)
	state.installPlatformChart(t, platform, 18*time.Minute, nMinusOneValueFiles(t, state)...)
}

func applyNMinusOneChartsStage(t *testing.T, state *chartState) {
	t.Helper()
	certificate, lvmStorage, platform := nMinusOneCharts(t, state)
	applyChartSet(t, state, certificate, lvmStorage, platform)
}

func installNMinusOneChartsStage(t *testing.T, state *chartState) {
	t.Helper()
	applyNMinusOneChartsStage(t, state)
	assertChartSetVersions(t, state, state.baseline.Version, state.baseline.Version, state.baseline.Version)
	assertLifecycleHealth(t, state)
	assertReleaseMechanics(t, state)
}

func applyHeadChartsStage(t *testing.T, state *chartState) {
	t.Helper()
	applyChartSet(t, state, state.substrate, state.lvmSubstrate, state.platform)
	assertCandidateImages(t, state)
}

// assertChartSetVersions proves each release currently runs the exact chart.
func assertChartSetVersions(t *testing.T, state *chartState, certificate, lvmStorage, platform string) {
	t.Helper()
	assertReleaseChartVersion(t, state, testRelease+"-cert-manager", "cert-manager-substrate", certificate)
	assertReleaseChartVersion(t, state, testRelease+"-lvm-storage", "lvm-storage-substrate", lvmStorage)
	assertReleaseChartVersion(t, state, testRelease, "iterabase-platform", platform)
}

func assertHeadChartSet(t *testing.T, state *chartState) {
	t.Helper()
	assertChartSetVersions(t, state,
		currentChartVersion(t, state, state.substrate),
		currentChartVersion(t, state, state.lvmSubstrate),
		currentChartVersion(t, state, state.platform),
	)
}

func assertChartSetRevision(t *testing.T, state *chartState, revision int) {
	t.Helper()
	for _, release := range []string{testRelease + "-cert-manager", testRelease + "-lvm-storage", testRelease} {
		assertReleaseRevision(t, state, release, revision)
	}
}

func captureNMinusOneStateStage(t *testing.T, state *chartState) {
	state.snapshots["n-1"] = captureLifecycleSnapshot(t, state)
}

func captureHeadStateStage(t *testing.T, state *chartState) {
	snapshot := captureLifecycleSnapshot(t, state)
	snapshot.ArtifactProvisionerJob = currentArtifactProvisionerJobIdentity(t, state)
	state.snapshots["head"] = snapshot
}

func assertUpgradeContractStage(t *testing.T, state *chartState) {
	assertHeadChartSet(t, state)
	assertChartSetRevision(t, state, 2)
	assertSchemaOwnership(t, state)
	assertLifecycleHealth(t, state)
	assertReleaseMechanics(t, state)
	assertPersistedState(t, state)
	assertRetainedState(t, state.snapshots["n-1"], captureLifecycleSnapshot(t, state), false)
}

func assertIdempotentReapplyStage(t *testing.T, state *chartState) {
	after := captureLifecycleSnapshot(t, state)
	after.ArtifactProvisionerJob = currentArtifactProvisionerJobIdentity(t, state)
	assertRetainedState(t, state.snapshots["head"], after, true)
	assertPersistedState(t, state)
	assertLifecycleHealth(t, state)
	assertChartSetRevision(t, state, 3)
}

func assertRollbackBoundaryStage(t *testing.T, state *chartState) {
	for _, release := range []struct{ name, chart string }{
		{testRelease + "-cert-manager", "cert-manager-substrate"},
		{testRelease + "-lvm-storage", "lvm-storage-substrate"},
		{testRelease, "iterabase-platform"},
	} {
		assertRollbackReleaseHistory(t, state, release.name, release.chart, state.baseline.Version, 4)
	}
	assertPersistedState(t, state)
	assertRetainedState(t, state.snapshots["head"], captureLifecycleSnapshot(t, state), false)
	assertLifecycleHealth(t, state)
}

func assertForwardRecoveryStage(t *testing.T, state *chartState) {
	assertHeadChartSet(t, state)
	assertChartSetRevision(t, state, 5)
	assertPersistedState(t, state)
	assertRetainedState(t, state.snapshots["head"], captureLifecycleSnapshot(t, state), false)
	assertLifecycleHealth(t, state)
	assertSchemaOwnership(t, state)
}

func nMinusOneTestEnvironment() map[string]string {
	const checksum = "86b0f23012fb549e47b2afb19b16184c760adcd6fe930bc3d817e031f1d280dd"
	return map[string]string{
		"ITERABASE_E2E_N1_CERT_MANAGER_REFERENCE": "oci://ghcr.io/nunocgoncalves/iterabase-charts/cert-manager-substrate:0.4.6",
		"ITERABASE_E2E_N1_CERT_MANAGER_SHA256":    checksum,
		"ITERABASE_E2E_N1_LVM_STORAGE_REFERENCE":  "oci://ghcr.io/nunocgoncalves/iterabase-charts/lvm-storage-substrate:0.4.6",
		"ITERABASE_E2E_N1_LVM_STORAGE_SHA256":     checksum,
		"ITERABASE_E2E_N1_PLATFORM_REFERENCE":     "oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform:0.4.6",
		"ITERABASE_E2E_N1_PLATFORM_SHA256":        checksum,
	}
}

func TestUnitNMinusOneBaselineFailsClosed(t *testing.T) {
	valid := nMinusOneTestEnvironment()
	baseline, err := parseNMinusOneBaseline(func(key string) string { return valid[key] })
	if err != nil {
		t.Fatalf("valid N-1 baseline rejected: %v", err)
	}
	if baseline.Version != "0.4.6" || baseline.Platform.Chart != "iterabase-platform" ||
		baseline.Platform.Repository != "oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform" ||
		baseline.CertManager.Chart != "cert-manager-substrate" || baseline.LVMStorage.Chart != "lvm-storage-substrate" ||
		baseline.Platform.Archive != "" {
		t.Fatalf("parsed N-1 baseline = %+v", baseline)
	}
	for name, mutate := range map[string]func(map[string]string){
		"no environment":    func(env map[string]string) { clear(env) },
		"missing reference": func(env map[string]string) { delete(env, "ITERABASE_E2E_N1_LVM_STORAGE_REFERENCE") },
		"missing checksum":  func(env map[string]string) { delete(env, "ITERABASE_E2E_N1_PLATFORM_SHA256") },
		"floating latest": func(env map[string]string) {
			env["ITERABASE_E2E_N1_PLATFORM_REFERENCE"] = "oci://ghcr.io/x/iterabase-platform:latest"
		},
		"no version": func(env map[string]string) {
			env["ITERABASE_E2E_N1_PLATFORM_REFERENCE"] = "oci://ghcr.io/x/iterabase-platform"
		},
		"prerelease version": func(env map[string]string) {
			env["ITERABASE_E2E_N1_PLATFORM_REFERENCE"] = "oci://ghcr.io/x/iterabase-platform:0.4.7-rc.1"
		},
		"not oci": func(env map[string]string) {
			env["ITERABASE_E2E_N1_PLATFORM_REFERENCE"] = "https://ghcr.io/x/iterabase-platform:0.4.6"
		},
		"wrong chart": func(env map[string]string) {
			env["ITERABASE_E2E_N1_CERT_MANAGER_REFERENCE"] = "oci://ghcr.io/x/iterabase-platform:0.4.6"
		},
		"version mismatch": func(env map[string]string) {
			env["ITERABASE_E2E_N1_LVM_STORAGE_REFERENCE"] = "oci://ghcr.io/x/lvm-storage-substrate:0.4.5"
		},
		"uppercase checksum": func(env map[string]string) {
			env["ITERABASE_E2E_N1_PLATFORM_SHA256"] = strings.ToUpper(valid["ITERABASE_E2E_N1_PLATFORM_SHA256"])
		},
		"short checksum": func(env map[string]string) { env["ITERABASE_E2E_N1_PLATFORM_SHA256"] = "86b0f230" },
	} {
		t.Run(name, func(t *testing.T) {
			env := nMinusOneTestEnvironment()
			mutate(env)
			if _, err := parseNMinusOneBaseline(func(key string) string { return env[key] }); err == nil {
				t.Fatal("intentional N-1 baseline break passed")
			}
		})
	}
	if _, err := parseNMinusOneBaseline(func(string) string { return "" }); err == nil ||
		!strings.Contains(err.Error(), "ITERABASE_E2E_N1_PLATFORM_REFERENCE") {
		t.Fatalf("missing baseline error does not name the contract: %v", err)
	}
}

func TestUnitNMinusOneArchiveChecksumAndHeadOrder(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "iterabase-platform-0.4.6.tgz")
	if err := os.WriteFile(archive, []byte("chart"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("chart"))
	if err := verifyArchiveChecksum(archive, hex.EncodeToString(digest[:])); err != nil {
		t.Fatalf("matching archive rejected: %v", err)
	}
	if err := verifyArchiveChecksum(archive, strings.Repeat("0", 64)); err == nil {
		t.Fatal("mismatched archive checksum passed")
	}
	for _, head := range []string{"0.4.6", "0.4.7", "0.5.0"} {
		if err := headNotOlderError(head, "0.4.6"); err != nil {
			t.Fatalf("head %s rejected: %v", head, err)
		}
	}
	for _, head := range []string{"0.4.5", "0.3.19", "0.4.7-rc.1"} {
		if err := headNotOlderError(head, "0.4.6"); err == nil {
			t.Fatalf("older or unsupported head %s passed", head)
		}
	}
}
