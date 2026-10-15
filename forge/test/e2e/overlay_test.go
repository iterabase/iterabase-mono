package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nunocgoncalves/iterabase-mono/forge/test/e2e/internal/remotecluster"
)

// runOverlayStage installs the current platform on the fresh CPU host through
// Forge's production ordering: render the fixture overlay locally, stage its
// value files, install the certificate and LVM substrates, write the customer
// tools archive, apply the platform chart, then apply the overlay objects
// (DES-HOR-593-03/05). It then proves exactly what reached the installation.
func runOverlayStage(t *testing.T, state *cpuFixtureState) {
	runOverlayInstall(t, state, nil)
}

// runOverlayInstall is runOverlayStage with an optional step that runs on the
// prepared host immediately before the platform apply that must succeed.
func runOverlayInstall(t *testing.T, state *cpuFixtureState, beforeApply func(candidateOverlayPlan)) {
	prepareCandidateChart(t, state.ip, state.privKeyPath)
	plan := prepareCandidateOverlay(t)
	candidateConfig := writeCurrentOverlayForgeConfig(
		t, state.runID, state.ip, state.privKeyPath, state.chartVersion, plan,
	)
	if state.freshInstall {
		bootstrap := applyOnceArgs(t, state.forgeBin, state.forgeHome, candidateConfig,
			"--skip-chart", "--skip-gpu", "--skip-overlay", "--skip-secrets")
		assertApplyMarkers(t, bootstrap, "action:     install", "node ready: true", "data storage: iterabase-data", "LVM storage substrate applied: false")
	}
	state.runtimeImageDigests = prepareCandidateImages(t, state.ip, state.privKeyPath)
	preparePinnedImageCache(t, &state.diagnostics, state.ip, state.privKeyPath, "cpu")
	if beforeApply != nil {
		beforeApply(plan)
	}
	out := applyOnce(t, state.forgeBin, state.forgeHome, candidateConfig)
	assertApplyMarkers(t, out, "action:     skip", "node ready: true", "data storage: iterabase-data",
		"LVM storage ready: true", "certificate substrate applied: true", "LVM storage substrate applied: true",
		"chart applied: true", "overlay revision: sha256:", "overlay tools:  sha256:")
	t.Logf("apply output:\n%s", out)
	candidateCluster := remotecluster.Use(t, filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml"))
	assertCandidateImageDigests(t, candidateCluster, "iterabase-system", state.runtimeImageDigests,
		controlPlaneDigestEnv, inferenceGatewayDigestEnv, toolRunnerDigestEnv)

	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()
	state.overlay = assertOverlayDelivered(t, sc, state.runID, plan, out)
}

// writeCurrentOverlayForgeConfig points forge.yaml at the run's local overlay
// copy. Candidate image values live in that copy's last value file.
func writeCurrentOverlayForgeConfig(
	t *testing.T, name, ip, keyPath, chartVersion string, plan candidateOverlayPlan,
) string {
	return writeForgeConfigSpec(t, forgeConfigSpec{
		Name: name, Address: ip, SSHKeyPath: keyPath, RunLabel: true, DualStack: true,
		ChartVersion:      chartVersion,
		ChartRepository:   os.Getenv("FORGE_E2E_CHART_REPOSITORY"),
		OverlayPath:       plan.path,
		OverlayValueFiles: plan.valueFiles,
		OverlayToolDirs:   plan.toolDirs,
	})
}
