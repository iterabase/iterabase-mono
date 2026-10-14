package e2e

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	// candidateOverlayFixture is the versioned fixture overlay every real-host
	// run deploys. Forge renders a per-run copy of it on the operator machine
	// (DES-HOR-593-03); nothing about the overlay is prepared on the host.
	candidateOverlayFixture = "overlay"
	// candidateOverlayRunValues carries the run's exact image identities. It is
	// the last value file, so it wins over the fixture's own values.
	candidateOverlayRunValues = "values.e2e.yaml"
	workspaceBehaviorEnv      = "FORGE_E2E_WORKSPACE_BEHAVIOR"
)

var (
	candidateOverlayValueFiles = []string{"values.yaml", "values.client.yaml", candidateOverlayRunValues}
	candidateOverlayToolDirs   = []string{"tools/client"}
)

// candidateOverlayPlan is one run's rendered-overlay input: a private copy of
// the fixture plus the run values, and the forge.yaml overlay block naming it.
type candidateOverlayPlan struct {
	path       string
	valueFiles []string
	toolDirs   []string
	values     string
}

func candidateOverlayPlanForEnvironment(t *testing.T) candidateOverlayPlan {
	t.Helper()
	return candidateOverlayPlan{
		valueFiles: slices.Clone(candidateOverlayValueFiles),
		toolDirs:   slices.Clone(candidateOverlayToolDirs),
		values:     candidateOverlayValues(t),
	}
}

// prepareCandidateOverlay copies the versioned fixture into a run-private
// directory and adds the run's image identities as the last value file. Forge
// renders that directory locally; only the rendered result reaches the host.
func prepareCandidateOverlay(t *testing.T) candidateOverlayPlan {
	t.Helper()
	plan := candidateOverlayPlanForEnvironment(t)
	plan.path = filepath.Join(t.TempDir(), "overlay")
	if err := copyOverlayTree(candidateOverlayFixture, plan.path); err != nil {
		t.Fatalf("copy fixture overlay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(plan.path, candidateOverlayRunValues), []byte(plan.values), 0o600); err != nil {
		t.Fatalf("write run overlay values: %v", err)
	}
	t.Logf("prepared fixture overlay copy %s from ./%s with the run's immutable image identities", plan.path, candidateOverlayFixture)
	return plan
}

// copyOverlayTree copies the regular files and directories under src to dst.
func copyOverlayTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("fixture overlay entry %s is not a regular file", path)
		}
		contents, err := os.ReadFile(path) //nolint:gosec // versioned fixture file
		if err != nil {
			return err
		}
		return os.WriteFile(target, contents, 0o600)
	})
}

func candidateOverlayValues(t *testing.T) string {
	imageValues := func(repositoryEnv, tagEnv, prefix string) string {
		repository, tag := os.Getenv(repositoryEnv), os.Getenv(tagEnv)
		if repository == "" || tag == "" {
			return ""
		}
		return fmt.Sprintf("%srepository: %q\n%stag: %q\n%spullPolicy: Never\n", prefix, repository, prefix, tag, prefix)
	}

	controlPlane := imageValues("CONTROL_PLANE_IMAGE_REPO", "CONTROL_PLANE_IMAGE_TAG", "    ")
	toolRunner := imageValues("TOOL_RUNNER_IMAGE_REPO", "TOOL_RUNNER_IMAGE_TAG", "      ")
	inference := imageValues("INFERENCE_GATEWAY_IMAGE_REPO", "INFERENCE_GATEWAY_IMAGE_TAG", "    ")

	// Storage has no overlay-selectable backend, class, path, or access mode.
	// Forge reconciles the fixed receipt-bound LVM storage substrate before Helm.
	var values strings.Builder
	values.WriteString("\n# Forge real-machine fixture values.\n")
	// The fixture data VG is at least 25 GiB. Keep both real thick XFS
	// platform claims enabled while leaving headroom for AgentPool/lifecycle proof.
	values.WriteString("control-plane:\n  dispatch:\n    enabled: true\n    defaultModel:\n      id: forge-workspace-model\n      api: openai-completions\n  postgresql:\n    persistence:\n      size: 5Gi\n")
	if controlPlane != "" {
		values.WriteString("  image:\n")
		values.WriteString(controlPlane)
	}
	if toolRunner != "" {
		values.WriteString("  toolRunner:\n    image:\n")
		values.WriteString(toolRunner)
	}
	values.WriteString("minio:\n  persistence:\n    size: 5Gi\n")
	if os.Getenv(workspaceBehaviorEnv) == "true" {
		values.WriteString("inference-gateway:\n  workload:\n    enabled: true\n")
		if inference != "" {
			values.WriteString("  image:\n")
			values.WriteString(inference)
		}
	} else if inference != "" {
		values.WriteString("inference-gateway:\n  image:\n")
		values.WriteString(inference)
	}
	return values.String()
}

func TestCandidateOverlayValues(t *testing.T) {
	t.Setenv(workspaceBehaviorEnv, "true")
	digest := "sha256:" + strings.Repeat("a", 64)
	t.Setenv("CONTROL_PLANE_IMAGE_REPO", "ghcr.io/example/control-plane")
	t.Setenv("CONTROL_PLANE_IMAGE_TAG", "candidate-run")
	t.Setenv(controlPlaneDigestEnv, digest)
	t.Setenv("TOOL_RUNNER_IMAGE_REPO", "")
	t.Setenv("TOOL_RUNNER_IMAGE_TAG", "")
	t.Setenv(toolRunnerDigestEnv, "")
	t.Setenv("INFERENCE_GATEWAY_IMAGE_REPO", "")
	t.Setenv("INFERENCE_GATEWAY_IMAGE_TAG", "")
	t.Setenv(inferenceGatewayDigestEnv, "")

	plan := candidateOverlayPlanForEnvironment(t)
	if !slices.Equal(plan.valueFiles, candidateOverlayValueFiles) || plan.valueFiles[len(plan.valueFiles)-1] != candidateOverlayRunValues {
		t.Fatalf("candidate plan must apply the run values last: %+v", plan)
	}
	for expected := range map[string]struct{}{
		"control-plane:":        {},
		"dispatch:":             {},
		"enabled: true":         {},
		"forge-workspace-model": {},
		"postgresql:":           {},
		"minio:":                {},
		"size: 5Gi":             {},
		"workload:":             {},
		"repository: \"ghcr.io/example/control-plane\"": {},
		"tag: \"candidate-run\"":                        {},
		"pullPolicy: Never":                             {},
	} {
		if !strings.Contains(plan.values, expected) {
			t.Fatalf("candidate values missing %q:\n%s", expected, plan.values)
		}
	}
	controlPlaneImage := strings.Index(plan.values, "repository: \"ghcr.io/example/control-plane\"")
	minio := strings.Index(plan.values, "\nminio:\n")
	if controlPlaneImage < 0 || minio < 0 || controlPlaneImage >= minio {
		t.Fatalf("control-plane image escaped into the later MinIO mapping:\n%s", plan.values)
	}
}

func TestArchiveOnlyOverlayValuesRetainImportedTagAndConfigDigest(t *testing.T) {
	t.Setenv("CONTROL_PLANE_IMAGE_REPO", "iterabase-e2e/control-plane")
	t.Setenv("CONTROL_PLANE_IMAGE_TAG", "exact-source-sha")
	t.Setenv(controlPlaneDigestEnv, "sha256:"+strings.Repeat("c", 64))

	values := candidateOverlayValues(t)
	if !strings.Contains(values, "repository: \"iterabase-e2e/control-plane\"") ||
		!strings.Contains(values, "tag: \"exact-source-sha\"") {
		t.Fatalf("source overlay lost the exact imported image tag:\n%s", values)
	}
	if strings.Contains(values, "tag: \"exact-source-sha@") || !strings.Contains(values, "pullPolicy: Never") {
		t.Fatalf("archive-only overlay did not pin the imported reference without pulling:\n%s", values)
	}
}

func TestCandidateOverlayValuesKeepWorkloadListenerScopedToWorkspaceScenario(t *testing.T) {
	t.Setenv(workspaceBehaviorEnv, "")
	t.Setenv("INFERENCE_GATEWAY_IMAGE_REPO", "iterabase-e2e/inference-gateway")
	t.Setenv("INFERENCE_GATEWAY_IMAGE_TAG", "exact")
	t.Setenv(inferenceGatewayDigestEnv, "")
	values := candidateOverlayValues(t)
	if strings.Contains(values, "workload:\n    enabled: true") {
		t.Fatalf("non-workspace Forge scenarios must not enable the workload listener:\n%s", values)
	}
	if !strings.Contains(values, "inference-gateway:\n  image:") {
		t.Fatalf("non-workspace fixture lost the exact inference image override:\n%s", values)
	}
}

func TestCandidateOverlayValuesContainNoStorageBackendSelection(t *testing.T) {
	values := candidateOverlayValues(t)
	for _, forbidden := range []string{"storage.rwx", "managed-longhorn", "external-rwx", "iterabase-rwx", "longhorn", "storageclassname", "vgpattern", "local.csi"} {
		if strings.Contains(strings.ToLower(values), forbidden) {
			t.Fatalf("candidate values retain obsolete storage selector %q:\n%s", forbidden, values)
		}
	}
}

func TestCandidateOverlayValuesEnableDispatchForRealWorkspaceBehavior(t *testing.T) {
	t.Setenv(workspaceBehaviorEnv, "true")
	t.Setenv("ITERABASE_E2E_FIXTURE_MODE", "source")
	t.Setenv(controlPlaneDigestEnv, "")
	t.Setenv(toolRunnerDigestEnv, "")
	t.Setenv(inferenceGatewayDigestEnv, "")

	values := candidateOverlayValues(t)
	if !strings.Contains(values, "control-plane:\n  dispatch:\n    enabled: true\n    defaultModel:\n      id: forge-workspace-model") {
		t.Fatalf("source machine fixture must enable dispatch for the exact-source real-workspace scenario:\n%s", values)
	}
}

func TestCandidateOverlayCopyCarriesFixtureAndValues(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	t.Setenv("CONTROL_PLANE_IMAGE_REPO", "ghcr.io/example/control-plane")
	t.Setenv("CONTROL_PLANE_IMAGE_TAG", "candidate-run")
	t.Setenv(controlPlaneDigestEnv, digest)
	t.Setenv(toolRunnerDigestEnv, "")
	t.Setenv(inferenceGatewayDigestEnv, "")

	plan := prepareCandidateOverlay(t)
	if !filepath.IsAbs(plan.path) {
		t.Fatalf("overlay copy path %q must be absolute so forge.yaml can live elsewhere", plan.path)
	}
	if !slices.Equal(plan.valueFiles, []string{"values.yaml", "values.client.yaml", "values.e2e.yaml"}) ||
		!slices.Equal(plan.toolDirs, []string{"tools/client"}) {
		t.Fatalf("overlay plan does not name the fixture contract: %+v", plan)
	}
	for _, required := range []string{"kustomization.yaml", "values.yaml", "values.client.yaml", "crds/client/kustomization.yaml", "crds/client/overlay-probe.yaml", "tools/client/validation-echo/index.mjs", "tools/client/validation-echo/manifest.json"} {
		if _, err := os.Stat(filepath.Join(plan.path, required)); err != nil {
			t.Fatalf("fixture overlay copy is missing %s: %v", required, err)
		}
	}
	contents, err := os.ReadFile(filepath.Join(plan.path, candidateOverlayRunValues))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != plan.values || !strings.Contains(plan.values, "repository: \"ghcr.io/example/control-plane\"") {
		t.Fatalf("run values file does not carry the run's image identities:\n%s", contents)
	}
	fixture, err := os.ReadFile(filepath.Join(candidateOverlayFixture, "values.client.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(plan.path, "values.client.yaml"))
	if err != nil || string(copied) != string(fixture) {
		t.Fatalf("the run must not rewrite fixture value files: %v", err)
	}
}

func TestCandidateOverlayForgeConfigUsesLocalOverlay(t *testing.T) {
	t.Setenv(hostFixtureHostKeyEnv, "")
	plan := prepareCandidateOverlay(t)
	cfgPath := writeCurrentOverlayForgeConfig(t, "forge-e2e", "192.0.2.10", "/tmp/key", "0.6.0", plan)
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("  overlay:\n    path: %q\n    valueFiles: [\"values.yaml\", \"values.client.yaml\", \"values.e2e.yaml\"]\n    toolDirs: [\"tools/client\"]\n", plan.path)
	if !strings.Contains(string(cfg), want) {
		t.Fatalf("forge.yaml lacks the local overlay block %q:\n%s", want, cfg)
	}
	for _, removed := range []string{"flux:", "repo:", "ref:", "file://"} {
		if strings.Contains(string(cfg), removed) {
			t.Fatalf("forge.yaml still carries removed Git/Flux overlay field %q:\n%s", removed, cfg)
		}
	}
}
