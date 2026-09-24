package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/forge/test/e2e/internal/remotecluster"
	"golang.org/x/crypto/ssh"
)

const workspaceNamespace = "iterabase-system"

var bootstrapCredentialPattern = regexp.MustCompile(`API key \(scope=([^)]+)\): (\S+)`)

type workspaceWorkItem struct {
	ID               string `json:"id"`
	State            string `json:"state"`
	CurrentAttemptID string `json:"currentAttemptId"`
}

type workspaceBlocker struct {
	ID string `json:"id"`
}

type workspaceModelStats struct {
	CapacityWaiting int64 `json:"capacity_waiting"`
}

func refuseProcessHeldDataStorageDiskStage(t *testing.T, state *permanentCPUFixtureState) {
	t.Helper()
	client, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer client.Close()
	const pidFile = "/tmp/forge-e2e-data-storage-consumer.pid"
	start := fmt.Sprintf(`sudo rm -f %[1]s; sudo nohup bash -c 'exec 9<>"$1"; printf "%%s\n" "$$" > "$2"; exec sleep 300' bash %[2]s %[1]s </dev/null >/tmp/forge-e2e-data-storage-consumer.log 2>&1 &
for i in $(seq 1 30); do test -s %[1]s && exit 0; sleep 1; done; exit 1`, pidFile, candidateShellQuote(state.dataStorageDevice))
	if output, startErr := sshOutput(client, start); startErr != nil {
		t.Fatalf("start raw-device consumer: %v\n%s", startErr, output)
	}
	stop := func() {
		_, _ = sshOutput(client, fmt.Sprintf(`sudo bash -c '
if test -s %[1]s; then
  pid=$(cat %[1]s)
  kill "$pid" 2>/dev/null || true
  for i in $(seq 1 30); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
  kill -9 "$pid" 2>/dev/null || true
fi
rm -f %[1]s
'`, pidFile))
	}
	defer stop()

	cfg := writeForgeConfigSpec(t, forgeConfigSpec{
		Name: state.runID, Address: state.ip, SSHKeyPath: state.privKeyPath, RunLabel: true, DualStack: true,
	})
	output, applyErr := runForgeE(state.forgeBin, state.forgeHome, "apply", "--config", cfg,
		"--skip-chart", "--skip-gpu", "--skip-overlay", "--skip-secrets", "--skip-flux")
	if applyErr == nil {
		t.Fatalf("Forge created a PV on a process-held raw data disk:\n%s", output)
	}
	if !strings.Contains(output, "held open as a raw block device") {
		t.Fatalf("process-held raw data-storage refusal was not actionable:\n%s", output)
	}
	unchanged := mustSSHOutput(t, client, fmt.Sprintf(`sudo bash -ceu '
test ! -e /var/lib/iterabase/data-storage.receipt
test ! -e /etc/rancher/k3s/k3s.yaml
set +e
signature=$(blkid -p -s TYPE -o value -- %s 2>&1)
rc=$?
set -e
test "$rc" = 2
test -z "$signature"
printf raw-consumer-refusal=pass
'`, state.dataStorageDevice))
	if !strings.Contains(unchanged, "raw-consumer-refusal=pass") {
		t.Fatalf("raw-consumer refusal did not preserve the blank pre-install boundary: %s", unchanged)
	}
	stop()
}

func setupWorkspaceExecutionFixtureStage(t *testing.T, state *permanentCPUFixtureState) {
	t.Helper()
	repository := os.Getenv("FORGE_E2E_RUNTIME_IMAGE_REPO")
	tag := os.Getenv("FORGE_E2E_RUNTIME_IMAGE_TAG")
	if repository == "" || tag == "" {
		t.Fatal("exact real-machine workspace behavior requires the deterministic runtime fixture image")
	}
	cluster := workspaceCluster(t, state)
	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata: {name: forge-workspace-model, namespace: iterabase-system}
spec:
  replicas: 1
  selector: {matchLabels: {app: forge-workspace-model}}
  template:
    metadata: {labels: {app: forge-workspace-model}}
    spec:
      containers:
        - name: model
          image: %s:%s
          imagePullPolicy: Never
          ports: [{name: http, containerPort: 8080}]
          readinessProbe: {httpGet: {path: /health, port: http}}
          securityContext: {runAsNonRoot: true, allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
---
apiVersion: v1
kind: Service
metadata: {name: forge-workspace-model, namespace: iterabase-system}
spec:
  selector: {app: forge-workspace-model}
  ports: [{name: http, port: 8080, targetPort: http}]
---
apiVersion: platform.iterabase.com/v1alpha1
kind: ModelBackend
metadata: {name: forge-workspace-model, namespace: iterabase-system}
spec:
  kind: external
  external: {baseURL: http://forge-workspace-model.iterabase-system.svc:8080}
---
apiVersion: platform.iterabase.com/v1alpha1
kind: Model
metadata: {name: forge-workspace-model, namespace: iterabase-system}
spec:
  modelID: forge-workspace-model
  displayName: Forge workspace deterministic model
  contextLength: 8192
  capabilities: [chat, tools]
  backendRef: forge-workspace-model
  defaultParams: {max_tokens: 1024}
  transforms: {rewrite_model_name: true}
---
%s
---
%s
---
%s
---
%s
`, repository, tag,
		workspaceBarrierWorkflow("forge-workspace-a", "e2e/forge-workspace-a", workspaceBarrierPrompt("session-a")),
		workspaceBarrierWorkflow("forge-workspace-b", "e2e/forge-workspace-b", workspaceBarrierPrompt("session-b")),
		workspaceAgentWorkflow("forge-workspace-capacity", "e2e/forge-workspace-capacity", workspaceCapacityPrompt()),
		workspaceHumanGateWorkflow())
	applyWorkspaceManifest(t, cluster, "workspace-real-behavior.yaml", manifest)
	cluster.Kubectl(t, "rollout", "status", "deployment/forge-workspace-model", "-n", workspaceNamespace, "--timeout=3m")
	cluster.Kubectl(t, "wait", "--for=jsonpath={.status.deployed}=true", "modelbackend/forge-workspace-model", "-n", workspaceNamespace, "--timeout=3m")
	cluster.Kubectl(t, "wait", "--for=jsonpath={.status.available}=true", "model/forge-workspace-model", "-n", workspaceNamespace, "--timeout=3m")
	for _, workflow := range []string{"forge-workspace-a", "forge-workspace-b", "forge-workspace-capacity", "forge-workspace-recovery"} {
		cluster.Kubectl(t, "wait", "--for=jsonpath={.status.ready}=true", "workflow/"+workflow, "-n", workspaceNamespace, "--timeout=3m")
	}

	apiPod := cluster.FirstPodName(t, workspaceNamespace, "app.kubernetes.io/name=control-plane,app.kubernetes.io/component=api")
	bootstrap := cluster.PodLogs(t, workspaceNamespace, apiPod, "bootstrap")
	keys := map[string]string{}
	for _, match := range bootstrapCredentialPattern.FindAllStringSubmatch(bootstrap, -1) {
		keys[match[1]] = match[2]
	}
	if keys["admin"] == "" {
		t.Fatal("exact candidate bootstrap did not emit an admin credential")
	}
	state.workspaceAdminKey = keys["admin"]
	state.diagnostics.redactor.Add(state.workspaceAdminKey)

	baseURL, stop := openWorkspaceAPI(t, cluster, state)
	defer stop()
	status, body := workspaceAPIRequest(t, baseURL, state.workspaceAdminKey, http.MethodPost, "/v1/users", map[string]any{
		"email": "forge-workspace-e2e@example.test", "role": "user",
	}, nil)
	requireWorkspaceStatus(t, status, http.StatusCreated)
	var user struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &user); err != nil || user.ID == "" {
		t.Fatalf("decode workspace E2E identity: %v", err)
	}
	status, body = workspaceAPIRequest(t, baseURL, state.workspaceAdminKey, http.MethodPost, "/v1/api-keys", map[string]any{
		"identityID": user.ID, "name": "forge-workspace-e2e", "scope": "work",
	}, nil)
	requireWorkspaceStatus(t, status, http.StatusCreated)
	var key struct {
		FullKey string `json:"fullKey"`
	}
	if err := json.Unmarshal(body, &key); err != nil || key.FullKey == "" {
		t.Fatalf("decode workspace E2E work credential: %v", err)
	}
	state.workspaceWorkKey = key.FullKey
	state.diagnostics.redactor.Add(state.workspaceWorkKey)
}

func exerciseConcurrentWorkspaceWorkStage(t *testing.T, state *permanentCPUFixtureState) {
	t.Helper()
	cluster := workspaceCluster(t, state)
	baseURL, stopAPI := openWorkspaceAPI(t, cluster, state)
	defer stopAPI()
	modelURL, stopModel := openWorkspaceService(t, cluster, "svc/forge-workspace-model", 8080)
	defer stopModel()

	first := startWorkspaceWork(t, baseURL, state.workspaceWorkKey, "e2e/forge-workspace-a", "Concurrent workspace session A", "hor-538-workspace-a")
	second := startWorkspaceWork(t, baseURL, state.workspaceWorkKey, "e2e/forge-workspace-b", "Concurrent workspace session B", "hor-538-workspace-b")

	// Initial model requests wait for an identity configuration rather than
	// incrementing an anonymous aggregate counter. Resolve both production
	// assignments and supervisor-observed sandbox identities before allowing
	// either child command to run.
	initialRows := waitWorkspaceConcurrencyRows(t, cluster, state, first.ID, second.ID, false, time.Minute)
	expected := expectedWorkspaceParticipants(t, first, second, initialRows)
	if err := validateInitialWorkspaceConcurrency(expected, initialRows); err != nil {
		t.Fatalf("initial concurrent workspace identity is invalid: %v", err)
	}
	workerA := initialRows[0].WorkerID
	configureConcurrentSiblingTargets(t, cluster, workerA, expected[0].SessionID, expected[1].SessionID)
	configureWorkspaceModelBarrier(t, modelURL, expected)

	// Each real child now executes the production bash path, emits its numeric
	// identity and direct denial proof, and reaches the keyed barrier on its next
	// model request. The barrier is successful only while one first arrival from
	// each exact expected participant remains simultaneously held.
	barrier := waitWorkspaceModelBarrier(t, modelURL, 2*time.Minute)
	rows := waitWorkspaceConcurrencyRows(t, cluster, state, first.ID, second.ID, true, time.Minute)
	if err := validateWorkspaceConcurrencyProof(expected, rows, barrier); err != nil {
		t.Fatalf("correlated concurrent workspace proof failed: %v", err)
	}
	status, _ := workspaceAPIRequest(t, modelURL, "", http.MethodPost, "/workspace/barrier/release", nil, nil)
	requireWorkspaceStatus(t, status, http.StatusNoContent)
	first = waitWorkspaceWorkState(t, baseURL, state.workspaceWorkKey, first.ID, "blocked", 4*time.Minute)
	second = waitWorkspaceWorkState(t, baseURL, state.workspaceWorkKey, second.ID, "blocked", 4*time.Minute)

	bashCalls := workspaceDatabaseQuery(t, cluster, state, fmt.Sprintf(`SELECT count(*) FROM runtime.events WHERE run_id IN ('%s','%s') AND kind='tool_call_started' AND payload->>'tool_name'='bash'`, first.CurrentAttemptID, second.CurrentAttemptID))
	if bashCalls != "2" {
		t.Fatalf("concurrent workspace work did not execute exactly one isolated bash command per session: %s", bashCalls)
	}
	assertConcurrentWorkspaceMarkers(t, cluster, workerA, expected[0].SessionID, expected[1].SessionID)
	respondWorkspaceBlocker(t, baseURL, state.workspaceWorkKey, first.ID)
	respondWorkspaceBlocker(t, baseURL, state.workspaceWorkKey, second.ID)
	_ = waitWorkspaceWorkState(t, baseURL, state.workspaceWorkKey, first.ID, "done", 2*time.Minute)
	_ = waitWorkspaceWorkState(t, baseURL, state.workspaceWorkKey, second.ID, "done", 2*time.Minute)
}

func exerciseActiveWorkspaceCapacityStage(t *testing.T, state *permanentCPUFixtureState) {
	t.Helper()
	cluster := workspaceCluster(t, state)
	baseURL, stopAPI := openWorkspaceAPI(t, cluster, state)
	defer stopAPI()
	modelURL, stopModel := openWorkspaceService(t, cluster, "svc/forge-workspace-model", 8080)
	defer stopModel()
	client, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer client.Close()
	const filler = "/data/sandboxes/.forge-e2e-capacity-fill"
	removeFiller := func() {
		_, _ = sshOutput(client, `pod=$(sudo k3s kubectl get pods -n iterabase-system -l platform.iterabase.com/agentpool=forge-storage-pool -o jsonpath='{.items[0].metadata.name}'); sudo k3s kubectl exec -n iterabase-system "$pod" -- bash -ceu 'rm -f /data/sandboxes/.forge-e2e-capacity-fill && sync'`)
	}
	defer removeFiller()

	active := startWorkspaceWork(t, baseURL, state.workspaceWorkKey, "e2e/forge-workspace-capacity", "Active turn across capacity floor", "hor-545-capacity-active")
	waitWorkspaceModelCapacity(t, modelURL, 1, 2*time.Minute)
	waitWorkspaceDatabaseValue(t, cluster, state, fmt.Sprintf(`SELECT count(*) FROM runtime.turn_assignments WHERE attempt_id='%s' AND state='active'`, active.CurrentAttemptID), "1", time.Minute)
	growAgentPoolDuringActiveTurn(t, cluster, state, client, active)
	setWorkspaceFreeTarget(t, client, filler, 19)
	waitWorkspaceGate(t, client, true, storageReasonWorkspaceCapacityGatedE2E, 3*time.Minute)
	otherMetrics := mustSSHOutput(t, client, `pod=$(sudo k3s kubectl get pods -n iterabase-system -l platform.iterabase.com/agentpool=forge-storage-pool-b -o jsonpath='{.items[0].metadata.name}'); sudo k3s kubectl get --raw "/api/v1/namespaces/iterabase-system/pods/$pod:8081/proxy/metrics"`)
	if !strings.Contains(otherMetrics, "control_plane_harness_workspace_credit_gated 0") {
		t.Fatalf("filling one AgentPool PVC gated the separate pool:\n%s", otherMetrics)
	}
	if rows := workspaceDatabaseQuery(t, cluster, state, `SELECT count(*) FROM runtime.workspace_capacity_state WHERE observed`); rows != "2" {
		t.Fatalf("per-pool durable capacity rows=%s want=2", rows)
	}

	// The model request remains deliberately blocked while the actual filesystem
	// crosses the floor. The threshold alone must not terminalize/fence it.
	time.Sleep(12 * time.Second)
	active = readWorkspaceWork(t, baseURL, state.workspaceWorkKey, active.ID)
	if active.State != "in_progress" {
		t.Fatalf("capacity threshold aborted the active work item: state=%s", active.State)
	}
	if got := workspaceDatabaseQuery(t, cluster, state, fmt.Sprintf(`SELECT count(*) FROM runtime.turn_assignments WHERE attempt_id='%s' AND state='active'`, active.CurrentAttemptID)); got != "1" {
		t.Fatalf("capacity threshold fenced the active assignment: active=%s", got)
	}

	status, _ := workspaceAPIRequest(t, modelURL, "", http.MethodPost, "/release/capacity", nil, nil)
	requireWorkspaceStatus(t, status, http.StatusNoContent)
	active = waitWorkspaceWorkState(t, baseURL, state.workspaceWorkKey, active.ID, "done", 4*time.Minute)
	queued := startWorkspaceWork(t, baseURL, state.workspaceWorkKey, "e2e/forge-workspace-capacity", "Queued while workspace gated", "hor-545-capacity-queued")
	time.Sleep(15 * time.Second)
	if got := workspaceDatabaseQuery(t, cluster, state, fmt.Sprintf(`SELECT count(*) FROM runtime.turn_assignments WHERE attempt_id='%s'`, queued.CurrentAttemptID)); got != "0" {
		t.Fatalf("fresh assignment escaped the <=20%% capacity gate: assignments=%s", got)
	}

	// Return only to 22% and replace a worker. The shared/durable gate must stay
	// closed in the hysteresis band on the fresh supervisor.
	setWorkspaceFreeTarget(t, client, filler, 22)
	waitWorkspaceGate(t, client, true, storageReasonWorkspaceCapacityGatedE2E, 2*time.Minute)
	replaceIdleWorkspaceWorkerAtGate(t, cluster, state)
	waitWorkspaceGate(t, client, true, storageReasonWorkspaceCapacityGatedE2E, 2*time.Minute)
	if got := workspaceDatabaseQuery(t, cluster, state, fmt.Sprintf(`SELECT count(*) FROM runtime.turn_assignments WHERE attempt_id='%s'`, queued.CurrentAttemptID)); got != "0" {
		t.Fatalf("replacement worker reopened credit inside the 20-25%% band: assignments=%s", got)
	}

	removeFiller()
	waitWorkspaceGate(t, client, false, storageReasonWorkspaceCapacityHealthyE2E, 3*time.Minute)
	_ = waitWorkspaceWorkState(t, baseURL, state.workspaceWorkKey, queued.ID, "done", 4*time.Minute)
}

func growAgentPoolDuringActiveTurn(t *testing.T, cluster *remotecluster.Cluster, state *permanentCPUFixtureState, client *ssh.Client, active workspaceWorkItem) {
	t.Helper()
	const (
		pool  = "forge-storage-pool"
		claim = "forge-storage-pool-sandbox"
	)
	workersBefore := strings.TrimSpace(mustSSHOutput(t, client, `sudo k3s kubectl get pods -n iterabase-system -l platform.iterabase.com/agentpool=forge-storage-pool -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' | sort`))
	pod := strings.Fields(mustSSHOutput(t, client, `sudo k3s kubectl get pods -n iterabase-system -l platform.iterabase.com/agentpool=forge-storage-pool -o name`))[0]
	beforeFS := strings.TrimSpace(mustSSHOutput(t, client, fmt.Sprintf(`sudo k3s kubectl exec -n iterabase-system -c supervisor %s -- sh -ceu 'df -B1 --output=size /data/sandboxes | tail -1 | tr -d " "'`, pod)))
	mustSSHOutput(t, client, fmt.Sprintf(`sudo k3s kubectl exec -n iterabase-system -c supervisor %s -- sh -ceu 'printf HOR-557-agentpool-growth > /data/sandboxes/.growth-marker; sync'`, pod))
	identity := strings.TrimSpace(mustSSHOutput(t, client, `sudo k3s kubectl get pvc forge-storage-pool-sandbox -n iterabase-system -o jsonpath='{.metadata.uid}|{.spec.volumeName}'`))
	parts := strings.Split(identity, "|")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("AgentPool pre-growth identity is malformed: %q", identity)
	}
	handle := strings.TrimSpace(mustSSHOutput(t, client, fmt.Sprintf(`sudo k3s kubectl get pv %s -o jsonpath='{.spec.csi.volumeHandle}'`, parts[1])))
	hostIdentity := strings.TrimSpace(mustSSHOutput(t, client, fmt.Sprintf(`sudo bash -ceu 'lv=/dev/iterabase-data/%s; lvuuid=$(lvs --noheadings -o lv_uuid "$lv" | xargs); fsuuid=$(blkid -s UUID -o value "$lv"); printf "%%s|%%s" "$lvuuid" "$fsuuid"'`, candidateShellQuote(handle))))
	if handle == "" || strings.Contains(hostIdentity, "||") || !strings.Contains(hostIdentity, "|") {
		t.Fatalf("AgentPool pre-growth LVM/XFS identity is incomplete: handle=%q host=%q", handle, hostIdentity)
	}

	cluster.Kubectl(t, "patch", "agentpool/"+pool, "-n", workspaceNamespace, "--type=merge", "-p", `{"spec":{"sandbox":{"size":"3Gi"}}}`)
	deadline := time.Now().Add(3 * time.Minute)
	observedClosed := false
	for time.Now().Before(deadline) {
		status, err := kubectlAllowFail(t, cluster.Kubeconfig, "get", "agentpool/"+pool, "-n", workspaceNamespace, "-o", `jsonpath={.status.conditions[?(@.type=="StorageReady")].status}|{.status.conditions[?(@.type=="StorageReady")].reason}`)
		request, requestErr := kubectlAllowFail(t, cluster.Kubeconfig, "get", "pvc/"+claim, "-n", workspaceNamespace, "-o", "jsonpath={.spec.resources.requests.storage}")
		if err == nil && requestErr == nil && strings.TrimSpace(request) == "3Gi" && strings.HasPrefix(strings.TrimSpace(status), "False|PVCExpansion") {
			observedClosed = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !observedClosed {
		t.Fatal("AgentPool did not expose StorageReady=False while its existing claim grew")
	}
	workersDuring := strings.TrimSpace(mustSSHOutput(t, client, `sudo k3s kubectl get pods -n iterabase-system -l platform.iterabase.com/agentpool=forge-storage-pool -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' | sort`))
	if workersDuring != workersBefore {
		t.Fatalf("safe AgentPool resize replaced mounted workers: before=%q during=%q", workersBefore, workersDuring)
	}
	if got := workspaceDatabaseQuery(t, cluster, state, fmt.Sprintf(`SELECT count(*) FROM runtime.turn_assignments WHERE attempt_id='%s' AND state='active'`, active.CurrentAttemptID)); got != "1" {
		t.Fatalf("safe AgentPool resize aborted/fenced the active turn: active assignments=%s", got)
	}

	cluster.Kubectl(t, "wait", "pvc/"+claim, "-n", workspaceNamespace, "--for=jsonpath={.status.capacity.storage}=3Gi", "--timeout=15m")
	cluster.Kubectl(t, "wait", "agentpool/"+pool, "-n", workspaceNamespace, "--for=jsonpath={.status.conditions[?(@.type==\"StorageReady\")].status}=True", "--timeout=15m")
	workersAfter := strings.TrimSpace(mustSSHOutput(t, client, `sudo k3s kubectl get pods -n iterabase-system -l platform.iterabase.com/agentpool=forge-storage-pool -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' | sort`))
	if workersAfter != workersBefore {
		t.Fatalf("converged AgentPool resize replaced workers: before=%q after=%q", workersBefore, workersAfter)
	}
	if after := strings.TrimSpace(mustSSHOutput(t, client, `sudo k3s kubectl get pvc forge-storage-pool-sandbox -n iterabase-system -o jsonpath='{.metadata.uid}|{.spec.volumeName}'`)); after != identity {
		t.Fatalf("AgentPool growth replaced PVC/PV identity: before=%s after=%s", identity, after)
	}
	lvm := strings.TrimSpace(mustSSHOutput(t, client, fmt.Sprintf(`sudo k3s kubectl get lvmvolume.local.openebs.io %s -n iterabase-system -o jsonpath='{.spec.capacity}|{.status.state}'`, handle)))
	assertLVMVolumeCapacity(t, lvm, "3Gi")
	if after := strings.TrimSpace(mustSSHOutput(t, client, fmt.Sprintf(`sudo bash -ceu 'lv=/dev/iterabase-data/%s; lvuuid=$(lvs --noheadings -o lv_uuid "$lv" | xargs); fsuuid=$(blkid -s UUID -o value "$lv"); printf "%%s|%%s" "$lvuuid" "$fsuuid"'`, candidateShellQuote(handle)))); after != hostIdentity {
		t.Fatalf("AgentPool growth replaced LV/filesystem identity: before=%s after=%s", hostIdentity, after)
	}
	afterFS := strings.TrimSpace(mustSSHOutput(t, client, fmt.Sprintf(`sudo k3s kubectl exec -n iterabase-system -c supervisor %s -- sh -ceu 'test "$(cat /data/sandboxes/.growth-marker)" = HOR-557-agentpool-growth; df -B1 --output=size /data/sandboxes | tail -1 | tr -d " "'`, pod)))
	beforeBytes, beforeErr := strconv.ParseUint(beforeFS, 10, 64)
	afterBytes, afterErr := strconv.ParseUint(afterFS, 10, 64)
	if beforeErr != nil || afterErr != nil || afterBytes <= beforeBytes {
		t.Fatalf("AgentPool mounted XFS did not grow: before=%q after=%q errors=%v/%v", beforeFS, afterFS, beforeErr, afterErr)
	}
	if got := workspaceDatabaseQuery(t, cluster, state, fmt.Sprintf(`SELECT count(*) FROM runtime.turn_assignments WHERE attempt_id='%s' AND state='active'`, active.CurrentAttemptID)); got != "1" {
		t.Fatalf("AgentPool resize convergence changed the active assignment: %s", got)
	}
}

func exerciseAggregateVGCapacityStage(t *testing.T, state *permanentCPUFixtureState) {
	t.Helper()
	client, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer client.Close()
	before := strings.Fields(strings.TrimSpace(mustSSHOutput(t, client, `sudo vgs --noheadings --units b --nosuffix -o vg_free,lv_count iterabase-data | awk '{$1=$1;printf "%.0f %s",$1,$2}'`)))
	if len(before) != 2 {
		t.Fatalf("aggregate VG baseline is malformed: %v", before)
	}
	free, err := strconv.ParseUint(before[0], 10, 64)
	if err != nil || free < 2<<30 {
		t.Fatalf("aggregate VG has insufficient test capacity: free=%q err=%v", before[0], err)
	}
	allocate := free * 80 / 100
	manifest := fmt.Sprintf(`cat <<'YAML' | sudo k3s kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: forge-vg-pressure, namespace: iterabase-system}
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: iterabase-lvm-xfs
  resources: {requests: {storage: %d}}
---
apiVersion: v1
kind: Pod
metadata: {name: forge-vg-pressure, namespace: iterabase-system}
spec:
  restartPolicy: Never
  containers:
    - name: hold
      image: debian:13-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
      command: [bash, -ceu]
      args: ['printf aggregate-vg-pressure=pass > /data/marker; sync -f /data/marker; sleep 600']
      volumeMounts: [{name: data, mountPath: /data}]
  volumes: [{name: data, persistentVolumeClaim: {claimName: forge-vg-pressure}}]
YAML`, allocate)
	mustSSHOutput(t, client, manifest)
	mustSSHOutput(t, client, "sudo k3s kubectl wait -n iterabase-system --for=jsonpath='{.status.phase}'=Bound pvc/forge-vg-pressure --timeout=10m")
	mustSSHOutput(t, client, "sudo k3s kubectl wait -n iterabase-system --for=condition=Ready pod/forge-vg-pressure --timeout=10m")

	metricsCommand := `for i in $(seq 1 90); do
  service=$(sudo k3s kubectl get service -n iterabase-system -l name=openebs-lvm-node,openebs.io/component-name=openebs-lvm-node -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  metrics=$(sudo k3s kubectl get --raw "/api/v1/namespaces/iterabase-system/services/http:$service:9500/proxy/metrics" 2>/dev/null || true)
  free=$(printf '%s\n' "$metrics" | awk '$1=="lvm_vg_free_size_bytes{name=\"iterabase-data\"}" {printf "%.0f",$2}')
  total=$(printf '%s\n' "$metrics" | awk '$1=="lvm_vg_total_size_bytes{name=\"iterabase-data\"}" {printf "%.0f",$2}')
  test -n "$free" && test -n "$total" && test "$free" -gt 0 && test $((free * 100 / total)) -le 25 && { printf '%s|%s\n' "$free" "$total"; exit 0; }
  sleep 2
done
exit 1`
	metricEvidence, err := sshOutput(client, metricsCommand)
	if err != nil {
		t.Fatalf("aggregate iterabase-data metrics did not report pressure: %v\n%s", err, metricEvidence)
	}
	remainingText := strings.TrimSpace(mustSSHOutput(t, client, `sudo vgs --noheadings --units b --nosuffix -o vg_free iterabase-data | awk '{$1=$1;printf "%.0f",$1}'`))
	remaining, err := strconv.ParseUint(remainingText, 10, 64)
	if err != nil {
		t.Fatalf("parse pressured VG free capacity %q: %v", remainingText, err)
	}
	exerciseResizeExhaustion(t, client, remaining, state.agentPoolPVCUID)
	exhaustRequest := remaining + 1<<30
	exhaust := fmt.Sprintf(`cat <<'YAML' | sudo k3s kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: forge-vg-exhaustion, namespace: iterabase-system}
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: iterabase-lvm-xfs
  resources: {requests: {storage: %d}}
---
apiVersion: v1
kind: Pod
metadata: {name: forge-vg-exhaustion, namespace: iterabase-system}
spec:
  containers:
    - name: hold
      image: debian:13-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
      command: [sleep, "600"]
      volumeMounts: [{name: data, mountPath: /data}]
  volumes: [{name: data, persistentVolumeClaim: {claimName: forge-vg-exhaustion}}]
YAML`, exhaustRequest)
	mustSSHOutput(t, client, exhaust)
	// DES-HOR-545-01 honest new-claim exhaustion. Under WaitForFirstConsumer +
	// CSI storage-capacity scheduling the exhausted claim must reach a concrete,
	// actionable terminal to count as evidence: either ProvisioningFailed (OpenEBS
	// attempted and was refused because the VG cannot fit it) or a scheduler
	// capacity decision (FailedBinding on the PVC, or FailedScheduling on the
	// consumer pod) withholding the claim because no node can satisfy it. The
	// bare post-create Pending/Pending state is not proof of exhaustion (it is
	// the ordinary initial state before any scheduling), so we require an observed
	// reason event. The claim must never become Bound and no LV may be created;
	// observe deterministically with a bounded poll rather than a fixed sleep.
	deadline := time.Now().Add(2 * time.Minute)
	var phase, podPhase, ev string
	for {
		phase = strings.TrimSpace(mustSSHOutput(t, client, `sudo k3s kubectl get pvc forge-vg-exhaustion -n iterabase-system -o jsonpath='{.status.phase}'`))
		podPhase = strings.TrimSpace(mustSSHOutput(t, client, `sudo k3s kubectl get pod forge-vg-exhaustion -n iterabase-system -o jsonpath='{.status.phase}'`))
		ev = mustSSHOutput(t, client, `sudo k3s kubectl get events -n iterabase-system --field-selector involvedObject.name=forge-vg-exhaustion -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}'`)
		if phase == "Bound" {
			t.Fatalf("aggregate VG exhaustion claim became Bound (overcommit/fallback forbidden): podPhase=%s events=%s", podPhase, ev)
		}
		concrete := strings.Contains(ev, "ProvisioningFailed") || strings.Contains(ev, "FailedBinding") || strings.Contains(ev, "FailedScheduling")
		capacityWithheld := phase == "Pending" && concrete
		if capacityWithheld {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("aggregate VG exhaustion did not reach a concrete honest terminal (ProvisioningFailed/FailedBinding/FailedScheduling): phase=%s podPhase=%s events=%s", phase, podPhase, ev)
		}
		time.Sleep(time.Second)
	}
	if phase != "Pending" {
		t.Fatalf("aggregate VG exhaustion was not an honest Pending claim (must be withheld with a concrete event, not another terminal): phase=%s podPhase=%s events=%s", phase, podPhase, ev)
	}
	mustSSHOutput(t, client, "sudo k3s kubectl delete pod/forge-vg-exhaustion pvc/forge-vg-exhaustion pod/forge-vg-pressure pvc/forge-vg-pressure -n iterabase-system --ignore-not-found=true --wait=true --timeout=10m")
	command := fmt.Sprintf(`for i in $(seq 1 150); do test "$(sudo lvs --noheadings -o lv_name iterabase-data | awk 'NF {n++} END {print n+0}')" = %s && exit 0; sleep 2; done; exit 1`, before[1])
	if output, err := sshOutput(client, command); err != nil {
		t.Fatalf("aggregate pressure claim cleanup leaked an LV: %v\n%s", err, output)
	}
}

func exerciseResizeExhaustion(t *testing.T, client *ssh.Client, freeBefore uint64, agentPoolPVCUID string) {
	t.Helper()
	manifest := `cat <<'YAML' | sudo k3s kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: forge-resize-exhaustion, namespace: iterabase-system}
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: iterabase-lvm-xfs
  resources: {requests: {storage: 512Mi}}
---
apiVersion: v1
kind: Pod
metadata: {name: forge-resize-exhaustion, namespace: iterabase-system}
spec:
  containers:
    - name: hold
      image: debian:13-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
      command: [bash, -ceu]
      args: ['printf resize-exhaustion-preserved > /data/marker; sync -f /data/marker; sleep 1200']
      volumeMounts: [{name: data, mountPath: /data}]
  volumes: [{name: data, persistentVolumeClaim: {claimName: forge-resize-exhaustion}}]
YAML`
	mustSSHOutput(t, client, manifest)
	mustSSHOutput(t, client, "sudo k3s kubectl wait -n iterabase-system --for=condition=Ready pod/forge-resize-exhaustion --timeout=10m")
	identity := strings.TrimSpace(mustSSHOutput(t, client, `sudo k3s kubectl get pvc forge-resize-exhaustion -n iterabase-system -o jsonpath='{.metadata.uid}|{.spec.volumeName}'`))
	parts := strings.Split(identity, "|")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("resize-exhaustion identity is malformed: %q", identity)
	}
	handle := strings.TrimSpace(mustSSHOutput(t, client, fmt.Sprintf(`sudo k3s kubectl get pv %s -o jsonpath='{.spec.csi.volumeHandle}'`, parts[1])))
	request := freeBefore + 2<<30
	mustSSHOutput(t, client, fmt.Sprintf(`sudo k3s kubectl patch pvc/forge-resize-exhaustion -n iterabase-system --type=merge -p '{"spec":{"resources":{"requests":{"storage":"%d"}}}}'`, request))

	deadline := time.Now().Add(3 * time.Minute)
	var events string
	for time.Now().Before(deadline) {
		events = mustSSHOutput(t, client, `sudo k3s kubectl get events -n iterabase-system --field-selector involvedObject.name=forge-resize-exhaustion -o jsonpath='{range .items[*]}{.reason}|{.message}{"\n"}{end}'`)
		if resizeFailureActionable(events) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !resizeFailureActionable(events) {
		t.Fatalf("insufficient-VG resize did not emit an actionable failure: %s", events)
	}
	observed := strings.TrimSpace(mustSSHOutput(t, client, `sudo k3s kubectl get pvc forge-resize-exhaustion -n iterabase-system -o jsonpath='{.metadata.uid}|{.spec.volumeName}|{.status.capacity.storage}'`))
	if observed != identity+"|512Mi" {
		t.Fatalf("failed resize changed claim/PV/current usable capacity: before=%s after=%s", identity, observed)
	}
	if after := strings.TrimSpace(mustSSHOutput(t, client, fmt.Sprintf(`sudo k3s kubectl get pv %s -o jsonpath='{.spec.csi.volumeHandle}'`, parts[1]))); after != handle {
		t.Fatalf("failed resize replaced the OpenEBS volume handle: before=%s after=%s", handle, after)
	}
	mustSSHOutput(t, client, `sudo k3s kubectl exec -n iterabase-system forge-resize-exhaustion -- sh -ceu 'test "$(cat /data/marker)" = resize-exhaustion-preserved'`)
	if unrelated := strings.TrimSpace(mustSSHOutput(t, client, `sudo k3s kubectl get pvc forge-storage-pool-sandbox -n iterabase-system -o jsonpath='{.metadata.uid}'`)); unrelated != agentPoolPVCUID {
		t.Fatalf("failed resize damaged unrelated AgentPool claim: before=%s after=%s", agentPoolPVCUID, unrelated)
	}
	if output, err := sshOutput(client, `sudo k3s kubectl patch pvc/forge-resize-exhaustion -n iterabase-system --type=merge -p '{"spec":{"resources":{"requests":{"storage":"256Mi"}}}}' 2>&1`); err == nil {
		t.Fatalf("PVC shrink unexpectedly succeeded after failed growth: %s", output)
	}
	mustSSHOutput(t, client, "sudo k3s kubectl delete pod/forge-resize-exhaustion pvc/forge-resize-exhaustion -n iterabase-system --wait=true --timeout=10m")
	cleanup := fmt.Sprintf(`for i in $(seq 1 150); do ! sudo k3s kubectl get pv %s >/dev/null 2>&1 && ! sudo k3s kubectl get lvmvolume.local.openebs.io %s -n iterabase-system >/dev/null 2>&1 && exit 0; sleep 2; done; exit 1`, parts[1], handle)
	if output, err := sshOutput(client, cleanup); err != nil {
		t.Fatalf("failed-resize claim cleanup leaked storage identity: %v\n%s", err, output)
	}
}

// resizeFailureActionable reports whether the resize events carry the actionable
// failure signal the insufficient-VG stage requires: an explicit
// VolumeResizeFailed event, or an ExternalExpanding event that names capacity.
// The wait loop and the assertion share this one definition.
func resizeFailureActionable(events string) bool {
	return strings.Contains(events, "VolumeResizeFailed") ||
		(strings.Contains(events, "ExternalExpanding") && strings.Contains(strings.ToLower(events), "capacity"))
}

func exerciseHumanGateWorkspaceReplacementStage(t *testing.T, state *permanentCPUFixtureState) {
	t.Helper()
	cluster := workspaceCluster(t, state)
	cluster.Kubectl(t, "patch", "agentpool/forge-storage-pool", "-n", workspaceNamespace, "--type=merge", "-p", `{"spec":{"replicas":1}}`)
	waitWorkspacePoolReplicas(t, cluster, 1, 4*time.Minute)
	baseURL, stopAPI := openWorkspaceAPI(t, cluster, state)
	defer stopAPI()

	item := startWorkspaceWork(t, baseURL, state.workspaceWorkKey, "e2e/forge-workspace-recovery", "Human-gated workspace recovery", "hor-545-human-recovery")
	item = waitWorkspaceWorkState(t, baseURL, state.workspaceWorkKey, item.ID, "blocked", 4*time.Minute)
	sessionBefore := workspaceDatabaseQuery(t, cluster, state, fmt.Sprintf(`SELECT session_id FROM runtime.workflow_runs WHERE id='%s'`, item.CurrentAttemptID))
	assignedWorker := workspaceDatabaseQuery(t, cluster, state, fmt.Sprintf(`SELECT worker_id FROM runtime.turn_assignments WHERE attempt_id='%s' ORDER BY assigned_at LIMIT 1`, item.CurrentAttemptID))
	if assignedWorker == "" {
		t.Fatal("human-gated work has no attributable initial worker")
	}
	oldUID := strings.TrimSpace(cluster.Kubectl(t, "get", "pod/"+assignedWorker, "-n", workspaceNamespace, "-o", "jsonpath={.metadata.uid}"))
	pvcBefore := strings.TrimSpace(cluster.Kubectl(t, "get", "pvc/forge-storage-pool-sandbox", "-n", workspaceNamespace, "-o", "jsonpath={.metadata.uid}"))
	assertRecoveryWorkspaceState(t, cluster, sessionBefore, "initial", false)
	cluster.Kubectl(t, "delete", "pod/"+assignedWorker, "-n", workspaceNamespace, "--wait=true", "--timeout=3m")
	waitWorkspaceReplacementPod(t, cluster, assignedWorker, oldUID, 4*time.Minute)
	pvcAfter := strings.TrimSpace(cluster.Kubectl(t, "get", "pvc/forge-storage-pool-sandbox", "-n", workspaceNamespace, "-o", "jsonpath={.metadata.uid}"))
	if pvcAfter != pvcBefore {
		t.Fatalf("human-gate worker replacement changed the RWO claim: before=%s after=%s", pvcBefore, pvcAfter)
	}
	assertRecoveryWorkspaceState(t, cluster, sessionBefore, "replacement", false)

	respondWorkspaceBlocker(t, baseURL, state.workspaceWorkKey, item.ID)
	waitWorkspaceDatabaseValue(t, cluster, state, fmt.Sprintf(`SELECT count(*) FROM runtime.turn_assignments WHERE attempt_id='%s'`, item.CurrentAttemptID), "2", 4*time.Minute)
	item = waitWorkspaceWorkState(t, baseURL, state.workspaceWorkKey, item.ID, "blocked", 2*time.Minute)
	recoveryEvents := workspaceDatabaseQuery(t, cluster, state, fmt.Sprintf(`SELECT string_agg(seq::text || ':' || kind || ':' || payload::text, E'\n' ORDER BY seq) FROM runtime.events WHERE run_id='%s'`, item.CurrentAttemptID))
	t.Logf("human-gate recovery events before external resumed proof:\n%s", recoveryEvents)
	assertRecoveryWorkspaceState(t, cluster, sessionBefore, "resumed", true)
	sessionAfter := workspaceDatabaseQuery(t, cluster, state, fmt.Sprintf(`SELECT session_id FROM runtime.workflow_runs WHERE id='%s'`, item.CurrentAttemptID))
	if sessionAfter != sessionBefore {
		t.Fatalf("human-gate resume changed durable session identity: before=%s after=%s", sessionBefore, sessionAfter)
	}
	assignments := workspaceDatabaseQuery(t, cluster, state, fmt.Sprintf(`SELECT count(*)::text || '|' || count(DISTINCT fencing_generation)::text FROM runtime.turn_assignments WHERE attempt_id='%s'`, item.CurrentAttemptID))
	if assignments != "2|2" {
		t.Fatalf("human-gate resume did not use a fresh fenced worker generation: %q", assignments)
	}
	bashCalls := workspaceDatabaseQuery(t, cluster, state, fmt.Sprintf(`SELECT count(*) FROM runtime.events WHERE run_id='%s' AND kind='tool_call_started' AND payload->>'tool_name'='bash'`, item.CurrentAttemptID))
	if bashCalls != "2" {
		t.Fatalf("worker replacement duplicated or omitted the intended workspace consequence: bash calls=%s", bashCalls)
	}
	respondWorkspaceBlocker(t, baseURL, state.workspaceWorkKey, item.ID)
	_ = waitWorkspaceWorkState(t, baseURL, state.workspaceWorkKey, item.ID, "done", 2*time.Minute)
}

const (
	storageReasonWorkspaceCapacityGatedE2E   = "WorkspaceCapacityGateActive"
	storageReasonWorkspaceCapacityHealthyE2E = "WorkspaceCapacityHealthy"
)

func workspaceCluster(t *testing.T, state *permanentCPUFixtureState) *remotecluster.Cluster {
	t.Helper()
	return remotecluster.Use(t, filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml"))
}

func applyWorkspaceManifest(t *testing.T, cluster *remotecluster.Cluster, name, manifest string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatalf("write workspace behavior manifest: %v", err)
	}
	cluster.Kubectl(t, "apply", "-f", path)
}

func openWorkspaceAPI(t *testing.T, cluster *remotecluster.Cluster, state *permanentCPUFixtureState) (string, func()) {
	t.Helper()
	return openWorkspaceService(t, cluster, "svc/"+state.runID+"-control-plane-api", 8080)
}

func openWorkspaceService(t *testing.T, cluster *remotecluster.Cluster, service string, port int) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port: %v", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return cluster.PortForward(t, workspaceNamespace, service, port, localPort)
}

func workspaceAPIRequest(t *testing.T, baseURL, key, method, path string, body any, headers map[string]string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal workspace API request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(context.Background(), method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("build workspace API request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("workspace API %s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("read workspace API response: %v", err)
	}
	return response.StatusCode, data
}

func requireWorkspaceStatus(t *testing.T, got int, want ...int) {
	t.Helper()
	for _, candidate := range want {
		if got == candidate {
			return
		}
	}
	t.Fatalf("workspace API status=%d want=%v", got, want)
}

func startWorkspaceWork(t *testing.T, baseURL, key, workflowKey, title, idempotencyKey string) workspaceWorkItem {
	t.Helper()
	payload := map[string]any{
		"workflowKey": workflowKey,
		"title":       title,
		"source":      map[string]any{"fixture": "HOR-545"},
		"sourcePresentation": map[string]any{
			"kind": "api", "title": title, "subtitle": "Exact-candidate workspace behavior",
		},
	}
	headers := map[string]string{"Idempotency-Key": idempotencyKey}
	status, body := workspaceAPIRequest(t, baseURL, key, http.MethodPost, "/v1/work-items", payload, headers)
	requireWorkspaceStatus(t, status, http.StatusCreated)
	var item workspaceWorkItem
	if err := json.Unmarshal(body, &item); err != nil || item.ID == "" || item.CurrentAttemptID == "" {
		t.Fatalf("decode started workspace work: %v", err)
	}
	status, replayBody := workspaceAPIRequest(t, baseURL, key, http.MethodPost, "/v1/work-items", payload, headers)
	requireWorkspaceStatus(t, status, http.StatusOK, http.StatusCreated)
	var replay workspaceWorkItem
	if err := json.Unmarshal(replayBody, &replay); err != nil || replay.ID != item.ID || replay.CurrentAttemptID != item.CurrentAttemptID {
		t.Fatalf("idempotent manual_api replay diverged: first=%s/%s replay=%s/%s err=%v", item.ID, item.CurrentAttemptID, replay.ID, replay.CurrentAttemptID, err)
	}
	return item
}

func respondWorkspaceBlocker(t *testing.T, baseURL, key, itemID string) {
	t.Helper()
	status, body := workspaceAPIRequest(t, baseURL, key, http.MethodGet, "/v1/work-items/"+itemID+"/blocker", nil, nil)
	requireWorkspaceStatus(t, status, http.StatusOK)
	var blocker workspaceBlocker
	if err := json.Unmarshal(body, &blocker); err != nil || blocker.ID == "" {
		t.Fatalf("decode human-gate blocker: %v", err)
	}
	status, _ = workspaceAPIRequest(t, baseURL, key, http.MethodPost, "/v1/work-blockers/"+blocker.ID+"/responses", map[string]any{
		"outcome": "continued", "response": map[string]any{},
	}, nil)
	requireWorkspaceStatus(t, status, http.StatusOK)
}

func readWorkspaceWork(t *testing.T, baseURL, key, id string) workspaceWorkItem {
	t.Helper()
	status, body := workspaceAPIRequest(t, baseURL, key, http.MethodGet, "/v1/work-items/"+id, nil, nil)
	requireWorkspaceStatus(t, status, http.StatusOK)
	var item workspaceWorkItem
	if err := json.Unmarshal(body, &item); err != nil {
		t.Fatalf("decode workspace work item: %v", err)
	}
	return item
}

func waitWorkspaceWorkState(t *testing.T, baseURL, key, id, wanted string, timeout time.Duration) workspaceWorkItem {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var item workspaceWorkItem
	for time.Now().Before(deadline) {
		item = readWorkspaceWork(t, baseURL, key, id)
		if item.State == wanted {
			return item
		}
		if item.State == "failed" && wanted != "failed" {
			t.Fatalf("workspace work %s failed while waiting for %s", id, wanted)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("workspace work %s state=%s did not reach %s", id, item.State, wanted)
	return workspaceWorkItem{}
}

func workspaceDatabaseQuery(t *testing.T, cluster *remotecluster.Cluster, state *permanentCPUFixtureState, query string) string {
	t.Helper()
	return strings.TrimSpace(cluster.Kubectl(t, "exec", "-n", workspaceNamespace,
		"statefulset/"+state.runID+"-postgresql", "-c", "postgresql", "--",
		"psql", "-U", "controlplane", "-d", "controlplane", "-Atc", query))
}

func waitWorkspaceConcurrencyRows(t *testing.T, cluster *remotecluster.Cluster, state *permanentCPUFixtureState, firstWorkID, secondWorkID string, requireChild bool, timeout time.Duration) []workspaceConcurrencyRow {
	t.Helper()
	query := fmt.Sprintf(`
SELECT json_build_object(
  'work_item_id', wi.id::text,
  'attempt_id', wr.id::text,
  'session_id', wr.session_id,
  'assignment_state', COALESCE(ta.state,''),
  'turn_id', COALESCE(ta.turn_id::text,''),
  'worker_id', COALESCE(ta.worker_id,''),
  'allocation_state', COALESCE(alloc.state,''),
  'allocation_uid', COALESCE(alloc.uid,0),
  'assigned_session', COALESCE(started.payload->>'session_id',''),
  'assigned_uid', COALESCE((started.payload->'sandbox'->>'uid')::int,0),
  'assigned_gid', COALESCE((started.payload->'sandbox'->>'gid')::int,0),
  'child_result', COALESCE(child.payload::text,'')
)::text
FROM work.work_items wi
JOIN runtime.workflow_runs wr ON wr.id=wi.current_attempt_id
LEFT JOIN LATERAL (
  SELECT turn_id, state, worker_id
  FROM runtime.turn_assignments
  WHERE attempt_id=wr.id::text
  ORDER BY assigned_at DESC LIMIT 1
) ta ON true
LEFT JOIN runtime.session_uid_allocations alloc ON alloc.session_id=wr.session_id
LEFT JOIN LATERAL (
  SELECT payload FROM runtime.events
  WHERE run_id=wr.id AND turn_id=ta.turn_id AND kind='turn_started' AND payload ? 'sandbox'
  ORDER BY seq DESC LIMIT 1
) started ON true
LEFT JOIN LATERAL (
  SELECT payload FROM runtime.events
  WHERE run_id=wr.id AND turn_id=ta.turn_id AND kind='tool_result' AND payload::text LIKE '%%workspace-child-proof%%'
  ORDER BY seq DESC LIMIT 1
) child ON true
WHERE wi.id IN ('%s'::uuid,'%s'::uuid)
ORDER BY wi.id`, firstWorkID, secondWorkID)
	deadline := time.Now().Add(timeout)
	var last []workspaceConcurrencyRow
	var lastRaw string
	for time.Now().Before(deadline) {
		lastRaw = workspaceDatabaseQuery(t, cluster, state, query)
		last = last[:0]
		valid := lastRaw != ""
		for _, line := range strings.Split(lastRaw, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var row workspaceConcurrencyRow
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				valid = false
				break
			}
			last = append(last, row)
			if row.AssignmentState == "" || row.TurnID == "" || row.WorkerID == "" || row.AssignedSession == "" || (requireChild && row.ChildResult == "") {
				valid = false
			}
		}
		if valid && len(last) == 2 {
			return append([]workspaceConcurrencyRow(nil), last...)
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("workspace concurrency authorities did not become observable (require_child=%t raw=%q)", requireChild, lastRaw)
	return nil
}

func expectedWorkspaceParticipants(t *testing.T, first, second workspaceWorkItem, rows []workspaceConcurrencyRow) []workspaceExpectedParticipant {
	t.Helper()
	byWork := map[string]workspaceConcurrencyRow{}
	for _, row := range rows {
		byWork[row.WorkItemID] = row
	}
	firstRow, firstOK := byWork[first.ID]
	secondRow, secondOK := byWork[second.ID]
	if !firstOK || !secondOK || firstRow.AttemptID != first.CurrentAttemptID || secondRow.AttemptID != second.CurrentAttemptID ||
		firstRow.SessionID == "" || secondRow.SessionID == "" || firstRow.SessionID == secondRow.SessionID {
		t.Fatalf("could not bind expected work/attempt/session identities: first=%+v second=%+v rows=%+v", first, second, rows)
	}
	return []workspaceExpectedParticipant{
		{Participant: workspaceParticipantA, WorkItemID: first.ID, AttemptID: first.CurrentAttemptID, SessionID: firstRow.SessionID, MarkerSHA256: workspaceMarkerDigest(workspaceParticipantA)},
		{Participant: workspaceParticipantB, WorkItemID: second.ID, AttemptID: second.CurrentAttemptID, SessionID: secondRow.SessionID, MarkerSHA256: workspaceMarkerDigest(workspaceParticipantB)},
	}
}

func validateInitialWorkspaceConcurrency(expected []workspaceExpectedParticipant, rows []workspaceConcurrencyRow) error {
	if len(expected) != 2 || len(rows) != 2 {
		return fmt.Errorf("expected two participants and two authority rows")
	}
	byWork := map[string]workspaceExpectedParticipant{}
	turns := map[string]bool{}
	workers := map[string]bool{}
	uids := map[uint32]bool{}
	for _, participant := range expected {
		byWork[participant.WorkItemID] = participant
	}
	for _, row := range rows {
		participant, ok := byWork[row.WorkItemID]
		if !ok || row.AttemptID != participant.AttemptID || row.SessionID != participant.SessionID {
			return fmt.Errorf("work/attempt/session mismatch for row %+v", row)
		}
		if row.AssignmentState != "active" || row.TurnID == "" || turns[row.TurnID] || row.WorkerID == "" || workers[row.WorkerID] {
			return fmt.Errorf("expected two distinct active assignments and workers (row=%+v)", row)
		}
		turns[row.TurnID] = true
		workers[row.WorkerID] = true
		if row.AllocationState != "in_use" || row.AllocationUID < 10000 || row.AllocationUID >= 60000 || uids[row.AllocationUID] {
			return fmt.Errorf("expected two distinct in-use in-range allocations (row=%+v)", row)
		}
		uids[row.AllocationUID] = true
		if row.AssignedSession != row.SessionID || row.AssignedUID != row.AllocationUID || row.AssignedGID != row.AllocationUID {
			return fmt.Errorf("supervisor-observed sandbox identity mismatch (row=%+v)", row)
		}
	}
	return nil
}

func configureWorkspaceModelBarrier(t *testing.T, baseURL string, expected []workspaceExpectedParticipant) {
	t.Helper()
	status, body := workspaceAPIRequest(t, baseURL, "", http.MethodPost, "/workspace/barrier/configure", map[string]any{"participants": expected}, nil)
	if status != http.StatusNoContent {
		t.Fatalf("configure keyed workspace barrier: status=%d body=%s", status, body)
	}
}

func waitWorkspaceModelBarrier(t *testing.T, baseURL string, timeout time.Duration) workspaceBarrierStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last workspaceBarrierStatus
	for time.Now().Before(deadline) {
		status, body := workspaceAPIRequest(t, baseURL, "", http.MethodGet, "/workspace/barrier", nil, nil)
		if status == http.StatusOK && json.Unmarshal(body, &last) == nil {
			if last.State == "failed" {
				t.Fatalf("keyed workspace barrier failed causally: %s", last.Failure)
			}
			if last.State == "ready" && last.Ready && len(last.Arrivals) == 2 {
				return last
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("keyed workspace barrier did not hold both expected participants (last=%+v)", last)
	return workspaceBarrierStatus{}
}

func waitWorkspaceDatabaseValue(t *testing.T, cluster *remotecluster.Cluster, state *permanentCPUFixtureState, query, wanted string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		last = workspaceDatabaseQuery(t, cluster, state, query)
		if last == wanted {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("database query did not reach %q (last=%q): %s", wanted, last, query)
}

func waitWorkspaceModelCapacity(t *testing.T, baseURL string, capacityWaiting int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last workspaceModelStats
	for time.Now().Before(deadline) {
		status, body := workspaceAPIRequest(t, baseURL, "", http.MethodGet, "/stats", nil, nil)
		if status == http.StatusOK && json.Unmarshal(body, &last) == nil && last.CapacityWaiting >= capacityWaiting {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("deterministic model stats did not reach capacity=%d (last=%+v)", capacityWaiting, last)
}

func setWorkspaceFreeTarget(t *testing.T, client *ssh.Client, filler string, targetPercent int) {
	t.Helper()
	command := fmt.Sprintf(`pod=$(sudo k3s kubectl get pods -n iterabase-system -l platform.iterabase.com/agentpool=forge-storage-pool -o jsonpath='{.items[0].metadata.name}')
sudo k3s kubectl exec -n iterabase-system "$pod" -- bash -ceu '
mount=/data/sandboxes
current=0
test ! -e %s || current=$(stat -c %%s %s)
read -r size avail < <(df -B1 --output=size,avail "$mount" | tail -1)
target=$((size * %d / 100))
new_size=$((current + avail - target))
test "$new_size" -gt 0
if test "$new_size" -ge "$current"; then fallocate -l "$new_size" %s; else truncate -s "$new_size" %s; fi
sync -f %s
read -r final_size final_avail < <(df -B1 --output=size,avail "$mount" | tail -1)
percent=$((final_avail * 100 / final_size))
test "$percent" -ge $((%d - 1))
test "$percent" -le $((%d + 1))
printf "workspace-free-target=%%s actual=%%s\n" %d "$percent"
'`, filler, filler, targetPercent, filler, filler, filler, targetPercent, targetPercent, targetPercent)
	output, err := sshOutput(client, command)
	if err != nil {
		t.Fatalf("set AgentPool PVC to %d%% free: %v\n%s", targetPercent, err, output)
	}
}

func waitWorkspaceGate(t *testing.T, client *ssh.Client, gated bool, reason string, timeout time.Duration) {
	t.Helper()
	want := "0"
	if gated {
		want = "1"
	}
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		command := fmt.Sprintf(`set -eu
ok=0
total=0
for pod in $(sudo k3s kubectl get pods -n iterabase-system -l platform.iterabase.com/agentpool=forge-storage-pool -o name | cut -d/ -f2); do
  total=$((total+1))
  metrics=$(sudo k3s kubectl get --raw "/api/v1/namespaces/iterabase-system/pods/$pod:8081/proxy/metrics" 2>/dev/null || true)
  if printf '%%s\n' "$metrics" | grep -Eq '^control_plane_harness_workspace_credit_gated %s(\.0+)?$'; then ok=$((ok+1)); fi
done
condition=$(sudo k3s kubectl get agentpool forge-storage-pool -n iterabase-system -o jsonpath='{.status.conditions[?(@.type=="WorkspaceCapacityHealthy")].reason}')
printf '%%s|%%s|%%s\n' "$total" "$ok" "$condition"`, want)
		output, err := sshOutput(client, command)
		last = strings.TrimSpace(output)
		if err == nil {
			parts := strings.Split(last, "|")
			if len(parts) == 3 && parts[0] != "0" && parts[0] == parts[1] && parts[2] == reason {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("workspace gate did not reach gated=%t condition=%s (last=%q)", gated, reason, last)
}

func configureConcurrentSiblingTargets(t *testing.T, cluster *remotecluster.Cluster, trustedSupervisor, sessionA, sessionB string) {
	t.Helper()
	if trustedSupervisor == "" {
		t.Fatal("concurrent workspace proof has no trusted supervisor")
	}
	// Exec enters the already-running production supervisor container as its
	// rendered UID 0. It demonstrates the explicit pool-wide trusted boundary by
	// accessing both sessions and the pod-scoped key. It creates only adversarial
	// fixture inputs; it does not chmod/chown/repair any production path.
	script := `
set -eu
session_a=$1
session_b=$2
root=/data/sandboxes
key=/etc/harness/tls/tls.key
/usr/local/bin/node --input-type=module -e '
import { validateSupervisorTLSKey } from "/app/dist/tls-key.js";
validateSupervisorTLSKey(process.argv[1]);
' "$key"
test "$(readlink "$key")" = ..data/tls.key
data_target=$(readlink /etc/harness/tls/..data)
resolved=/etc/harness/tls/$data_target/tls.key
test ! -L "$resolved"
test -f "$resolved"
test "$(stat -c '%u:%g:%a' "$resolved")" = 0:0:440
dd if="$key" of=/dev/null bs=1 count=1 status=none
/usr/local/bin/node -e '
const status = require("node:fs").readFileSync("/proc/1/status", "utf8");
const value = (name) => status.match(new RegExp("^" + name + ":[ \\t]+(\\S+)", "m"))?.[1] ?? "";
if (value("Uid") !== "0") throw new Error("supervisor is not root");
const caps = BigInt("0x" + value("CapEff"));
const required = (1n << 6n) | (1n << 7n);
if ((caps & required) !== required || (caps & ~required) === 0n) throw new Error("supervisor does not retain runtime-default capabilities plus SETGID/SETUID");
'
printf session-a-private > "$root/$session_a/workspace/sibling-secret.txt"
printf session-b-private > "$root/$session_b/workspace/sibling-secret.txt"
printf '%s\n' "$root/$session_b/workspace/sibling-secret.txt" > "$root/$session_a/workspace/.sibling-target"
printf '%s\n' "$root/$session_a/workspace/sibling-secret.txt" > "$root/$session_b/workspace/.sibling-target"
printf trusted-supervisor-setup=pass
`
	output := cluster.Kubectl(t, "exec", "pod/"+trustedSupervisor, "-n", workspaceNamespace, "-c", "supervisor", "--",
		"/bin/sh", "-ceu", script, "harness-proof", sessionA, sessionB)
	if !strings.Contains(output, "trusted-supervisor-setup=pass") {
		t.Fatalf("trusted supervisor did not establish known sibling targets: %s", output)
	}
}

func assertConcurrentWorkspaceMarkers(t *testing.T, cluster *remotecluster.Cluster, trustedSupervisor, sessionA, sessionB string) {
	t.Helper()
	script := fmt.Sprintf(`
set -eu
root=/data/sandboxes
test "$(cat "$root/%s/workspace/marker.txt")" = session-a
test "$(sha256sum "$root/%s/workspace/marker.txt" | cut -d" " -f1)" = %s
test "$(cat "$root/%s/workspace/marker.txt")" = session-b
test "$(sha256sum "$root/%s/workspace/marker.txt" | cut -d" " -f1)" = %s
test "$(cat "$root/%s/workspace/sibling-secret.txt")" = session-a-private
test "$(cat "$root/%s/workspace/sibling-secret.txt")" = session-b-private
printf trusted-supervisor-marker-proof=pass
`, sessionA, sessionA, workspaceMarkerDigest("session-a"), sessionB, sessionB, workspaceMarkerDigest("session-b"), sessionA, sessionB)
	output := cluster.Kubectl(t, "exec", "pod/"+trustedSupervisor, "-n", workspaceNamespace, "-c", "supervisor", "--", "/bin/sh", "-ceu", script)
	if !strings.Contains(output, "trusted-supervisor-marker-proof=pass") {
		t.Fatalf("trusted pool supervisor did not verify both checksummed workspace markers: %s", output)
	}
}

func assertRecoveryWorkspaceState(t *testing.T, cluster *remotecluster.Cluster, session, phase string, resumed bool) {
	t.Helper()
	repository, tag := os.Getenv("HARNESS_IMAGE_REPO"), os.Getenv("HARNESS_IMAGE_TAG")
	if repository == "" || tag == "" {
		t.Fatal("workspace recovery observer requires the exact harness image")
	}
	resumeCheck := "test ! -e /sessions/" + session + "/workspace/resume-proof.txt"
	if resumed {
		resumeCheck = "test \"$(cat /sessions/" + session + "/workspace/resume-proof.txt)\" = resumed"
	}
	name := "forge-workspace-recovery-proof-" + phase
	manifest := fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata: {name: %s, namespace: iterabase-system}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: proof
          image: %s:%s
          imagePullPolicy: Never
          command: [/bin/sh, -ceu]
          securityContext: {runAsUser: 0, runAsGroup: 0}
          args:
            - |
              test "$(cat /sessions/%s/workspace/consequence.count)" = once
              test "$(cat /sessions/%s/workspace/recovery-marker.txt)" = recovery-marker
              test "$(sha256sum /sessions/%s/workspace/recovery-marker.txt | cut -d" " -f1)" = %s
              %s
              printf recovery-proof=%s
          volumeMounts: [{name: sessions, mountPath: /sessions}]
      volumes:
        - name: sessions
          persistentVolumeClaim: {claimName: forge-storage-pool-sandbox}
`, name, repository, tag, session, session, session, workspaceMarkerDigest("recovery-marker"), resumeCheck, phase)
	applyWorkspaceManifest(t, cluster, name+".yaml", manifest)
	waitWorkspaceProofJob(t, cluster, name, 3*time.Minute)
	logs := cluster.Kubectl(t, "logs", "job/"+name, "-n", workspaceNamespace)
	if !strings.Contains(logs, "recovery-proof="+phase) {
		t.Fatalf("workspace recovery observer %s returned no durable proof: %s", phase, logs)
	}
}

func waitWorkspaceProofJob(t *testing.T, cluster *remotecluster.Cluster, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		last = strings.TrimSpace(cluster.Kubectl(t, "get", "job/"+name, "-n", workspaceNamespace, "-o", `jsonpath={.status.succeeded}|{.status.failed}`))
		parts := strings.Split(last, "|")
		if len(parts) == 2 && parts[0] == "1" {
			return
		}
		if len(parts) == 2 && parts[1] != "" && parts[1] != "0" {
			logs := cluster.Kubectl(t, "logs", "job/"+name, "-n", workspaceNamespace)
			t.Fatalf("workspace proof Job %s failed (status=%q): %s", name, last, logs)
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("workspace proof Job %s did not complete in %s (last=%q)", name, timeout, last)
}

func replaceIdleWorkspaceWorkerAtGate(t *testing.T, cluster *remotecluster.Cluster, state *permanentCPUFixtureState) {
	t.Helper()
	pod := strings.Fields(cluster.Kubectl(t, "get", "pods", "-n", workspaceNamespace, "-l", "platform.iterabase.com/agentpool=forge-storage-pool", "-o", "name"))[0]
	name := strings.TrimPrefix(pod, "pod/")
	oldUID := strings.TrimSpace(cluster.Kubectl(t, "get", pod, "-n", workspaceNamespace, "-o", "jsonpath={.metadata.uid}"))
	pvcBefore := strings.TrimSpace(cluster.Kubectl(t, "get", "pvc/forge-storage-pool-sandbox", "-n", workspaceNamespace, "-o", "jsonpath={.metadata.uid}"))
	cluster.Kubectl(t, "delete", pod, "-n", workspaceNamespace, "--wait=true", "--timeout=3m")
	waitWorkspaceReplacementPod(t, cluster, name, oldUID, 4*time.Minute)
	pvcAfter := strings.TrimSpace(cluster.Kubectl(t, "get", "pvc/forge-storage-pool-sandbox", "-n", workspaceNamespace, "-o", "jsonpath={.metadata.uid}"))
	if pvcAfter != pvcBefore || pvcAfter != state.agentPoolPVCUID {
		t.Fatalf("in-band worker replacement changed AgentPool PVC: original=%s before=%s after=%s", state.agentPoolPVCUID, pvcBefore, pvcAfter)
	}
}

func waitWorkspaceReplacementPod(t *testing.T, cluster *remotecluster.Cluster, name, oldUID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		last = strings.TrimSpace(cluster.Kubectl(t, "get", "pod/"+name, "-n", workspaceNamespace, "--ignore-not-found", "-o", `jsonpath={.metadata.uid}|{.status.conditions[?(@.type=="Ready")].status}`))
		parts := strings.Split(last, "|")
		if len(parts) == 2 && parts[0] != "" && parts[0] != oldUID && parts[1] == "True" {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("worker %s was not replaced Ready (old=%s last=%q)", name, oldUID, last)
}

func waitWorkspacePoolReplicas(t *testing.T, cluster *remotecluster.Cluster, replicas int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		last = strings.TrimSpace(cluster.Kubectl(t, "get", "agentpool/forge-storage-pool", "-n", workspaceNamespace,
			"-o", `jsonpath={.status.ready}|{.status.readyReplicas}`))
		pods := strings.Fields(cluster.Kubectl(t, "get", "pods", "-n", workspaceNamespace, "-l", "platform.iterabase.com/agentpool=forge-storage-pool", "-o", "name"))
		if last == "true|"+strconv.Itoa(replicas) && len(pods) == replicas {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("AgentPool did not settle at %d replicas (last=%q)", replicas, last)
}

func workspaceAgentWorkflow(name, key, prompt string) string {
	return fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: Workflow
metadata: {name: %s, namespace: iterabase-system}
spec:
  key: %s
  version: "1"
  poolRef: forge-storage-pool
  defaultModelRef: forge-workspace-model
  source: {type: manual_api}
  graph:
    entryNode: execute
    maxTransitions: 4
    nodes:
      - key: execute
        label: {en: Execute workspace proof, pt: Executar prova do workspace}
        kind: agent_task
        prompt: %q
        workspaceTools: true
        outcomes: [completed]
        outputSchema: {type: object, additionalProperties: false, required: [result], properties: {result: {type: string}}}
        resultPresentation:
          outcomes: [{outcome: completed, summary: {en: Workspace proof completed, pt: Prova do workspace concluída}}]
          fields: [{path: [result], label: {en: Result, pt: Resultado}}]
    terminalOutcomes: [{node: execute, outcome: completed}]
  presentation: {workflowTitle: Workspace behavior proof, personaName: E2E Operator, locale: en}
`, name, key, prompt)
}

func workspaceBarrierWorkflow(name, key, prompt string) string {
	return fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: Workflow
metadata: {name: %s, namespace: iterabase-system}
spec:
  key: %s
  version: "1"
  poolRef: forge-storage-pool
  defaultModelRef: forge-workspace-model
  source: {type: manual_api}
  graph:
    entryNode: execute
    maxTransitions: 4
    nodes:
      - key: execute
        label: {en: Execute workspace proof, pt: Executar prova do workspace}
        kind: agent_task
        prompt: %q
        workspaceTools: true
        outcomes: [completed]
      - key: hold
        label: {en: Preserve marker for observation, pt: Preservar marcador para observação}
        kind: human_gate
        outcomes: [continued]
        humanGate:
          type: approval
          title: {en: Finish workspace proof, pt: Terminar prova do workspace}
          description: {en: Release the observed isolated session., pt: Libertar a sessão isolada observada.}
          responseSchema: {type: object, additionalProperties: false, properties: {}}
          presentation: {outcomes: [{en: Continued, pt: Continuado}], fields: []}
        resultPresentation:
          outcomes: [{outcome: continued, summary: {en: Workspace proof completed, pt: Prova do workspace concluída}}]
          fields: []
    edges: [{from: execute, outcome: completed, to: hold}]
    terminalOutcomes: [{node: hold, outcome: continued}]
  presentation: {workflowTitle: Workspace behavior proof, personaName: E2E Operator, locale: en}
`, name, key, prompt)
}

func workspaceHumanGateWorkflow() string {
	recovery := recoveryCommand()
	return fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: Workflow
metadata: {name: forge-workspace-recovery, namespace: iterabase-system}
spec:
  key: e2e/forge-workspace-recovery
  version: "1"
  poolRef: forge-storage-pool
  defaultModelRef: forge-workspace-model
  source: {type: manual_api}
  graph:
    entryNode: execute
    maxTransitions: 8
    nodes:
      - key: execute
        label: {en: Write durable workspace state, pt: Escrever estado durável}
        kind: agent_task
        prompt: %q
        workspaceTools: true
        outcomes: [completed]
      - key: hold
        label: {en: Await customer response, pt: Aguardar resposta}
        kind: human_gate
        outcomes: [continued]
        humanGate:
          type: approval
          title: {en: Continue workspace recovery, pt: Continuar recuperação}
          description: {en: Resume the same durable session., pt: Retomar a mesma sessão durável.}
          responseSchema: {type: object, additionalProperties: false, properties: {}}
          presentation: {outcomes: [{en: Continued, pt: Continuado}], fields: []}
      - key: resume
        label: {en: Verify durable workspace state, pt: Verificar estado durável}
        kind: agent_task
        prompt: %q
        workspaceTools: true
        outcomes: [completed]
      - key: verified
        label: {en: Preserve resumed proof, pt: Preservar prova retomada}
        kind: human_gate
        outcomes: [continued]
        humanGate:
          type: approval
          title: {en: Finish recovery proof, pt: Terminar prova de recuperação}
          description: {en: Release the externally verified resumed session., pt: Libertar a sessão retomada verificada externamente.}
          responseSchema: {type: object, additionalProperties: false, properties: {}}
          presentation: {outcomes: [{en: Continued, pt: Continuado}], fields: []}
        resultPresentation:
          outcomes: [{outcome: continued, summary: {en: Recovery completed, pt: Recuperação concluída}}]
          fields: []
    edges:
      - {from: execute, outcome: completed, to: hold}
      - {from: hold, outcome: continued, to: resume}
      - {from: resume, outcome: completed, to: verified}
    terminalOutcomes: [{node: verified, outcome: continued}]
  presentation: {workflowTitle: Workspace replacement recovery, personaName: E2E Operator, locale: en}
`, "E2E_MODE:isolation E2E_BASH:"+base64.StdEncoding.EncodeToString([]byte(recovery)), "E2E_MODE:isolation E2E_BASH:"+base64.StdEncoding.EncodeToString([]byte(recovery)))
}

func workspaceBarrierPrompt(marker string) string {
	command := fmt.Sprintf(`set -eu
/usr/local/bin/node <<'NODE'
const fs = require("node:fs");
const path = require("node:path");
const status = fs.readFileSync("/proc/self/status", "utf8");
const field = (name) => status.match(new RegExp("^" + name + ":[ \\t]*(.*)$", "m"))?.[1].trim() ?? "";
const zero = (name) => /^0+$/.test(field(name));
if (process.getuid() <= 0 || process.getuid() !== process.getgid()) throw new Error("child UID=GID invariant failed");
if (field("Groups") !== "") throw new Error("child supplementary groups were not cleared");
if (field("NoNewPrivs") !== "1") throw new Error("child no_new_privs invariant failed");
for (const capability of ["CapEff", "CapPrm", "CapBnd", "CapAmb"]) if (!zero(capability)) throw new Error(capability + " was not cleared");
if (process.umask() !== 0o077) throw new Error("child umask is not 0077");
const workspace = process.cwd();
const root = path.dirname(workspace);
const poolRoot = path.dirname(root);
const pool = fs.lstatSync(poolRoot);
if (!pool.isDirectory() || pool.isSymbolicLink() || pool.uid !== 0 || pool.gid !== 0 || (pool.mode & 0o777) !== 0o711) throw new Error("pool root is not root-owned 0711");
for (const entry of [root, path.join(root, "home"), path.join(root, "tmp"), path.join(root, "session"), workspace]) {
  const stat = fs.lstatSync(entry);
  if (!stat.isDirectory() || stat.isSymbolicLink() || stat.uid !== process.getuid() || stat.gid !== process.getgid() || (stat.mode & 0o777) !== 0o700) throw new Error("session path is not owned 0700: " + entry);
}
const expectEACCES = (target, label) => {
  try { fs.openSync(target, "r"); } catch (error) {
    if (error.code === "EACCES") return;
    throw new Error(label + " returned " + error.code + ", want EACCES");
  }
  throw new Error(label + " unexpectedly opened");
};
const sibling = fs.readFileSync(".sibling-target", "utf8").trim();
expectEACCES(sibling, "known sibling path");
expectEACCES("/etc/harness/tls/tls.key", "supervisor tls.key");
NODE
printf %s > marker.txt
marker_sha=$(/usr/bin/sha256sum marker.txt | /usr/bin/cut -d" " -f1)
test "$marker_sha" = %s
session=${HARNESS_SANDBOX_ROOT##*/}
uid=$(/usr/bin/id -u)
gid=$(/usr/bin/id -g)
printf 'workspace-child-proof participant=%s session=%%s uid=%%s gid=%%s marker_sha256=%%s invariants=groups-cleared,caps-cleared,no-new-privs,umask-0077,pool-root-0:0:0711,session-tree-0700,sibling-EACCES,tls-key-EACCES' "$session" "$uid" "$gid" "$marker_sha"`, marker, workspaceMarkerDigest(marker), marker)
	return "E2E_MODE:workspace-barrier E2E_WORKSPACE_PARTICIPANT:" + marker + " E2E_BASH:" + base64.StdEncoding.EncodeToString([]byte(command))
}

func workspaceCapacityPrompt() string {
	command := fmt.Sprintf(`set -eu
printf capacity-active > capacity-marker.txt
test "$(/usr/bin/sha256sum capacity-marker.txt | /usr/bin/cut -d" " -f1)" = %s
printf 'capacity-marker=%s'`, workspaceMarkerDigest("capacity-active"), workspaceMarkerDigest("capacity-active"))
	return "E2E_MODE:capacity-active E2E_BASH:" + base64.StdEncoding.EncodeToString([]byte(command))
}

func recoveryCommand() string {
	return fmt.Sprintf(`set -eu
workspace=${HARNESS_SANDBOX_ROOT:?}/workspace
cd "$workspace"
if test ! -e consequence.count; then
  printf once > consequence.count
  printf recovery-marker > recovery-marker.txt
  test "$(/usr/bin/sha256sum recovery-marker.txt | /usr/bin/cut -d" " -f1)" = %[1]s
  printf 'recovery-initial=%[1]s'
else
  test "$(/usr/bin/cat consequence.count)" = once
  test "$(/usr/bin/cat recovery-marker.txt)" = recovery-marker
  test "$(/usr/bin/sha256sum recovery-marker.txt | /usr/bin/cut -d" " -f1)" = %[1]s
  test ! -e resume-proof.txt
  printf resumed > resume-proof.txt
  printf 'recovery-resume=%[1]s'
fi`, workspaceMarkerDigest("recovery-marker"))
}

func workspaceMarkerDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
