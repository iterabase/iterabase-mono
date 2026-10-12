package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	runtimestore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/runtime"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/workflow"
)

// Interruption causes. Worker and dispatch loss are classified here; provider
// and runner owners raise their pause condition first, then interrupt, so the
// rerun waits for the cause to clear.
const (
	InterruptWorkerLost          = "worker_lost"
	InterruptProviderUnavailable = "provider_unavailable"
	InterruptRunnerOffline       = "runner_offline"

	// MaxConsecutiveInterruptions stops an attempt whose same node keeps
	// being interrupted (DES-HOR-649-04).
	MaxConsecutiveInterruptions = 3

	// interruptedRouteOutcome marks the rerun route in graph_transitions. It
	// cannot collide with a node outcome because it is not a valid
	// complete_step/human outcome name in any reviewed workflow.
	interruptedRouteOutcome = "$interrupted"
)

// InterruptTurn settles a graph turn that was lost mid-way, using the Tool
// Gateway invocation ledger as the only authority (DES-HOR-649-04, amending
// DES-HOR-457-04/07). With no write-class invocation the step reruns from its
// start as a new visit in a fresh session; with any write (succeeded, failed,
// or outcome-unknown) the attempt stops for an explicit restart. The old turn
// is never replayed. It returns the resulting run state.
//
//nolint:gocyclo // Classification, evidence, and the stop-or-rerun transition are one transaction.
func (s *Store) InterruptTurn(ctx context.Context, turnID, cause string) (string, error) {
	switch cause {
	case InterruptWorkerLost, InterruptProviderUnavailable, InterruptRunnerOffline:
	default:
		return "", fmt.Errorf("%w: interruption cause %q is unknown", ErrInvalidInput, cause)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var turnState, attemptID, nodeID, itemID, sessionID string
	if err := tx.QueryRow(ctx, `
		SELECT t.state, t.run_id::text, ne.id::text, a.work_item_id::text, t.session_id
		FROM runtime.turns t JOIN runtime.node_executions ne ON ne.id=t.node_execution_id
		JOIN work.attempts a ON a.id=ne.attempt_id
		WHERE t.id=$1 FOR UPDATE OF t,ne,a`, turnID).Scan(&turnState, &attemptID, &nodeID, &itemID, &sessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if turnState != runtimestore.TurnRunning {
		return "", fmt.Errorf("%w: turn state is %s", ErrInvalidTransition, turnState)
	}
	var runState string
	if err := tx.QueryRow(ctx, `SELECT state FROM runtime.workflow_runs WHERE id=$1 FOR UPDATE`, attemptID).Scan(&runState); err != nil {
		return "", err
	}
	if runtimestore.IsTerminalRun(runState) {
		return "", fmt.Errorf("%w: attempt state is %s", ErrInvalidTransition, runState)
	}
	node, err := scanNodeExecution(tx.QueryRow(ctx, nodeExecutionSelect+` WHERE id=$1 FOR UPDATE`, nodeID))
	if err != nil {
		return "", err
	}

	writes := make([]string, 0)
	rows, err := tx.Query(ctx, `
		SELECT id::text FROM toolgateway.invocations
		WHERE caller_scope='turn' AND caller_scope_id=$1
		  AND effect_class IN ('idempotent_write','non_idempotent_write')
		ORDER BY created_at, id`, turnID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		writes = append(writes, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}

	if _, err := tx.Exec(ctx, `UPDATE runtime.turns SET state='aborted', settled_at=now() WHERE id=$1`, turnID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE runtime.node_executions SET state='interrupted', finished_at=now() WHERE id=$1`, nodeID); err != nil {
		return "", err
	}
	if err := appendRuntimeEventTx(ctx, tx, attemptID, turnID, nodeID, runtimestore.EvTurnInterrupted,
		map[string]any{"cause": cause, "writeInvocations": len(writes)}); err != nil {
		return "", err
	}

	decision := "rerun"
	stopCode := ""
	var consecutive int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM runtime.node_executions
		WHERE attempt_id=$1 AND node_key=$2 AND state='interrupted'
		  AND execution_seq > COALESCE((
			SELECT MAX(execution_seq) FROM runtime.node_executions
			WHERE attempt_id=$1 AND node_key=$2 AND state<>'interrupted'), 0)`, attemptID, node.NodeKey).Scan(&consecutive); err != nil {
		return "", err
	}
	var graphJSON []byte
	if err := tx.QueryRow(ctx, `SELECT graph_snapshot FROM work.attempts WHERE id=$1`, attemptID).Scan(&graphJSON); err != nil {
		return "", err
	}
	var graph workflow.CanonicalGraph
	if err := json.Unmarshal(graphJSON, &graph); err != nil {
		return "", err
	}
	var transitions int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.graph_transitions WHERE attempt_id=$1 AND NOT terminal`, attemptID).Scan(&transitions); err != nil {
		return "", err
	}
	switch {
	case len(writes) > 0:
		decision, stopCode = "stop_after_write", StopInterruptedAfterWrite
	case consecutive >= MaxConsecutiveInterruptions:
		decision, stopCode = "stop_repeated_interruption", StopRepeatedInterruption
	case transitions >= int(graph.MaxTransitions):
		decision, stopCode = "stop_transition_limit", StopTransitionLimit
	}
	writesJSON, _ := json.Marshal(writes)

	if stopCode != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO runtime.turn_interruptions
				(turn_id, node_execution_id, attempt_id, cause, decision, write_invocation_ids, lost_session_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, turnID, nodeID, attemptID, cause, decision, writesJSON, sessionID); err != nil {
			return "", err
		}
		if err := failAttemptTx(ctx, tx, itemID, attemptID, nodeID, node.NodeKey, stopCode, "", map[string]any{
			"turnId": turnID, "cause": cause, "decision": decision, "writeInvocationIds": writes,
		}); err != nil {
			return "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return runtimestore.RunFailed, nil
	}

	// Rerun from the step's start: a new visit with the interrupted visit's
	// exact immutable context and inputs, in a fresh session. The lost session
	// keeps its UID allocation non-recyclable until it is reaped.
	def, ok := findNode(graph, node.NodeKey)
	if !ok {
		return "", fmt.Errorf("graph node %q missing from snapshot", node.NodeKey)
	}
	var modelsJSON, specJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT a.models_snapshot, d.spec_json FROM work.attempts a JOIN workflow.definitions d ON d.id=a.definition_id
		WHERE a.id=$1`, attemptID).Scan(&modelsJSON, &specJSON); err != nil {
		return "", err
	}
	var models map[string]modelSnapshot
	var spec workflow.CanonicalSpec
	if err := json.Unmarshal(modelsJSON, &models); err != nil {
		return "", err
	}
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return "", err
	}
	var contextValue map[string]any
	if err := json.Unmarshal(node.Context, &contextValue); err != nil {
		return "", err
	}
	var visit, seq int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(visit),0)+1 FROM runtime.node_executions WHERE attempt_id=$1 AND node_key=$2`, attemptID, node.NodeKey).Scan(&visit); err != nil {
		return "", err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(execution_seq),0)+1 FROM runtime.node_executions WHERE attempt_id=$1`, attemptID).Scan(&seq); err != nil {
		return "", err
	}
	rerunID, err := insertNodeExecutionTx(ctx, tx, attemptID, def, visit, seq, contextValue, models, spec)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO work.artifact_links (artifact_id,work_item_id,attempt_id,node_execution_id,role,metadata)
		SELECT artifact_id,work_item_id,attempt_id,$2,role,metadata FROM work.artifact_links
		WHERE attempt_id=$1 AND node_execution_id=$3 AND role IN ('source','input')
		ON CONFLICT (artifact_id,work_item_id,attempt_id,node_execution_id,role) DO NOTHING`, attemptID, rerunID, nodeID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO runtime.graph_transitions (attempt_id,from_execution_id,outcome,to_execution_id,terminal)
		VALUES ($1,$2,$3,$4,false)`, attemptID, nodeID, interruptedRouteOutcome, rerunID); err != nil {
		return "", err
	}
	freshSession := uuid.NewString()
	if _, err := tx.Exec(ctx, `UPDATE runtime.workflow_runs SET session_id=$2, session_dir=$2, state='running' WHERE id=$1`, attemptID, freshSession); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO runtime.turn_interruptions
			(turn_id, node_execution_id, attempt_id, cause, decision, write_invocation_ids, lost_session_id, rerun_execution_id, rerun_session_id)
		VALUES ($1,$2,$3,$4,'rerun',$5,$6,$7,$8)`, turnID, nodeID, attemptID, cause, writesJSON, sessionID, rerunID, freshSession); err != nil {
		return "", err
	}
	if err := appendTimelineTx(ctx, tx, itemID, attemptID, nodeID, "step_interrupted",
		map[string]any{"nodeKey": node.NodeKey, "visit": node.Visit, "rerun": true}, nil, ""); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return runtimestore.RunRunning, nil
}
