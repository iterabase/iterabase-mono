-- HOR-649: durable external-event waits, paused-with-reason, and interrupted-step
-- rerun on the single-active-node runtime (DES-HOR-649-01..05).
--
-- PostgreSQL stays the only authority (DES-HOR-457-01): waits, expected events,
-- received external events, pause conditions, attempt pauses, and turn
-- interruptions are rows here. No worker, turn, or session is held while a
-- node waits; the run moves to `waiting` and the reconciler skips it.

ALTER TABLE runtime.workflow_runs DROP CONSTRAINT workflow_runs_state_check;
ALTER TABLE runtime.workflow_runs ADD CONSTRAINT workflow_runs_state_check CHECK (state IN
    ('pending', 'running', 'awaiting_approval', 'waiting', 'succeeded', 'failed', 'aborted'));

-- `wait` is a database-only node kind. `waiting` is its active state;
-- `interrupted` is the terminal state of a visit whose turn was lost mid-way.
-- A rerun is a new visit, never a reset of this one.
ALTER TABLE runtime.node_executions DROP CONSTRAINT node_executions_kind_check;
ALTER TABLE runtime.node_executions ADD CONSTRAINT node_executions_kind_check
    CHECK (kind IN ('agent_task', 'human_gate', 'wait'));
ALTER TABLE runtime.node_executions DROP CONSTRAINT node_executions_state_check;
ALTER TABLE runtime.node_executions ADD CONSTRAINT node_executions_state_check CHECK (state IN
    ('pending', 'running', 'blocked', 'waiting', 'succeeded', 'failed', 'interrupted'));
DROP INDEX runtime.idx_node_executions_one_active;
CREATE UNIQUE INDEX idx_node_executions_one_active
    ON runtime.node_executions (attempt_id)
    WHERE state IN ('pending', 'running', 'blocked', 'waiting');

-- One row per entered wait visit. The row lock serializes event arrival,
-- deadline expiry, and Operator "stop waiting" for that visit.
CREATE TABLE runtime.waits (
    node_execution_id uuid PRIMARY KEY REFERENCES runtime.node_executions(id) ON DELETE RESTRICT,
    attempt_id        uuid NOT NULL REFERENCES work.attempts(id) ON DELETE RESTRICT,
    completion        text NOT NULL CHECK (completion IN ('all', 'any')),
    expected_count    integer NOT NULL CHECK (expected_count BETWEEN 1 AND 256),
    deadline_at       timestamptz NOT NULL,
    entered_at        timestamptz NOT NULL DEFAULT now(),
    outcome           text CHECK (outcome IN ('received', 'deadline', 'cancelled')),
    completed_at      timestamptz,
    stopped_by        uuid REFERENCES identity.identities(id),
    CHECK ((outcome IS NULL) = (completed_at IS NULL)),
    CHECK (stopped_by IS NULL OR outcome = 'cancelled')
);
CREATE INDEX idx_waits_due ON runtime.waits (deadline_at) WHERE completed_at IS NULL;

-- Immutable evidence of every event an event source posted. (source,
-- event_key) is the source's idempotency key: a repeat post is ignored.
-- disposition records how the event was handled when it was received.
CREATE TABLE runtime.external_events (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source          text NOT NULL CHECK (source ~ '^[a-z][a-z0-9_]{0,62}$'),
    event_key       text NOT NULL CHECK (length(event_key) BETWEEN 1 AND 512),
    correlation_key text NOT NULL CHECK (length(correlation_key) BETWEEN 1 AND 512),
    payload         jsonb NOT NULL DEFAULT '{}'::jsonb,
    artifact_refs   jsonb NOT NULL DEFAULT '[]'::jsonb,
    disposition     text NOT NULL CHECK (disposition IN ('matched', 'late', 'unmatched')),
    received_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source, event_key)
);
CREATE INDEX idx_external_events_key ON runtime.external_events (source, correlation_key, received_at);
CREATE TRIGGER external_events_append_only BEFORE UPDATE OR DELETE ON runtime.external_events
    FOR EACH ROW EXECUTE FUNCTION work.reject_history_mutation();

-- One expected event per correlation key of a wait visit. A pending key is
-- globally unique per source so an arriving event matches at most one wait.
CREATE TABLE runtime.wait_expectations (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    node_execution_id uuid NOT NULL REFERENCES runtime.waits(node_execution_id) ON DELETE RESTRICT,
    attempt_id        uuid NOT NULL REFERENCES work.attempts(id) ON DELETE RESTRICT,
    ordinal           integer NOT NULL CHECK (ordinal >= 0),
    source            text NOT NULL,
    correlation_key   text NOT NULL,
    state             text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'received', 'closed')),
    event_id          uuid UNIQUE REFERENCES runtime.external_events(id) ON DELETE RESTRICT,
    received_at       timestamptz,
    CHECK ((state = 'received') = (event_id IS NOT NULL)),
    CHECK ((state = 'received') = (received_at IS NOT NULL)),
    UNIQUE (node_execution_id, ordinal),
    UNIQUE (node_execution_id, source, correlation_key)
);
CREATE UNIQUE INDEX idx_wait_expectations_pending_key
    ON runtime.wait_expectations (source, correlation_key) WHERE state = 'pending';
CREATE INDEX idx_wait_expectations_key ON runtime.wait_expectations (source, correlation_key);

-- Interrupted-step evidence (DES-HOR-649-04, amending DES-HOR-457-04/07).
-- The Tool Gateway ledger decides the classification; the exact write
-- invocations seen are retained.
CREATE TABLE runtime.turn_interruptions (
    turn_id              uuid PRIMARY KEY REFERENCES runtime.turns(id) ON DELETE RESTRICT,
    node_execution_id    uuid NOT NULL REFERENCES runtime.node_executions(id) ON DELETE RESTRICT,
    attempt_id           uuid NOT NULL REFERENCES work.attempts(id) ON DELETE RESTRICT,
    cause                text NOT NULL CHECK (cause IN ('worker_lost', 'provider_unavailable', 'runner_offline')),
    decision             text NOT NULL CHECK (decision IN
                             ('rerun', 'stop_after_write', 'stop_repeated_interruption', 'stop_transition_limit')),
    write_invocation_ids jsonb NOT NULL DEFAULT '[]'::jsonb,
    lost_session_id      text NOT NULL,
    rerun_execution_id   uuid REFERENCES runtime.node_executions(id) ON DELETE RESTRICT,
    rerun_session_id     text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    CHECK ((decision = 'rerun') = (rerun_execution_id IS NOT NULL)),
    CHECK ((decision = 'rerun') = (rerun_session_id IS NOT NULL))
);
CREATE INDEX idx_turn_interruptions_attempt ON runtime.turn_interruptions (attempt_id, created_at);
CREATE TRIGGER turn_interruptions_append_only BEFORE UPDATE OR DELETE ON runtime.turn_interruptions
    FOR EACH ROW EXECUTE FUNCTION work.reject_history_mutation();

-- Pause causes (DES-HOR-649-03). Owners raise and clear conditions; the
-- scheduler matches live conditions before preparing any pending node.
CREATE TABLE work.pause_conditions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cause       text NOT NULL CHECK (cause IN ('trial_ended', 'provider_unavailable', 'runner_offline')),
    scope       text NOT NULL CHECK (scope = 'installation' OR scope ~ '^(model|tool):.+$'),
    raised_by   text NOT NULL CHECK (length(raised_by) BETWEEN 1 AND 128),
    raised_at   timestamptz NOT NULL DEFAULT now(),
    cleared_at  timestamptz,
    CHECK (cleared_at IS NULL OR cleared_at >= raised_at)
);
CREATE UNIQUE INDEX idx_pause_conditions_live ON work.pause_conditions (cause, scope) WHERE cleared_at IS NULL;

-- One row per pause of an attempt at a step boundary. Only resumed_at may be
-- set, once; the rest is immutable history.
CREATE TABLE work.attempt_pauses (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id      uuid NOT NULL REFERENCES work.work_items(id) ON DELETE RESTRICT,
    attempt_id        uuid NOT NULL REFERENCES work.attempts(id) ON DELETE RESTRICT,
    node_execution_id uuid NOT NULL REFERENCES runtime.node_executions(id) ON DELETE RESTRICT,
    condition_id      uuid NOT NULL REFERENCES work.pause_conditions(id) ON DELETE RESTRICT,
    cause             text NOT NULL,
    scope             text NOT NULL,
    paused_at         timestamptz NOT NULL DEFAULT now(),
    resumed_at        timestamptz,
    CHECK (resumed_at IS NULL OR resumed_at >= paused_at)
);
CREATE UNIQUE INDEX idx_attempt_pauses_open ON work.attempt_pauses (attempt_id) WHERE resumed_at IS NULL;
CREATE INDEX idx_attempt_pauses_item ON work.attempt_pauses (work_item_id, paused_at);

CREATE OR REPLACE FUNCTION work.guard_attempt_pause_mutation() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'attempt_pauses is append-only';
    END IF;
    IF OLD.resumed_at IS NOT NULL
       OR NEW.resumed_at IS NULL
       OR (NEW.id, NEW.work_item_id, NEW.attempt_id, NEW.node_execution_id, NEW.condition_id,
           NEW.cause, NEW.scope, NEW.paused_at)
          IS DISTINCT FROM
          (OLD.id, OLD.work_item_id, OLD.attempt_id, OLD.node_execution_id, OLD.condition_id,
           OLD.cause, OLD.scope, OLD.paused_at) THEN
        RAISE EXCEPTION 'attempt_pauses only records resumed_at once';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER attempt_pauses_guard BEFORE UPDATE OR DELETE ON work.attempt_pauses
    FOR EACH ROW EXECUTE FUNCTION work.guard_attempt_pause_mutation();

-- Customer state (DES-HOR-649-05), in the architecture section 7 precedence.
-- Waiting and paused never count as blocked.
-- The work-item columns are the view's original HOR-254 set, listed explicitly
-- so later work_items columns stay independent of this view.
DROP VIEW work.current_work_items;
CREATE VIEW work.current_work_items AS
SELECT wi.id, wi.workflow_key, wi.scope_identity_id, wi.title, wi.source, wi.start_identity_id,
       wi.start_idempotency_key, wi.start_payload_hash, wi.current_attempt_id, wi.created_at, wi.updated_at,
       CASE
           WHEN wr.state IN ('failed', 'aborted') THEN 'failed'
           WHEN wr.state = 'succeeded' THEN 'done'
           WHEN EXISTS (SELECT 1 FROM work.attempt_pauses p WHERE p.attempt_id = wr.id AND p.resumed_at IS NULL) THEN 'paused'
           WHEN wr.state = 'waiting' THEN 'waiting'
           WHEN EXISTS (SELECT 1 FROM work.blockers b WHERE b.work_item_id = wi.id AND b.state = 'open') THEN 'blocked'
           WHEN wr.state = 'pending' THEN 'todo'
           WHEN wr.state IN ('running', 'awaiting_approval') THEN 'in_progress'
       END AS customer_state,
       wr.state AS runtime_state,
       wr.started_at,
       wr.finished_at
FROM work.work_items wi
JOIN runtime.workflow_runs wr ON wr.id = wi.current_attempt_id;
