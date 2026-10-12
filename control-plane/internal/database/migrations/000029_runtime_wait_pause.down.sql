-- HOR-649: development rollback of waits, pauses, and turn interruptions.
-- Rows in the new node kinds/states have no pre-HOR-649 shape, so restoring
-- the old constraints fails closed if any such evidence exists.

DROP VIEW work.current_work_items;
CREATE VIEW work.current_work_items AS
SELECT wi.id, wi.workflow_key, wi.scope_identity_id, wi.title, wi.source, wi.start_identity_id,
       wi.start_idempotency_key, wi.start_payload_hash, wi.current_attempt_id, wi.created_at, wi.updated_at,
       CASE
           WHEN wr.state IN ('failed', 'aborted') THEN 'failed'
           WHEN EXISTS (SELECT 1 FROM work.blockers b WHERE b.work_item_id = wi.id AND b.state = 'open') THEN 'blocked'
           WHEN wr.state = 'pending' THEN 'todo'
           WHEN wr.state IN ('running', 'awaiting_approval') THEN 'in_progress'
           WHEN wr.state = 'succeeded' THEN 'done'
       END AS customer_state,
       wr.state AS runtime_state,
       wr.started_at,
       wr.finished_at
FROM work.work_items wi
JOIN runtime.workflow_runs wr ON wr.id = wi.current_attempt_id;

DROP TABLE work.attempt_pauses;
DROP FUNCTION work.guard_attempt_pause_mutation();
DROP TABLE work.pause_conditions;
DROP TABLE runtime.turn_interruptions;
DROP TABLE runtime.wait_expectations;
DROP TABLE runtime.external_events;
DROP TABLE runtime.waits;

DROP INDEX runtime.idx_node_executions_one_active;
CREATE UNIQUE INDEX idx_node_executions_one_active
    ON runtime.node_executions (attempt_id)
    WHERE state IN ('pending', 'running', 'blocked');
ALTER TABLE runtime.node_executions DROP CONSTRAINT node_executions_state_check;
ALTER TABLE runtime.node_executions ADD CONSTRAINT node_executions_state_check CHECK (state IN
    ('pending', 'running', 'blocked', 'succeeded', 'failed'));
ALTER TABLE runtime.node_executions DROP CONSTRAINT node_executions_kind_check;
ALTER TABLE runtime.node_executions ADD CONSTRAINT node_executions_kind_check
    CHECK (kind IN ('agent_task', 'human_gate'));

ALTER TABLE runtime.workflow_runs DROP CONSTRAINT workflow_runs_state_check;
ALTER TABLE runtime.workflow_runs ADD CONSTRAINT workflow_runs_state_check CHECK (state IN
    ('pending', 'running', 'awaiting_approval', 'succeeded', 'failed', 'aborted'));
