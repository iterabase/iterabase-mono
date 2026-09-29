package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nunocgoncalves/iterabase-mono/forge/test/e2e/internal/remotecluster"
)

const (
	workspacePollInterval            = 500 * time.Millisecond
	workspaceFailureEvidenceMaxBytes = 32 << 10
)

type workspaceWorkStateObserver struct {
	read     func() (workspaceWorkItem, error)
	qualify  func(workspaceWorkItem) (bool, string, error)
	collect  func(workspaceWorkItem) (string, error)
	persist  func(string) error
	now      func() time.Time
	sleep    func(time.Duration)
	interval time.Duration
}

type workspaceWorkStateError struct {
	WorkID        string
	Wanted        string
	Phase         string
	State         string
	Qualification string
	Evidence      string
	Cause         error
	TimedOut      bool
}

func (e *workspaceWorkStateError) Error() string {
	prefix := fmt.Sprintf("workspace work %s", e.WorkID)
	if e.Phase != "" {
		prefix = fmt.Sprintf("human-gate workspace work %s in phase %s", e.WorkID, e.Phase)
	}
	if e.TimedOut {
		message := fmt.Sprintf("%s state=%s did not reach %s", prefix, e.State, e.Wanted)
		if e.Qualification != "" {
			message = fmt.Sprintf("%s state=%s did not qualify for %s (%s)", prefix, e.State, e.Wanted, e.Qualification)
		}
		if e.Cause != nil {
			message += fmt.Sprintf("; %v", e.Cause)
		}
		if e.Evidence != "" {
			message += fmt.Sprintf("; durable evidence:\n%s", e.Evidence)
		}
		return message
	}
	if e.Cause != nil {
		if e.Evidence != "" {
			return fmt.Sprintf("%s while waiting for %s: %v; durable evidence:\n%s", prefix, e.Wanted, e.Cause, e.Evidence)
		}
		return fmt.Sprintf("%s while waiting for %s: %v", prefix, e.Wanted, e.Cause)
	}
	if e.Evidence != "" {
		return fmt.Sprintf("%s failed while waiting for %s; durable evidence:\n%s", prefix, e.Wanted, e.Evidence)
	}
	return fmt.Sprintf("%s failed while waiting for %s", prefix, e.Wanted)
}

func (o workspaceWorkStateObserver) wait(id, wanted, phase string, timeout time.Duration) (workspaceWorkItem, error) {
	now := o.now
	if now == nil {
		now = time.Now
	}
	sleep := o.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	interval := o.interval
	if interval <= 0 {
		interval = workspacePollInterval
	}
	deadline := now().Add(timeout)
	var item workspaceWorkItem
	var qualification string
	for now().Before(deadline) {
		var err error
		item, err = o.read()
		if err != nil {
			return workspaceWorkItem{}, &workspaceWorkStateError{WorkID: id, Wanted: wanted, Phase: phase, Cause: fmt.Errorf("read work state: %w", err)}
		}
		if item.State == wanted {
			if o.qualify == nil {
				return item, nil
			}
			qualified, detail, qualifyErr := o.qualify(item)
			qualification = detail
			if qualifyErr != nil {
				return workspaceWorkItem{}, &workspaceWorkStateError{WorkID: id, Wanted: wanted, Phase: phase, State: item.State, Cause: fmt.Errorf("qualify durable gate evidence: %w", qualifyErr)}
			}
			if qualified {
				return item, nil
			}
		}
		if item.State == "failed" && wanted != "failed" {
			waitErr := &workspaceWorkStateError{WorkID: id, Wanted: wanted, Phase: phase, State: item.State}
			return workspaceWorkItem{}, o.attachEvidence(item, waitErr, "")
		}
		sleep(interval)
	}
	waitErr := &workspaceWorkStateError{
		WorkID: id, Wanted: wanted, Phase: phase, State: item.State,
		Qualification: qualification, TimedOut: true,
	}
	return workspaceWorkItem{}, o.attachEvidence(item, waitErr, " after timeout")
}

func (o workspaceWorkStateObserver) attachEvidence(item workspaceWorkItem, waitErr *workspaceWorkStateError, context string) *workspaceWorkStateError {
	if o.collect == nil {
		return waitErr
	}
	evidence, collectErr := o.collect(item)
	if collectErr != nil {
		waitErr.Cause = fmt.Errorf("collect causal evidence%s: %w", context, collectErr)
		return waitErr
	}
	waitErr.Evidence = evidence
	if o.persist != nil {
		if persistErr := o.persist(evidence); persistErr != nil {
			waitErr.Cause = fmt.Errorf("persist causal evidence%s: %w", context, persistErr)
		}
	}
	return waitErr
}

type workspaceFailureEvidenceCollector struct {
	query  func(string) (string, error)
	redact func(string) string
}

func (c workspaceFailureEvidenceCollector) collect(item workspaceWorkItem) (string, error) {
	query, err := workspaceFailureEvidenceQuery(item)
	if err != nil {
		return "", fmt.Errorf("build evidence query: %w", err)
	}
	evidence, err := c.query(query)
	if err != nil {
		return "", err
	}
	if c.redact != nil {
		evidence = c.redact(evidence)
	}
	return boundedWorkspaceEvidence(strings.TrimSpace(evidence), workspaceFailureEvidenceMaxBytes), nil
}

func waitWorkspaceWorkState(t *testing.T, baseURL, key, id, wanted string, timeout time.Duration) workspaceWorkItem {
	t.Helper()
	observer := workspaceWorkStateObserver{
		read: func() (workspaceWorkItem, error) {
			return readWorkspaceWork(t, baseURL, key, id), nil
		},
	}
	item, err := observer.wait(id, wanted, "", timeout)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func waitWorkspaceRecoveryGate(t *testing.T, cluster *remotecluster.Cluster, state *permanentCPUFixtureState, baseURL, key, id, phase string, timeout time.Duration) (workspaceWorkItem, workspaceRecoveryGateEvidence) {
	t.Helper()
	var gateEvidence workspaceRecoveryGateEvidence
	collector := workspaceFailureEvidenceCollector{
		query: func(query string) (string, error) {
			return workspaceDatabaseQueryResult(cluster, state, query)
		},
		redact: state.diagnostics.redactor.String,
	}
	observer := workspaceWorkStateObserver{
		read: func() (workspaceWorkItem, error) {
			return readWorkspaceWork(t, baseURL, key, id), nil
		},
		qualify: func(item workspaceWorkItem) (bool, string, error) {
			query, err := workspaceRecoveryGateEvidenceQuery(item)
			if err != nil {
				return false, "", err
			}
			result, err := workspaceDatabaseQueryResult(cluster, state, query)
			if err != nil {
				return false, "", err
			}
			if err := json.Unmarshal([]byte(result), &gateEvidence); err != nil {
				return false, "", fmt.Errorf("decode recovery gate evidence %q: %w", result, err)
			}
			return gateEvidence.qualifies(item, phase)
		},
		collect: collector.collect,
		persist: func(evidence string) error {
			path := filepath.Join(state.diagnostics.outputDir, "workspace-human-gate-failure.log")
			return os.WriteFile(path, []byte(evidence+"\n"), 0o600)
		},
	}
	item, err := observer.wait(id, "blocked", phase, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return item, gateEvidence
}

type workspaceRecoveryGateEvidence struct {
	WorkItemID         string `json:"work_item_id"`
	CurrentAttemptID   string `json:"current_attempt_id"`
	AttemptID          string `json:"attempt_id"`
	SessionID          string `json:"session_id"`
	RunState           string `json:"run_state"`
	FirstWorkerID      string `json:"first_worker_id"`
	FirstGeneration    int64  `json:"first_generation"`
	AssignmentCount    int    `json:"assignment_count"`
	GenerationCount    int    `json:"generation_count"`
	BashCallCount      int    `json:"bash_call_count"`
	InitialResultCount int    `json:"initial_result_count"`
	ResumeResultCount  int    `json:"resume_result_count"`
	LastEventSequence  int    `json:"last_event_sequence"`
}

func (e workspaceRecoveryGateEvidence) qualifies(item workspaceWorkItem, phase string) (bool, string, error) {
	detail := fmt.Sprintf("work=%s attempt=%s session=%s run=%s assignments=%d generations=%d bash=%d initial=%d resume=%d last_event=%d",
		e.WorkItemID, e.AttemptID, e.SessionID, e.RunState, e.AssignmentCount, e.GenerationCount,
		e.BashCallCount, e.InitialResultCount, e.ResumeResultCount, e.LastEventSequence)
	if e.WorkItemID != item.ID || e.CurrentAttemptID != item.CurrentAttemptID || e.AttemptID != item.CurrentAttemptID || e.SessionID == "" || e.RunState != "awaiting_approval" {
		return false, detail, nil
	}
	switch phase {
	case "initial":
		return e.FirstWorkerID != "" && e.FirstGeneration > 0 && e.AssignmentCount == 1 && e.GenerationCount == 1 && e.BashCallCount == 1 && e.InitialResultCount == 1 && e.ResumeResultCount == 0, detail, nil
	case "resumed":
		return e.FirstWorkerID != "" && e.FirstGeneration > 0 && e.AssignmentCount == 2 && e.GenerationCount == 2 && e.BashCallCount == 2 && e.InitialResultCount == 1 && e.ResumeResultCount == 1, detail, nil
	default:
		return false, detail, fmt.Errorf("unknown recovery phase %q", phase)
	}
}

func workspaceRecoveryGateEvidenceQuery(item workspaceWorkItem) (string, error) {
	if !workspaceUUIDPattern.MatchString(item.ID) || !workspaceUUIDPattern.MatchString(item.CurrentAttemptID) {
		return "", fmt.Errorf("invalid work/attempt identity %q/%q", item.ID, item.CurrentAttemptID)
	}
	digest := workspaceMarkerDigest("recovery-marker")
	return fmt.Sprintf(`
SELECT json_build_object(
  'work_item_id', wi.id,
  'current_attempt_id', wi.current_attempt_id,
  'attempt_id', wr.id,
  'session_id', wr.session_id,
  'run_state', wr.state,
  'first_worker_id', COALESCE((SELECT ta.worker_id FROM runtime.turn_assignments ta WHERE ta.attempt_id='%[2]s' ORDER BY ta.assigned_at,ta.turn_id LIMIT 1),''),
  'first_generation', COALESCE((SELECT ta.fencing_generation FROM runtime.turn_assignments ta WHERE ta.attempt_id='%[2]s' ORDER BY ta.assigned_at,ta.turn_id LIMIT 1),0),
  'assignment_count', (SELECT count(*) FROM runtime.turn_assignments ta WHERE ta.attempt_id='%[2]s'),
  'generation_count', (SELECT count(DISTINCT ta.fencing_generation) FROM runtime.turn_assignments ta WHERE ta.attempt_id='%[2]s'),
  'bash_call_count', (SELECT count(*) FROM runtime.events ev WHERE ev.run_id='%[2]s' AND ev.kind='tool_call_started' AND ev.payload->>'tool_name'='bash'),
  'initial_result_count', (SELECT count(*) FROM runtime.events ev WHERE ev.run_id='%[2]s' AND ev.kind='tool_result' AND ev.payload->>'tool_name'='bash' AND ev.payload->>'result_text'='recovery-initial=%[3]s'),
  'resume_result_count', (SELECT count(*) FROM runtime.events ev WHERE ev.run_id='%[2]s' AND ev.kind='tool_result' AND ev.payload->>'tool_name'='bash' AND ev.payload->>'result_text'='recovery-resume=%[3]s'),
  'last_event_sequence', COALESCE((SELECT max(ev.seq) FROM runtime.events ev WHERE ev.run_id='%[2]s'),0)
)::text
FROM work.work_items wi
JOIN runtime.workflow_runs wr ON wr.id=wi.current_attempt_id
WHERE wi.id='%[1]s' AND wr.id='%[2]s'`, item.ID, item.CurrentAttemptID, digest), nil
}

func workspaceFailureEvidenceQuery(item workspaceWorkItem) (string, error) {
	if !workspaceUUIDPattern.MatchString(item.ID) || !workspaceUUIDPattern.MatchString(item.CurrentAttemptID) {
		return "", fmt.Errorf("invalid work/attempt identity %q/%q", item.ID, item.CurrentAttemptID)
	}
	digest := workspaceMarkerDigest("recovery-marker")
	return fmt.Sprintf(`
WITH
node_rows AS (
  SELECT ne.* FROM runtime.node_executions ne
  WHERE ne.attempt_id='%[2]s' ORDER BY ne.execution_seq LIMIT 16
),
turn_rows AS (
  SELECT tr.*, row_number() OVER (ORDER BY tr.started_at,tr.id) AS row_order
  FROM runtime.turns tr WHERE tr.run_id='%[2]s' ORDER BY tr.started_at,tr.id LIMIT 16
),
assignment_rows AS (
  SELECT ta.*, row_number() OVER (ORDER BY ta.assigned_at,ta.turn_id) AS row_order
  FROM runtime.turn_assignments ta WHERE ta.attempt_id='%[2]s' ORDER BY ta.assigned_at,ta.turn_id LIMIT 16
),
event_rows AS (
  SELECT recent.*,
    CASE
      WHEN recent.kind IN ('error','model_call_failed') THEN recent.payload#>>'{error,message}'
      WHEN recent.kind='settled' THEN recent.payload->>'message'
      WHEN recent.kind='tool_result' THEN recent.payload->>'error_message'
      ELSE NULL
    END AS error_message
  FROM (
    SELECT ev.* FROM runtime.events ev WHERE ev.run_id='%[2]s' ORDER BY ev.seq DESC LIMIT 64
  ) recent
),
timeline_rows AS (
  SELECT te.* FROM work.timeline_events te
  WHERE te.work_item_id='%[1]s' ORDER BY te.cursor DESC LIMIT 32
),
evidence(source_order,row_order,line) AS (
  SELECT 10::int, 0::bigint, 'work=' || json_build_object(
    'id', wi.id, 'workflow_key', left(wi.workflow_key,128),
    'current_attempt_id', wi.current_attempt_id, 'created_at', wi.created_at, 'updated_at', wi.updated_at
  )::text
  FROM work.work_items wi WHERE wi.id='%[1]s'
  UNION ALL
  SELECT 20, 0, 'attempt=' || json_build_object(
    'id', wr.id, 'work_item_id', a.work_item_id, 'state', wr.state, 'session_id', left(wr.session_id,128),
    'operator_failure_reason', left(a.operator_failure_detail->>'reason',64),
    'operator_failure_turn_id', left(a.operator_failure_detail->>'turnId',64),
    'customer_failure_code', left(a.customer_failure_summary->>'code',64),
    'started_at', wr.started_at, 'finished_at', wr.finished_at
  )::text
  FROM runtime.workflow_runs wr JOIN work.attempts a ON a.id=wr.id WHERE wr.id='%[2]s'
  UNION ALL
  SELECT 30, 0, 'allocation=' || json_build_object(
    'session_id', left(wr.session_id,128), 'uid', alloc.uid, 'state', alloc.state,
    'allocated_at', alloc.allocated_at, 'freed_at', alloc.freed_at
  )::text
  FROM runtime.workflow_runs wr
  LEFT JOIN runtime.session_uid_allocations alloc ON alloc.session_id=wr.session_id
  WHERE wr.id='%[2]s'
  UNION ALL
  SELECT 40, 0, 'limits=' || json_build_object(
    'nodes_total', (SELECT count(*) FROM runtime.node_executions WHERE attempt_id='%[2]s'), 'nodes_limit', 16,
    'turns_total', (SELECT count(*) FROM runtime.turns WHERE run_id='%[2]s'), 'turns_limit', 16,
    'assignments_total', (SELECT count(*) FROM runtime.turn_assignments WHERE attempt_id='%[2]s'), 'assignments_limit', 16,
    'events_total', (SELECT count(*) FROM runtime.events WHERE run_id='%[2]s'), 'events_limit', 64,
    'timeline_total', (SELECT count(*) FROM work.timeline_events WHERE work_item_id='%[1]s'), 'timeline_limit', 32,
    'output_byte_limit', %[4]d
  )::text
  UNION ALL
  SELECT 100, ne.execution_seq::bigint, 'node=' || json_build_object(
    'id', ne.id, 'attempt_id', ne.attempt_id, 'key', left(ne.node_key,128), 'visit', ne.visit,
    'execution_seq', ne.execution_seq, 'kind', ne.kind, 'state', ne.state,
    'completion_outcome', left(ne.completion_outcome,64),
    'started_at', ne.started_at, 'finished_at', ne.finished_at
  )::text FROM node_rows ne
  UNION ALL
  SELECT 200, tr.row_order, 'turn=' || json_build_object(
    'id', tr.id, 'run_id', tr.run_id, 'node_execution_id', tr.node_execution_id,
    'session_id', left(tr.session_id,128), 'state', tr.state,
    'started_at', tr.started_at, 'settled_at', tr.settled_at
  )::text FROM turn_rows tr
  UNION ALL
  SELECT 300, ta.row_order, 'assignment=' || json_build_object(
    'turn_id', ta.turn_id, 'run_id', ta.run_id, 'attempt_id', ta.attempt_id,
    'work_item_id', ta.work_item_id, 'node_execution_id', ta.node_execution_id,
    'pool_id', ta.pool_id, 'worker_id', left(ta.worker_id,128),
    'generation', ta.fencing_generation, 'state', ta.state,
    'highest_applied_sequence', ta.highest_applied_sequence,
    'assigned_at', ta.assigned_at, 'terminalized_at', ta.terminalized_at
  )::text FROM assignment_rows ta
  UNION ALL
  SELECT 1000, ev.seq::bigint, 'event=' || json_build_object(
    'id', ev.id, 'run_id', ev.run_id, 'seq', ev.seq, 'turn_id', ev.turn_id, 'step_id', ev.step_id,
    'node_execution_id', ev.node_execution_id, 'kind', left(ev.kind,64), 'ts', ev.ts,
    'detail', json_strip_nulls(json_build_object(
      'session_id', CASE WHEN ev.kind='turn_started' THEN left(ev.payload->>'session_id',128) END,
      'sandbox_id', CASE WHEN ev.kind='turn_started' THEN left(ev.payload#>>'{sandbox,sandbox_id}',128) END,
      'uid', CASE WHEN ev.kind='turn_started' THEN left(ev.payload#>>'{sandbox,uid}',32) END,
      'gid', CASE WHEN ev.kind='turn_started' THEN left(ev.payload#>>'{sandbox,gid}',32) END,
      'model', CASE WHEN ev.kind IN ('turn_started','model_call_started') THEN left(ev.payload->>'model',128) END,
      'tool_name', CASE WHEN ev.kind IN ('tool_call_started','tool_result') THEN left(ev.payload->>'tool_name',128) END,
      'tool_call_id', CASE WHEN ev.kind IN ('tool_call_started','tool_result') THEN left(ev.payload->>'tool_call_id',128) END,
      'is_error', CASE WHEN ev.kind='tool_result' THEN left(ev.payload->>'is_error',16) END,
      'workspace_marker', CASE
        WHEN ev.kind='tool_result' AND ev.payload->>'result_text'='recovery-initial=%[3]s' THEN 'recovery_initial'
        WHEN ev.kind='tool_result' AND ev.payload->>'result_text'='recovery-resume=%[3]s' THEN 'recovery_resume'
        ELSE NULL END,
      'error_code', CASE WHEN ev.kind IN ('error','model_call_failed') THEN left(ev.payload#>>'{error,code}',64) END,
      'retryability', CASE WHEN ev.kind IN ('error','model_call_failed') THEN left(ev.payload#>>'{error,retryability}',32) END,
      'error_class', CASE
        WHEN lower(COALESCE(ev.error_message,'')) LIKE '%%gateway discovery failed:%%' AND lower(ev.error_message) SIMILAR TO '%%(abort|deadline|timeout)%%' THEN 'gateway_discovery_timeout'
        WHEN lower(COALESCE(ev.error_message,'')) LIKE '%%gateway discovery failed:%%' AND lower(ev.error_message) LIKE '%%permission denied%%' THEN 'gateway_discovery_permission_denied'
        WHEN lower(COALESCE(ev.error_message,'')) LIKE '%%gateway discovery failed:%%' AND lower(ev.error_message) LIKE '%%unavailable%%' THEN 'gateway_discovery_unavailable'
        WHEN lower(COALESCE(ev.error_message,'')) LIKE '%%gateway discovery failed:%%' THEN 'gateway_discovery_failed'
        WHEN lower(COALESCE(ev.error_message,'')) LIKE '%%artifact materialization failed:%%' THEN 'artifact_materialization_failed'
        WHEN lower(COALESCE(ev.error_message,'')) LIKE '%%sandbox invalid:%%' THEN 'sandbox_invalid'
        WHEN lower(COALESCE(ev.error_message,'')) LIKE '%%outbox overflow:%%' THEN 'outbox_overflow'
        WHEN lower(COALESCE(ev.error_message,'')) SIMILAR TO '%%(deadline|timeout)%%' THEN 'timeout'
        WHEN ev.error_message IS NOT NULL AND ev.error_message <> '' THEN 'other'
        ELSE NULL END,
      'error_message_bytes', CASE WHEN ev.error_message IS NOT NULL THEN octet_length(ev.error_message) END,
      'outcome', CASE WHEN ev.kind='settled' THEN left(ev.payload->>'outcome',32) END
    ))
  )::text FROM event_rows ev
  UNION ALL
  SELECT 100000, te.cursor, 'timeline=' || json_build_object(
    'id', te.id, 'cursor', te.cursor, 'work_item_id', te.work_item_id,
    'attempt_id', te.attempt_id, 'node_execution_id', te.node_execution_id,
    'code', left(te.code,128), 'created_at', te.created_at
  )::text FROM timeline_rows te
),
aggregated AS (
  SELECT COALESCE(string_agg(left(line,768), E'\n' ORDER BY source_order,row_order), 'no durable evidence') AS body
  FROM evidence
)
SELECT CASE WHEN length(body) <= 32000 THEN body
  ELSE left(body,31900) || E'\n...[query output truncated before %[4]d-byte process cap]'
END FROM aggregated`, item.ID, item.CurrentAttemptID, digest, workspaceFailureEvidenceMaxBytes), nil
}

func workspaceDatabaseQueryResult(cluster *remotecluster.Cluster, state *permanentCPUFixtureState, query string) (string, error) {
	stdout, _, err := cluster.KubectlSeparated("exec", "-n", workspaceNamespace,
		"statefulset/"+state.runID+"-postgresql", "-c", "postgresql", "--",
		"psql", "-U", "controlplane", "-d", "controlplane", "-Atc", query)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

func workspaceDatabaseQuery(t *testing.T, cluster *remotecluster.Cluster, state *permanentCPUFixtureState, query string) string {
	t.Helper()
	result, err := workspaceDatabaseQueryResult(cluster, state, query)
	if err != nil {
		t.Fatalf("workspace database query: %v", err)
	}
	return result
}

func boundedWorkspaceEvidence(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	suffix := fmt.Sprintf("\n...[process output truncated; %d bytes total]", len(value))
	keep := limit - len(suffix)
	if keep < 0 {
		return suffix[len(suffix)-limit:]
	}
	for keep > 0 && !utf8.ValidString(value[:keep]) {
		keep--
	}
	return value[:keep] + suffix
}

func TestWorkspaceRecoveryGateRequiresAttributableConsequences(t *testing.T) {
	item := workspaceWorkItem{ID: "2084979d-bbb4-4f05-8f12-8c3d6ab7c78e", CurrentAttemptID: "5cf5a3ce-21bf-4b3f-90d9-a22847680b97", State: "blocked"}
	initial := workspaceRecoveryGateEvidence{
		WorkItemID: item.ID, CurrentAttemptID: item.CurrentAttemptID, AttemptID: item.CurrentAttemptID,
		SessionID: "session-1", RunState: "awaiting_approval", FirstWorkerID: "worker-0", FirstGeneration: 7,
		AssignmentCount: 1, GenerationCount: 1, BashCallCount: 1, InitialResultCount: 1, LastEventSequence: 14,
	}
	if qualified, detail, err := initial.qualifies(item, "initial"); err != nil || !qualified {
		t.Fatalf("complete initial gate did not qualify: qualified=%t detail=%s err=%v", qualified, detail, err)
	}
	missingConsequence := initial
	missingConsequence.InitialResultCount = 0
	if qualified, _, err := missingConsequence.qualifies(item, "initial"); err != nil || qualified {
		t.Fatalf("blocked API state qualified without its successful initial consequence: qualified=%t err=%v", qualified, err)
	}
	resumed := initial
	resumed.AssignmentCount = 2
	resumed.GenerationCount = 2
	resumed.BashCallCount = 2
	resumed.ResumeResultCount = 1
	resumed.LastEventSequence = 28
	if qualified, detail, err := resumed.qualifies(item, "resumed"); err != nil || !qualified {
		t.Fatalf("complete resumed gate did not qualify: qualified=%t detail=%s err=%v", qualified, detail, err)
	}
	staleGeneration := resumed
	staleGeneration.GenerationCount = 1
	if qualified, _, err := staleGeneration.qualifies(item, "resumed"); err != nil || qualified {
		t.Fatalf("resume qualified without a fresh fencing generation: qualified=%t err=%v", qualified, err)
	}
}

func TestWorkspaceWorkStateObserverWaitsForDurableGateQualification(t *testing.T) {
	item := workspaceWorkItem{ID: "2084979d-bbb4-4f05-8f12-8c3d6ab7c78e", CurrentAttemptID: "5cf5a3ce-21bf-4b3f-90d9-a22847680b97", State: "blocked"}
	reads, qualifications := 0, 0
	now := time.Unix(0, 0)
	observer := workspaceWorkStateObserver{
		read: func() (workspaceWorkItem, error) { reads++; return item, nil },
		qualify: func(workspaceWorkItem) (bool, string, error) {
			qualifications++
			return qualifications == 2, fmt.Sprintf("qualification=%d", qualifications), nil
		},
		now:      func() time.Time { return now },
		sleep:    func(duration time.Duration) { now = now.Add(duration) },
		interval: time.Millisecond,
	}
	got, err := observer.wait(item.ID, "blocked", "initial", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != item || reads != 2 || qualifications != 2 {
		t.Fatalf("observer returned before durable qualification: got=%+v reads=%d qualifications=%d", got, reads, qualifications)
	}
}

func TestWorkspaceWorkStateObserverPersistsBoundedEvidenceOnQualificationTimeout(t *testing.T) {
	item := workspaceWorkItem{ID: "2084979d-bbb4-4f05-8f12-8c3d6ab7c78e", CurrentAttemptID: "5cf5a3ce-21bf-4b3f-90d9-a22847680b97", State: "blocked"}
	const secret = "timeout-secret-must-not-survive"
	raw := "work={\"id\":\"" + item.ID + "\"}\n" +
		"attempt={\"id\":\"" + item.CurrentAttemptID + "\"}\n" +
		"assignment={\"generation\":1}\nevent={\"seq\":7}\n" + secret + strings.Repeat("x", workspaceFailureEvidenceMaxBytes)
	collector := workspaceFailureEvidenceCollector{
		query:  func(string) (string, error) { return raw, nil },
		redact: func(value string) string { return strings.ReplaceAll(value, secret, "[REDACTED]") },
	}
	now := time.Unix(0, 0)
	reads, qualifications, collections := 0, 0, 0
	var persisted string
	observer := workspaceWorkStateObserver{
		read: func() (workspaceWorkItem, error) { reads++; return item, nil },
		qualify: func(workspaceWorkItem) (bool, string, error) {
			qualifications++
			return false, "assignments=1 initial=0", nil
		},
		collect: func(item workspaceWorkItem) (string, error) {
			collections++
			return collector.collect(item)
		},
		persist:  func(value string) error { persisted = value; return nil },
		now:      func() time.Time { return now },
		sleep:    func(duration time.Duration) { now = now.Add(duration) },
		interval: time.Millisecond,
	}
	_, err := observer.wait(item.ID, "blocked", "initial", 2*time.Millisecond)
	var waitErr *workspaceWorkStateError
	if !errors.As(err, &waitErr) || !waitErr.TimedOut {
		t.Fatalf("qualification timeout error=%v want timed workspaceWorkStateError", err)
	}
	if reads != 2 || qualifications != 2 || collections != 1 {
		t.Fatalf("timeout path reads=%d qualifications=%d collections=%d want 2/2/1", reads, qualifications, collections)
	}
	if persisted == "" || waitErr.Evidence != persisted || len(persisted) > workspaceFailureEvidenceMaxBytes || strings.Contains(persisted, secret) {
		t.Fatalf("timeout evidence was not persisted, bounded, and redacted: bytes=%d evidence=%q", len(persisted), persisted)
	}
	for _, required := range []string{item.ID, item.CurrentAttemptID, `"generation":1`, `"seq":7`} {
		if !strings.Contains(persisted, required) {
			t.Fatalf("timeout evidence lost %q: %s", required, persisted)
		}
	}
	if message := waitErr.Error(); !strings.Contains(message, "did not qualify") || !strings.Contains(message, "assignments=1 initial=0") || !strings.Contains(message, "durable evidence") {
		t.Fatalf("timeout error lost timeout, qualification, or evidence context: %s", message)
	}
}

func TestWorkspaceWorkStateObserverPersistsBoundedRedactedEarlyFailureEvidence(t *testing.T) {
	item := workspaceWorkItem{ID: "2084979d-bbb4-4f05-8f12-8c3d6ab7c78e", CurrentAttemptID: "5cf5a3ce-21bf-4b3f-90d9-a22847680b97", State: "failed"}
	const secret = "api-key-must-not-survive"
	raw := "work={\"id\":\"" + item.ID + "\"}\n" +
		"attempt={\"id\":\"" + item.CurrentAttemptID + "\"}\n" +
		"event={\"seq\":1,\"kind\":\"error\"}\n" + secret + strings.Repeat("x", workspaceFailureEvidenceMaxBytes)
	queryCalled := false
	collector := workspaceFailureEvidenceCollector{
		query: func(query string) (string, error) {
			queryCalled = true
			if !strings.Contains(query, item.ID) || !strings.Contains(query, item.CurrentAttemptID) {
				t.Fatalf("query did not bind exact work identity:\n%s", query)
			}
			return raw, nil
		},
		redact: func(value string) string { return strings.ReplaceAll(value, secret, "[REDACTED]") },
	}
	var persisted string
	observer := workspaceWorkStateObserver{
		read:    func() (workspaceWorkItem, error) { return item, nil },
		collect: collector.collect,
		persist: func(value string) error { persisted = value; return nil },
	}
	_, err := observer.wait(item.ID, "blocked", "initial", time.Second)
	var waitErr *workspaceWorkStateError
	if !errors.As(err, &waitErr) {
		t.Fatalf("early failure error=%v want workspaceWorkStateError", err)
	}
	if !queryCalled || persisted == "" || waitErr.Evidence != persisted {
		t.Fatalf("early failure did not query and persist the same evidence: query=%t persisted=%d evidence=%d", queryCalled, len(persisted), len(waitErr.Evidence))
	}
	if len(persisted) > workspaceFailureEvidenceMaxBytes || strings.Contains(persisted, secret) || !strings.Contains(persisted, "[REDACTED]") {
		t.Fatalf("evidence was not bounded and redacted: bytes=%d evidence=%q", len(persisted), persisted)
	}
	workAt := strings.Index(persisted, item.ID)
	attemptAt := strings.Index(persisted, item.CurrentAttemptID)
	eventAt := strings.Index(persisted, `"seq":1`)
	if workAt < 0 || attemptAt <= workAt || eventAt <= attemptAt {
		t.Fatalf("evidence lost exact ordered identities: work=%d attempt=%d event=%d\n%s", workAt, attemptAt, eventAt, persisted)
	}
}

func TestWorkspaceWorkStateObserverReportsEvidenceQueryFailureWithoutPersisting(t *testing.T) {
	item := workspaceWorkItem{ID: "2084979d-bbb4-4f05-8f12-8c3d6ab7c78e", CurrentAttemptID: "5cf5a3ce-21bf-4b3f-90d9-a22847680b97", State: "failed"}
	collector := workspaceFailureEvidenceCollector{
		query: func(string) (string, error) {
			return "", errors.New("kubectl separated execution: exit status 1; stderr=psql unavailable")
		},
	}
	persisted := false
	observer := workspaceWorkStateObserver{
		read:    func() (workspaceWorkItem, error) { return item, nil },
		collect: collector.collect,
		persist: func(string) error { persisted = true; return nil },
	}
	_, err := observer.wait(item.ID, "blocked", "initial", time.Second)
	if err == nil || !strings.Contains(err.Error(), "collect causal evidence") || !strings.Contains(err.Error(), "psql unavailable") {
		t.Fatalf("query failure was not actionable: %v", err)
	}
	if persisted {
		t.Fatal("query failure persisted invented evidence")
	}
}

func TestWorkspaceFailureEvidenceQueryIsAllowlistedAndBounded(t *testing.T) {
	item := workspaceWorkItem{ID: "2084979d-bbb4-4f05-8f12-8c3d6ab7c78e", CurrentAttemptID: "5cf5a3ce-21bf-4b3f-90d9-a22847680b97"}
	query, err := workspaceFailureEvidenceQuery(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"work.work_items", "work.attempts", "runtime.workflow_runs", "runtime.session_uid_allocations",
		"runtime.node_executions", "runtime.turns", "runtime.turn_assignments", "runtime.events", "work.timeline_events",
		item.ID, item.CurrentAttemptID, "highest_applied_sequence", "'events_limit', 64", "LIMIT 64",
		"left(line,768)", "query output truncated", "error_class", "workspace_marker",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("failure evidence query does not contain %q:\n%s", required, query)
		}
	}
	for _, forbidden := range []string{
		"'payload', ev.payload", "completion_summary", "'params', te.params", "arguments_json", "assistant_message",
		"'operator_failure_detail', a.operator_failure_detail", "'result_text', ev.payload",
	} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("failure evidence query emits non-allowlisted field %q:\n%s", forbidden, query)
		}
	}
}

func TestWorkspaceFailureEvidenceQueryRejectsUntrustedIdentity(t *testing.T) {
	_, err := workspaceFailureEvidenceQuery(workspaceWorkItem{
		ID:               "2084979d-bbb4-4f05-8f12-8c3d6ab7c78e'; DROP TABLE runtime.events; --",
		CurrentAttemptID: "5cf5a3ce-21bf-4b3f-90d9-a22847680b97",
	})
	if err == nil {
		t.Fatal("failure evidence query accepted a non-UUID work identity")
	}
}
