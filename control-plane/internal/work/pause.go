package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	runtimestore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/runtime"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/workflow"
)

// Pause causes (DES-HOR-649-03), in precedence order when several match.
const (
	PauseTrialEnded          = "trial_ended"
	PauseProviderUnavailable = "provider_unavailable"
	PauseRunnerOffline       = "runner_offline"

	// PauseScopeInstallation holds every pending node.
	PauseScopeInstallation = "installation"

	// runnerReconcilerActor raises and clears the runner_offline conditions
	// derived from Tool Gateway runner heartbeats.
	runnerReconcilerActor = "runner-heartbeat-reconciler"
	// runnerHeartbeatWindow matches toolgateway.available_tool_versions.
	runnerHeartbeatWindow = 30 * time.Second
)

var pauseCauses = []string{PauseTrialEnded, PauseProviderUnavailable, PauseRunnerOffline}

// PauseScopeModel holds agent nodes whose snapshotted model id is modelID.
func PauseScopeModel(modelID string) string { return "model:" + modelID }

// PauseScopeTool holds nodes granted the tool, at any version.
func PauseScopeTool(name string) string { return "tool:" + name }

// PauseScopeToolVersion holds nodes whose attempt pins the exact tool digest.
func PauseScopeToolVersion(name, digest string) string { return "tool:" + name + "@" + digest }

// PauseCondition is one live or cleared pause cause.
type PauseCondition struct {
	ID        string     `json:"id"`
	Cause     string     `json:"cause"`
	Scope     string     `json:"scope"`
	RaisedBy  string     `json:"raisedBy"`
	RaisedAt  time.Time  `json:"raisedAt"`
	ClearedAt *time.Time `json:"clearedAt,omitempty"`
}

func validatePauseCondition(cause, scope string) error {
	known := false
	for _, c := range pauseCauses {
		known = known || c == cause
	}
	if !known {
		return fmt.Errorf("%w: pause cause %q is unknown", ErrInvalidInput, cause)
	}
	if scope != PauseScopeInstallation && !scopedTo(scope, "model:") && !scopedTo(scope, "tool:") {
		return fmt.Errorf("%w: pause scope %q must be installation, model:<id>, or tool:<name>", ErrInvalidInput, scope)
	}
	return nil
}

func scopedTo(scope, prefix string) bool {
	return strings.HasPrefix(scope, prefix) && len(scope) > len(prefix)
}

// RaisePauseCondition records a live pause cause. It is idempotent: raising a
// condition that is already live is a no-op. Matching pending nodes pause at
// their next step boundary.
func (s *Store) RaisePauseCondition(ctx context.Context, cause, scope, raisedBy string) error {
	if err := validatePauseCondition(cause, scope); err != nil {
		return err
	}
	if raisedBy == "" || len(raisedBy) > 128 {
		return fmt.Errorf("%w: raisedBy requires 1-128 bytes", ErrInvalidInput)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO work.pause_conditions (cause, scope, raised_by) VALUES ($1,$2,$3)
		ON CONFLICT (cause, scope) WHERE cleared_at IS NULL DO NOTHING`, cause, scope, raisedBy)
	return err
}

// ClearPauseCondition clears a live pause cause. Paused attempts with no other
// matching cause resume automatically on the next scheduling pass. Clearing a
// condition that is not live is a no-op.
func (s *Store) ClearPauseCondition(ctx context.Context, cause, scope string) error {
	if err := validatePauseCondition(cause, scope); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE work.pause_conditions SET cleared_at=now() WHERE cause=$1 AND scope=$2 AND cleared_at IS NULL`, cause, scope)
	return err
}

// ListPauseConditions returns live conditions, or all when includeCleared.
func (s *Store) ListPauseConditions(ctx context.Context, includeCleared bool) ([]PauseCondition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, cause, scope, raised_by, raised_at, cleared_at FROM work.pause_conditions
		WHERE $1 OR cleared_at IS NULL ORDER BY raised_at, id`, includeCleared)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PauseCondition, 0)
	for rows.Next() {
		var c PauseCondition
		if err := rows.Scan(&c.ID, &c.Cause, &c.Scope, &c.RaisedBy, &c.RaisedAt, &c.ClearedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// nodePauseScopes lists every scope that can hold this pending node.
func nodePauseScopesTx(ctx context.Context, tx pgx.Tx, attemptID string, node NodeExecution) ([]string, error) {
	scopes := []string{PauseScopeInstallation}
	if node.Kind == workflow.NodeAgentTask && len(node.ModelSnapshot) > 0 {
		var model struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(node.ModelSnapshot, &model); err == nil && model.ID != "" {
			scopes = append(scopes, PauseScopeModel(model.ID))
		}
	}
	var caps []workflow.CanonicalCapability
	if len(node.CapabilitiesSnapshot) > 0 {
		if err := json.Unmarshal(node.CapabilitiesSnapshot, &caps); err != nil {
			return nil, err
		}
	}
	for _, c := range caps {
		scopes = append(scopes, PauseScopeTool(c.Tool))
		var digest string
		err := tx.QueryRow(ctx, `SELECT tool_version_digest FROM toolgateway.attempt_tool_pins WHERE attempt_id=$1 AND tool_name=$2`,
			attemptID, c.Tool).Scan(&digest)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, PauseScopeToolVersion(c.Tool, digest))
	}
	return scopes, nil
}

// holdForPauseTx is the single pause gate at a step boundary. Before a pending
// node is prepared it either records/keeps the attempt paused with the
// highest-precedence live cause that matches the node, or resumes a paused
// attempt whose causes have all cleared. The caller holds the node and run
// locks.
//
//nolint:gocyclo // Pause, reason change, and resume are one transition at the step boundary.
func holdForPauseTx(ctx context.Context, tx pgx.Tx, itemID, attemptID string, node NodeExecution) (bool, error) {
	scopes, err := nodePauseScopesTx(ctx, tx, attemptID, node)
	if err != nil {
		return false, err
	}
	var conditionID, cause, scope string
	err = tx.QueryRow(ctx, `
		SELECT id::text, cause, scope FROM work.pause_conditions
		WHERE cleared_at IS NULL AND scope = ANY($1)
		ORDER BY array_position($2::text[], cause), raised_at, id LIMIT 1`, scopes, pauseCauses).Scan(&conditionID, &cause, &scope)
	matched := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var openID, openCondition, openCause string
	err = tx.QueryRow(ctx, `SELECT id::text, condition_id::text, cause FROM work.attempt_pauses WHERE attempt_id=$1 AND resumed_at IS NULL`,
		attemptID).Scan(&openID, &openCondition, &openCause)
	open := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if matched && open && openCondition == conditionID {
		return true, nil
	}
	if open {
		if _, err := tx.Exec(ctx, `UPDATE work.attempt_pauses SET resumed_at=now() WHERE id=$1`, openID); err != nil {
			return false, err
		}
		if !matched {
			if err := appendRuntimeEventTx(ctx, tx, attemptID, "", node.ID, runtimestore.EvAttemptResumed, map[string]any{"cause": openCause}); err != nil {
				return false, err
			}
			return false, appendTimelineTx(ctx, tx, itemID, attemptID, node.ID, "work_resumed",
				map[string]any{"nodeKey": node.NodeKey, "reason": openCause}, nil, "")
		}
	}
	if !matched {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO work.attempt_pauses (work_item_id, attempt_id, node_execution_id, condition_id, cause, scope)
		VALUES ($1,$2,$3,$4,$5,$6)`, itemID, attemptID, node.ID, conditionID, cause, scope); err != nil {
		return false, err
	}
	if err := appendRuntimeEventTx(ctx, tx, attemptID, "", node.ID, runtimestore.EvAttemptPaused,
		map[string]any{"cause": cause, "scope": scope, "conditionId": conditionID}); err != nil {
		return false, err
	}
	return true, appendTimelineTx(ctx, tx, itemID, attemptID, node.ID, "work_paused",
		map[string]any{"nodeKey": node.NodeKey, "reason": cause}, nil, "")
}

// ReconcileRunnerPauseConditions derives runner_offline conditions from Tool
// Gateway runner heartbeats. A tool digest pinned by unfinished work with no
// active runner heartbeating within the gateway's 30s window is raised; it is
// cleared once a runner serves it again or no unfinished work pins it.
func (s *Store) ReconcileRunnerPauseConditions(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		WITH needed AS (
			SELECT DISTINCT p.tool_name, p.tool_version_digest
			FROM toolgateway.attempt_tool_pins p
			JOIN runtime.workflow_runs wr ON wr.id::text = p.attempt_id::text
			WHERE wr.finished_at IS NULL
		)
		SELECT n.tool_name, n.tool_version_digest
		FROM needed n
		WHERE NOT EXISTS (
			SELECT 1 FROM toolgateway.runner_registrations r
			WHERE r.tool_name=n.tool_name AND r.tool_digest=n.tool_version_digest
			  AND r.active AND r.last_heartbeat_at > now() - make_interval(secs => $1)
		)`, runnerHeartbeatWindow.Seconds())
	if err != nil {
		return err
	}
	offline := map[string]struct{}{}
	for rows.Next() {
		var name, digest string
		if err := rows.Scan(&name, &digest); err != nil {
			rows.Close()
			return err
		}
		offline[PauseScopeToolVersion(name, digest)] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for scope := range offline {
		if err := s.RaisePauseCondition(ctx, PauseRunnerOffline, scope, runnerReconcilerActor); err != nil {
			return err
		}
	}
	live, err := s.pool.Query(ctx, `
		SELECT scope FROM work.pause_conditions
		WHERE cause=$1 AND raised_by=$2 AND cleared_at IS NULL`, PauseRunnerOffline, runnerReconcilerActor)
	if err != nil {
		return err
	}
	var clear []string
	for live.Next() {
		var scope string
		if err := live.Scan(&scope); err != nil {
			live.Close()
			return err
		}
		if _, still := offline[scope]; !still {
			clear = append(clear, scope)
		}
	}
	live.Close()
	if err := live.Err(); err != nil {
		return err
	}
	for _, scope := range clear {
		if err := s.ClearPauseCondition(ctx, PauseRunnerOffline, scope); err != nil {
			return err
		}
	}
	return nil
}
