package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// runSecretsStage exercises forge secret-sync on the composed CPU fixture. The
// run's overlay copy declares a Secret in secrets.yaml; forge reads it on the
// operator machine, resolves the value from an operator env var, and
// materializes the Secret via `kubectl apply -f -` over SSH stdin, so the value
// never appears in a command line or in the overlay. The chart is skipped to
// isolate the secret-sync mechanics; the rest of the overlay is unchanged, so
// the apply must deliver the same tools archive and prune nothing. Validates
// the HOR-364 path cert-manager (cert-issuers, HOR-342) and external-dns
// (HOR-343) rely on.
func runSecretsStage(t *testing.T, state *cpuFixtureState) {
	const (
		secretName  = "e2e-test-secret"
		secretNs    = "forge-e2e-secrets"
		secretKey   = "token"
		secretValue = "supersecret-e2e-value"
		envVar      = "FORGE_E2E_TEST_SECRET"
	)
	t.Setenv(envVar, secretValue)
	state.diagnostics.redactor.Add(secretValue)

	plan := prepareCandidateOverlay(t)
	declaration := fmt.Sprintf("secrets:\n  - name: %s\n    namespace: %s\n    key: %s\n    envVar: %s\n", secretName, secretNs, secretKey, envVar)
	if err := os.WriteFile(filepath.Join(plan.path, "secrets.yaml"), []byte(declaration), 0o600); err != nil {
		t.Fatalf("write overlay secrets.yaml: %v", err)
	}
	cfgPath := writeCurrentOverlayForgeConfig(t, state.runID, state.ip, state.privKeyPath, state.chartVersion, plan)
	out := applyOnceArgs(t, state.forgeBin, state.forgeHome, cfgPath, "--skip-chart")
	assertApplyMarkers(t, out, "action:     skip", "node ready: true", "secrets applied: true")
	t.Logf("apply output:\n%s", out)
	if strings.Contains(out, secretValue) {
		t.Fatal("forge apply printed the secret value")
	}

	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()
	delivery := assertOverlayDelivered(t, sc, state.runID, plan, out)
	if delivery.toolsDigest != state.overlay.toolsDigest || len(delivery.pruned) != 0 {
		t.Fatalf("declaring a secret changed the overlay tools or pruned objects: first %+v, now %+v", state.overlay, delivery)
	}
	got := mustSSHOutput(t, sc, fmt.Sprintf(
		"sudo k3s kubectl get secret %s -n %s -o jsonpath='{.data.%s}' | base64 -d",
		secretName, secretNs, secretKey))
	if strings.TrimSpace(got) != secretValue {
		t.Fatalf("secret %s/%s data[%s] = %q, want %q", secretNs, secretName, secretKey, got, secretValue)
	}
	gotType := strings.TrimSpace(mustSSHOutput(t, sc, fmt.Sprintf(
		"sudo k3s kubectl get secret %s -n %s -o jsonpath='{.type}'", secretName, secretNs)))
	if gotType != "Opaque" {
		t.Fatalf("secret type = %q, want Opaque", gotType)
	}
	if staged := mustSSHOutput(t, sc, "sudo grep -rlF "+candidateShellQuote(secretValue)+" "+candidateShellQuote(overlayStageRoot+state.runID)+" || true"); strings.TrimSpace(staged) != "" {
		t.Fatalf("the secret value reached the staged overlay files: %s", staged)
	}
	t.Logf("secret %s/%s materialized with the expected value + type", secretNs, secretName)
}

// mustSSHOutput runs a command over SSH and fails the test on error.
func mustSSHOutput(t *testing.T, sc *ssh.Client, cmd string) string {
	t.Helper()
	out, err := sshOutput(sc, cmd)
	if err != nil {
		t.Fatalf("ssh %q: %v\n%s", cmd, err, out)
	}
	return out
}
