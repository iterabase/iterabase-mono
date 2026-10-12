package work

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// Customer-safe stop reasons recorded on a failed attempt by the runtime
// itself. Each maps to plain-language copy and never carries technical detail.
const (
	StopTransitionLimit       = "transition_limit"
	StopInvalidWaitInput      = "invalid_wait_input"
	StopInterruptedAfterWrite = "interrupted_after_write"
	StopRepeatedInterruption  = "repeated_interruption"
)

var stopMessages = map[string]string{
	StopTransitionLimit:       "The workflow could not complete within its configured transition limit.",
	StopInvalidWaitInput:      "The workflow could not start waiting because an earlier step did not produce valid references to wait for.",
	StopInterruptedAfterWrite: "A step was interrupted after it had taken external actions. Review those actions and restart the work to continue.",
	StopRepeatedInterruption:  "A step was interrupted repeatedly and was stopped. Restart the work to try again.",
}

// failAttemptTx stops a nonterminal attempt with a customer-safe reason. The
// caller holds the attempt's run row lock. Completed visits, events, and
// invocation evidence are left untouched.
func failAttemptTx(ctx context.Context, tx pgx.Tx, itemID, attemptID, nodeID, nodeKey, code, actorID string, operator map[string]any) error {
	if _, err := tx.Exec(ctx, `UPDATE runtime.workflow_runs SET state='failed', finished_at=now() WHERE id=$1 AND finished_at IS NULL`, attemptID); err != nil {
		return err
	}
	customer, _ := json.Marshal(map[string]string{"code": code, "message": stopMessages[code]})
	var operatorJSON any
	if operator != nil {
		operatorJSON, _ = json.Marshal(operator)
	}
	if _, err := tx.Exec(ctx, `UPDATE work.attempts SET finished_at=now(), customer_failure_summary=$2, operator_failure_detail=$3 WHERE id=$1`,
		attemptID, customer, operatorJSON); err != nil {
		return err
	}
	params := map[string]any{"reason": code}
	if nodeKey != "" {
		params["nodeKey"] = nodeKey
	}
	return appendTimelineTx(ctx, tx, itemID, attemptID, nodeID, "work_failed", params, nil, actorID)
}
