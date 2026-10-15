package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// failedInstallValues is the extra overlay value file the failing revisions
// add after the run's own values.
const failedInstallValues = "values.failed-first-install.yaml"

// runRecoveredFirstInstallStage reproduces the HOR-601 sequence on the fresh
// host (HOR-677). Revision 1 creates the normally disabled internal ingress
// controller and then fails; revision 2 disables it and fails the same way,
// so Helm's baseline for the next revision no longer lists that controller.
// The unchanged fixture overlay must then converge in one forge apply with no
// manual Kubernetes repair: the controller left behind by revision 1 is gone
// (DES-HOR-677-02) and the fail-closed admission webhook has its caBundle
// (DES-HOR-677-01).
//
// Each failing revision fails fast and deterministically: an IngressClass
// name the API server rejects. Helm creates IngressClasses after Deployments,
// so the revision has already created its workloads, and it never reaches its
// post-install hooks — the same partial state a Deployment that never becomes
// ready leaves, without waiting out Helm's timeout.
func runRecoveredFirstInstallStage(t *testing.T, state *cpuFixtureState) {
	if !state.freshInstall {
		t.Fatal("failed-first-install recovery needs a host with no platform release")
	}
	runOverlayInstall(t, state, func(plan candidateOverlayPlan) {
		failPlatformRevision(t, state, plan, true)
		failPlatformRevision(t, state, plan, false)
	})
	assertRecoveredFirstInstall(t, state)
}

func failPlatformRevision(t *testing.T, state *cpuFixtureState, plan candidateOverlayPlan, internalIngress bool) {
	t.Helper()
	values := fmt.Sprintf(`
# HOR-677 failing revision: rejected by the API server after Deployments exist.
ingress-nginx:
  controller:
    ingressClassResource:
      name: Invalid_Class
internal-ingress-nginx:
  enabled: %t
  controller:
    service:
      type: NodePort
`, internalIngress)
	if err := os.WriteFile(filepath.Join(plan.path, failedInstallValues), []byte(values), 0o600); err != nil {
		t.Fatalf("write failing revision values: %v", err)
	}
	cfgPath := writeForgeConfigSpec(t, forgeConfigSpec{
		Name: state.runID, Address: state.ip, SSHKeyPath: state.privKeyPath, RunLabel: true, DualStack: true,
		ChartVersion:      state.chartVersion,
		ChartRepository:   os.Getenv("FORGE_E2E_CHART_REPOSITORY"),
		OverlayPath:       plan.path,
		OverlayValueFiles: append(slices.Clone(plan.valueFiles), failedInstallValues),
		OverlayToolDirs:   plan.toolDirs,
	})
	out, err := runForgeE(state.forgeBin, state.forgeHome, "apply", "--config", cfgPath)
	if err == nil {
		t.Fatalf("forge apply with an invalid IngressClass succeeded:\n%s", out)
	}
	if !strings.Contains(out, "Invalid_Class") {
		t.Fatalf("forge apply failed for another reason than the injected IngressClass:\n%s", out)
	}

	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()
	if internalIngress && len(internalIngressObjects(t, sc, state.runID)) == 0 {
		t.Fatal("the failing revision created no internal ingress objects; the recovery proof would be vacuous")
	}
}

func assertRecoveredFirstInstall(t *testing.T, state *cpuFixtureState) {
	t.Helper()
	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()

	out := mustSSHOutput(t, sc, fmt.Sprintf("sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml helm history %s -n iterabase-system -o json", state.runID))
	var history []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &history); err != nil {
		t.Fatalf("decode Helm history: %v\n%s", err, out)
	}
	var statuses []string
	for _, h := range history {
		statuses = append(statuses, h.Status)
	}
	if !slices.Equal(statuses, []string{"failed", "failed", "deployed"}) {
		t.Fatalf("platform release history = %v, want two failed revisions then the recovered deployed one", statuses)
	}

	if left := internalIngressObjects(t, sc, state.runID); len(left) != 0 {
		t.Fatalf("objects from the failed revisions survived the recovered apply: %v", left)
	}

	webhook := state.runID + "-ingress-nginx-admission"
	deadline := time.Now().Add(2 * time.Minute)
	for {
		caBundle := mustSSHOutput(t, sc, "sudo k3s kubectl get validatingwebhookconfiguration "+webhook+" -o jsonpath={.webhooks[0].clientConfig.caBundle}")
		if strings.TrimSpace(caBundle) != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("admission webhook %s has no caBundle after the recovered apply", webhook)
		}
		time.Sleep(5 * time.Second)
	}
}

// internalIngressObjects lists the internal ingress controller's namespaced
// workload objects and its cluster-scoped IngressClass and RBAC.
func internalIngressObjects(t *testing.T, sc *ssh.Client, release string) []string {
	t.Helper()
	selector := "app.kubernetes.io/name=internal-ingress-nginx,app.kubernetes.io/instance=" + release
	out := mustSSHOutput(t, sc, "sudo k3s kubectl get deployment,service,serviceaccount,configmap,role,rolebinding -n iterabase-system -l "+selector+" -o name && "+
		"sudo k3s kubectl get ingressclass,clusterrole,clusterrolebinding -l "+selector+" -o name")
	return strings.Fields(out)
}
