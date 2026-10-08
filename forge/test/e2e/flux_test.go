package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// runFluxStage exercises the Flux GitOps phase on the composed CPU fixture:
// Forge reconciles Flux, its GitRepository, and Kustomization against the
// public exact-artifact E2E overlay fixture using only the workflow's
// ephemeral read credential,
// and Flux source-controller materializes the fork in-cluster + kustomize-controller
// reconciles crds/client. Validates the MECHANICS (install → sync resources →
// Flux reconciles) rather than a writable push-to-git loop (that's Flux upstream
// behavior, validated end-to-end by HOR-299's real OPO1 client fork).
//
// No explicit FORGE_OVERLAY_TOKEN is accepted. The E2E process maps the
// workflow's ephemeral GITHUB_TOKEN into each Forge subprocess; tokenless and
// prompt behavior remains covered by unit + fake-SSH tests.
func runFluxStage(t *testing.T, state *cpuFixtureState) {
	if _, ok := os.LookupEnv("FORGE_OVERLAY_TOKEN"); ok {
		t.Fatal("FORGE_OVERLAY_TOKEN must be unset; E2E supplies only the ephemeral workflow token")
	}

	cfgPath := writeFluxForgeConfig(t, state.runID, state.ip, state.privKeyPath, state.chartVersion)
	// The platform chart is already proven by the baseline/overlay stages. Flux
	// owns only continuous overlay reconciliation, so keep this phase isolated.
	out := applyOnceArgs(t, state.forgeBin, state.forgeHome, cfgPath, "--skip-chart")
	assertApplyMarkers(t, out, "action:     skip", "node ready: true", "flux installed: true")
	t.Logf("apply output:\n%s", out)

	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()

	// Flux source-controller + kustomize-controller pods are Running.
	pods, err := sshOutput(sc, "sudo k3s kubectl get pods -n flux-system --no-headers")
	if err != nil {
		t.Fatalf("get flux-system pods: %v\n%s", err, pods)
	}
	if !strings.Contains(pods, "source-controller") || !strings.Contains(pods, "kustomize-controller") {
		t.Fatalf("flux-system missing source/kustomize controller pods:\n%s", pods)
	}
	for _, line := range strings.Split(strings.TrimSpace(pods), "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(line, "Running") && !strings.Contains(line, "Completed") {
			t.Fatalf("flux-system pod not Running:\n%s", line)
		}
	}

	// Root-flux scoping contract (HOR-576): a second, tenant-owned Flux
	// instance shares the cluster, so the root instance must be namespace-scoped
	// — otherwise each source-controller overwrites the other instance's
	// artifact URLs, and Flux's generated NetworkPolicies refuse the
	// cross-namespace fetch.
	for _, controller := range []string{"source-controller", "kustomize-controller", "helm-controller", "notification-controller"} {
		args, err := sshOutput(sc, fmt.Sprintf("sudo k3s kubectl get deployment -n flux-system %s -o jsonpath='{.spec.template.spec.containers[*].args}'", controller))
		if err != nil {
			t.Fatalf("get %s args: %v\n%s", controller, err, args)
		}
		if !strings.Contains(args, "--watch-all-namespaces=false") {
			t.Fatalf("%s is not namespace-scoped (args: %s)", controller, args)
		}
	}

	// GitRepository becomes Ready + source-controller materializes the fork
	// (.status.artifact.revision non-empty) — the HOR-351 in-cluster source
	// contract. Poll: Flux reconciles async after the GitRepository is applied.
	gitReady, gitArtifact, gitDigest := pollFluxReady(t, sc, "gitrepository", "overlay", 4*time.Minute)
	if !gitReady {
		t.Fatalf("GitRepository overlay never reached Ready=True")
	}
	t.Logf("gitrepository artifact revision: %s digest: %s", gitArtifact, gitDigest)
	if gitArtifact == "" || gitDigest == "" {
		t.Fatalf("GitRepository overlay has no exact materialized revision/digest — source-controller did not fetch the fork")
	}
	if !isCanonicalSHA256Digest(gitDigest) {
		t.Fatalf("GitRepository overlay artifact digest is not canonical sha256: %q", gitDigest)
	}

	// Kustomization becomes Ready (reconciled crds/client — empty scaffold, 0
	// objects, but Healthy wiring end-to-end).
	kustReady, _, _ := pollFluxReady(t, sc, "kustomization", "overlay-crds", 2*time.Minute)
	if !kustReady {
		t.Fatalf("Kustomization overlay-crds never reached Ready=True")
	}
	t.Logf("flux gitops reconcile verified on %s", state.ip)
}

// pollFluxReady polls a Flux CR's Ready condition until True or the timeout
// elapses. For a GitRepository it also reads the exact revision and digest
// that the runner materializer verifies (HOR-397).
func pollFluxReady(t *testing.T, client *ssh.Client, kind, name string, timeout time.Duration) (ready bool, artifact, digest string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	condPath := `'{.status.conditions[?(@.type=="Ready")].status}'`
	for {
		out, err := sshOutput(client, fmt.Sprintf("sudo k3s kubectl get %s -n flux-system %s -o jsonpath=%s", kind, name, condPath))
		if err == nil && strings.TrimSpace(out) == "True" {
			ready = true
			if kind == "gitrepository" {
				art, _ := sshOutput(client, fmt.Sprintf("sudo k3s kubectl get %s -n flux-system %s -o jsonpath='{.status.artifact.revision}'", kind, name))
				artifact = strings.TrimSpace(art)
				dig, _ := sshOutput(client, fmt.Sprintf("sudo k3s kubectl get %s -n flux-system %s -o jsonpath='{.status.artifact.digest}'", kind, name))
				digest = strings.TrimSpace(dig)
			}
			return ready, artifact, digest
		}
		if time.Now().After(deadline) {
			t.Logf("timeout waiting for %s/%s Ready (last output: %q)", kind, name, strings.TrimSpace(out))
			return false, "", ""
		}
		time.Sleep(10 * time.Second)
	}
}

// writeFluxForgeConfig writes a forge.yaml identical to the overlay e2e config
// but with Flux enabled (pointing at the public iterabase-overlay base repo).
func writeFluxForgeConfig(t *testing.T, name, ip, keyPath, chartVersion string) string {
	return writeForgeConfigSpec(t, forgeConfigSpec{
		Name: name, Address: ip, SSHKeyPath: keyPath, RunLabel: true, DualStack: true,
		ChartVersion: chartVersion,
		OverlayRepo:  "https://github.com/nunocgoncalves/iterabase-overlay.git",
		OverlayRef:   envOr("FORGE_E2E_OVERLAY_REF", "e2e"),
		Flux:         true,
	})
}
