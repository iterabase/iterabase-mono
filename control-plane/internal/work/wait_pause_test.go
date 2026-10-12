package work_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/gateway"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
	workstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/work"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/workflow"
)

// waitEnv is a database seeded with the DES-HOR-649-02 fan-out workflow:
// send (agent, records reply tokens) -> collect (wait) -> decide (agent).
type waitEnv struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	store   *workstore.Store
	actorID string
	poolID  string
}

func newWaitEnv(t *testing.T, completion string) *waitEnv {
	t.Helper()
	pool := testutil.NewPostgresPool(t)
	ctx := context.Background()
	actorID, scopeID, poolID := seedFoundation(t, ctx, pool)
	label := func(en string) workflow.CanonicalLocalizedText {
		return workflow.CanonicalLocalizedText{EN: en, PT: en + " (pt)"}
	}
	spec := workflow.CanonicalSpec{
		Key: "dev/quotes", ScopeIdentityKey: "workflow:default/walter", PoolRef: "pool", DefaultModelRef: "model-one",
		Source:                workflow.CanonicalSource{Type: "manual_api"},
		RequestedCapabilities: []workflow.CanonicalCapability{{Tool: "graph.read", MaxEffectClass: "read_only"}, {Tool: "graph.excel.write", MaxEffectClass: "idempotent_write"}},
		Graph: workflow.CanonicalGraph{EntryNode: "send", MaxTransitions: 20,
			Nodes: []workflow.CanonicalNode{
				{Key: "send", Label: label("Send requests"), Kind: workflow.NodeAgentTask, Prompt: "send",
					Capabilities: []string{"graph.read", "graph.excel.write"}, Outcomes: []string{"sent"},
					OutputSchema: json.RawMessage(`{"type":"object","properties":{"tokens":{"type":"array","items":{"type":"string"}}}}`)},
				{Key: "collect", Label: label("Wait for quotes"), Kind: workflow.NodeWait, Outcomes: append([]string(nil), workflow.WaitOutcomes...),
					Wait: &workflow.CanonicalWait{
						Events:     []workflow.CanonicalWaitEvent{{Source: "email_reply", Correlation: workflow.CanonicalOutputReference{Node: "send", Path: []string{"tokens"}}}},
						Completion: completion,
						Deadline:   workflow.CanonicalWaitDeadline{After: "72h"},
					}},
				{Key: "decide", Label: label("Decide"), Kind: workflow.NodeAgentTask, Prompt: "decide", Outcomes: []string{"done"},
					ResultPresentation: terminalResult("done", "Decided", "Decidido")},
			},
			Edges: []workflow.CanonicalEdge{
				{From: "send", Outcome: "sent", To: "collect"},
				{From: "collect", Outcome: workflow.WaitOutcomeReceived, To: "decide"},
				{From: "collect", Outcome: workflow.WaitOutcomeDeadline, To: "decide"},
				{From: "collect", Outcome: workflow.WaitOutcomeCancelled, To: "decide"},
			},
			TerminalOutcomes: []workflow.CanonicalTerminalOutcome{{Node: "decide", Outcome: "done"}},
		},
		Presentation: workflow.CanonicalPresentation{WorkflowTitle: "Quotes", PersonaName: "Marco", Locale: "en"},
	}
	require.NoError(t, workflow.ValidateGraph(spec))
	specJSON, _ := json.Marshal(spec)
	presentation, _ := json.Marshal(spec.Presentation)
	def, err := workflow.NewStore(pool).RegisterDefinition(ctx, workflow.Definition{Key: "dev/quotes", Version: "1", Digest: "sha256:quotes-v1",
		SpecJSON: specJSON, ValidationStatus: workflow.ValidationValid, ScopeIdentityID: scopeID, SourceType: "manual_api",
		PoolKey: "default/pool", Presentation: presentation})
	require.NoError(t, err)
	caps, _ := json.Marshal(spec.RequestedCapabilities)
	_, err = pool.Exec(ctx, `INSERT INTO toolgateway.workflow_pool_bindings(workflow_definition_key,pool_id,permitted_tools)VALUES($1,$2,$3)`,
		workflow.DefinitionKey(def.Key, def.Version), poolID, caps)
	require.NoError(t, err)
	return &waitEnv{ctx: ctx, pool: pool, store: workstore.NewStore(pool), actorID: actorID, poolID: poolID}
}

func (e *waitEnv) start(t *testing.T, key string) workstore.WorkItem {
	t.Helper()
	item, created, err := e.store.Start(e.ctx, workstore.StartInput{ActorIdentityID: e.actorID, WorkflowKey: "dev/quotes",
		IdempotencyKey: key, Title: "Quotes " + key, Source: json.RawMessage(`{}`),
		SourcePresentation: workstore.SourcePresentation{Kind: "api", Title: "Quotes"}})
	require.NoError(t, err)
	require.True(t, created)
	return item
}

// runAgent prepares the attempt's pending agent node and completes it.
func (e *waitEnv) runAgent(t *testing.T, attemptID, nodeKey, outcome, output string) string {
	t.Helper()
	node, turn, dispatch, err := e.store.PrepareNode(e.ctx, attemptID)
	require.NoError(t, err)
	require.True(t, dispatch, "agent node %s must need a worker", nodeKey)
	require.Equal(t, nodeKey, node.NodeKey)
	require.NoError(t, e.store.RecordCompletionReport(e.ctx, turn.ID, workstore.CompletionReport{
		Outcome: outcome, Summary: nodeKey + " done", Output: json.RawMessage(output)}))
	state, err := e.store.CompleteTurn(e.ctx, turn.ID, "completed", nil, nil)
	require.NoError(t, err)
	return state
}

// enterWait completes send with tokens and enters the wait.
func (e *waitEnv) enterWait(t *testing.T, item workstore.WorkItem, tokens ...string) workstore.NodeExecution {
	t.Helper()
	out, _ := json.Marshal(map[string]any{"tokens": tokens})
	e.runAgent(t, item.CurrentAttemptID, "send", "sent", string(out))
	node, _, dispatch, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	assert.False(t, dispatch, "a wait never needs a worker")
	return node
}

func (e *waitEnv) post(t *testing.T, eventKey, token string) workstore.ExternalEventResult {
	t.Helper()
	res, err := e.store.PostExternalEvent(e.ctx, workstore.ExternalEventInput{Source: "email_reply", EventKey: eventKey,
		CorrelationKey: token, Payload: json.RawMessage(fmt.Sprintf(`{"from":"%s@supplier.example"}`, token))})
	require.NoError(t, err)
	return res
}

func (e *waitEnv) item(t *testing.T, id string) workstore.WorkItem {
	t.Helper()
	item, err := e.store.GetWorkItem(e.ctx, id)
	require.NoError(t, err)
	return item
}

func (e *waitEnv) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(e.ctx, query, args...).Scan(&n))
	return n
}

func (e *waitEnv) timelineCodes(t *testing.T, itemID string) map[string]int {
	t.Helper()
	events, err := e.store.ListTimeline(e.ctx, itemID, 0, 1000)
	require.NoError(t, err)
	out := map[string]int{}
	for _, ev := range events {
		out[ev.Code]++
	}
	return out
}

func waitOutput(t *testing.T, e *waitEnv, attemptID string) (string, map[string]any) {
	t.Helper()
	var outcome string
	var raw []byte
	require.NoError(t, e.pool.QueryRow(e.ctx, `
		SELECT completion_outcome, output FROM runtime.node_executions
		WHERE attempt_id=$1 AND node_key='collect' AND state='succeeded' ORDER BY execution_seq DESC LIMIT 1`, attemptID).Scan(&outcome, &raw))
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return outcome, out
}

func TestWait_AllEventsContinueSameAttemptAndIgnoreDuplicates(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	item := e.start(t, "all")
	wait := e.enterWait(t, item, "t1", "t2", "t3")
	assert.Equal(t, workstore.NodeWaiting, wait.State)

	waiting := e.item(t, item.ID)
	assert.Equal(t, workstore.StateWaiting, waiting.State, "a wait is Waiting, never Blocked")
	require.NotNil(t, waiting.Wait)
	assert.Equal(t, 3, waiting.Wait.Expected)
	assert.Equal(t, 0, waiting.Wait.Received)
	assert.WithinDuration(t, time.Now().Add(72*time.Hour), waiting.Wait.DeadlineAt, time.Minute)
	assert.Zero(t, e.count(t, `SELECT count(*) FROM runtime.turns WHERE node_execution_id=$1`, wait.ID), "no turn or session is held while waiting")
	again, _, dispatch, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	assert.False(t, dispatch)
	assert.Equal(t, wait.ID, again.ID, "repeated scheduling passes leave one wait")

	first := e.post(t, "m1", "t1")
	assert.Equal(t, workstore.EventMatched, first.Disposition)
	assert.Equal(t, item.ID, first.WorkItemID)
	dup := e.post(t, "m1", "t1")
	assert.True(t, dup.Duplicate, "a repeated event key is ignored")
	assert.Equal(t, first.EventID, dup.EventID)
	assert.Equal(t, 1, e.item(t, item.ID).Wait.Received)

	e.post(t, "m2", "t2")
	assert.Equal(t, workstore.StateWaiting, e.item(t, item.ID).State)
	e.post(t, "m3", "t3")

	resumed := e.item(t, item.ID)
	assert.Equal(t, workstore.StateInProgress, resumed.State)
	assert.Equal(t, item.CurrentAttemptID, resumed.CurrentAttemptID, "the same attempt continues")
	outcome, out := waitOutput(t, e, item.CurrentAttemptID)
	assert.Equal(t, workflow.WaitOutcomeReceived, outcome)
	assert.EqualValues(t, 3, out["received"])
	require.Len(t, out["events"], 3)
	assert.Empty(t, out["missing"])

	decide, err := e.store.ActiveNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	assert.Equal(t, "decide", decide.NodeKey)
	var context struct {
		Previous struct {
			NodeKey string         `json:"nodeKey"`
			Outcome string         `json:"outcome"`
			Output  map[string]any `json:"output"`
		} `json:"previous"`
	}
	require.NoError(t, json.Unmarshal(decide.Context, &context))
	assert.Equal(t, "collect", context.Previous.NodeKey)
	assert.Len(t, context.Previous.Output["events"], 3, "collected events are the next node's input")

	assert.Equal(t, "succeeded", e.runAgent(t, item.CurrentAttemptID, "decide", "done", `{}`))
	assert.Equal(t, workstore.StateDone, e.item(t, item.ID).State)
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM work.attempts WHERE work_item_id=$1`, item.ID))
	codes := e.timelineCodes(t, item.ID)
	assert.Equal(t, 1, codes["work_waiting"])
	assert.Equal(t, 3, codes["wait_event_received"])

	late := e.post(t, "m4", "t3")
	assert.Equal(t, workstore.EventLate, late.Disposition, "an event after completion is evidence only")
	assert.Equal(t, workstore.StateDone, e.item(t, item.ID).State)
}

func TestWait_DeadlineRoutesWithPartialEvents(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	item := e.start(t, "deadline")
	e.enterWait(t, item, "t1", "t2", "t3")
	e.post(t, "m1", "t1")
	e.post(t, "m2", "t2")

	completed, err := e.store.CompleteDueWaits(e.ctx, time.Now(), 10)
	require.NoError(t, err)
	assert.Zero(t, completed, "nothing is due before the deadline")
	completed, err = e.store.CompleteDueWaits(e.ctx, time.Now().Add(73*time.Hour), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, completed)
	completed, err = e.store.CompleteDueWaits(e.ctx, time.Now().Add(73*time.Hour), 10)
	require.NoError(t, err)
	assert.Zero(t, completed, "a repeated sweep never completes a wait twice")

	outcome, out := waitOutput(t, e, item.CurrentAttemptID)
	assert.Equal(t, workflow.WaitOutcomeDeadline, outcome)
	assert.EqualValues(t, 2, out["received"])
	require.Len(t, out["missing"], 1)
	assert.Equal(t, "t3", out["missing"].([]any)[0].(map[string]any)["correlationKey"])
	assert.Equal(t, workstore.StateInProgress, e.item(t, item.ID).State)
	late := e.post(t, "m3", "t3")
	assert.Equal(t, workstore.EventLate, late.Disposition)
}

// An event arriving after the deadline, before the sweep ran, never changes
// the outcome: the wait routes `deadline` with what arrived in time.
func TestWait_EventAfterDeadlineBeforeSweepRoutesDeadline(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	item := e.start(t, "deadline-race")
	node := e.enterWait(t, item, "t1", "t2", "t3")
	e.post(t, "m1", "t1")
	e.post(t, "m2", "t2")
	_, err := e.pool.Exec(e.ctx, `UPDATE runtime.waits SET deadline_at=now()-interval '1 second' WHERE node_execution_id=$1`, node.ID)
	require.NoError(t, err)

	late := e.post(t, "m3", "t3")
	assert.Equal(t, workstore.EventLate, late.Disposition, "a post-deadline event is evidence only")
	outcome, out := waitOutput(t, e, item.CurrentAttemptID)
	assert.Equal(t, workflow.WaitOutcomeDeadline, outcome)
	assert.EqualValues(t, 2, out["received"])
	require.Len(t, out["missing"], 1)
	assert.Equal(t, "t3", out["missing"].([]any)[0].(map[string]any)["correlationKey"])
	completed, err := e.store.CompleteDueWaits(e.ctx, time.Now().Add(73*time.Hour), 10)
	require.NoError(t, err)
	assert.Zero(t, completed, "the arrival already completed the wait")
}

// Late evidence for a stopped wait is never claimed by a later wait that
// reuses the correlation key.
func TestWait_LateEvidenceIsNotClaimedByALaterWait(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	first := e.start(t, "late-first")
	e.enterWait(t, first, "k")
	_, err := e.store.StopWaiting(e.ctx, workstore.StopWaitingInput{WorkItemID: first.ID, ActorIdentityID: e.actorID})
	require.NoError(t, err)
	assert.Equal(t, workstore.EventLate, e.post(t, "late-1", "k").Disposition)

	second := e.start(t, "late-second")
	e.enterWait(t, second, "k")
	assert.Equal(t, workstore.StateWaiting, e.item(t, second.ID).State, "the old late event does not complete the new wait")
	assert.Zero(t, e.count(t, `SELECT count(*) FROM runtime.wait_expectations WHERE attempt_id=$1 AND state='received'`, second.CurrentAttemptID))
	assert.Equal(t, workstore.EventMatched, e.post(t, "fresh-1", "k").Disposition, "a new event for the key still matches")
}

func TestWait_StopWaitingRoutesCancelledAndLaterEventsAreEvidence(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	item := e.start(t, "stop")
	e.enterWait(t, item, "t1", "t2")
	e.post(t, "m1", "t1")

	stopped, err := e.store.StopWaiting(e.ctx, workstore.StopWaitingInput{WorkItemID: item.ID, ActorIdentityID: e.actorID})
	require.NoError(t, err)
	assert.Equal(t, workstore.StateInProgress, stopped.State)
	outcome, out := waitOutput(t, e, item.CurrentAttemptID)
	assert.Equal(t, workflow.WaitOutcomeCancelled, outcome)
	assert.EqualValues(t, 1, out["received"])
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM runtime.waits WHERE attempt_id=$1 AND stopped_by=$2`, item.CurrentAttemptID, e.actorID))

	_, err = e.store.StopWaiting(e.ctx, workstore.StopWaitingInput{WorkItemID: item.ID, ActorIdentityID: e.actorID})
	assert.ErrorIs(t, err, workstore.ErrInvalidTransition, "a second stop is rejected")
	late := e.post(t, "m2", "t2")
	assert.Equal(t, workstore.EventLate, late.Disposition)
	assert.Zero(t, e.count(t, `SELECT count(*) FROM runtime.wait_expectations WHERE attempt_id=$1 AND state='pending'`, item.CurrentAttemptID))
	assert.Equal(t, 1, e.timelineCodes(t, item.ID)["wait_stopped"])
}

func TestWait_EventBeforeWaitIsClaimedAndAnyCompletesImmediately(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAny)
	item := e.start(t, "early")
	early := e.post(t, "m1", "t2")
	assert.Equal(t, workstore.EventUnmatched, early.Disposition)

	out, _ := json.Marshal(map[string]any{"tokens": []string{"t1", "t2"}})
	e.runAgent(t, item.CurrentAttemptID, "send", "sent", string(out))
	_, _, _, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)

	outcome, collected := waitOutput(t, e, item.CurrentAttemptID)
	assert.Equal(t, workflow.WaitOutcomeReceived, outcome, "an event that arrived before the wait is never lost")
	assert.EqualValues(t, 1, collected["received"])
	assert.Equal(t, workstore.StateInProgress, e.item(t, item.ID).State)
}

func TestWait_InvalidCorrelationOutputFailsClosed(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	for name, tokens := range map[string][]string{"none": {}, "duplicate": {"t1", "t1"}, "empty": {""}} {
		item := e.start(t, "invalid-"+name)
		out, _ := json.Marshal(map[string]any{"tokens": tokens})
		e.runAgent(t, item.CurrentAttemptID, "send", "sent", string(out))
		_, _, dispatch, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
		require.NoError(t, err)
		assert.False(t, dispatch)
		failed := e.item(t, item.ID)
		assert.Equal(t, workstore.StateFailed, failed.State, name)
		assert.JSONEq(t, `{"code":"invalid_wait_input","message":"The workflow could not start waiting because an earlier step did not produce valid references to wait for."}`,
			string(failed.FailureSummary), name)
	}
	// A key already awaited by other work cannot be awaited twice.
	first := e.start(t, "owner")
	e.enterWait(t, first, "shared")
	second := e.start(t, "contender")
	e.enterWait(t, second, "shared")
	assert.Equal(t, workstore.StateWaiting, e.item(t, first.ID).State)
	assert.Equal(t, workstore.StateFailed, e.item(t, second.ID).State)
}

func TestWait_ConcurrentEventsCompleteOnce(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	item := e.start(t, "race")
	tokens := []string{"t1", "t2", "t3", "t4", "t5", "t6"}
	e.enterWait(t, item, tokens...)
	var wg sync.WaitGroup
	errs := make(chan error, len(tokens)*2)
	for i, token := range tokens {
		for copy := 0; copy < 2; copy++ {
			wg.Add(1)
			go func(i int, token string) {
				defer wg.Done()
				_, err := e.store.PostExternalEvent(e.ctx, workstore.ExternalEventInput{Source: "email_reply",
					EventKey: fmt.Sprintf("m%d", i), CorrelationKey: token})
				errs <- err
			}(i, token)
		}
	}
	// The deadline sweep races the arrivals too.
	_, sweepErr := e.store.CompleteDueWaits(e.ctx, time.Now().Add(73*time.Hour), 10)
	wg.Wait()
	close(errs)
	require.NoError(t, sweepErr)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, len(tokens), e.count(t, `SELECT count(*) FROM runtime.external_events`), "duplicates are ignored")
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM runtime.node_executions WHERE attempt_id=$1 AND node_key='decide'`, item.CurrentAttemptID),
		"the wait completes and routes exactly once")
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM runtime.graph_transitions t JOIN runtime.node_executions n ON n.id=t.from_execution_id WHERE n.node_key='collect' AND n.attempt_id=$1`, item.CurrentAttemptID))
}

func TestWait_RestartAtEveryBoundaryLeavesOneHistory(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	item := e.start(t, "restart")
	out, _ := json.Marshal(map[string]any{"tokens": []string{"t1", "t2"}})
	e.runAgent(t, item.CurrentAttemptID, "send", "sent", string(out))
	// Each step runs against a fresh store and repeats every reconcile pass,
	// as a restarted process would.
	restart := func() {
		e.store = workstore.NewStore(e.pool)
		for range 2 {
			_, _, _, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
			require.NoError(t, err)
			_, err = e.store.CompleteDueWaits(e.ctx, time.Now(), 10)
			require.NoError(t, err)
			require.NoError(t, e.store.ReconcileRunnerPauseConditions(e.ctx))
		}
	}
	restart()
	e.post(t, "m1", "t1")
	restart()
	e.post(t, "m1", "t1")
	e.post(t, "m2", "t2")
	restart()
	codes := e.timelineCodes(t, item.ID)
	assert.Equal(t, 1, codes["work_waiting"])
	assert.Equal(t, 2, codes["wait_event_received"])
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM runtime.waits WHERE attempt_id=$1`, item.CurrentAttemptID))
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM runtime.node_executions WHERE attempt_id=$1 AND node_key='decide'`, item.CurrentAttemptID))
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM runtime.turns t JOIN runtime.node_executions n ON n.id=t.node_execution_id WHERE n.attempt_id=$1 AND n.node_key='decide'`, item.CurrentAttemptID),
		"the next agent step got exactly one turn")
}

func TestPause_InstallationCauseHoldsAtStepBoundaryAndResumes(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	require.NoError(t, e.store.RaisePauseCondition(e.ctx, workstore.PauseTrialEnded, workstore.PauseScopeInstallation, "plan"))
	require.NoError(t, e.store.RaisePauseCondition(e.ctx, workstore.PauseTrialEnded, workstore.PauseScopeInstallation, "plan"), "raise is idempotent")
	item := e.start(t, "trial")
	for range 3 {
		_, _, dispatch, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
		require.NoError(t, err)
		assert.False(t, dispatch, "a held node gets no turn")
	}
	paused := e.item(t, item.ID)
	assert.Equal(t, workstore.StatePaused, paused.State, "paused is never Blocked")
	require.NotNil(t, paused.Pause)
	assert.Equal(t, workstore.PauseTrialEnded, paused.Pause.Reason)
	assert.Zero(t, e.count(t, `SELECT count(*) FROM runtime.turns WHERE run_id=$1`, item.CurrentAttemptID))
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM work.attempt_pauses WHERE attempt_id=$1`, item.CurrentAttemptID))

	// A provider outage also matches; the higher-precedence trial reason stays.
	require.NoError(t, e.store.RaisePauseCondition(e.ctx, workstore.PauseProviderUnavailable, workstore.PauseScopeModel("model-one"), "inference"))
	_, _, _, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	assert.Equal(t, workstore.PauseTrialEnded, e.item(t, item.ID).Pause.Reason)
	// Converting the trial reveals the remaining provider reason.
	require.NoError(t, e.store.ClearPauseCondition(e.ctx, workstore.PauseTrialEnded, workstore.PauseScopeInstallation))
	_, _, _, err = e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	assert.Equal(t, workstore.PauseProviderUnavailable, e.item(t, item.ID).Pause.Reason)

	require.NoError(t, e.store.ClearPauseCondition(e.ctx, workstore.PauseProviderUnavailable, workstore.PauseScopeModel("model-one")))
	_, _, dispatch, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	assert.True(t, dispatch, "the step starts once every cause clears")
	resumed := e.item(t, item.ID)
	assert.Equal(t, workstore.StateInProgress, resumed.State)
	assert.Nil(t, resumed.Pause)
	codes := e.timelineCodes(t, item.ID)
	assert.Equal(t, 2, codes["work_paused"])
	assert.Equal(t, 1, codes["work_resumed"])
	assert.Zero(t, e.count(t, `SELECT count(*) FROM work.attempt_pauses WHERE attempt_id=$1 AND resumed_at IS NULL`, item.CurrentAttemptID))
}

func TestPause_RunningStepFinishesAndNextStepPauses(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	item := e.start(t, "boundary")
	_, turn, dispatch, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	require.True(t, dispatch)
	require.NoError(t, e.store.RaisePauseCondition(e.ctx, workstore.PauseTrialEnded, workstore.PauseScopeInstallation, "plan"))
	assert.Equal(t, workstore.StateInProgress, e.item(t, item.ID).State, "a running step is not interrupted by a pause")
	require.NoError(t, e.store.RecordCompletionReport(e.ctx, turn.ID, workstore.CompletionReport{Outcome: "sent", Summary: "sent", Output: json.RawMessage(`{"tokens":["t1"]}`)}))
	_, err = e.store.CompleteTurn(e.ctx, turn.ID, "completed", nil, nil)
	require.NoError(t, err)

	// While paused before the wait, a reply arrives; it is kept and claimed
	// on resume, so pausing never loses an event.
	_, _, _, err = e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	assert.Equal(t, workstore.StatePaused, e.item(t, item.ID).State)
	assert.Equal(t, workstore.EventUnmatched, e.post(t, "m1", "t1").Disposition)
	require.NoError(t, e.store.ClearPauseCondition(e.ctx, workstore.PauseTrialEnded, workstore.PauseScopeInstallation))
	_, _, _, err = e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	outcome, _ := waitOutput(t, e, item.CurrentAttemptID)
	assert.Equal(t, workflow.WaitOutcomeReceived, outcome)
}

func TestPause_RunnerOfflineDerivedFromHeartbeats(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	item := e.start(t, "runner")
	_, err := e.pool.Exec(e.ctx, `UPDATE toolgateway.runner_registrations SET last_heartbeat_at=now()-interval '5 minutes' WHERE tool_name='graph.read'`)
	require.NoError(t, err)
	require.NoError(t, e.store.ReconcileRunnerPauseConditions(e.ctx))
	conditions, err := e.store.ListPauseConditions(e.ctx, false)
	require.NoError(t, err)
	require.Len(t, conditions, 1)
	assert.Equal(t, workstore.PauseRunnerOffline, conditions[0].Cause)
	assert.Equal(t, workstore.PauseScopeToolVersion("graph.read", "sha256:read-v1"), conditions[0].Scope)

	_, _, dispatch, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	assert.False(t, dispatch)
	paused := e.item(t, item.ID)
	assert.Equal(t, workstore.StatePaused, paused.State)
	assert.Equal(t, workstore.PauseRunnerOffline, paused.Pause.Reason)

	_, err = e.pool.Exec(e.ctx, `UPDATE toolgateway.runner_registrations SET last_heartbeat_at=now() WHERE tool_name='graph.read'`)
	require.NoError(t, err)
	require.NoError(t, e.store.ReconcileRunnerPauseConditions(e.ctx))
	conditions, err = e.store.ListPauseConditions(e.ctx, false)
	require.NoError(t, err)
	assert.Empty(t, conditions, "the condition clears when the runner heartbeats again")
	_, _, dispatch, err = e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	assert.True(t, dispatch, "work resumes automatically")
}

func TestPause_RejectsUnknownCauseAndScope(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	assert.ErrorIs(t, e.store.RaisePauseCondition(e.ctx, "maintenance", workstore.PauseScopeInstallation, "x"), workstore.ErrInvalidInput)
	assert.ErrorIs(t, e.store.RaisePauseCondition(e.ctx, workstore.PauseTrialEnded, "pool:a", "x"), workstore.ErrInvalidInput)
	assert.ErrorIs(t, e.store.RaisePauseCondition(e.ctx, workstore.PauseTrialEnded, "tool:", "x"), workstore.ErrInvalidInput)
}

func (e *waitEnv) insertInvocation(t *testing.T, attemptID, turnID, effect, state string) {
	t.Helper()
	tool, digest, summary := "graph.read", "sha256:read-v1", any(nil)
	if effect != "read_only" {
		tool, digest, summary = "graph.excel.write", "sha256:excel-v1", `{"en":"Update workbook","pt":"Atualizar livro"}`
	}
	_, err := e.pool.Exec(e.ctx, `
		INSERT INTO toolgateway.invocations
		(attempt_id,caller_scope,caller_scope_id,tool_call_id,tool_name,tool_version_digest,idempotency_key,effect_class,pool_id,consequence_summary,state)
		VALUES($1,'turn',$2,$3,$4,$5,$3,$6,$7,$8,$9)`, attemptID, turnID, "call-"+turnID+"-"+effect, tool, digest, effect, e.poolID, summary, state)
	require.NoError(t, err)
}

func TestInterrupt_ReadOnlyTurnRerunsInFreshSessionAndCompletes(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	item := e.start(t, "read-only")
	lost, turn, _, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	e.insertInvocation(t, item.CurrentAttemptID, turn.ID, "read_only", "succeeded")

	state, err := e.store.InterruptTurn(e.ctx, turn.ID, workstore.InterruptWorkerLost)
	require.NoError(t, err)
	assert.Equal(t, "running", state)
	_, err = e.store.InterruptTurn(e.ctx, turn.ID, workstore.InterruptWorkerLost)
	assert.ErrorIs(t, err, workstore.ErrInvalidTransition, "an interruption is settled once")

	// The lost turn's late report and outcome cannot advance anything.
	assert.Error(t, e.store.RecordCompletionReport(e.ctx, turn.ID, workstore.CompletionReport{Outcome: "sent", Summary: "late", Output: json.RawMessage(`{"tokens":["x"]}`)}))
	_, err = e.store.CompleteTurn(e.ctx, turn.ID, "completed", nil, nil)
	assert.ErrorIs(t, err, workstore.ErrInvalidTransition)

	rerun, rerunTurn, dispatch, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	require.True(t, dispatch)
	assert.Equal(t, "send", rerun.NodeKey)
	assert.NotEqual(t, lost.ID, rerun.ID, "a rerun is a new visit, never a replay")
	assert.Equal(t, lost.Visit+1, rerun.Visit)
	assert.JSONEq(t, string(lost.Context), string(rerun.Context), "the step reruns from its start")
	assert.NotEqual(t, turn.SessionID, rerunTurn.SessionID, "the rerun uses a fresh session")
	var decision, lostSession, rerunSession string
	require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT decision, lost_session_id, rerun_session_id FROM runtime.turn_interruptions WHERE turn_id=$1`, turn.ID).
		Scan(&decision, &lostSession, &rerunSession))
	assert.Equal(t, "rerun", decision)
	assert.Equal(t, turn.SessionID, lostSession)
	assert.Equal(t, rerunTurn.SessionID, rerunSession)
	assert.Equal(t, 1, e.timelineCodes(t, item.ID)["step_interrupted"])

	require.NoError(t, e.store.RecordCompletionReport(e.ctx, rerunTurn.ID, workstore.CompletionReport{Outcome: "sent", Summary: "sent", Output: json.RawMessage(`{"tokens":["t1"]}`)}))
	_, err = e.store.CompleteTurn(e.ctx, rerunTurn.ID, "completed", nil, nil)
	require.NoError(t, err)
	_, _, _, err = e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	assert.Equal(t, workstore.StateWaiting, e.item(t, item.ID).State, "the rerun completed and the workflow continued")
}

func TestInterrupt_TurnWithAnyWriteStopsWithoutRepeating(t *testing.T) {
	for _, state := range []string{"succeeded", "failed", "outcome_unknown"} {
		t.Run(state, func(t *testing.T) {
			e := newWaitEnv(t, workflow.WaitCompletionAll)
			item := e.start(t, "write-"+state)
			_, turn, _, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
			require.NoError(t, err)
			e.insertInvocation(t, item.CurrentAttemptID, turn.ID, "idempotent_write", state)

			runState, err := e.store.InterruptTurn(e.ctx, turn.ID, workstore.InterruptWorkerLost)
			require.NoError(t, err)
			assert.Equal(t, "failed", runState)
			stopped := e.item(t, item.ID)
			assert.Equal(t, workstore.StateFailed, stopped.State)
			assert.JSONEq(t, `{"code":"interrupted_after_write","message":"A step was interrupted after it had taken external actions. Review those actions and restart the work to continue."}`,
				string(stopped.FailureSummary))
			assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM runtime.node_executions WHERE attempt_id=$1`, item.CurrentAttemptID), "no rerun visit")
			assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM toolgateway.invocations WHERE attempt_id=$1`, item.CurrentAttemptID), "no write is repeated")
			var writes []string
			var raw []byte
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT write_invocation_ids FROM runtime.turn_interruptions WHERE turn_id=$1 AND decision='stop_after_write'`, turn.ID).Scan(&raw))
			require.NoError(t, json.Unmarshal(raw, &writes))
			assert.Len(t, writes, 1, "the ledger evidence that decided the stop is retained")
		})
	}
}

// beginWrite starts a write-class invocation through the real gateway ledger
// entry point for the given turn.
func (e *waitEnv) beginWrite(attemptID, turnID string) error {
	pool := e.poolID
	_, _, err := gateway.NewStore(e.pool).BeginInvocation(e.ctx, gateway.InvocationKey{
		AttemptID: attemptID, CallerScope: gateway.CallerScopeTurn, CallerScopeID: turnID,
		ToolCallID: "call-race", ToolVersionDigest: "sha256:excel-v1", IdempotencyKey: "race",
	}, gateway.ToolVersion{Name: "graph.excel.write", Digest: "sha256:excel-v1", EffectClass: gateway.EffectIdempotentWrite},
		&pool, []byte(`{}`), []byte(`[]`), []byte(`{"en":"Update workbook","pt":"Atualizar livro"}`), time.Now().Add(time.Minute), "gw-test")
	return err
}

// The classifier and write-invocation creation are serialized on the turn row
// (DES-HOR-649-04): whichever commits first decides, and a write can never
// commit after a rerun was chosen.
func TestInterrupt_SerializedWithWriteInvocationCreation(t *testing.T) {
	t.Run("write wins", func(t *testing.T) {
		e := newWaitEnv(t, workflow.WaitCompletionAll)
		item := e.start(t, "race-write")
		_, turn, _, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
		require.NoError(t, err)
		// An in-flight invocation holds the fence it takes before inserting.
		tx, err := e.pool.Begin(e.ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(e.ctx) }()
		_, err = tx.Exec(e.ctx, `SELECT 1 FROM runtime.turns WHERE id=$1 FOR SHARE`, turn.ID)
		require.NoError(t, err)

		result := make(chan string, 1)
		go func() {
			state, err := e.store.InterruptTurn(e.ctx, turn.ID, workstore.InterruptWorkerLost)
			if err != nil {
				state = "error: " + err.Error()
			}
			result <- state
		}()
		select {
		case state := <-result:
			t.Fatalf("classifier did not wait for the in-flight invocation: %s", state)
		case <-time.After(300 * time.Millisecond):
		}
		_, err = tx.Exec(e.ctx, `
			INSERT INTO toolgateway.invocations
			(attempt_id,caller_scope,caller_scope_id,tool_call_id,tool_name,tool_version_digest,idempotency_key,effect_class,pool_id,consequence_summary,state)
			VALUES($1,'turn',$2,'call-race','graph.excel.write','sha256:excel-v1','race','idempotent_write',$3,'{"en":"x","pt":"x"}','dispatching')`,
			item.CurrentAttemptID, turn.ID, e.poolID)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(e.ctx))
		assert.Equal(t, "failed", <-result, "the committed write stops the attempt")
		assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM runtime.turn_interruptions WHERE turn_id=$1 AND decision='stop_after_write'`, turn.ID))
	})

	t.Run("classifier wins", func(t *testing.T) {
		e := newWaitEnv(t, workflow.WaitCompletionAll)
		item := e.start(t, "race-rerun")
		_, turn, _, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
		require.NoError(t, err)
		// The classifier holds the turn while it reads the ledger.
		tx, err := e.pool.Begin(e.ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(e.ctx) }()
		_, err = tx.Exec(e.ctx, `SELECT 1 FROM runtime.turns WHERE id=$1 FOR UPDATE`, turn.ID)
		require.NoError(t, err)

		result := make(chan error, 1)
		go func() { result <- e.beginWrite(item.CurrentAttemptID, turn.ID) }()
		select {
		case err := <-result:
			t.Fatalf("invocation did not wait for the classifier: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		_, err = tx.Exec(e.ctx, `UPDATE runtime.turns SET state='aborted', settled_at=now() WHERE id=$1`, turn.ID)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(e.ctx))
		assert.ErrorIs(t, <-result, gateway.ErrScopeDenied, "a write is refused once the turn is settled")
		assert.Equal(t, 0, e.count(t, `SELECT count(*) FROM toolgateway.invocations WHERE caller_scope_id=$1`, turn.ID))
	})

	t.Run("sequential orders", func(t *testing.T) {
		e := newWaitEnv(t, workflow.WaitCompletionAll)
		item := e.start(t, "race-seq")
		_, turn, _, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
		require.NoError(t, err)
		state, err := e.store.InterruptTurn(e.ctx, turn.ID, workstore.InterruptWorkerLost)
		require.NoError(t, err)
		assert.Equal(t, "running", state)
		assert.ErrorIs(t, e.beginWrite(item.CurrentAttemptID, turn.ID), gateway.ErrScopeDenied, "the old turn cannot write after a rerun")

		_, rerun, _, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
		require.NoError(t, err)
		require.NoError(t, e.beginWrite(item.CurrentAttemptID, rerun.ID))
		state, err = e.store.InterruptTurn(e.ctx, rerun.ID, workstore.InterruptWorkerLost)
		require.NoError(t, err)
		assert.Equal(t, "failed", state, "a write committed first stops the attempt")
	})
}

func TestInterrupt_RepeatedInterruptionStopsAfterThree(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	item := e.start(t, "repeated")
	var state string
	for i := 1; i <= workstore.MaxConsecutiveInterruptions; i++ {
		_, turn, dispatch, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
		require.NoError(t, err)
		require.True(t, dispatch, "interruption %d", i)
		state, err = e.store.InterruptTurn(e.ctx, turn.ID, workstore.InterruptWorkerLost)
		require.NoError(t, err)
	}
	assert.Equal(t, "failed", state)
	stopped := e.item(t, item.ID)
	assert.Equal(t, workstore.StateFailed, stopped.State)
	assert.Contains(t, string(stopped.FailureSummary), workstore.StopRepeatedInterruption)
	assert.Equal(t, 3, e.count(t, `SELECT count(*) FROM runtime.node_executions WHERE attempt_id=$1 AND state='interrupted'`, item.CurrentAttemptID))
	assert.Equal(t, 2, e.count(t, `SELECT count(*) FROM runtime.turn_interruptions WHERE attempt_id=$1 AND decision='rerun'`, item.CurrentAttemptID))
}

func TestInterrupt_ProviderCauseRerunWaitsForThePauseToClear(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	item := e.start(t, "provider")
	_, turn, _, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	// The provider owner raises its condition, then interrupts the turn.
	require.NoError(t, e.store.RaisePauseCondition(e.ctx, workstore.PauseProviderUnavailable, workstore.PauseScopeModel("model-one"), "inference"))
	_, err = e.store.InterruptTurn(e.ctx, turn.ID, workstore.InterruptProviderUnavailable)
	require.NoError(t, err)
	_, _, dispatch, err := e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	assert.False(t, dispatch)
	assert.Equal(t, workstore.StatePaused, e.item(t, item.ID).State)
	require.NoError(t, e.store.ClearPauseCondition(e.ctx, workstore.PauseProviderUnavailable, workstore.PauseScopeModel("model-one")))
	_, _, dispatch, err = e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	assert.True(t, dispatch)
	_, err = e.store.InterruptTurn(e.ctx, turn.ID, "bogus")
	assert.ErrorIs(t, err, workstore.ErrInvalidInput)
}

func TestWaitPause_HistoryTablesAreImmutable(t *testing.T) {
	e := newWaitEnv(t, workflow.WaitCompletionAll)
	item := e.start(t, "immutable")
	e.enterWait(t, item, "t1")
	e.post(t, "m1", "t1")
	_, err := e.pool.Exec(e.ctx, `UPDATE runtime.external_events SET payload='{}'`)
	assert.ErrorContains(t, err, "append-only")
	_, err = e.pool.Exec(e.ctx, `DELETE FROM runtime.external_events`)
	assert.ErrorContains(t, err, "append-only")

	require.NoError(t, e.store.RaisePauseCondition(e.ctx, workstore.PauseTrialEnded, workstore.PauseScopeInstallation, "plan"))
	_, _, _, err = e.store.PrepareNode(e.ctx, item.CurrentAttemptID)
	require.NoError(t, err)
	_, err = e.pool.Exec(e.ctx, `UPDATE work.attempt_pauses SET cause='runner_offline'`)
	assert.ErrorContains(t, err, "only records resumed_at once")
	_, err = e.pool.Exec(e.ctx, `DELETE FROM work.attempt_pauses`)
	assert.ErrorContains(t, err, "append-only")
}
