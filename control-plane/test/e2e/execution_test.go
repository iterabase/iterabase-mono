package e2e_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/httpx"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/poll"
)

func assertWorkloadRejectsAnonymousCaller(t *testing.T, state *deployedState) {
	t.Helper()
	forward, err := state.client.PortForward(state.ctx, controlPlaneNamespace, "svc/iterabase-gateway", 8443, "https")
	if err != nil {
		t.Fatalf("port-forward inference workload listener: %v", err)
	}
	state.forwards = append(state.forwards, forward)
	defer state.stopForward(t, forward)
	client, err := httpx.TLSClient(httpx.TLSOptions{
		Timeout:    5 * time.Second,
		RootCAPEM:  state.decodeSecret(t, "iterabase-control-plane-gateway-ca", "ca.crt"),
		ServerName: "iterabase-gateway." + controlPlaneNamespace + ".svc",
	})
	if err != nil {
		t.Fatalf("create anonymous workload client: %v", err)
	}
	response, requestErr := client.Get(forward.URL + "/healthz")
	if requestErr == nil {
		response.Body.Close()
		t.Fatalf("inference workload listener accepted an anonymous caller: status=%d", response.StatusCode)
	}
}

func exerciseAgentPoolLateSecretRecoveryStage(t *testing.T, state *deployedState) {
	t.Helper()
	const poolName = "late-secret-pool"
	state.applyYAML(t, "late-secret-pool.yaml", fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: AgentPool
metadata: {name: %s, namespace: iterabase-system}
spec:
  replicas: 1
  workerImage: %s
  podSecurity: baseline
  identity:
    trustDomain: iterabase.local
    caSecretRef: {name: late-platform-ca}
  sandbox:
    storageClassName: iterabase-agentpool-lvm-xfs
    accessMode: ReadWriteOnce
    size: 1Gi
  gateways:
    controlPlane:
      url: https://iterabase-control-plane-dispatch.iterabase-system.svc:8091
      serverName: iterabase-control-plane-dispatch.iterabase-system.svc
      selector: {podSelector: {matchLabels: {app.kubernetes.io/name: control-plane, app.kubernetes.io/component: dispatch}}}
    toolGateway:
      url: https://iterabase-control-plane-gateway.iterabase-system.svc:8090
      serverName: iterabase-control-plane-gateway.iterabase-system.svc
      selector: {podSelector: {matchLabels: {app.kubernetes.io/name: control-plane, app.kubernetes.io/component: gateway}}}
    inferenceGateway:
      url: https://iterabase-gateway.iterabase-system.svc:8443
      serverName: iterabase-gateway.iterabase-system.svc
      selector: {podSelector: {matchLabels: {app.kubernetes.io/name: inference-gateway, app.kubernetes.io/component: gateway}}}
  networkPolicy: {egress: denied}
  gatewayGrants:
    - {tool: platform.fixture_read, maxEffectClass: read_only}
  credentialBindings:
    - toolName: platform.fixture_read
      slot: fixture_token
      scheme: bearer
      bearer: {valueSecretRef: {name: late-fixture-credential, key: token}}
`, poolName, state.harnessImage.reference()))

	var initial string
	err := poll.Until(state.ctx, 45*time.Second, time.Second, func(_ context.Context) (bool, string, error) {
		out, commandErr := state.client.Kubectl(state.ctx, 30*time.Second, "get", "agentpool/"+poolName, "-n", controlPlaneNamespace,
			"-o", `jsonpath={.metadata.uid}|{.metadata.generation}|{.status.ready}|{.status.observedGeneration}|{.status.message}`)
		initial = strings.TrimSpace(out)
		if commandErr != nil {
			return false, initial, commandErr
		}
		parts := strings.Split(initial, "|")
		return len(parts) == 5 && strings.Contains(parts[4], "late-platform-ca"), initial, nil
	})
	if err != nil {
		t.Fatalf("missing-Secret AgentPool did not surface its retryable dependency: %v (status %q)", err, initial)
	}
	parts := strings.Split(initial, "|")
	if len(parts) != 5 {
		t.Fatalf("unexpected missing-Secret AgentPool status %q", initial)
	}
	uid := parts[0]
	if parts[2] == "true" || (parts[3] != "" && parts[3] != "0") {
		t.Fatalf("missing-Secret dependency advanced readiness/observed generation: %q", initial)
	}
	if count := state.databaseQuery(t, `SELECT count(*) FROM toolgateway.pools WHERE key='iterabase-system/late-secret-pool' AND deleted_at IS NULL`); count != "0" {
		t.Fatalf("missing-Secret AgentPool materialized partial gateway state: %s pools", count)
	}

	// Materialize only the external dependencies. Do not update, annotate, or
	// recreate the AgentPool: its original UID and generation must recover on the
	// controller's bounded health cadence.
	state.applyYAML(t, "late-secrets.yaml", `apiVersion: v1
kind: Secret
metadata: {name: late-platform-ca, namespace: iterabase-system}
type: Opaque
stringData: {ca.crt: deterministic-ca-existence-fixture}
---
apiVersion: v1
kind: Secret
metadata: {name: late-fixture-credential, namespace: iterabase-system}
type: Opaque
stringData: {token: synthetic-e2e-token}
`)
	var recovered string
	err = poll.Until(state.ctx, 95*time.Second, time.Second, func(_ context.Context) (bool, string, error) {
		out, commandErr := state.client.Kubectl(state.ctx, 30*time.Second, "get", "agentpool/"+poolName, "-n", controlPlaneNamespace,
			"-o", `jsonpath={.metadata.uid}|{.metadata.generation}|{.status.ready}|{.status.observedGeneration}|{.status.message}`)
		recovered = strings.TrimSpace(out)
		if commandErr != nil {
			return false, recovered, commandErr
		}
		current := strings.Split(recovered, "|")
		return len(current) == 5 && current[2] == "true" && current[3] == current[1], recovered, nil
	})
	if err != nil {
		t.Fatalf("AgentPool did not recover after referenced Secrets appeared: %v (status %q)", err, recovered)
	}
	recoveredParts := strings.Split(recovered, "|")
	if recoveredParts[0] != uid {
		t.Fatalf("AgentPool was recreated during dependency recovery: before=%q after=%q", initial, recovered)
	}
	materialized := state.databaseQuery(t, `SELECT
		(SELECT count(*) FROM toolgateway.pools p WHERE p.key='iterabase-system/late-secret-pool' AND p.deleted_at IS NULL)::text || '|' ||
		(SELECT count(*) FROM toolgateway.pool_grants g JOIN toolgateway.pools p ON p.id=g.pool_id WHERE p.key='iterabase-system/late-secret-pool' AND g.deleted_at IS NULL)::text || '|' ||
		(SELECT count(*) FROM toolgateway.credential_bindings b JOIN toolgateway.pools p ON p.id=b.pool_id WHERE p.key='iterabase-system/late-secret-pool' AND b.deleted_at IS NULL)::text`)
	if materialized != "1|1|1" {
		t.Fatalf("late-Secret recovery did not atomically materialize pool/grant/binding: %q", materialized)
	}

	manifest := strings.Replace(workflowYAML("late-secret-read", "e2e/late-secret-read", "1", "E2E_MODE:read-artifact", false,
		[]string{"platform.fixture_read"}, "", ""), "  poolRef: execution-pool", "  poolRef: late-secret-pool", 1)
	state.applyYAML(t, "late-secret-read.yaml", manifest)
	waitForWorkflowReady(t, state, "late-secret-read", 30*time.Second)
	item := startWorkflow(t, state, "e2e/late-secret-read", "Late dependency recovered execution")
	item = waitForWorkState(t, state, item.ID, "done", 4*time.Minute)
	invocation := state.databaseQuery(t, fmt.Sprintf(`SELECT tool_version_digest || '|' || state FROM toolgateway.invocations WHERE attempt_id='%s' AND tool_name='platform.fixture_read'`, item.CurrentAttemptID))
	wantInvocation := state.toolDigests["platform.fixture_read"] + "|succeeded"
	if invocation != wantInvocation {
		t.Fatalf("late-Secret AgentPool did not discover/invoke its declared tool: got=%q want=%q", invocation, wantInvocation)
	}

	denied := strings.Replace(workflowYAML("late-secret-denied", "e2e/late-secret-denied", "1", "must never execute", false,
		[]string{"platform.fixture_write"}, "", ""), "  poolRef: execution-pool", "  poolRef: late-secret-pool", 1)
	state.applyYAML(t, "late-secret-denied.yaml", denied)
	var deniedStatus string
	err = poll.Until(state.ctx, 30*time.Second, 250*time.Millisecond, func(_ context.Context) (bool, string, error) {
		out, commandErr := state.client.Kubectl(state.ctx, 30*time.Second, "get", "workflow", "late-secret-denied", "-n", controlPlaneNamespace,
			"-o", `jsonpath={.status.observedGeneration}|{.status.ready}|{.status.message}`)
		deniedStatus = strings.TrimSpace(out)
		if commandErr != nil {
			return false, deniedStatus, nil
		}
		parts := strings.SplitN(deniedStatus, "|", 3)
		return len(parts) == 3 && parts[0] != "" && parts[1] != "true" && strings.Contains(parts[2], "not granted by AgentPool"), deniedStatus, nil
	})
	if err != nil {
		t.Fatalf("ungranted workflow capability did not fail closed: %v (last %q)", err, deniedStatus)
	}
	if count := state.databaseQuery(t, `SELECT count(*) FROM workflow.definitions WHERE key='e2e/late-secret-denied'`); count != "0" {
		t.Fatalf("denied workflow was partially materialized: %s definitions", count)
	}
}

func setupExecutionResourcesStage(t *testing.T, state *deployedState) {
	t.Helper()
	state.redactor.Add("synthetic-e2e-token")
	state.applyYAML(t, "execution-resources.yaml", fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata: {name: fixture-credential, namespace: iterabase-system}
type: Opaque
stringData: {token: synthetic-e2e-token}
---
apiVersion: platform.iterabase.com/v1alpha1
kind: ModelBackend
metadata: {name: deterministic-model, namespace: iterabase-system}
spec:
  kind: external
  external: {baseURL: http://deterministic-model.default.svc:8080/v1}
---
apiVersion: platform.iterabase.com/v1alpha1
kind: Model
metadata: {name: e2e-model, namespace: iterabase-system}
spec:
  modelID: e2e-model
  displayName: Deterministic E2E model
  contextLength: 8192
  capabilities: [chat, tools]
  backendRef: deterministic-model
  defaultParams: {max_tokens: 1024}
  transforms: {rewrite_model_name: true}
---
apiVersion: platform.iterabase.com/v1alpha1
kind: AgentPool
metadata: {name: execution-pool, namespace: iterabase-system}
spec:
  replicas: 2
  workerImage: %s
  podSecurity: baseline
  identity:
    trustDomain: iterabase.local
    caSecretRef: {name: iterabase-control-plane-gateway-ca}
  sandbox:
    storageClassName: iterabase-agentpool-lvm-xfs
    accessMode: ReadWriteOnce
    size: 1Gi
  gateways:
    controlPlane:
      url: https://iterabase-control-plane-dispatch.iterabase-system.svc:8091
      serverName: iterabase-control-plane-dispatch.iterabase-system.svc
      selector: {podSelector: {matchLabels: {app.kubernetes.io/name: control-plane, app.kubernetes.io/component: dispatch}}}
    toolGateway:
      url: https://iterabase-control-plane-gateway.iterabase-system.svc:8090
      serverName: iterabase-control-plane-gateway.iterabase-system.svc
      selector: {podSelector: {matchLabels: {app.kubernetes.io/name: control-plane, app.kubernetes.io/component: gateway}}}
    inferenceGateway:
      url: https://iterabase-gateway.iterabase-system.svc:8443
      serverName: iterabase-gateway.iterabase-system.svc
      selector: {podSelector: {matchLabels: {app.kubernetes.io/name: inference-gateway}}}
  networkPolicy: {egress: denied}
  workspaceTools: true
  gatewayGrants:
    - {tool: platform.fixture_read, maxEffectClass: read_only}
    - {tool: platform.fixture_barrier, maxEffectClass: read_only}
    - {tool: platform.fixture_upsert, maxEffectClass: idempotent_write}
    - {tool: platform.fixture_write, maxEffectClass: non_idempotent_write}
    - {tool: platform.diagnostic, maxEffectClass: read_only}
  credentialBindings:
    - toolName: platform.fixture_read
      slot: fixture_token
      scheme: bearer
      bearer: {valueSecretRef: {name: fixture-credential, key: token}}
---
%s
`, state.harnessImage.reference(), workflowYAML("execution-read", "e2e/execution-read", "1", "E2E_MODE:read-artifact", false,
		[]string{"platform.fixture_read"}, "", "")))
	state.kubectl(t, 3*time.Minute, "wait", "--for=jsonpath={.status.deployed}=true", "modelbackend/deterministic-model", "-n", controlPlaneNamespace, "--timeout=2m")
	state.kubectl(t, 3*time.Minute, "wait", "--for=jsonpath={.status.available}=true", "model/e2e-model", "-n", controlPlaneNamespace, "--timeout=2m")
	for name, digest := range state.toolDigests {
		query := fmt.Sprintf("SELECT count(*) FROM toolgateway.runner_registrations WHERE tool_name='%s' AND tool_digest='%s' AND active AND accepting_new", name, digest)
		waitForDatabaseValue(t, state, query, "1", 3*time.Minute)
	}
	waitForAgentPoolReady(t, state, "execution-pool", 3*time.Minute)
	state.kubectl(t, 3*time.Minute, "wait", "--for=jsonpath={.status.ready}=true", "workflow/execution-read", "-n", controlPlaneNamespace, "--timeout=2m")
	assertWorkerIdentity(t, state)
	state.createWorkIdentity(t, "execution-e2e@example.test")
}

func waitForAgentPoolReady(t *testing.T, state *deployedState, name string, timeout time.Duration) {
	t.Helper()
	var last string
	err := poll.Until(state.ctx, timeout, time.Second, func(_ context.Context) (bool, string, error) {
		out, err := state.client.Kubectl(state.ctx, 30*time.Second, "get", "agentpool/"+name, "-n", controlPlaneNamespace,
			"-o", `jsonpath={.spec.replicas}|{.status.ready}|{.status.readyReplicas}|{.status.observedGeneration}|{.status.message}`)
		last = strings.TrimSpace(out)
		if err != nil {
			return false, last, err
		}
		parts := strings.SplitN(last, "|", 5)
		if len(parts) != 5 {
			return false, last, nil
		}
		desired, desiredErr := strconv.Atoi(parts[0])
		readyReplicas, readyErr := strconv.Atoi(parts[2])
		if desiredErr != nil || readyErr != nil || desired < 1 {
			return false, last, nil
		}
		return parts[1] == "true" && readyReplicas == desired, last, nil
	})
	if err == nil {
		return
	}
	pods, _ := state.client.Kubectl(state.ctx, 30*time.Second, "get", "pods", "-n", controlPlaneNamespace,
		"-l", "platform.iterabase.com/agentpool="+name, "-o", "wide")
	describe, _ := state.client.Kubectl(state.ctx, 30*time.Second, "describe", "pods", "-n", controlPlaneNamespace,
		"-l", "platform.iterabase.com/agentpool="+name)
	logs, _ := state.client.Kubectl(state.ctx, 30*time.Second, "logs", "-n", controlPlaneNamespace,
		"-l", "platform.iterabase.com/agentpool="+name, "-c", "supervisor", "--tail=200")
	t.Fatalf("AgentPool %s did not become Ready: %v (status %q)\n--- pods ---\n%s\n--- describe ---\n%s\n--- supervisor logs ---\n%s", name, err, last, pods, describe, logs)
}

func workflowYAML(resourceName, key, version, prompt string, workspaceTools bool, capabilities []string, edges, extraNodes string) string {
	requested := ""
	caps := ""
	for _, capability := range capabilities {
		effect := "read_only"
		if strings.Contains(capability, "upsert") {
			effect = "idempotent_write"
		}
		if strings.Contains(capability, "write") {
			effect = "non_idempotent_write"
		}
		requested += fmt.Sprintf("\n    - {tool: %s, maxEffectClass: %s}", capability, effect)
		caps += fmt.Sprintf("\n          - %s", capability)
	}
	requestedBlock := ""
	if requested != "" {
		requestedBlock = "\n  requestedCapabilities:" + requested
	}
	capBlock := ""
	if caps != "" {
		capBlock = "\n        capabilities:" + caps
	}
	return fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: Workflow
metadata: {name: %s, namespace: iterabase-system}
spec:
  key: %s
  version: %q
  poolRef: execution-pool
  defaultModelRef: e2e-model
  source: {type: manual_api}%s
  graph:
    entryNode: execute
    maxTransitions: 8
    nodes:
      - key: execute
        label: {en: Execute deterministic fixture, pt: Executar fixture determinística}
        kind: agent_task
        prompt: %q
        workspaceTools: %t%s
        outcomes: [completed]
        outputSchema:
          type: object
          additionalProperties: false
          required: [result]
          properties: {result: {type: string}}
        resultPresentation:
          outcomes: [{outcome: completed, summary: {en: Execution completed, pt: Execução concluída}}]
          fields: [{path: [result], label: {en: Result, pt: Resultado}}]%s%s
    terminalOutcomes: [{node: execute, outcome: completed}]
  presentation: {workflowTitle: Deterministic execution, personaName: E2E Operator, locale: en}
`, resourceName, key, version, requestedBlock, prompt, workspaceTools, capBlock, extraNodes, edges)
}

func assertWorkerIdentity(t *testing.T, state *deployedState) {
	t.Helper()
	pod := state.firstPod(t, "platform.iterabase.com/agentpool=execution-pool")
	imageID := state.kubectl(t, 30*time.Second, "get", "pod", pod, "-n", controlPlaneNamespace, "-o", "jsonpath={.status.containerStatuses[?(@.name=='supervisor')].imageID}")
	if !strings.Contains(imageID, state.harnessImage.digest) {
		t.Fatalf("AgentPool worker does not run exact harness digest %s: %s", state.harnessImage.digest, imageID)
	}
	poolUID := state.kubectl(t, 30*time.Second, "get", "agentpool/execution-pool", "-n", controlPlaneNamespace, "-o", "jsonpath={.metadata.uid}")
	uri := state.kubectl(t, 30*time.Second, "get", "pod", pod, "-n", controlPlaneNamespace,
		"-o", "jsonpath={.spec.volumes[?(@.name=='harness-tls')].csi.volumeAttributes.csi\\.cert-manager\\.io/uri-sans}")
	want := "spiffe://iterabase.local/pools/" + poolUID + "/workers/" + pod
	if uri != want {
		t.Fatalf("worker SPIFFE URI=%q want=%q", uri, want)
	}
}

type runtimeStats struct {
	Requests  int64 `json:"requests"`
	Cancelled int64 `json:"cancelled"`
}

func exerciseWorkerLossCancellationStage(t *testing.T, state *deployedState) {
	t.Helper()
	forward, err := state.client.PortForward(state.ctx, "default", "svc/deterministic-model", 8080, "http")
	if err != nil {
		t.Fatalf("port-forward deterministic model: %v", err)
	}
	state.forwards = append(state.forwards, forward)
	statsClient := &http.Client{Timeout: 5 * time.Second}
	stats := func() runtimeStats {
		response, requestErr := statsClient.Get(forward.URL + "/stats") // #nosec G107 -- loopback-only test fixture URL
		if requestErr != nil {
			t.Fatalf("read deterministic model stats: %v", requestErr)
		}
		defer response.Body.Close()
		var current runtimeStats
		if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&current) != nil {
			t.Fatalf("invalid deterministic model stats response: status=%d", response.StatusCode)
		}
		return current
	}
	before := stats()

	state.applyYAML(t, "worker-loss.yaml", workflowYAML("worker-loss", "e2e/worker-loss", "1", "E2E_SLOW", false, nil, "", ""))
	waitForWorkflowReady(t, state, "worker-loss", 30*time.Second)
	item := startWorkflow(t, state, "e2e/worker-loss", "Worker loss cancellation")
	item = waitForWorkState(t, state, item.ID, "in_progress", 2*time.Minute)
	err = poll.Until(state.ctx, 30*time.Second, 250*time.Millisecond, func(_ context.Context) (bool, string, error) {
		current := stats()
		return current.Requests > before.Requests, fmt.Sprintf("requests=%d", current.Requests), nil
	})
	if err != nil {
		t.Fatalf("slow model request did not start: %v", err)
	}
	// With multiple Ready same-pool workers, kill the worker durably assigned to
	// this exact turn rather than whichever pod happens to sort first.
	pod := state.databaseQuery(t, fmt.Sprintf(`SELECT worker_id FROM runtime.turn_assignments WHERE attempt_id='%s' AND state='active'`, item.CurrentAttemptID))
	if pod == "" {
		t.Fatal("slow model turn has no active assigned worker")
	}
	poolLabel := state.kubectl(t, 30*time.Second, "get", "pod", pod, "-n", controlPlaneNamespace,
		"-o", "jsonpath={.metadata.labels.platform\\.iterabase\\.com/agentpool}")
	if poolLabel != "execution-pool" {
		t.Fatalf("assigned worker %q has AgentPool label %q", pod, poolLabel)
	}
	oldUID := state.kubectl(t, 30*time.Second, "get", "pod", pod, "-n", controlPlaneNamespace, "-o", "jsonpath={.metadata.uid}")
	lostTurn := state.databaseQuery(t, fmt.Sprintf(`SELECT t.id::text || '|' || t.session_id FROM runtime.turn_assignments a JOIN runtime.turns t ON t.id=a.turn_id WHERE a.attempt_id='%s' AND a.state='active'`, item.CurrentAttemptID))
	state.kubectl(t, time.Minute, "delete", "pod", pod, "-n", controlPlaneNamespace, "--wait=true")
	err = poll.Until(state.ctx, 30*time.Second, 250*time.Millisecond, func(_ context.Context) (bool, string, error) {
		current := stats()
		return current.Cancelled > before.Cancelled, fmt.Sprintf("cancelled=%d", current.Cancelled), nil
	})
	if err != nil {
		t.Fatalf("worker loss did not cancel in-flight model work: %v", err)
	}
	// DES-HOR-649-04: the lost read-only turn is never redelivered; the step
	// reruns as a new visit in a fresh session in the same attempt and completes.
	rerun := waitForWorkState(t, state, item.ID, "done", 4*time.Minute)
	if rerun.CurrentAttemptID != item.CurrentAttemptID {
		t.Fatalf("worker loss on a read-only step started a new attempt: before=%s after=%s", item.CurrentAttemptID, rerun.CurrentAttemptID)
	}
	lostParts := strings.Split(lostTurn, "|")
	if len(lostParts) != 2 {
		t.Fatalf("lost turn evidence is malformed: %q", lostTurn)
	}
	interruption := state.databaseQuery(t, fmt.Sprintf(`SELECT decision || '|' || cause || '|' || lost_session_id || '|' || rerun_session_id FROM runtime.turn_interruptions WHERE turn_id='%s'`, lostParts[0]))
	parts := strings.Split(interruption, "|")
	if len(parts) != 4 || parts[0] != "rerun" || parts[1] != "worker_lost" || parts[2] != lostParts[1] || parts[3] == lostParts[1] {
		t.Fatalf("read-only interruption evidence = %q, want rerun in a fresh session (lost session %s)", interruption, lostParts[1])
	}
	assignment := state.databaseQuery(t, fmt.Sprintf(`SELECT state FROM runtime.turn_assignments WHERE turn_id='%s'::uuid`, lostParts[0]))
	if assignment != "terminal" {
		t.Fatalf("lost worker assignment state=%q want terminal", assignment)
	}
	if visits := state.databaseQuery(t, fmt.Sprintf(`SELECT string_agg(state, ',' ORDER BY execution_seq) FROM runtime.node_executions WHERE attempt_id='%s'`, item.CurrentAttemptID)); visits != "interrupted,succeeded" {
		t.Fatalf("worker-loss visits = %q, want interrupted,succeeded", visits)
	}
	var replacement string
	err = poll.Until(state.ctx, 3*time.Minute, time.Second, func(_ context.Context) (bool, string, error) {
		out, commandErr := state.client.Kubectl(state.ctx, 30*time.Second, "get", "pod", pod, "-n", controlPlaneNamespace,
			"-o", `jsonpath={.metadata.uid}|{.status.conditions[?(@.type=="Ready")].status}`)
		replacement = strings.TrimSpace(out)
		if commandErr != nil {
			return false, replacement, nil
		}
		parts := strings.Split(replacement, "|")
		return len(parts) == 2 && parts[0] != oldUID && parts[1] == "True", replacement, nil
	})
	if err != nil {
		t.Fatalf("AgentPool did not replace the lost worker with a Ready process: %v (last %q)", err, replacement)
	}
}

func exerciseConcurrentSamePoolWorkStage(t *testing.T, state *deployedState) {
	t.Helper()
	state.applyYAML(t, "same-pool-barrier.yaml", workflowYAML("same-pool-barrier", "e2e/same-pool-barrier", "1", "E2E_MODE:barrier", false,
		[]string{"platform.fixture_barrier"}, "", ""))
	waitForWorkflowReady(t, state, "same-pool-barrier", 30*time.Second)

	first := startWorkflow(t, state, "e2e/same-pool-barrier", "Concurrent same-pool work A")
	second := startWorkflow(t, state, "e2e/same-pool-barrier", "Concurrent same-pool work B")
	first = waitForWorkState(t, state, first.ID, "done", 4*time.Minute)
	second = waitForWorkState(t, state, second.ID, "done", 4*time.Minute)

	assignmentEvidence := state.databaseQuery(t, fmt.Sprintf(`SELECT count(DISTINCT worker_id)::text || '|' || count(*)::text FROM runtime.turn_assignments WHERE attempt_id IN ('%s','%s')`, first.CurrentAttemptID, second.CurrentAttemptID))
	if assignmentEvidence != "2|2" {
		t.Fatalf("same-pool barrier did not consume two simultaneous worker credits: %q", assignmentEvidence)
	}
	invocations := state.databaseQuery(t, fmt.Sprintf(`SELECT count(*) FROM toolgateway.invocations WHERE attempt_id IN ('%s','%s') AND tool_name='platform.fixture_barrier' AND state='succeeded'`, first.CurrentAttemptID, second.CurrentAttemptID))
	if invocations != "2" {
		t.Fatalf("concurrency barrier did not settle both authenticated work items: %q", invocations)
	}
}

func exerciseImmutableToolGenerationStage(t *testing.T, state *deployedState) {
	t.Helper()
	extraNode := `
      - key: hold
        label: {en: Hold pinned generation, pt: Suspender geração fixada}
        kind: human_gate
        outcomes: [continued]
        humanGate:
          type: approval
          title: {en: Retire pinned generation, pt: Retirar geração fixada}
          description: {en: Release the attempt that pins tool v1., pt: Libertar a tentativa que fixa a ferramenta v1.}
          responseSchema: {type: object, additionalProperties: false, properties: {}}
          presentation: {outcomes: [{en: Continued, pt: Continuado}], fields: []}
        resultPresentation:
          outcomes: [{outcome: continued, summary: {en: Pin released, pt: Fixação libertada}}]
          fields: []`
	edges := `
    edges:
      - {from: execute, outcome: completed, to: hold}
    terminalOutcomes: [{node: hold, outcome: continued}]`
	manifest := workflowYAML("generation-pin", "e2e/generation-pin", "1", "E2E_MODE:read-artifact", false,
		[]string{"platform.fixture_read"}, edges, extraNode)
	manifest = strings.ReplaceAll(manifest, `        resultPresentation:
          outcomes: [{outcome: completed, summary: {en: Execution completed, pt: Execução concluída}}]
          fields: [{path: [result], label: {en: Result, pt: Resultado}}]`, "")
	manifest = strings.ReplaceAll(manifest, "    terminalOutcomes: [{node: execute, outcome: completed}]\n", "")
	state.applyYAML(t, "generation-pin.yaml", manifest)
	waitForWorkflowReady(t, state, "generation-pin", 30*time.Second)
	pinned := startWorkflow(t, state, "e2e/generation-pin", "Pinned tool generation")
	pinned = waitForWorkState(t, state, pinned.ID, "blocked", 4*time.Minute)
	v1Digest := state.toolDigests["platform.fixture_read"]
	pinQuery := fmt.Sprintf(`SELECT tool_version_digest FROM toolgateway.attempt_tool_pins WHERE attempt_id='%s' AND tool_name='platform.fixture_read'`, pinned.CurrentAttemptID)
	if digest := state.databaseQuery(t, pinQuery); digest != v1Digest {
		t.Fatalf("active attempt pinned tool digest=%q want=%q", digest, v1Digest)
	}

	oldRevision, oldDigest := state.fluxRevision, state.fluxDigest
	publishToolGeneration(t, state, state.toolV2)
	v2Digest := state.toolV2[0].digest
	waitForDatabaseValue(t, state, fmt.Sprintf("SELECT count(*) FROM toolgateway.runner_registrations WHERE tool_name='platform.fixture_read' AND tool_digest='%s' AND active AND accepting_new", v2Digest), "1", 3*time.Minute)
	waitForDatabaseValue(t, state, fmt.Sprintf("SELECT count(*) FROM toolgateway.runner_registrations WHERE tool_name='platform.fixture_read' AND tool_digest='%s' AND active AND NOT accepting_new", v1Digest), "1", 3*time.Minute)
	if digest := state.databaseQuery(t, pinQuery); digest != v1Digest {
		t.Fatalf("active attempt pin changed across tool generation: got=%q want=%q", digest, v1Digest)
	}
	state.fluxRevision = state.kubectl(t, 30*time.Second, "get", "gitrepository", "overlay", "-n", "flux-system", "-o", "jsonpath={.status.artifact.revision}")
	state.fluxDigest = state.kubectl(t, 30*time.Second, "get", "gitrepository", "overlay", "-n", "flux-system", "-o", "jsonpath={.status.artifact.digest}")
	if state.fluxRevision == oldRevision || state.fluxDigest == oldDigest || !canonicalDigest(state.fluxDigest) {
		t.Fatalf("tool generation did not advance exact Flux identity: before=%s/%s after=%s/%s", oldRevision, oldDigest, state.fluxRevision, state.fluxDigest)
	}

	status, blockerBody := state.request(t, http.MethodGet, "/v1/work-items/"+pinned.ID+"/blocker", state.workKey, nil, nil)
	requireStatus(t, status, http.StatusOK, blockerBody)
	var blocker blockerResponse
	mustDecode(t, blockerBody, &blocker)
	status, response := state.requestJSON(t, http.MethodPost, "/v1/work-blockers/"+blocker.ID+"/responses", state.workKey,
		map[string]any{"outcome": "continued", "response": map[string]any{}})
	requireStatus(t, status, http.StatusOK, response)
	_ = waitForWorkState(t, state, pinned.ID, "done", 4*time.Minute)
	waitForDatabaseValue(t, state, fmt.Sprintf("SELECT count(*) FROM toolgateway.runner_registrations WHERE tool_name='platform.fixture_read' AND tool_digest='%s' AND active", v1Digest), "0", 3*time.Minute)
	state.toolDigests["platform.fixture_read"] = v2Digest
}

// exerciseProductOnlyInstallStage proves HOR-651's fresh-install criterion:
// with toolRunner.flux.enabled=false, no overlay source and no client tools,
// the first generation is product-only and a workflow runs the reference
// product tool from the tool-runner image.
func exerciseProductOnlyInstallStage(t *testing.T, state *deployedState) {
	t.Helper()
	// Flux is not even installed yet, so no overlay source can exist.
	if crd := state.kubectl(t, 30*time.Second, "get", "crd", "gitrepositories.source.toolkit.fluxcd.io", "-o", "name", "--ignore-not-found"); crd != "" {
		t.Fatalf("product-only install has a Flux source API: %s", crd)
	}
	env := state.kubectl(t, 30*time.Second, "get", "deployment/iterabase-tool-runner", "-n", controlPlaneNamespace, "-o",
		`jsonpath={range .spec.template.spec.containers[?(@.name=="materializer")].env[*]}{.name}={.value}{"\n"}{end}`)
	if !strings.Contains(env, "FLUX_ENABLED=false") || strings.Contains(env, "FLUX_SOURCE_NAME") {
		t.Fatalf("product-only materializer env still configures a Flux client: %s", env)
	}
	active := state.databaseQuery(t, "SELECT string_agg(DISTINCT tool_name, ',') FROM toolgateway.runner_registrations WHERE active")
	if active != productDiagnosticTool {
		t.Fatalf("product-only generation registered %q, want only %q", active, productDiagnosticTool)
	}
	runProductDiagnostic(t, state, "Product-only fresh install")
}

// exerciseProductToolLayerStage proves the release-versioned product layer
// (HOR-651) after the overlay is enabled: the overlay source carries only
// customer tools, yet a workflow still runs the reference product tool served
// from the tool-runner image.
func exerciseProductToolLayerStage(t *testing.T, state *deployedState) {
	t.Helper()
	pod := state.kubectl(t, 30*time.Second, "get", "pods", "-n", "default", "-l", "app=tool-git", "-o", "jsonpath={.items[0].metadata.name}")
	state.kubectl(t, 30*time.Second, "exec", "-n", "default", pod, "--", "/bin/sh", "-ceu", "test -d /git/repo/tools/client && test ! -e /git/repo/tools/product")
	runProductDiagnostic(t, state, "Product tool layer")
}

func runProductDiagnostic(t *testing.T, state *deployedState, title string) {
	t.Helper()
	digest := state.toolDigests[productDiagnosticTool]
	waitForDatabaseValue(t, state, fmt.Sprintf("SELECT count(*) FROM toolgateway.runner_registrations WHERE tool_name='%s' AND tool_digest='%s' AND active AND accepting_new", productDiagnosticTool, digest), "1", 3*time.Minute)

	state.applyYAML(t, "product-diagnostic.yaml", workflowYAML("product-diagnostic", "e2e/product-diagnostic", "1", "E2E_MODE:product-diagnostic", false,
		[]string{productDiagnosticTool}, "", ""))
	waitForWorkflowReady(t, state, "product-diagnostic", 30*time.Second)
	item := startWorkflow(t, state, "e2e/product-diagnostic", title)
	item = waitForWorkState(t, state, item.ID, "done", 4*time.Minute)
	ledger := state.databaseQuery(t, fmt.Sprintf(`SELECT tool_version_digest || '|' || state || '|' || (result_json->>'layer') FROM toolgateway.invocations WHERE attempt_id='%s' AND tool_name='%s'`, item.CurrentAttemptID, productDiagnosticTool))
	if want := digest + "|succeeded|product"; ledger != want {
		t.Fatalf("product tool invocation=%q want=%q", ledger, want)
	}
}

func exerciseRepresentativeExecutionStage(t *testing.T, state *deployedState) {
	t.Helper()
	item := startWorkflow(t, state, "e2e/execution-read", "Representative execution")
	item = waitForWorkState(t, state, item.ID, "done", 4*time.Minute)
	attemptID := item.CurrentAttemptID
	if attemptID == "" {
		t.Fatal("terminal execution has no attributable attempt")
	}
	assertExecutionEvidence(t, state, item.ID, attemptID)
}

func startWorkflow(t *testing.T, state *deployedState, key, title string) workItemResponse {
	t.Helper()
	status, body := state.requestJSONWithHeaders(t, http.MethodPost, "/v1/work-items", state.workKey, map[string]any{
		"workflowKey":        key,
		"title":              title,
		"source":             map[string]any{"fixture": "HOR-477", "private": "not-customer-visible"},
		"sourcePresentation": map[string]any{"kind": "api", "title": title, "subtitle": "Deterministic source fixture"},
	}, map[string]string{"Idempotency-Key": key + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)})
	requireStatus(t, status, http.StatusCreated, body)
	var item workItemResponse
	mustDecode(t, body, &item)
	return item
}

func waitForWorkState(t *testing.T, state *deployedState, itemID, wanted string, timeout time.Duration) workItemResponse {
	t.Helper()
	var item workItemResponse
	var lastResponse string
	err := poll.Until(state.ctx, timeout, 500*time.Millisecond, func(_ context.Context) (bool, string, error) {
		status, body, err := state.doRequest(http.MethodGet, "/v1/work-items/"+itemID, state.workKey, nil, nil)
		if err != nil {
			return false, "request failed", err
		}
		if status != http.StatusOK {
			return false, safeResponse(body), fmt.Errorf("observe work item: status %d", status)
		}
		if err := json.Unmarshal(body, &item); err != nil {
			return false, safeResponse(body), err
		}
		lastResponse = safeResponse(body)
		if item.State == "failed" && wanted != "failed" {
			return false, lastResponse, fmt.Errorf("work item failed")
		}
		return item.State == wanted, lastResponse, nil
	})
	if err != nil {
		nodeEvidence := "no attempt"
		eventEvidence := "no attempt"
		if item.CurrentAttemptID != "" {
			_, body, requestErr := state.doRequest(http.MethodGet, "/v1/work-attempts/"+item.CurrentAttemptID+"/nodes", state.workKey, nil, nil)
			if requestErr != nil {
				nodeEvidence = requestErr.Error()
			} else {
				nodeEvidence = safeResponse(body)
			}
			eventEvidence = state.databaseQuery(t, fmt.Sprintf(`SELECT string_agg(seq::text || ':' || kind || ':' || payload::text, E'\n' ORDER BY seq) FROM runtime.events WHERE run_id='%s'::uuid`, item.CurrentAttemptID))
		}
		t.Fatalf("wait for work item %s state %s: %v; last response: %s\n--- nodes ---\n%s\n--- events ---\n%s", itemID, wanted, err, lastResponse, nodeEvidence, eventEvidence)
	}
	return item
}

func assertExecutionEvidence(t *testing.T, state *deployedState, itemID, attemptID string) {
	t.Helper()
	status, nodes := state.request(t, http.MethodGet, "/v1/work-attempts/"+attemptID+"/nodes", state.workKey, nil, nil)
	requireStatus(t, status, http.StatusOK, nodes)
	assertCustomerSafeJSON(t, nodes, "synthetic-e2e-token")
	status, artifacts := state.request(t, http.MethodGet, "/v1/work-items/"+itemID+"/artifacts", state.workKey, nil, nil)
	requireStatus(t, status, http.StatusOK, artifacts)
	var links []struct {
		ArtifactID string  `json:"artifactId"`
		Digest     *string `json:"digest"`
		Role       string  `json:"role"`
	}
	mustDecode(t, artifacts, &links)
	roles := map[string]bool{}
	if len(links) != 2 {
		t.Fatalf("representative workflow does not expose output + evidence lineage: %s", safeResponse(artifacts))
	}
	for _, link := range links {
		if link.Digest == nil || link.ArtifactID == "" || link.ArtifactID != links[0].ArtifactID || *link.Digest != *links[0].Digest {
			t.Fatalf("representative workflow artifact lineage is not immutable/attributable: %s", safeResponse(artifacts))
		}
		roles[link.Role] = true
	}
	if !roles["output"] || !roles["evidence"] {
		t.Fatalf("representative workflow artifact roles=%v want output+evidence", roles)
	}
	query := fmt.Sprintf(`SELECT string_agg(kind,',' ORDER BY seq) FROM runtime.events WHERE run_id='%s'::uuid`, attemptID)
	events := state.databaseQuery(t, query)
	position := -1
	for _, kind := range []string{"turn_started", "model_call_started", "tool_call_started", "tool_result", "step_completion_reported", "settled"} {
		next := strings.Index(events[position+1:], kind)
		if next < 0 {
			t.Fatalf("runtime event order lacks %s after offset %d: %s", kind, position, events)
		}
		position += next + 1
	}
	if runState := state.databaseQuery(t, fmt.Sprintf(`SELECT state FROM runtime.workflow_runs WHERE id='%s'::uuid`, attemptID)); runState != "succeeded" {
		t.Fatalf("representative workflow durable run state=%q want succeeded", runState)
	}
	invocation := state.databaseQuery(t, fmt.Sprintf(`SELECT tool_name || '|' || tool_version_digest || '|' || state FROM toolgateway.invocations WHERE attempt_id='%s'`, attemptID))
	want := "platform.fixture_read|" + state.toolDigests["platform.fixture_read"] + "|succeeded"
	if invocation != want {
		t.Fatalf("tool attribution=%q want=%q", invocation, want)
	}
	if leaked := state.databaseQuery(t, "SELECT count(*) FROM toolgateway.invocations WHERE result_json::text LIKE '%synthetic-e2e-token%' OR error::text LIKE '%synthetic-e2e-token%'"); leaked != "0" {
		t.Fatalf("credential value leaked into invocation persistence: %s rows", leaked)
	}
}

func exerciseIdempotentInvocationRaceStage(t *testing.T, state *deployedState) {
	t.Helper()
	state.applyYAML(t, "idempotent-race.yaml", workflowYAML("idempotent-race", "e2e/idempotent-race", "1", "E2E_MODE:idempotent-race", false,
		[]string{"platform.fixture_upsert"}, "", ""))
	waitForWorkflowReady(t, state, "idempotent-race", 30*time.Second)
	item := startWorkflow(t, state, "e2e/idempotent-race", "Idempotent invocation race")
	item = waitForWorkState(t, state, item.ID, "done", 4*time.Minute)
	ledger := state.databaseQuery(t, fmt.Sprintf(`SELECT count(*)::text || '|' || min(state) || '|' || min(tool_version_digest) FROM toolgateway.invocations WHERE attempt_id='%s' AND tool_name='platform.fixture_upsert'`, item.CurrentAttemptID))
	want := "1|succeeded|" + state.toolDigests["platform.fixture_upsert"]
	if ledger != want {
		t.Fatalf("concurrent duplicate idempotent calls did not converge on one attributable invocation: got=%q want=%q", ledger, want)
	}
	started := state.databaseQuery(t, fmt.Sprintf(`SELECT count(*) FROM runtime.events WHERE run_id='%s'::uuid AND kind='tool_call_started' AND payload->>'tool_name'='platform.fixture_upsert'`, item.CurrentAttemptID))
	if started != "2" {
		t.Fatalf("fixture did not exercise two duplicate logical tool calls: %s", started)
	}
}

func exerciseIsolationCompositionStage(t *testing.T, state *deployedState) {
	t.Helper()
	const orphanMutationDelay = 2 * time.Second
	prepareCommand := fmt.Sprintf(`rm -f background-timer-state background-fd-state; exec 9>background-fd-state; (sleep %d; echo leaked >&9; echo leaked >background-timer-state) </dev/null >/dev/null 2>&1 & printf intended-pvc-state > intended.txt; printf 'child=%%s' "$PPID"`, int(orphanMutationDelay/time.Second))
	extraNodes := `
      - key: hold
        label: {en: Hold execution, pt: Suspender execução}
        kind: human_gate
        outcomes: [continued]
        humanGate:
          type: approval
          title: {en: Continue isolation fixture, pt: Continuar fixture de isolamento}
          description: {en: Resume the same durable session., pt: Retomar a mesma sessão durável.}
          responseSchema: {type: object, additionalProperties: false, properties: {}}
          presentation: {outcomes: [{en: Continued, pt: Continuado}], fields: []}
      - key: resume
        label: {en: Resume deterministic fixture, pt: Retomar fixture determinística}
        kind: agent_task
        prompt: "E2E_MODE:isolation E2E_BASH:` + base64.StdEncoding.EncodeToString([]byte(`test "$(cat intended.txt)" = intended-pvc-state; test ! -s background-fd-state; test ! -e background-timer-state; printf 'child=%s' "$PPID"`)) + `"
        workspaceTools: true
        outcomes: [completed]
        outputSchema: {type: object, additionalProperties: false, required: [result], properties: {result: {type: string}}}
        resultPresentation:
          outcomes: [{outcome: completed, summary: {en: Resume completed, pt: Retoma concluída}}]
          fields: [{path: [result], label: {en: Result, pt: Resultado}}]`
	edges := `
    edges:
      - {from: execute, outcome: completed, to: hold}
      - {from: hold, outcome: continued, to: resume}
    terminalOutcomes: [{node: resume, outcome: completed}]`
	manifest := workflowYAML("isolation-a", "e2e/isolation-a", "1", "E2E_MODE:isolation E2E_BASH:"+base64.StdEncoding.EncodeToString([]byte(prepareCommand)), true, nil, edges, extraNodes)
	// workflowYAML emits an execute terminal presentation; this graph routes
	// execute onward, so only resume is terminal and customer-presentable.
	manifest = strings.ReplaceAll(manifest, `        resultPresentation:
          outcomes: [{outcome: completed, summary: {en: Execution completed, pt: Execução concluída}}]
          fields: [{path: [result], label: {en: Result, pt: Resultado}}]`, "")
	manifest = strings.ReplaceAll(manifest, "    terminalOutcomes: [{node: execute, outcome: completed}]\n", "")
	state.applyYAML(t, "isolation-a.yaml", manifest)
	waitForWorkflowReady(t, state, "isolation-a", 30*time.Second)
	itemA := startWorkflow(t, state, "e2e/isolation-a", "Isolation session A")
	itemA = waitForWorkState(t, state, itemA.ID, "blocked", 4*time.Minute)
	attemptA := itemA.CurrentAttemptID
	sessionA := state.databaseQuery(t, fmt.Sprintf("SELECT session_id FROM runtime.workflow_runs WHERE id='%s'::uuid", attemptA))
	uidA := state.databaseQuery(t, fmt.Sprintf("SELECT uid::text FROM runtime.session_uid_allocations WHERE session_id='%s' AND state='in_use'", sessionA))
	pidA := toolResultPID(t, state, attemptA)

	// The first child is already disposed once the human gate is blocked. Wait
	// beyond the orphan's declared mutation deadline before any absence check,
	// so a leaked timer/descriptor cannot make this required gate false-pass.
	select {
	case <-time.After(orphanMutationDelay + time.Second):
	case <-state.ctx.Done():
		t.Fatalf("waiting for disposed-child mutation deadline: %v", state.ctx.Err())
	}

	probe := fmt.Sprintf(`set -eu; test ! -e intended.txt; if ls /data/sandboxes >/dev/null 2>&1; then exit 90; fi; if cat /data/sandboxes/%s/workspace/intended.txt >/dev/null 2>&1; then exit 91; fi; printf 'child=%%s' "$PPID"`, sessionA)
	state.applyYAML(t, "isolation-b.yaml", workflowYAML("isolation-b", "e2e/isolation-b", "1", "E2E_MODE:isolation E2E_BASH:"+base64.StdEncoding.EncodeToString([]byte(probe)), true, nil, "", ""))
	waitForWorkflowReady(t, state, "isolation-b", 30*time.Second)
	itemB := startWorkflow(t, state, "e2e/isolation-b", "Isolation session B")
	itemB = waitForWorkState(t, state, itemB.ID, "done", 4*time.Minute)
	sessionB := state.databaseQuery(t, fmt.Sprintf("SELECT session_id FROM runtime.workflow_runs WHERE id='%s'::uuid", itemB.CurrentAttemptID))
	uidB := state.databaseQuery(t, fmt.Sprintf("SELECT uid::text FROM runtime.session_uid_allocations WHERE session_id='%s'", sessionB))
	pidB := toolResultPID(t, state, itemB.CurrentAttemptID)
	if sessionA == sessionB || uidA == uidB || pidA == pidB {
		t.Fatalf("disposable child/session isolation collapsed: sessions=%q/%q uids=%q/%q pids=%q/%q", sessionA, sessionB, uidA, uidB, pidA, pidB)
	}

	status, blockerBody := state.request(t, http.MethodGet, "/v1/work-items/"+itemA.ID+"/blocker", state.workKey, nil, nil)
	requireStatus(t, status, http.StatusOK, blockerBody)
	var blocker blockerResponse
	mustDecode(t, blockerBody, &blocker)
	status, response := state.requestJSON(t, http.MethodPost, "/v1/work-blockers/"+blocker.ID+"/responses", state.workKey,
		map[string]any{"outcome": "continued", "response": map[string]any{}})
	requireStatus(t, status, http.StatusOK, response)
	itemA = waitForWorkState(t, state, itemA.ID, "done", 4*time.Minute)
	uidAResume := state.databaseQuery(t, fmt.Sprintf("SELECT uid::text FROM runtime.session_uid_allocations WHERE session_id='%s'", sessionA))
	pids := toolResultPIDs(t, state, attemptA)
	if uidAResume != uidA || len(pids) != 2 || pids[0] == pids[1] {
		t.Fatalf("session resume did not preserve only intended PVC/UID state with a fresh child: uid=%q/%q pids=%v", uidA, uidAResume, pids)
	}
}

func waitForWorkflowReady(t *testing.T, state *deployedState, name string, timeout time.Duration) {
	t.Helper()
	var last string
	err := poll.Until(state.ctx, timeout, time.Second, func(_ context.Context) (bool, string, error) {
		out, commandErr := state.client.Kubectl(state.ctx, 30*time.Second, "get", "workflow/"+name, "-n", controlPlaneNamespace,
			"-o", `jsonpath={.status.ready}|{.status.observedGeneration}|{.status.message}`)
		last = strings.TrimSpace(out)
		if commandErr != nil {
			return false, last, commandErr
		}
		return strings.HasPrefix(last, "true|"), last, nil
	})
	if err != nil {
		t.Fatalf("Workflow %s did not become Ready: %v (status %q)", name, err, last)
	}
}

func toolResultPID(t *testing.T, state *deployedState, attemptID string) string {
	t.Helper()
	pids := toolResultPIDs(t, state, attemptID)
	if len(pids) == 0 {
		t.Fatalf("attempt %s has no child PID evidence", attemptID)
	}
	return pids[0]
}

// toolResultPIDs returns each disposable child's process identity as
// worker/generation/pid. A PID alone is only unique within one worker's PID
// namespace: children on different AgentPool workers (or on a replaced pod in
// the same slot, which gets a new fencing generation) may share a number.
func toolResultPIDs(t *testing.T, state *deployedState, attemptID string) []string {
	t.Helper()
	raw := state.databaseQuery(t, fmt.Sprintf(`
		SELECT string_agg(a.worker_id || '/' || a.fencing_generation || ' ' || e.payload::text, E'\n' ORDER BY e.seq)
		FROM runtime.events e JOIN runtime.turn_assignments a ON a.turn_id=e.turn_id
		WHERE e.run_id='%s'::uuid AND e.kind='tool_result' AND e.payload::text LIKE '%%child=%%'`, attemptID))
	out := make([]string, 0)
	for _, line := range strings.Split(raw, "\n") {
		owner, payload, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		for _, match := range regexp.MustCompile(`child=([0-9]+)`).FindAllStringSubmatch(payload, -1) {
			out = append(out, owner+"/"+match[1])
		}
	}
	return out
}

func exerciseOutcomeUnknownRecoveryStage(t *testing.T, state *deployedState) {
	t.Helper()
	state.applyYAML(t, "outcome-unknown.yaml", workflowYAML("outcome-unknown", "e2e/outcome-unknown", "1", "E2E_MODE:outcome-unknown", false,
		[]string{"platform.fixture_write"}, "", ""))
	waitForWorkflowReady(t, state, "outcome-unknown", 30*time.Second)
	item := startWorkflow(t, state, "e2e/outcome-unknown", "Ambiguous write outcome")
	item = waitForWorkState(t, state, item.ID, "failed", 4*time.Minute)
	stateQuery := fmt.Sprintf(`SELECT state FROM toolgateway.invocations WHERE attempt_id='%s' AND tool_name='platform.fixture_write'`, item.CurrentAttemptID)
	waitForDatabaseValue(t, state, stateQuery, "outcome_unknown", 2*time.Minute)
	countQuery := fmt.Sprintf(`SELECT count(*) FROM toolgateway.invocations WHERE attempt_id='%s' AND tool_name='platform.fixture_write'`, item.CurrentAttemptID)
	if count := state.databaseQuery(t, countQuery); count != "1" {
		t.Fatalf("ambiguous non-idempotent effect was repeated: %s invocation rows", count)
	}
	state.kubectl(t, 7*time.Minute, "rollout", "status", "deployment/iterabase-tool-runner", "-n", controlPlaneNamespace, "--timeout=6m")
	waitForDatabaseValue(t, state, fmt.Sprintf("SELECT count(*) FROM toolgateway.runner_registrations WHERE tool_name='platform.fixture_write' AND tool_digest='%s' AND active AND accepting_new", state.toolDigests["platform.fixture_write"]), "1", 3*time.Minute)
	if count := state.databaseQuery(t, countQuery); count != "1" {
		t.Fatalf("runner recovery silently retried an ambiguous non-idempotent effect: %s invocation rows", count)
	}
}

func exerciseConsequenceConfirmationStage(t *testing.T, state *deployedState) {
	t.Helper()
	state.applyYAML(t, "consequence-workflow.yaml", workflowYAML("consequence", "e2e/consequence", "1", "E2E_MODE:consequence", false,
		[]string{"platform.fixture_write"}, "", ""))
	waitForWorkflowReady(t, state, "consequence", 30*time.Second)
	item := startWorkflow(t, state, "e2e/consequence", "Consequential execution")
	item = waitForWorkState(t, state, item.ID, "done", 4*time.Minute)
	status, body := state.request(t, http.MethodGet, "/v1/work-items/"+item.ID+"/consequences", state.workKey, nil, nil)
	requireStatus(t, status, http.StatusOK, body)
	var consequences []struct {
		InvocationID string          `json:"invocationId"`
		Summary      json.RawMessage `json:"summary"`
		State        string          `json:"state"`
	}
	mustDecode(t, body, &consequences)
	if len(consequences) != 1 || !strings.Contains(string(consequences[0].Summary), "synthetic-record") || strings.Contains(string(consequences[0].Summary), "synthetic-e2e-token") {
		t.Fatalf("consequence evidence is missing or unsafe: %s", safeResponse(body))
	}
	status, feedbackBody := state.requestJSON(t, http.MethodPost, "/v1/work-items/"+item.ID+"/feedback", state.workKey,
		map[string]any{"attemptId": item.CurrentAttemptID, "category": "incorrect_result", "explanation": "Exercise exact confirmation."})
	requireStatus(t, status, http.StatusCreated, feedbackBody)
	var feedback feedbackResponse
	mustDecode(t, feedbackBody, &feedback)
	revisionPath := "/v1/work-items/" + item.ID + "/revisions"
	status, denied := state.requestJSON(t, http.MethodPost, revisionPath, state.workKey,
		map[string]any{"feedbackId": feedback.ID, "actionableGuidance": "Repeat only after confirmation."})
	requireStatus(t, status, http.StatusConflict, denied)
	status, denied = state.requestJSON(t, http.MethodPost, revisionPath, state.workKey,
		map[string]any{"feedbackId": feedback.ID, "actionableGuidance": "Repeat only after confirmation.", "confirmedInvocationIds": []string{"00000000-0000-0000-0000-000000000000"}})
	requireStatus(t, status, http.StatusConflict, denied)
	status, revisionBody := state.requestJSON(t, http.MethodPost, revisionPath, state.workKey,
		map[string]any{"feedbackId": feedback.ID, "actionableGuidance": "Repeat only after confirmation.", "confirmedInvocationIds": []string{consequences[0].InvocationID}})
	requireStatus(t, status, http.StatusCreated, revisionBody)
	var revised workItemResponse
	mustDecode(t, revisionBody, &revised)
	revised = waitForWorkState(t, state, revised.ID, "done", 4*time.Minute)
	if revised.CurrentAttemptID == "" || revised.CurrentAttemptID == item.CurrentAttemptID {
		t.Fatalf("exact consequence confirmation did not create an attributable revised attempt: before=%s after=%s", item.CurrentAttemptID, revised.CurrentAttemptID)
	}
	count := state.databaseQuery(t, fmt.Sprintf(`SELECT count(*) FROM toolgateway.invocations WHERE tool_name='platform.fixture_write' AND state='succeeded' AND attempt_id IN ('%s','%s')`, item.CurrentAttemptID, revised.CurrentAttemptID))
	if count != "2" {
		t.Fatalf("consequential repetition did not occur exactly once after confirmation: %s invocations", count)
	}
}

func waitForDatabaseValue(t *testing.T, state *deployedState, query, expected string, timeout time.Duration) {
	t.Helper()
	err := poll.Until(state.ctx, timeout, 500*time.Millisecond, func(_ context.Context) (bool, string, error) {
		value := state.databaseQuery(t, query)
		return value == expected, value, nil
	})
	if err != nil {
		t.Fatalf("database value did not reach %q for %s: %v", expected, query, err)
	}
}

// waitWorkflowYAML marks each agent prompt with an E2E_MODE so the
// deterministic model scopes its transcript to the current turn of the
// shared session and completes each step.
func waitWorkflowYAML() string {
	return `apiVersion: platform.iterabase.com/v1alpha1
kind: Workflow
metadata: {name: wait-for-replies, namespace: iterabase-system}
spec:
  key: e2e/wait-for-replies
  version: "1"
  poolRef: execution-pool
  defaultModelRef: e2e-model
  source: {type: manual_api}
  graph:
    entryNode: send
    maxTransitions: 8
    nodes:
      - key: send
        label: {en: Send requests, pt: Enviar pedidos}
        kind: agent_task
        prompt: "E2E_MODE:wait-send Send the requests"
        outcomes: [completed]
        outputSchema:
          type: object
          additionalProperties: false
          required: [result]
          properties: {result: {type: string}}
      - key: collect
        label: {en: Wait for replies, pt: Aguardar respostas}
        kind: wait
        wait:
          events: [{source: email_reply, correlation: {node: send, path: [result]}}]
          completion: all
          deadline: {after: 1h}
      - key: decide
        label: {en: Decide, pt: Decidir}
        kind: agent_task
        prompt: "E2E_MODE:wait-decide Decide with the replies"
        outcomes: [completed]
        outputSchema:
          type: object
          additionalProperties: false
          required: [result]
          properties: {result: {type: string}}
        resultPresentation:
          outcomes: [{outcome: completed, summary: {en: Replies handled, pt: Respostas tratadas}}]
          fields: [{path: [result], label: {en: Result, pt: Resultado}}]
    edges:
      - {from: send, outcome: completed, to: collect}
      - {from: collect, outcome: received, to: decide}
      - {from: collect, outcome: deadline, to: decide}
      - {from: collect, outcome: cancelled, to: decide}
    terminalOutcomes: [{node: decide, outcome: completed}]
  presentation: {workflowTitle: Wait for replies, personaName: E2E Operator, locale: en}
`
}

// exerciseWaitAndWriteInterruptionStage proves the HOR-649 runtime on the
// deployed composition: a wait holds no worker and continues the same attempt
// on an Operator's stop-waiting command, and a worker lost after a write stops
// the attempt without repeating the write.
func exerciseWaitAndWriteInterruptionStage(t *testing.T, state *deployedState) {
	t.Helper()
	state.applyYAML(t, "wait-for-replies.yaml", waitWorkflowYAML())
	waitForWorkflowReady(t, state, "wait-for-replies", 30*time.Second)
	item := startWorkflow(t, state, "e2e/wait-for-replies", "Wait for supplier replies")
	item = waitForWorkState(t, state, item.ID, "waiting", 3*time.Minute)
	if active := state.databaseQuery(t, `SELECT count(*) FROM runtime.turn_assignments WHERE state='active'`); active != "0" {
		t.Fatalf("a waiting item holds %s active worker assignments", active)
	}
	if expected := state.databaseQuery(t, fmt.Sprintf(`SELECT expected_count FROM runtime.waits WHERE attempt_id='%s' AND completed_at IS NULL`, item.CurrentAttemptID)); expected != "1" {
		t.Fatalf("open wait expects %q events, want 1", expected)
	}
	status, body := state.request(t, http.MethodPost, "/v1/work-items/"+item.ID+"/wait/stop", state.workKey, nil, nil)
	requireStatus(t, status, http.StatusOK, body)
	done := waitForWorkState(t, state, item.ID, "done", 3*time.Minute)
	if done.CurrentAttemptID != item.CurrentAttemptID {
		t.Fatalf("stop waiting started a new attempt: before=%s after=%s", item.CurrentAttemptID, done.CurrentAttemptID)
	}
	if outcome := state.databaseQuery(t, fmt.Sprintf(`SELECT completion_outcome FROM runtime.node_executions WHERE attempt_id='%s' AND node_key='collect'`, item.CurrentAttemptID)); outcome != "cancelled" {
		t.Fatalf("stopped wait outcome = %q, want cancelled", outcome)
	}
	status, body = state.request(t, http.MethodPost, "/v1/work-items/"+item.ID+"/wait/stop", state.workKey, nil, nil)
	requireStatus(t, status, http.StatusBadRequest, body)

	state.applyYAML(t, "write-interruption.yaml", workflowYAML("write-interruption", "e2e/write-interruption", "1", "E2E_MODE:consequence E2E_SLOW", false,
		[]string{"platform.fixture_write"}, "", ""))
	waitForWorkflowReady(t, state, "write-interruption", 30*time.Second)
	written := startWorkflow(t, state, "e2e/write-interruption", "Worker lost after a write")
	invocations := fmt.Sprintf(`SELECT count(*) FROM toolgateway.invocations WHERE attempt_id='%s' AND tool_name='platform.fixture_write' AND state='succeeded'`, written.CurrentAttemptID)
	waitForDatabaseValue(t, state, invocations, "1", 3*time.Minute)
	pod := state.databaseQuery(t, fmt.Sprintf(`SELECT worker_id FROM runtime.turn_assignments WHERE attempt_id='%s' AND state='active'`, written.CurrentAttemptID))
	if pod == "" {
		t.Fatal("the turn that wrote has no active assigned worker to lose")
	}
	state.kubectl(t, time.Minute, "delete", "pod", pod, "-n", controlPlaneNamespace, "--wait=true")
	stopped := waitForWorkState(t, state, written.ID, "failed", 3*time.Minute)
	if decision := state.databaseQuery(t, fmt.Sprintf(`SELECT decision FROM runtime.turn_interruptions WHERE attempt_id='%s'`, stopped.CurrentAttemptID)); decision != "stop_after_write" {
		t.Fatalf("write interruption decision = %q, want stop_after_write", decision)
	}
	if code := state.databaseQuery(t, fmt.Sprintf(`SELECT customer_failure_summary->>'code' FROM work.attempts WHERE id='%s'`, stopped.CurrentAttemptID)); code != "interrupted_after_write" {
		t.Fatalf("stopped item reason = %q, want interrupted_after_write", code)
	}
	waitForAgentPoolReady(t, state, "execution-pool", 3*time.Minute)
	time.Sleep(5 * time.Second) // several dispatch passes with a Ready replacement worker
	if count := state.databaseQuery(t, fmt.Sprintf(`SELECT count(*) FROM toolgateway.invocations WHERE attempt_id='%s'`, stopped.CurrentAttemptID)); count != "1" {
		t.Fatalf("an interrupted write was repeated: %s invocations", count)
	}
	if turns := state.databaseQuery(t, fmt.Sprintf(`SELECT count(*) FROM runtime.turns WHERE run_id='%s'`, stopped.CurrentAttemptID)); turns != "1" {
		t.Fatalf("a step that wrote was rerun: %s turns", turns)
	}
}
