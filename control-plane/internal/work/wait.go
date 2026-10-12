package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	runtimestore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/runtime"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/workflow"
)

// External event dispositions recorded immutably when an event is received.
const (
	EventMatched   = "matched"
	EventLate      = "late"
	EventUnmatched = "unmatched"
)

// maxEventPayloadBytes bounds one posted external event's evidence payload.
const maxEventPayloadBytes = 256 << 10

var eventSourcePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// ExternalEventInput is one event posted by an event source (DES-HOR-649-02).
// (Source, EventKey) is the source's idempotency key.
type ExternalEventInput struct {
	Source         string
	EventKey       string
	CorrelationKey string
	Payload        json.RawMessage
	ArtifactRefs   []ArtifactRef
}

// ExternalEventResult reports how a posted event was handled.
type ExternalEventResult struct {
	EventID     string
	Duplicate   bool
	Disposition string
	WorkItemID  string
}

// StopWaitingInput is an Operator/Admin request to end the current wait early.
type StopWaitingInput struct {
	WorkItemID      string
	ActorIdentityID string
}

type waitExpectation struct {
	ordinal int
	source  string
	key     string
}

// enterWaitTx enters a pending wait visit: it resolves correlation keys and the
// deadline from earlier immutable output, registers the expected events,
// claims events that already arrived for those keys, and releases the run
// from scheduling. Invalid references fail the attempt closed. The caller holds
// the node and run locks.
//
//nolint:gocyclo // Entering a wait resolves, validates, registers, and claims in one transaction.
func (s *Store) enterWaitTx(ctx context.Context, tx pgx.Tx, itemID, attemptID string, node NodeExecution, def workflow.CanonicalNode) (string, error) {
	now := time.Now().UTC()
	expectations, deadline, reason := resolveWaitInputTx(ctx, tx, attemptID, node, def, now)
	if reason == "" {
		var conflict bool
		ordered := append([]waitExpectation(nil), expectations...)
		sort.Slice(ordered, func(i, j int) bool {
			if ordered[i].source != ordered[j].source {
				return ordered[i].source < ordered[j].source
			}
			return ordered[i].key < ordered[j].key
		})
		for _, e := range ordered {
			if err := lockCorrelationKeyTx(ctx, tx, e.source, e.key); err != nil {
				return "", err
			}
		}
		for _, e := range expectations {
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS(SELECT 1 FROM runtime.wait_expectations WHERE source=$1 AND correlation_key=$2 AND state='pending')`,
				e.source, e.key).Scan(&conflict); err != nil {
				return "", err
			}
			if conflict {
				reason = "correlation key is already awaited by other work"
				break
			}
		}
	}
	if reason != "" {
		if _, err := tx.Exec(ctx, `UPDATE runtime.node_executions SET state='failed', started_at=COALESCE(started_at,now()), finished_at=now() WHERE id=$1`, node.ID); err != nil {
			return "", err
		}
		if err := failAttemptTx(ctx, tx, itemID, attemptID, node.ID, node.NodeKey, StopInvalidWaitInput, "",
			map[string]any{"nodeExecutionId": node.ID, "reason": reason}); err != nil {
			return "", err
		}
		return runtimestore.RunFailed, nil
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO runtime.waits (node_execution_id, attempt_id, completion, expected_count, deadline_at)
		VALUES ($1,$2,$3,$4,$5)`, node.ID, attemptID, def.Wait.Completion, len(expectations), deadline); err != nil {
		return "", fmt.Errorf("insert wait: %w", err)
	}
	for _, e := range expectations {
		if _, err := tx.Exec(ctx, `
			INSERT INTO runtime.wait_expectations (node_execution_id, attempt_id, ordinal, source, correlation_key)
			VALUES ($1,$2,$3,$4,$5)`, node.ID, attemptID, e.ordinal, e.source, e.key); err != nil {
			return "", fmt.Errorf("insert wait expectation: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE runtime.node_executions SET state='waiting', started_at=COALESCE(started_at,now()) WHERE id=$1 AND state='pending'`, node.ID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE runtime.workflow_runs SET state='waiting', started_at=COALESCE(started_at,now()) WHERE id=$1 AND state IN ('pending','running')`, attemptID); err != nil {
		return "", err
	}
	if err := appendRuntimeEventTx(ctx, tx, attemptID, "", node.ID, runtimestore.EvWaitEntered,
		map[string]any{"expected": len(expectations), "completion": def.Wait.Completion, "deadlineAt": deadline}); err != nil {
		return "", err
	}
	if err := appendTimelineTx(ctx, tx, itemID, attemptID, node.ID, "work_waiting", map[string]any{
		"nodeKey": node.NodeKey, "expected": len(expectations), "completion": def.Wait.Completion, "deadlineAt": deadline,
	}, nil, ""); err != nil {
		return "", err
	}

	// An event may arrive between the sending step and this wait (or while the
	// attempt was paused before it). Claim the earliest unclaimed event for each
	// key so it is never lost.
	for _, e := range expectations {
		var eventID string
		var receivedAt time.Time
		err := tx.QueryRow(ctx, `
			SELECT e.id::text, e.received_at FROM runtime.external_events e
			WHERE e.source=$1 AND e.correlation_key=$2
			  AND NOT EXISTS (SELECT 1 FROM runtime.wait_expectations x WHERE x.event_id=e.id)
			ORDER BY e.received_at, e.id LIMIT 1`, e.source, e.key).Scan(&eventID, &receivedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE runtime.wait_expectations SET state='received', event_id=$3, received_at=$4
			WHERE node_execution_id=$1 AND ordinal=$2 AND state='pending'`, node.ID, e.ordinal, eventID, receivedAt); err != nil {
			return "", err
		}
	}
	node.State = NodeWaiting
	done, err := s.recordWaitProgressTx(ctx, tx, itemID, attemptID, node, def.Wait.Completion, len(expectations), false)
	if err != nil || !done {
		return runtimestore.RunWaiting, err
	}
	return s.completeWaitTx(ctx, tx, itemID, attemptID, node, workflow.WaitOutcomeReceived, "")
}

// resolveWaitInputTx returns the expected events and the deadline, or a
// bounded operator reason when earlier output cannot satisfy the contract.
func resolveWaitInputTx(ctx context.Context, tx pgx.Tx, attemptID string, node NodeExecution, def workflow.CanonicalNode, now time.Time) ([]waitExpectation, time.Time, string) {
	if def.Wait == nil {
		return nil, time.Time{}, "wait definition is missing from the snapshot"
	}
	output := func(ref workflow.CanonicalOutputReference) (any, error) {
		var raw []byte
		err := tx.QueryRow(ctx, `
			SELECT output FROM runtime.node_executions
			WHERE attempt_id=$1 AND node_key=$2 AND state='succeeded' AND execution_seq < $3
			ORDER BY execution_seq DESC LIMIT 1`, attemptID, ref.Node, node.ExecutionSeq).Scan(&raw)
		if err != nil {
			return nil, err
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		return value, nil
	}
	expectations, reason := resolveWaitKeys(def.Wait.Events, output)
	if reason != "" {
		return nil, time.Time{}, reason
	}
	deadline, reason := resolveWaitDeadline(def.Wait.Deadline, output, now)
	if reason != "" {
		return nil, time.Time{}, reason
	}
	return expectations, deadline, ""
}

type outputReader func(workflow.CanonicalOutputReference) (any, error)

func resolveWaitKeys(events []workflow.CanonicalWaitEvent, output outputReader) ([]waitExpectation, string) {
	var expectations []waitExpectation
	seen := map[[2]string]struct{}{}
	for _, event := range events {
		value, err := output(event.Correlation)
		if err != nil {
			return nil, fmt.Sprintf("correlation source %q has no successful output", event.Correlation.Node)
		}
		keys, ok := outputStrings(value, event.Correlation.Path)
		if !ok {
			return nil, fmt.Sprintf("correlation path on %q is not a string or array of strings", event.Correlation.Node)
		}
		for _, key := range keys {
			if key == "" || len(key) > 512 {
				return nil, "a correlation key is empty or longer than 512 bytes"
			}
			id := [2]string{event.Source, key}
			if _, dup := seen[id]; dup {
				return nil, "a correlation key is repeated"
			}
			seen[id] = struct{}{}
			expectations = append(expectations, waitExpectation{ordinal: len(expectations), source: event.Source, key: key})
		}
	}
	if len(expectations) == 0 {
		return nil, "no correlation keys were produced"
	}
	if len(expectations) > workflow.WaitMaxExpectedEvents {
		return nil, fmt.Sprintf("more than %d correlation keys were produced", workflow.WaitMaxExpectedEvents)
	}
	return expectations, ""
}

func resolveWaitDeadline(spec workflow.CanonicalWaitDeadline, output outputReader, now time.Time) (time.Time, string) {
	var deadline time.Time
	if spec.After != "" {
		d, err := time.ParseDuration(spec.After)
		if err != nil {
			return time.Time{}, "relative deadline is invalid"
		}
		deadline = now.Add(d)
	} else {
		value, err := output(*spec.At)
		if err != nil {
			return time.Time{}, "absolute deadline source has no successful output"
		}
		values, ok := outputStrings(value, spec.At.Path)
		if !ok || len(values) != 1 {
			return time.Time{}, "absolute deadline is not one string"
		}
		parsed, err := time.Parse(time.RFC3339, values[0])
		if err != nil {
			return time.Time{}, "absolute deadline is not RFC3339"
		}
		deadline = parsed.UTC()
	}
	if until := deadline.Sub(now); until < workflow.WaitMinDeadline || until > workflow.WaitMaxDeadline {
		return time.Time{}, "deadline is outside 1m-90d from entering the wait"
	}
	return deadline, ""
}

// outputStrings walks a JSON value along path. "*" expands array items, and a
// leaf array of strings is expanded implicitly. Any other shape is invalid.
func outputStrings(value any, path []string) ([]string, bool) {
	current := []any{value}
	for _, segment := range path {
		next := make([]any, 0, len(current))
		for _, v := range current {
			if segment == workflow.WaitOutputPathWildcard {
				items, ok := v.([]any)
				if !ok {
					return nil, false
				}
				next = append(next, items...)
				continue
			}
			object, ok := v.(map[string]any)
			if !ok {
				return nil, false
			}
			child, exists := object[segment]
			if !exists {
				return nil, false
			}
			next = append(next, child)
		}
		current = next
	}
	out := make([]string, 0, len(current))
	for _, v := range current {
		switch typed := v.(type) {
		case string:
			out = append(out, typed)
		case []any:
			for _, item := range typed {
				s, ok := item.(string)
				if !ok {
					return nil, false
				}
				out = append(out, s)
			}
		default:
			return nil, false
		}
	}
	return out, true
}

// recordWaitProgressTx reports whether the completion rule is met and, when an
// event was just matched, appends customer-safe progress.
func (s *Store) recordWaitProgressTx(ctx context.Context, tx pgx.Tx, itemID, attemptID string, node NodeExecution, completion string, expected int, announce bool) (bool, error) {
	var received int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.wait_expectations WHERE node_execution_id=$1 AND state='received'`, node.ID).Scan(&received); err != nil {
		return false, err
	}
	if announce {
		if err := appendTimelineTx(ctx, tx, itemID, attemptID, node.ID, "wait_event_received",
			map[string]any{"nodeKey": node.NodeKey, "received": received, "expected": expected}, nil, ""); err != nil {
			return false, err
		}
	}
	if completion == workflow.WaitCompletionAny {
		return received >= 1, nil
	}
	return received >= expected, nil
}

// completeWaitTx ends a waiting visit with one reserved outcome, records the
// collected events as its immutable output, and follows the declared route in
// the same attempt. The caller holds the wait row lock.
//
//nolint:gocyclo // Collecting evidence, closing expectations, and routing are one atomic completion.
func (s *Store) completeWaitTx(ctx context.Context, tx pgx.Tx, itemID, attemptID string, node NodeExecution, outcome, actorID string) (string, error) {
	rows, err := tx.Query(ctx, `
		SELECT x.source, x.correlation_key, x.state, e.id::text, e.received_at, e.payload, e.artifact_refs
		FROM runtime.wait_expectations x
		LEFT JOIN runtime.external_events e ON e.id=x.event_id
		WHERE x.node_execution_id=$1 ORDER BY x.ordinal`, node.ID)
	if err != nil {
		return "", err
	}
	type eventOut struct {
		Source         string          `json:"source"`
		CorrelationKey string          `json:"correlationKey"`
		EventID        string          `json:"eventId"`
		ReceivedAt     time.Time       `json:"receivedAt"`
		Payload        json.RawMessage `json:"payload"`
		ArtifactRefs   []ArtifactRef   `json:"artifactRefs"`
	}
	type missingOut struct {
		Source         string `json:"source"`
		CorrelationKey string `json:"correlationKey"`
	}
	events := make([]eventOut, 0)
	missing := make([]missingOut, 0)
	artifacts := make([]ArtifactRef, 0)
	expected := 0
	for rows.Next() {
		expected++
		var source, key, state string
		var eventID *string
		var receivedAt *time.Time
		var payload, refs []byte
		if err := rows.Scan(&source, &key, &state, &eventID, &receivedAt, &payload, &refs); err != nil {
			rows.Close()
			return "", err
		}
		if state != "received" || eventID == nil {
			missing = append(missing, missingOut{Source: source, CorrelationKey: key})
			continue
		}
		var eventRefs []ArtifactRef
		if err := json.Unmarshal(refs, &eventRefs); err != nil {
			rows.Close()
			return "", err
		}
		for i := range eventRefs {
			eventRefs[i].Role = "evidence"
		}
		artifacts = append(artifacts, eventRefs...)
		events = append(events, eventOut{Source: source, CorrelationKey: key, EventID: *eventID, ReceivedAt: *receivedAt,
			Payload: payload, ArtifactRefs: append([]ArtifactRef{}, eventRefs...)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE runtime.wait_expectations SET state='closed' WHERE node_execution_id=$1 AND state='pending'`, node.ID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE runtime.waits SET outcome=$2, completed_at=now(), stopped_by=$3 WHERE node_execution_id=$1 AND completed_at IS NULL`,
		node.ID, outcome, nullable(actorID)); err != nil {
		return "", err
	}
	output, _ := json.Marshal(map[string]any{"expected": expected, "received": len(events), "events": events, "missing": missing})
	refsJSON, _ := json.Marshal(artifacts)
	summary := map[string]string{
		workflow.WaitOutcomeReceived:  "Expected events received",
		workflow.WaitOutcomeDeadline:  "Deadline reached",
		workflow.WaitOutcomeCancelled: "Stopped waiting",
	}[outcome]
	if _, err := tx.Exec(ctx, `
		UPDATE runtime.node_executions SET state='succeeded', completion_outcome=$2, completion_summary=$3,
		       output=$4, artifact_refs=$5, completion_reported_at=now(), finished_at=now()
		WHERE id=$1 AND state='waiting'`, node.ID, outcome, summary, output, refsJSON); err != nil {
		return "", err
	}
	for _, ref := range artifacts {
		if _, err := tx.Exec(ctx, `
			INSERT INTO work.artifact_links (artifact_id,work_item_id,attempt_id,node_execution_id,role,metadata)
			SELECT id,$2,$3,$4,'evidence',$5 FROM artifact.artifacts WHERE id=$1 AND state='available'
			ON CONFLICT (artifact_id,work_item_id,attempt_id,node_execution_id,role) DO NOTHING`,
			ref.ArtifactID, itemID, attemptID, node.ID, jsonOrObject(ref.Metadata)); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE runtime.workflow_runs SET state='running' WHERE id=$1 AND state='waiting'`, attemptID); err != nil {
		return "", err
	}
	if err := appendRuntimeEventTx(ctx, tx, attemptID, "", node.ID, runtimestore.EvWaitCompleted,
		map[string]any{"outcome": outcome, "expected": expected, "received": len(events)}); err != nil {
		return "", err
	}
	node.State = NodeSucceeded
	node.CompletionOutcome = &outcome
	node.CompletionSummary = &summary
	node.Output = output
	node.ArtifactRefs = refsJSON
	return s.advanceGraphTx(ctx, tx, graphAdvanceInput{ItemID: itemID, AttemptID: attemptID, Node: node, ActorID: actorID})
}

// lockOpenWaitTx locks one wait visit and returns its waiting node, item, and
// definition when the wait can still complete. ok=false means the wait has
// already completed or its attempt is no longer active.
func (s *Store) lockOpenWaitTx(ctx context.Context, tx pgx.Tx, nodeID string, skipLocked bool) (NodeExecution, string, string, bool, error) {
	lock := `FOR UPDATE`
	if skipLocked {
		lock = `FOR UPDATE SKIP LOCKED`
	}
	var completion string
	var completed *time.Time
	err := tx.QueryRow(ctx, `SELECT completion, completed_at FROM runtime.waits WHERE node_execution_id=$1 `+lock, nodeID).Scan(&completion, &completed)
	if errors.Is(err, pgx.ErrNoRows) {
		return NodeExecution{}, "", "", false, nil
	}
	if err != nil || completed != nil {
		return NodeExecution{}, "", "", false, err
	}
	node, err := scanNodeExecution(tx.QueryRow(ctx, nodeExecutionSelect+` WHERE id=$1 FOR UPDATE`, nodeID))
	if err != nil {
		return NodeExecution{}, "", "", false, err
	}
	var itemID, runState string
	if err := tx.QueryRow(ctx, `
		SELECT a.work_item_id::text, wr.state FROM work.attempts a JOIN runtime.workflow_runs wr ON wr.id=a.id
		WHERE a.id=$1 FOR UPDATE OF wr`, node.AttemptID).Scan(&itemID, &runState); err != nil {
		return NodeExecution{}, "", "", false, err
	}
	if node.State != NodeWaiting || runState != runtimestore.RunWaiting {
		return NodeExecution{}, "", "", false, nil
	}
	return node, itemID, completion, true, nil
}

// PostExternalEvent records one event from an event source and, when it
// matches a pending expected event, advances that wait in the same attempt.
// A repeated (source, eventKey) is ignored. An event for a wait that already
// completed, or whose attempt stopped, is kept as late evidence only.
//
//nolint:gocyclo // Validation, idempotency, matching, and completion are one transaction.
func (s *Store) PostExternalEvent(ctx context.Context, in ExternalEventInput) (ExternalEventResult, error) {
	if !eventSourcePattern.MatchString(in.Source) {
		return ExternalEventResult{}, fmt.Errorf("%w: event source is invalid", ErrInvalidInput)
	}
	if in.EventKey == "" || len(in.EventKey) > 512 || in.CorrelationKey == "" || len(in.CorrelationKey) > 512 {
		return ExternalEventResult{}, fmt.Errorf("%w: event key and correlation key require 1-512 bytes", ErrInvalidInput)
	}
	if len(in.Payload) == 0 {
		in.Payload = json.RawMessage(`{}`)
	}
	if len(in.Payload) > maxEventPayloadBytes || !json.Valid(in.Payload) {
		return ExternalEventResult{}, fmt.Errorf("%w: payload must be valid JSON of at most %d bytes", ErrInvalidInput, maxEventPayloadBytes)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ExternalEventResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for i := range in.ArtifactRefs {
		ref := &in.ArtifactRefs[i]
		if ref.ArtifactID == "" || (len(ref.Metadata) > 0 && !json.Valid(ref.Metadata)) {
			return ExternalEventResult{}, fmt.Errorf("%w: artifactRefs[%d] is invalid", ErrInvalidInput, i)
		}
		ref.Role = "evidence"
		var available bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM artifact.artifacts WHERE id=$1 AND state='available')`, ref.ArtifactID).Scan(&available); err != nil {
			return ExternalEventResult{}, err
		}
		if !available {
			return ExternalEventResult{}, fmt.Errorf("%w: artifactRefs[%d] is not available", ErrInvalidInput, i)
		}
	}
	if err := lockCorrelationKeyTx(ctx, tx, in.Source, in.CorrelationKey); err != nil {
		return ExternalEventResult{}, err
	}
	if existing, ok, err := existingEventTx(ctx, tx, in.Source, in.EventKey); err != nil || ok {
		return existing, err
	}

	disposition := EventUnmatched
	var node NodeExecution
	var itemID, completion string
	var matched bool
	var waitNodeID string
	err = tx.QueryRow(ctx, `
		SELECT node_execution_id::text FROM runtime.wait_expectations
		WHERE source=$1 AND correlation_key=$2 AND state='pending'`, in.Source, in.CorrelationKey).Scan(&waitNodeID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return ExternalEventResult{}, err
	default:
		node, itemID, completion, matched, err = s.lockOpenWaitTx(ctx, tx, waitNodeID, false)
		if err != nil {
			return ExternalEventResult{}, err
		}
	}
	if matched {
		// Re-check under the wait lock: a concurrent post may have claimed it.
		var stillPending bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM runtime.wait_expectations
			WHERE node_execution_id=$1 AND source=$2 AND correlation_key=$3 AND state='pending')`,
			node.ID, in.Source, in.CorrelationKey).Scan(&stillPending); err != nil {
			return ExternalEventResult{}, err
		}
		matched = stillPending
	}
	if matched {
		disposition = EventMatched
	} else {
		var known bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runtime.wait_expectations WHERE source=$1 AND correlation_key=$2)`,
			in.Source, in.CorrelationKey).Scan(&known); err != nil {
			return ExternalEventResult{}, err
		}
		if known {
			disposition = EventLate
		}
	}
	refsJSON, _ := json.Marshal(append([]ArtifactRef{}, in.ArtifactRefs...))
	var eventID string
	var receivedAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO runtime.external_events (source, event_key, correlation_key, payload, artifact_refs, disposition)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (source, event_key) DO NOTHING
		RETURNING id::text, received_at`, in.Source, in.EventKey, in.CorrelationKey, in.Payload, refsJSON, disposition).Scan(&eventID, &receivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent post of the same event won; report it as the duplicate.
		_ = tx.Rollback(ctx)
		existing, _, err := existingEventTx(ctx, s.pool, in.Source, in.EventKey)
		return existing, err
	}
	if err != nil {
		return ExternalEventResult{}, err
	}
	result := ExternalEventResult{EventID: eventID, Disposition: disposition}
	if matched {
		result.WorkItemID = itemID
		if _, err := tx.Exec(ctx, `
			UPDATE runtime.wait_expectations SET state='received', event_id=$4, received_at=$5
			WHERE node_execution_id=$1 AND source=$2 AND correlation_key=$3 AND state='pending'`,
			node.ID, in.Source, in.CorrelationKey, eventID, receivedAt); err != nil {
			return ExternalEventResult{}, err
		}
		var expected int
		if err := tx.QueryRow(ctx, `SELECT expected_count FROM runtime.waits WHERE node_execution_id=$1`, node.ID).Scan(&expected); err != nil {
			return ExternalEventResult{}, err
		}
		if err := appendRuntimeEventTx(ctx, tx, node.AttemptID, "", node.ID, runtimestore.EvWaitEventReceived,
			map[string]any{"source": in.Source, "eventId": eventID}); err != nil {
			return ExternalEventResult{}, err
		}
		done, err := s.recordWaitProgressTx(ctx, tx, itemID, node.AttemptID, node, completion, expected, true)
		if err != nil {
			return ExternalEventResult{}, err
		}
		if done {
			if _, err := s.completeWaitTx(ctx, tx, itemID, node.AttemptID, node, workflow.WaitOutcomeReceived, ""); err != nil {
				return ExternalEventResult{}, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ExternalEventResult{}, err
	}
	return result, nil
}

// lockCorrelationKeyTx serializes event arrival and wait entry for one
// (source, correlation key). Without it a post and an entry could each commit
// without seeing the other, leaving the event unmatched and unclaimed. Callers
// that lock several keys take them in expectation order; no caller requests a
// key lock while holding a wait row lock, and wait entry takes its keys in
// sorted order.
func lockCorrelationKeyTx(ctx context.Context, tx pgx.Tx, source, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('wait-key' || chr(10) || $1 || chr(10) || $2, 0))`, source, key)
	return err
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func existingEventTx(ctx context.Context, q rowQuerier, source, eventKey string) (ExternalEventResult, bool, error) {
	var out ExternalEventResult
	err := q.QueryRow(ctx, `
		SELECT e.id::text, e.disposition, COALESCE(a.work_item_id::text,'')
		FROM runtime.external_events e
		LEFT JOIN runtime.wait_expectations x ON x.event_id=e.id
		LEFT JOIN work.attempts a ON a.id=x.attempt_id
		WHERE e.source=$1 AND e.event_key=$2`, source, eventKey).Scan(&out.EventID, &out.Disposition, &out.WorkItemID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExternalEventResult{}, false, nil
	}
	if err != nil {
		return ExternalEventResult{}, false, err
	}
	out.Duplicate = true
	return out, true, nil
}

// CompleteDueWaits routes every open wait whose deadline is at or before now
// through its `deadline` outcome, carrying whatever arrived. Waits locked by a
// concurrent event or Operator command are left for the next pass. It is safe
// to run from several replicas.
func (s *Store) CompleteDueWaits(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT node_execution_id::text FROM runtime.waits
		WHERE completed_at IS NULL AND deadline_at <= $1
		ORDER BY deadline_at, node_execution_id LIMIT $2`, now, limit)
	if err != nil {
		return 0, err
	}
	var due []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	completed := 0
	for _, id := range due {
		ok, err := s.completeDueWait(ctx, id, now)
		if err != nil {
			return completed, fmt.Errorf("complete due wait %s: %w", id, err)
		}
		if ok {
			completed++
		}
	}
	return completed, nil
}

func (s *Store) completeDueWait(ctx context.Context, nodeID string, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	node, itemID, _, ok, err := s.lockOpenWaitTx(ctx, tx, nodeID, true)
	if err != nil || !ok {
		return false, err
	}
	var deadline time.Time
	if err := tx.QueryRow(ctx, `SELECT deadline_at FROM runtime.waits WHERE node_execution_id=$1`, nodeID).Scan(&deadline); err != nil {
		return false, err
	}
	if deadline.After(now) {
		return false, nil
	}
	if _, err := s.completeWaitTx(ctx, tx, itemID, node.AttemptID, node, workflow.WaitOutcomeDeadline, ""); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// StopWaiting ends the current attempt's open wait through its `cancelled`
// outcome on an Operator/Admin's request, carrying whatever arrived. It does
// not cancel the work item; the workflow decides what happens next.
func (s *Store) StopWaiting(ctx context.Context, in StopWaitingInput) (WorkItem, error) {
	if in.WorkItemID == "" || in.ActorIdentityID == "" {
		return WorkItem{}, fmt.Errorf("%w: item and actor are required", ErrInvalidInput)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WorkItem{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var nodeID string
	err = tx.QueryRow(ctx, `
		SELECT w.node_execution_id::text FROM runtime.waits w
		JOIN work.work_items wi ON wi.current_attempt_id=w.attempt_id
		WHERE wi.id=$1 AND w.completed_at IS NULL`, in.WorkItemID).Scan(&nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM work.work_items WHERE id=$1)`, in.WorkItemID).Scan(&exists); err != nil {
			return WorkItem{}, err
		}
		if !exists {
			return WorkItem{}, ErrNotFound
		}
		return WorkItem{}, fmt.Errorf("%w: work item is not waiting", ErrInvalidTransition)
	}
	if err != nil {
		return WorkItem{}, err
	}
	node, itemID, _, ok, err := s.lockOpenWaitTx(ctx, tx, nodeID, false)
	if err != nil {
		return WorkItem{}, err
	}
	if !ok {
		return WorkItem{}, fmt.Errorf("%w: work item is no longer waiting", ErrInvalidTransition)
	}
	if err := appendTimelineTx(ctx, tx, itemID, node.AttemptID, node.ID, "wait_stopped",
		map[string]any{"nodeKey": node.NodeKey}, nil, in.ActorIdentityID); err != nil {
		return WorkItem{}, err
	}
	if _, err := s.completeWaitTx(ctx, tx, itemID, node.AttemptID, node, workflow.WaitOutcomeCancelled, in.ActorIdentityID); err != nil {
		return WorkItem{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkItem{}, err
	}
	return s.GetWorkItem(ctx, in.WorkItemID)
}
