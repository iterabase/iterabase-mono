package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nunocgoncalves/iterabase-mono/forge/test/e2e/internal/remotecluster"
)

// runOverlayStage upgrades the composed CPU fixture from the migration source
// to the current platform through Forge's production ordering: clone the
// fixture overlay, install the certificate substrate, establish an exact Flux
// artifact, migrate certificate ownership, apply the platform, then reconcile
// its CRs.
//
// The overlay is the minimal fixture under ./overlay, committed on the host as
// a file:// repository with the run's values and served to Flux over read-only
// node SSH (DES-HOR-632-01). No overlay token is involved.
func runOverlayStage(t *testing.T, state *cpuFixtureState) {
	if _, ok := os.LookupEnv("FORGE_OVERLAY_TOKEN"); ok {
		t.Fatal("FORGE_OVERLAY_TOKEN must be unset; the fixture overlay is host-local")
	}
	prepareCandidateChart(t, state.ip, state.privKeyPath)
	plan := prepareCandidateOverlay(t, state.runID, state.ip, state.privKeyPath)
	candidateConfig := writeCurrentOverlayForgeConfig(
		t, state.runID, state.ip, state.privKeyPath, state.chartVersion, plan,
	)
	if state.freshInstall {
		bootstrap := applyOnceArgs(t, state.forgeBin, state.forgeHome, candidateConfig,
			"--skip-chart", "--skip-gpu", "--skip-overlay", "--skip-secrets", "--skip-flux")
		assertApplyMarkers(t, bootstrap, "action:     install", "node ready: true", "data storage: iterabase-data", "LVM storage substrate applied: false")
	}
	state.runtimeImageDigests = prepareCandidateImages(t, state.ip, state.privKeyPath)
	preparePinnedImageCache(t, &state.diagnostics, state.ip, state.privKeyPath, "cpu")
	out := applyOnce(t, state.forgeBin, state.forgeHome, candidateConfig)
	markers := []string{"action:     skip", "node ready: true", "data storage: iterabase-data",
		"LVM storage ready: true", "certificate substrate applied: true", "LVM storage substrate applied: true",
		"chart applied: true", "overlay applied: true", "overlay commit:", "flux installed: true", "gitrepository: ready=True"}
	assertApplyMarkers(t, out, markers...)
	t.Logf("apply output:\n%s", out)
	candidateCluster := remotecluster.Use(t, filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml"))
	assertCandidateImageDigests(t, candidateCluster, "iterabase-system", state.runtimeImageDigests,
		controlPlaneDigestEnv, inferenceGatewayDigestEnv, toolRunnerDigestEnv)

	// The cloned overlay dir exists on the host (a real clone happened).
	overlayDir := "/var/lib/forge/overlay/" + state.runID
	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()
	if _, err := sshOutput(sc, "test -d "+overlayDir+"/.git && test -f "+overlayDir+"/values.yaml"); err != nil {
		t.Fatalf("overlay clone not present on host at %s: %v", overlayDir, err)
	}
}

// writeCurrentOverlayForgeConfig uses the public exact-Flux fixture. Candidate
// image values affect only Forge's checkout and never change this source identity.
func writeCurrentOverlayForgeConfig(
	t *testing.T, name, ip, keyPath, chartVersion string, plan candidateOverlayPlan,
) string {
	return writeForgeConfigSpec(t, forgeConfigSpec{
		Name: name, Address: ip, SSHKeyPath: keyPath, RunLabel: true, DualStack: true,
		ChartVersion:    chartVersion,
		ChartRepository: os.Getenv("FORGE_E2E_CHART_REPOSITORY"),
		OverlayRepo:     plan.repository,
		OverlayRef:      plan.ref,
		Flux:            plan.flux,
	})
}
