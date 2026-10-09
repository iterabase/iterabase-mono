package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	// candidateOverlayRepository is the host-local overlay Forge applies and
	// serves to Flux over read-only node SSH (DES-HOR-632-01). It is built on
	// every run from the fixture under ./overlay plus the run's values.
	candidateOverlayRoot       = "/var/lib/iterabase-e2e/overlay"
	candidateOverlayRepository = "file://" + candidateOverlayRoot
	candidateOverlayRef        = "e2e"
	candidateOverlayFixture    = "overlay"
	workspaceBehaviorEnv       = "FORGE_E2E_WORKSPACE_BEHAVIOR"
)

type candidateOverlayPlan struct {
	repository string
	ref        string
	flux       bool
	values     string
}

func candidateOverlayPlanForEnvironment(t *testing.T) candidateOverlayPlan {
	t.Helper()
	return candidateOverlayPlan{
		repository: candidateOverlayRepository,
		ref:        candidateOverlayRef,
		flux:       true,
		values:     candidateOverlayValues(t),
	}
}

// candidateOverlayArchive packs the versioned fixture overlay with the run's
// values appended to values.client.yaml. Entries are sorted and timestamp-free
// so the same inputs give the same archive.
func candidateOverlayArchive(fixture, values string) ([]byte, error) {
	var files []string
	err := filepath.WalkDir(fixture, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(gz)
	for _, path := range files {
		relative, err := filepath.Rel(fixture, path)
		if err != nil {
			return nil, err
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if filepath.ToSlash(relative) == "values.client.yaml" {
			contents = append(contents, []byte(values)...)
		}
		if err := archive.WriteHeader(&tar.Header{Name: filepath.ToSlash(relative), Mode: 0o644, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := archive.Write(contents); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// candidateOverlaySetupScript commits the archive as a fresh one-commit
// repository at root on the candidate ref. Forge clones it and Flux mirrors the
// same commit, so the values Helm applies are the values Flux tracks.
func candidateOverlaySetupScript(archive []byte, root string) string {
	return fmt.Sprintf(`set -eu
if ! command -v git >/dev/null 2>&1; then
  sudo apt-get update -qq
  sudo apt-get install -y git
fi
root=%s
sudo rm -rf "$root"
sudo install -d -o "$(id -un)" -g "$(id -gn)" -m 0755 "$root"
printf '%%s' %s | base64 --decode | tar -xz -C "$root"
cd "$root"
git init -q -b %s
git add -A
git -c user.email=forge-e2e@iterabase.invalid -c user.name="Forge E2E" commit -qm "Forge E2E fixture overlay"
git rev-parse HEAD
`, candidateShellQuote(root), candidateShellQuote(base64.StdEncoding.EncodeToString(archive)), candidateShellQuote(candidateOverlayRef))
}

// prepareCandidateOverlay builds the host-local overlay from the versioned
// fixture and the run's image identities. Forge's resolved source and Flux's
// exact artifact are both this commit.
func prepareCandidateOverlay(t *testing.T, runID, ip, keyPath string) candidateOverlayPlan {
	t.Helper()
	plan := candidateOverlayPlanForEnvironment(t)
	archive, err := candidateOverlayArchive(candidateOverlayFixture, plan.values)
	if err != nil {
		t.Fatalf("pack fixture overlay: %v", err)
	}
	client, err := sshDial(ip, keyPath)
	if err != nil {
		t.Fatalf("dial candidate host to prepare the fixture overlay: %v", err)
	}
	defer client.Close()
	output, err := sshOutput(client, candidateOverlaySetupScript(archive, candidateOverlayRoot))
	if err != nil {
		t.Fatalf("prepare the fixture overlay for %s: %v\n%s", runID, err, output)
	}
	t.Logf("prepared fixture overlay commit %s from ./%s with selected immutable image identities", strings.TrimSpace(output), candidateOverlayFixture)
	return plan
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
	if plan.repository != candidateOverlayRepository || plan.ref != candidateOverlayRef || !plan.flux {
		t.Fatalf("candidate plan must use the host-local fixture overlay with Flux: %+v", plan)
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

func TestCandidateOverlayRepositoryCarriesFixtureAndValues(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	t.Setenv("CONTROL_PLANE_IMAGE_REPO", "ghcr.io/example/control-plane")
	t.Setenv("CONTROL_PLANE_IMAGE_TAG", "candidate-run")
	t.Setenv(controlPlaneDigestEnv, digest)
	t.Setenv(toolRunnerDigestEnv, "")
	t.Setenv(inferenceGatewayDigestEnv, "")
	plan := candidateOverlayPlanForEnvironment(t)

	archive, err := candidateOverlayArchive(candidateOverlayFixture, plan.values)
	if err != nil {
		t.Fatal(err)
	}
	again, err := candidateOverlayArchive(candidateOverlayFixture, plan.values)
	if err != nil || !bytes.Equal(archive, again) {
		t.Fatalf("fixture archive is not deterministic: %v", err)
	}

	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "overlay")
	// The host script uses sudo for its root-owned parent; locally run it without.
	script := strings.ReplaceAll(candidateOverlaySetupScript(archive, repo), "sudo ", "")
	setup := exec.Command("bash", "-c", script)
	setup.Env = append(os.Environ(), "HOME="+home)
	out, err := setup.CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture overlay repository: %v\n%s", err, out)
	}

	checkout := filepath.Join(root, "checkout")
	clone := exec.Command("git", "clone", "-q", "--branch", candidateOverlayRef, "--depth", "1", "file://"+repo, checkout)
	clone.Env = setup.Env
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("Forge-style clone of the fixture overlay: %v\n%s", err, out)
	}
	for _, required := range []string{"values.yaml", "values.client.yaml", "crds/client/kustomization.yaml", "tools/client/validation-echo/index.mjs"} {
		if _, err := os.Stat(filepath.Join(checkout, required)); err != nil {
			t.Fatalf("fixture overlay is missing %s: %v", required, err)
		}
	}
	contents, err := os.ReadFile(filepath.Join(checkout, "values.client.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), plan.values) {
		t.Fatalf("fixture overlay values.client.yaml lacks the run values:\n%s", contents)
	}
	if head := strings.TrimSpace(string(out)); len(head) != 40 {
		t.Fatalf("setup did not report the overlay commit: %q", out)
	}
}
