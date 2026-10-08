package e2e

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	candidateOverlayRepository = "https://github.com/nunocgoncalves/iterabase-overlay.git"
	candidateOverlayRef        = "e2e"
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

func candidateOverlaySetupScript(values, prefix string) string {
	valuesPath := prefix + "-values.yaml"
	filterPath := prefix + "-smudge"
	attributesPath := prefix + "-attributes"
	encoded := base64.StdEncoding.EncodeToString([]byte(values))
	return fmt.Sprintf(`set -eu
if ! command -v git >/dev/null 2>&1; then
  sudo apt-get update -qq
  sudo apt-get install -y git
fi
printf '%%s' %s | base64 --decode > %s
cat > %s <<'FILTER'
#!/bin/sh
cat
printf '\n'
cat %s
FILTER
chmod 700 %s
printf 'values.client.yaml filter=iterabase-release-candidates\n' > %s
git config --global core.attributesFile %s
git config --global filter.iterabase-release-candidates.clean cat
git config --global filter.iterabase-release-candidates.smudge %s
git config --global filter.iterabase-release-candidates.required true
`, candidateShellQuote(encoded), candidateShellQuote(valuesPath), candidateShellQuote(filterPath),
		candidateShellQuote(valuesPath), candidateShellQuote(filterPath), candidateShellQuote(attributesPath),
		candidateShellQuote(attributesPath), candidateShellQuote(filterPath))
}

// prepareCandidateOverlay keeps the public overlay commit as both Forge's
// resolved source and Flux's exact artifact identity. A host-local Git smudge
// filter appends Forge-owned real-machine fixture values and any run-addressed
// image values only to Forge's checkout. The public source commit therefore
// still matches the Flux artifact while Helm sees the overrides on first install.
func prepareCandidateOverlay(t *testing.T, runID, ip, keyPath string) candidateOverlayPlan {
	t.Helper()
	plan := candidateOverlayPlanForEnvironment(t)
	if plan.values == "" {
		return plan
	}

	prefix := "/tmp/iterabase-release-overlay-" + runID
	valuesPath := prefix + "-values.yaml"
	filterPath := prefix + "-smudge"
	attributesPath := prefix + "-attributes"
	script := candidateOverlaySetupScript(plan.values, prefix)

	client, err := sshDial(ip, keyPath)
	if err != nil {
		t.Fatalf("dial candidate host to prepare overlay values: %v", err)
	}
	if output, err := sshOutput(client, script); err != nil {
		client.Close()
		t.Fatalf("prepare exact-candidate overlay values: %v\n%s", err, output)
	}
	client.Close()
	t.Cleanup(func() {
		cleanupClient, err := sshDial(ip, keyPath)
		if err != nil {
			t.Logf("remove candidate overlay values: dial host: %v", err)
			return
		}
		defer cleanupClient.Close()
		cleanup := fmt.Sprintf(
			"git config --global --unset-all core.attributesFile || true; "+
				"git config --global --remove-section filter.iterabase-release-candidates || true; "+
				"rm -f %s %s %s",
			candidateShellQuote(valuesPath), candidateShellQuote(filterPath), candidateShellQuote(attributesPath),
		)
		if output, err := sshOutput(cleanupClient, cleanup); err != nil {
			t.Logf("remove candidate overlay values: %v\n%s", err, output)
		}
	})
	t.Log("prepared exact-source overlay checkout with selected immutable image identities")
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
		t.Fatalf("candidate plan must retain exact public Flux source: %+v", plan)
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

func TestCandidateOverlayCheckoutKeepsExactSourceCommit(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	t.Setenv("CONTROL_PLANE_IMAGE_REPO", "ghcr.io/example/control-plane")
	t.Setenv("CONTROL_PLANE_IMAGE_TAG", "candidate-run")
	t.Setenv(controlPlaneDigestEnv, digest)
	t.Setenv(toolRunnerDigestEnv, "")
	t.Setenv(inferenceGatewayDigestEnv, "")
	plan := candidateOverlayPlanForEnvironment(t)
	if plan.values == "" {
		t.Fatal("non-empty candidate environment produced no overlay values")
	}

	root := t.TempDir()
	home := filepath.Join(root, "home")
	source := filepath.Join(root, "source")
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "crds", "client"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		"values.yaml":                    "base: true\n",
		"values.client.yaml":             "# fixture\n",
		"crds/client/kustomization.yaml": "resources: []\n",
	} {
		if err := os.WriteFile(filepath.Join(source, path), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit(source, "init", "-q", "-b", candidateOverlayRef)
	runGit(source, "add", ".")
	runGit(source, "-c", "user.email=e2e@example.com", "-c", "user.name=E2E", "commit", "-qm", "fixture")
	sourceCommit := runGit(source, "rev-parse", "HEAD")

	setup := exec.Command("bash", "-c", candidateOverlaySetupScript(plan.values, filepath.Join(root, "candidate")))
	setup.Env = append(os.Environ(), "HOME="+home)
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("prepare candidate checkout filter: %v\n%s", err, out)
	}
	runGit(root, "clone", "-q", "--branch", candidateOverlayRef, source, checkout)
	if checkoutCommit := runGit(checkout, "rev-parse", "HEAD"); checkoutCommit != sourceCommit {
		t.Fatalf("candidate checkout commit = %s, want exact source %s", checkoutCommit, sourceCommit)
	}
	contents, err := os.ReadFile(filepath.Join(checkout, "values.client.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), plan.values) {
		t.Fatalf("candidate checkout did not receive candidate values:\n%s", contents)
	}
}
