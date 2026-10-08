package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
	shareddiagnostics "github.com/nunocgoncalves/iterabase-mono/testkit/e2e/diagnostics"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

const (
	failureDomainFixturePrepare = "fixture-prepare"
	failureDomainSubstrate      = "forge-substrate"
	failureDomainForgeReconcile = "forge-reconciliation"
	failureDomainForgeHandoff   = "forge-artifact-handoff"
	failureDomainDependentSmoke = "dependent-layer-smoke"
)

type forgeDiagnostics struct {
	domain    string
	outputDir string
	redactor  *redact.Redactor
}

func newForgeDiagnostics(t *testing.T, scenario string) forgeDiagnostics {
	t.Helper()
	outputDir := os.Getenv("ITERABASE_E2E_DIAGNOSTICS")
	if outputDir == "" {
		outputDir = t.TempDir()
	} else {
		outputDir = filepath.Join(outputDir, scenario)
	}
	absolute, err := filepath.Abs(outputDir)
	if err != nil {
		t.Fatalf("resolve Forge diagnostics directory: %v", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		t.Fatalf("create Forge diagnostics directory: %v", err)
	}
	return forgeDiagnostics{domain: failureDomainFixturePrepare, outputDir: absolute, redactor: redact.New()}
}

func (diagnostics *forgeDiagnostics) setDomain(domain string) {
	diagnostics.domain = domain
}

func (diagnostics *forgeDiagnostics) recordDomain(t *testing.T) {
	t.Helper()
	path := filepath.Join(diagnostics.outputDir, "failure-domain.txt")
	if err := os.WriteFile(path, []byte(diagnostics.domain+"\n"), 0o600); err != nil {
		t.Logf("write failure domain: %v", err)
	}
	t.Logf("Forge failure domain: %s", diagnostics.domain)
}

func (diagnostics *forgeDiagnostics) collectSSH(t *testing.T, ip, keyPath string, commands map[string]string) {
	t.Helper()
	if ip == "" {
		return
	}
	client, err := sshDial(ip, keyPath)
	if err != nil {
		t.Logf("SSH diagnostics unavailable for %s: %v", ip, err)
		return
	}
	defer client.Close()
	for name, command := range commands {
		output, commandErr := sshOutput(client, command)
		diagnostics.recordRemoteLog(t, name, output)
		if commandErr != nil {
			t.Logf("remote diagnostic %s: %v", name, commandErr)
		}
	}
}

// recordRemoteLog persists one redacted remote command output under the run
// diagnostics directory so failure evidence survives the test process.
func (diagnostics *forgeDiagnostics) recordRemoteLog(t *testing.T, name, output string) {
	t.Helper()
	redacted := diagnostics.redactor.String(output)
	path := filepath.Join(diagnostics.outputDir, "remote-"+name+".log")
	if err := os.WriteFile(path, []byte(redacted), 0o600); err != nil {
		t.Logf("write remote diagnostic %s: %v", name, err)
	}
}

// registerBootstrapSecrets returns whether full pod-log collection is safe.
// If a bootstrap pod exists, every current and retained previous bootstrap log
// must expose the expected admin and token credentials in the reviewed format.
// Inspection reads complete logs, which is deliberately broader than the shared
// collector's retained tail, before generic current/previous logs are persisted.
func (diagnostics *forgeDiagnostics) registerBootstrapSecrets(t *testing.T, ip, keyPath string) bool {
	t.Helper()
	if ip == "" {
		return false
	}
	client, err := sshDial(ip, keyPath)
	if err != nil {
		t.Logf("skip shared pod-log diagnostics because bootstrap credentials cannot be inspected: %v", err)
		return false
	}
	defer client.Close()
	pods, err := sshOutput(client, bootstrapPodStatusCommand())
	if err != nil {
		t.Logf("skip shared pod-log diagnostics because bootstrap pod presence cannot be inspected: %v", err)
		return false
	}
	requests, err := bootstrapLogRequests(pods)
	if err != nil {
		t.Logf("skip shared pod-log diagnostics because bootstrap pod status cannot be inspected: %v", err)
		return false
	}
	secrets := make([]string, 0, len(requests)*2)
	for _, request := range requests {
		output, outputErr := sshOutput(client, bootstrapCredentialLogCommand(request))
		if outputErr != nil {
			t.Logf("skip shared pod-log diagnostics because %s bootstrap credentials cannot be inspected: %v", request.source(), outputErr)
			return false
		}
		literals, literalErr := bootstrapSecretLiterals(output)
		if literalErr != nil {
			t.Logf("skip shared pod-log diagnostics because %s bootstrap credential evidence is incomplete: %v", request.source(), literalErr)
			return false
		}
		secrets = append(secrets, literals...)
	}
	diagnostics.redactor.Add(secrets...)
	return true
}

type bootstrapLogRequest struct {
	pod      string
	previous bool
}

func (request bootstrapLogRequest) source() string {
	if request.previous {
		return "previous"
	}
	return "current"
}

func bootstrapPodStatusCommand() string {
	return "sudo k3s kubectl get pods -n iterabase-system -l app.kubernetes.io/component=api -o json"
}

func bootstrapLogRequests(output string) ([]bootstrapLogRequest, error) {
	var pods struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				InitContainerStatuses []struct {
					Name         string `json:"name"`
					RestartCount int32  `json:"restartCount"`
				} `json:"initContainerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(output), &pods); err != nil {
		return nil, fmt.Errorf("decode bootstrap pod status: %w", err)
	}
	requests := make([]bootstrapLogRequest, 0, len(pods.Items)*2)
	for _, pod := range pods.Items {
		if pod.Metadata.Name == "" {
			return nil, fmt.Errorf("bootstrap pod status has no name")
		}
		requests = append(requests, bootstrapLogRequest{pod: pod.Metadata.Name})
		for _, status := range pod.Status.InitContainerStatuses {
			if status.Name == "bootstrap" && status.RestartCount > 0 {
				requests = append(requests, bootstrapLogRequest{pod: pod.Metadata.Name, previous: true})
				break
			}
		}
	}
	return requests, nil
}

func bootstrapCredentialLogCommand(request bootstrapLogRequest) string {
	previous := ""
	if request.previous {
		previous = " --previous"
	}
	return fmt.Sprintf("sudo k3s kubectl logs -n iterabase-system %q -c bootstrap%s --tail=-1", request.pod, previous)
}

func bootstrapSecretLiterals(output string) ([]string, error) {
	matches := keyRe.FindAllStringSubmatch(output, -1)
	found := make(map[string]bool)
	secrets := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) <= 2 {
			continue
		}
		found[match[1]] = true
		secrets = append(secrets, match[2])
	}
	missing := make([]string, 0, 2)
	for _, scope := range []string{"scope=admin", "scope=token"} {
		if !found[scope] {
			missing = append(missing, scope)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing expected bootstrap credential scopes %s", strings.Join(missing, ", "))
	}
	return secrets, nil
}

func (diagnostics *forgeDiagnostics) collectSharedCluster(t *testing.T, kubeconfig string) {
	t.Helper()
	if _, err := os.Stat(kubeconfig); err != nil {
		t.Logf("shared cluster diagnostics unavailable: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	runner := process.Runner{Redactor: diagnostics.redactor, OutputDir: filepath.Join(diagnostics.outputDir, "process")}
	err := (shareddiagnostics.Collector{
		Executor: runner, Kubeconfig: kubeconfig,
		OutputDir: filepath.Join(diagnostics.outputDir, "shared-cluster"), Redactor: diagnostics.redactor,
	}).Collect(ctx)
	if err != nil {
		t.Logf("best-effort shared cluster diagnostics: %v", err)
	}
}

func cpuDiagnosticStage(domain string, run func(*testing.T, *cpuFixtureState)) func(*testing.T, *cpuFixtureState) {
	return func(t *testing.T, state *cpuFixtureState) {
		state.diagnostics.setDomain(domain)
		run(t, state)
	}
}

func gpuDiagnosticStage(domain string, run func(*testing.T, *gpuFixtureState)) func(*testing.T, *gpuFixtureState) {
	return func(t *testing.T, state *gpuFixtureState) {
		state.diagnostics.setDomain(domain)
		run(t, state)
	}
}

func storageStateDiagnosticCommand(device string) string {
	return fmt.Sprintf(`sudo bash -ceu '
selected=%s
resolved=$(readlink -f -- "$selected")
kernel=$(lsblk -dnro KNAME -- "$resolved")
printf "selected=%%s resolved=%%s kernel=%%s\n" "$selected" "$resolved" "$kernel"
printf "%%s\n" "--- holders"
holder_dir="/sys/class/block/$kernel/holders"
if test -d "$holder_dir"; then
  for entry in "$holder_dir"/*; do
    test -e "$entry" || continue
    holder=${entry##*/}
    printf "holder=%%s\n" "$holder"
    dmsetup info --columns --noheadings -o name,uuid,open,attr "/dev/$holder" 2>&1 || true
  done
fi
printf "%%s\n" "--- receipt"
if test -f /var/lib/iterabase/data-storage.receipt; then
  grep -E "^(contract|status|pv_done|vg_name|vg_uuid|ownership_tag|pv_uuid_[0-9]+)=" /var/lib/iterabase/data-storage.receipt || true
fi
printf "%%s\n" "--- receipt-bound LVM"
pvs --noheadings --separator "|" -o pv_name,pv_uuid,vg_name -- "$resolved" 2>&1 || true
vgs --noheadings --separator "|" -o vg_name,vg_uuid,vg_tags,pv_count,lv_count iterabase-data 2>&1 || true
lvs --noheadings --separator "|" -o lv_name,lv_uuid,lv_attr,vg_name --select "vg_name=iterabase-data" 2>&1 || true
'`, candidateShellQuote(device))
}

func collectCPUDiagnostics(t *testing.T, state *cpuFixtureState) {
	t.Helper()
	state.diagnostics.recordDomain(t)
	safeClusterLogs := state.diagnostics.registerBootstrapSecrets(t, state.ip, state.privKeyPath)
	state.diagnostics.collectSSH(t, state.ip, state.privKeyPath, map[string]string{
		"fixture-state":  "cloud-init status --long 2>&1; systemctl --no-pager --full status k3s 2>&1 || true",
		"forge-state":    fmt.Sprintf("sudo ls -la /var/lib/forge/overlay/%s 2>&1 || true; sudo k3s kubectl get gitrepositories,kustomizations -A -o wide 2>&1 || true", state.runID),
		"platform-state": "sudo k3s kubectl get nodes -o wide 2>&1 || true; sudo k3s kubectl get deployments,statefulsets,daemonsets,pods,jobs,pvc -A -o wide 2>&1 || true; sudo k3s kubectl get events -A --sort-by=.metadata.creationTimestamp 2>&1 | tail -300 || true",
		"storage-state":  storageStateDiagnosticCommand(state.dataStorageDevice),
	})
	if safeClusterLogs {
		state.diagnostics.collectSharedCluster(t, filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml"))
	}
}

func collectGPUDiagnostics(t *testing.T, state *gpuFixtureState) {
	t.Helper()
	state.diagnostics.recordDomain(t)
	if state.host == nil {
		return
	}
	safeClusterLogs := state.diagnostics.registerBootstrapSecrets(t, state.host.IP, state.privKeyPath)
	state.diagnostics.collectSSH(t, state.host.IP, state.privKeyPath, map[string]string{
		"fixture-state": "cloud-init status --long 2>&1; systemctl --no-pager --full status k3s 2>&1 || true",
		"gpu-policy":    "sudo k3s kubectl get clusterpolicy -o yaml 2>&1 || true; sudo k3s kubectl get nodes -o wide --show-labels 2>&1 || true",
		"gpu-workload":  "sudo k3s kubectl get daemonsets,pods -n gpu-operator -o wide 2>&1 || true; sudo k3s kubectl get deployment,pods,pvc -n forge-gpu-upgrade -o wide 2>&1 || true",
		"storage-state": storageStateDiagnosticCommand(state.host.DataStorageDevice),
	})
	dumpGPUDiagnostics(t, state.host.IP, state.privKeyPath)
	if safeClusterLogs {
		state.diagnostics.collectSharedCluster(t, filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml"))
	}
}

func cpuScenarioDiagnostics() []sharede2e.Hook[*cpuFixtureState] {
	return []sharede2e.Hook[*cpuFixtureState]{{Name: "shared-failure-evidence", Run: collectCPUDiagnostics}}
}

// cpuScenarioCleanup stops the local API tunnel; the workflow terminates the
// per-run host (C1).
func cpuScenarioCleanup() []sharede2e.Hook[*cpuFixtureState] {
	return []sharede2e.Hook[*cpuFixtureState]{{Name: "stop-api-tunnel", Run: func(_ *testing.T, state *cpuFixtureState) { state.fixture.stopAPITunnel() }}}
}

func gpuScenarioDiagnostics() []sharede2e.Hook[*gpuFixtureState] {
	return []sharede2e.Hook[*gpuFixtureState]{{Name: "shared-failure-evidence", Run: collectGPUDiagnostics}}
}

// gpuScenarioCleanup stops the local API tunnel; the workflow terminates the
// per-run host (C1).
func gpuScenarioCleanup() []sharede2e.Hook[*gpuFixtureState] {
	return []sharede2e.Hook[*gpuFixtureState]{{Name: "stop-api-tunnel", Run: func(_ *testing.T, state *gpuFixtureState) { state.fixture.stopAPITunnel() }}}
}

func TestForgeDiagnosticsRecordsFailureDomain(t *testing.T) {
	t.Setenv("ITERABASE_E2E_DIAGNOSTICS", t.TempDir())
	diagnostics := newForgeDiagnostics(t, "fixture")
	diagnostics.setDomain(failureDomainDependentSmoke)
	diagnostics.recordDomain(t)
	contents, err := os.ReadFile(filepath.Join(diagnostics.outputDir, "failure-domain.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(contents)) != failureDomainDependentSmoke {
		t.Fatalf("failure domain evidence = %q", contents)
	}
}

func TestBootstrapSecretLiteralsRequireExpectedCredentialShapes(t *testing.T) {
	t.Parallel()
	valid := "Admin API key (scope=admin): admin-secret\n" +
		"Service account API key (scope=token): token-secret\n"
	secrets, err := bootstrapSecretLiterals(valid)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(secrets, ",") != "admin-secret,token-secret" {
		t.Fatalf("bootstrap secrets = %v", secrets)
	}

	changed := "Admin bootstrap credential (scope=admin) => changed-admin-secret\n" +
		"Service account API key (scope=token): token-secret\n"
	if secrets, err = bootstrapSecretLiterals(changed); err == nil || secrets != nil {
		t.Fatalf("changed bootstrap credential format accepted: secrets=%v err=%v", secrets, err)
	} else if strings.Contains(err.Error(), "changed-admin-secret") {
		t.Fatalf("bootstrap parse error leaked credential: %v", err)
	}
}

func TestBootstrapCredentialInspectionCoversCurrentAndPreviousCollectorLogs(t *testing.T) {
	t.Parallel()
	requests, err := bootstrapLogRequests(`{
		"items": [{
			"metadata": {"name": "api-0"},
			"status": {"initContainerStatuses": [{"name": "bootstrap", "restartCount": 1}]}
		}]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0].previous || !requests[1].previous {
		t.Fatalf("bootstrap log requests = %+v, want current and previous", requests)
	}

	outputs := []string{
		"Admin API key (scope=admin): current-admin-secret\n" +
			"Service account API key (scope=token): current-token-secret\n" +
			strings.Repeat("later current log line\n", shareddiagnostics.PodLogTailLines+50),
		"Admin API key (scope=admin): previous-admin-secret\n" +
			"Service account API key (scope=token): previous-token-secret\n" +
			strings.Repeat("later previous log line\n", shareddiagnostics.PodLogTailLines+50),
	}
	var secrets []string
	for index, request := range requests {
		command := bootstrapCredentialLogCommand(request)
		if !strings.Contains(command, "--tail=-1") {
			t.Fatalf("bootstrap inspection must read the complete log: %s", command)
		}
		if request.previous != strings.Contains(command, "--previous") {
			t.Fatalf("bootstrap previous-log command mismatch: request=%+v command=%s", request, command)
		}
		literals, literalErr := bootstrapSecretLiterals(outputs[index])
		if literalErr != nil {
			t.Fatal(literalErr)
		}
		secrets = append(secrets, literals...)
	}
	if strings.Join(secrets, ",") != "current-admin-secret,current-token-secret,previous-admin-secret,previous-token-secret" {
		t.Fatalf("current/previous bootstrap secrets were not all registered: %v", secrets)
	}
}

func TestBootstrapCredentialInspectionRejectsChangedPreviousFormat(t *testing.T) {
	t.Parallel()
	previous := "Admin bootstrap credential (scope=admin) => previous-admin-secret\n" +
		"Service account API key (scope=token): previous-token-secret\n"
	secrets, err := bootstrapSecretLiterals(previous)
	if err == nil || secrets != nil {
		t.Fatalf("changed previous bootstrap credential format accepted: secrets=%v err=%v", secrets, err)
	}
	if strings.Contains(err.Error(), "previous-admin-secret") {
		t.Fatalf("bootstrap parse error leaked previous credential: %v", err)
	}
}

func TestBootstrapCredentialInspectionOmitsPreviousWithoutRestart(t *testing.T) {
	t.Parallel()
	requests, err := bootstrapLogRequests(`{
		"items": [{
			"metadata": {"name": "api-0"},
			"status": {"initContainerStatuses": [{"name": "bootstrap", "restartCount": 0}]}
		}]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].previous {
		t.Fatalf("bootstrap log requests = %+v, want current only", requests)
	}
}
