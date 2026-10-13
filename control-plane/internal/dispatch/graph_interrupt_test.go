package dispatch_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/harnessrpc/iterabase/harness/v1"
	cpmetrics "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/metrics"
	workstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/work"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/workflow"
)

// seedGraphWaitItem registers send (agent) -> collect (wait) -> decide (agent)
// on the dispatch test pool and starts one work item.
func seedGraphWaitItem(t *testing.T, env *dispatchEnv) workstore.WorkItem {
	t.Helper()
	ctx := context.Background()
	actorID := insertIdentity(t, env.pgpool, "user/graph-operator")
	var scopeID string
	require.NoError(t, env.pgpool.QueryRow(ctx, `
		INSERT INTO identity.identities(key,kind,source,display_name)
		VALUES('workflow:ns/quotes','workflow','local','Quotes') RETURNING id::text`).Scan(&scopeID))
	_, err := env.pgpool.Exec(ctx, `INSERT INTO catalog.backends(key,name,namespace,kind,model,service_url,image,deployed,healthy)
		VALUES('ns/backend','backend','ns','vLLM','model/base','http://model','image',true,true)`)
	require.NoError(t, err)
	_, err = env.pgpool.Exec(ctx, `INSERT INTO catalog.models(key,namespace,model_id,display_name,context_length,backend_ref,available)
		VALUES('ns/model-one','ns','model-one','Model One',32768,'backend',true)`)
	require.NoError(t, err)
	label := func(en string) workflow.CanonicalLocalizedText {
		return workflow.CanonicalLocalizedText{EN: en, PT: en}
	}
	result := &workflow.CanonicalResultPresentation{Outcomes: []workflow.CanonicalResultOutcomePresentation{{Outcome: "done", Summary: label("Done")}}}
	spec := workflow.CanonicalSpec{
		Key: "ns/quotes", ScopeIdentityKey: "workflow:ns/quotes", PoolRef: "pool-1", DefaultModelRef: "model-one",
		Source: workflow.CanonicalSource{Type: "manual_api"},
		Graph: workflow.CanonicalGraph{EntryNode: "send", MaxTransitions: 10,
			Nodes: []workflow.CanonicalNode{
				{Key: "send", Label: label("Send"), Kind: workflow.NodeAgentTask, Prompt: "send", Outcomes: []string{"sent"},
					OutputSchema: json.RawMessage(`{"type":"object","properties":{"token":{"type":"string"}}}`)},
				{Key: "collect", Label: label("Collect"), Kind: workflow.NodeWait, Outcomes: append([]string(nil), workflow.WaitOutcomes...),
					Wait: &workflow.CanonicalWait{Completion: workflow.WaitCompletionAll, Deadline: workflow.CanonicalWaitDeadline{After: "1h"},
						Events: []workflow.CanonicalWaitEvent{{Source: "email_reply", Correlation: workflow.CanonicalOutputReference{Node: "send", Path: []string{"token"}}}}}},
				{Key: "decide", Label: label("Decide"), Kind: workflow.NodeAgentTask, Prompt: "decide", Outcomes: []string{"done"}, ResultPresentation: result},
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
	def, err := workflow.NewStore(env.pgpool).RegisterDefinition(ctx, workflow.Definition{Key: "ns/quotes", Version: "1", Digest: "sha256:quotes",
		SpecJSON: specJSON, ValidationStatus: workflow.ValidationValid, ScopeIdentityID: scopeID, SourceType: "manual_api",
		PoolKey: "ns/pool-1", Presentation: presentation})
	require.NoError(t, err)
	_, err = env.pgpool.Exec(ctx, `INSERT INTO toolgateway.workflow_pool_bindings(workflow_definition_key,pool_id,permitted_tools)VALUES($1,$2,'[]')`,
		workflow.DefinitionKey(def.Key, def.Version), env.poolID)
	require.NoError(t, err)
	item, _, err := workstore.NewStore(env.pgpool).Start(ctx, workstore.StartInput{ActorIdentityID: actorID, WorkflowKey: "ns/quotes",
		IdempotencyKey: "graph-1", Title: "Quotes", Source: json.RawMessage(`{}`),
		SourcePresentation: workstore.SourcePresentation{Kind: "api", Title: "Quotes"}})
	require.NoError(t, err)
	return item
}

func completeGraphTurn(t *testing.T, w *fakeWorker, turnID, outcome, output string) {
	t.Helper()
	require.NoError(t, w.sendEvent(&v1.TurnEvent{TurnId: turnID, Sequence: 1, Kind: &v1.TurnEvent_StepCompletion{StepCompletion: &v1.StepCompletion{
		Outcome: outcome, Summary: outcome, OutputJson: output}}}))
	w.recvControl(t, func(c *v1.ControlMessage) bool { return c.GetEventAck().GetThroughSequence() == 1 })
	require.NoError(t, w.sendEvent(&v1.TurnEvent{TurnId: turnID, Sequence: 2,
		Kind: &v1.TurnEvent_WorkerOutcome{WorkerOutcome: &v1.WorkerOutcome{Outcome: v1.Outcome_OUTCOME_COMPLETED}}}))
	w.recvControl(t, func(c *v1.ControlMessage) bool { return c.GetEventAck().GetThroughSequence() == 2 })
}

// TestDispatch_GraphWorkerLossRerunsReadOnlyStepAndWaitHoldsNoWorker kills a
// worker mid-turn on a read-only step: the step reruns as a new visit in a
// fresh session on the replacement worker and completes. The following wait
// holds no worker (pool metrics show it idle) and the attempt continues when
// the event arrives.
func TestDispatch_GraphWorkerLossRerunsReadOnlyStepAndWaitHoldsNoWorker(t *testing.T) {
	// Configure startup-only metrics before the reconciler can read them.
	env := newDispatchEnvWithReconciler(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	metrics := cpmetrics.New("dispatch", "test", "test")
	env.svc.SetMetrics(metrics)
	env.svc.StartReconciler(ctx)
	item := seedGraphWaitItem(t, env)
	work := workstore.NewStore(env.pgpool)

	w1 := env.connectWorker(t)
	require.NoError(t, w1.ready())
	lost := w1.recvControl(t, func(c *v1.ControlMessage) bool { return c.GetAssignTurn() != nil }).GetAssignTurn()
	assert.Equal(t, "send", lost.GetNodeKey())
	w1.close() // worker killed mid-turn

	w2 := env.connectWorker(t)
	defer w2.close()
	require.NoError(t, w2.ready())
	rerun := w2.recvControl(t, func(c *v1.ControlMessage) bool { return c.GetAssignTurn() != nil }).GetAssignTurn()
	assert.Equal(t, "send", rerun.GetNodeKey())
	assert.NotEqual(t, lost.GetTurnId(), rerun.GetTurnId(), "the lost turn is never redelivered")
	assert.NotEqual(t, lost.GetNodeExecutionId(), rerun.GetNodeExecutionId(), "the rerun is a new visit")
	assert.NotEqual(t, lost.GetSessionId(), rerun.GetSessionId(), "the rerun uses a fresh session")
	assert.NotEqual(t, lost.GetSandbox().GetUid(), rerun.GetSandbox().GetUid(), "the lost session's UID stays allocated")
	assert.Equal(t, item.CurrentAttemptID, rerun.GetRunId(), "the same attempt continues")

	completeGraphTurn(t, w2, rerun.GetTurnId(), "sent", `{"token":"reply-1"}`)
	require.Eventually(t, func() bool {
		current, err := work.GetWorkItem(ctx, item.ID)
		return err == nil && current.State == workstore.StateWaiting
	}, 5*time.Second, 20*time.Millisecond, "the item did not enter its wait")

	// The worker advertises credit; the waiting attempt must not consume it.
	require.NoError(t, w2.ready())
	require.Eventually(t, func() bool {
		return promtest.ToFloat64(metrics.DispatchWorkers.WithLabelValues("busy")) == 0 &&
			promtest.ToFloat64(metrics.DispatchWorkers.WithLabelValues("idle")) == 1
	}, 5*time.Second, 20*time.Millisecond, "a waiting attempt must leave the worker idle")
	var active int
	require.NoError(t, env.pgpool.QueryRow(ctx, `SELECT count(*) FROM runtime.turn_assignments WHERE run_id=$1 AND state='active'`, item.CurrentAttemptID).Scan(&active))
	assert.Zero(t, active)

	res, err := work.PostExternalEvent(ctx, workstore.ExternalEventInput{Source: "email_reply", EventKey: "msg-1", CorrelationKey: "reply-1"})
	require.NoError(t, err)
	assert.Equal(t, workstore.EventMatched, res.Disposition)
	decide := w2.recvControl(t, func(c *v1.ControlMessage) bool { return c.GetAssignTurn() != nil }).GetAssignTurn()
	assert.Equal(t, "decide", decide.GetNodeKey())
	assert.Equal(t, rerun.GetSessionId(), decide.GetSessionId(), "later nodes keep the fresh session")
	completeGraphTurn(t, w2, decide.GetTurnId(), "done", `{}`)
	require.Eventually(t, func() bool {
		current, err := work.GetWorkItem(ctx, item.ID)
		return err == nil && current.State == workstore.StateDone
	}, 5*time.Second, 20*time.Millisecond)
	assert.Equal(t, 1.0, promtest.ToFloat64(metrics.DispatchTurns.WithLabelValues("interrupted", "rerun")))
}

// TestDispatch_GraphWorkerLossAfterWriteStopsAttempt proves a turn that made a
// write invocation is not rerun: the attempt stops and no new turn is assigned.
func TestDispatch_GraphWorkerLossAfterWriteStopsAttempt(t *testing.T) {
	env := newDispatchEnv(t)
	ctx := context.Background()
	item := seedGraphWaitItem(t, env)

	w1 := env.connectWorker(t)
	require.NoError(t, w1.ready())
	at := w1.recvControl(t, func(c *v1.ControlMessage) bool { return c.GetAssignTurn() != nil }).GetAssignTurn()
	_, err := env.pgpool.Exec(ctx, `
		INSERT INTO toolgateway.invocations
		(attempt_id,caller_scope,caller_scope_id,tool_call_id,tool_name,tool_version_digest,idempotency_key,effect_class,consequence_summary,state)
		VALUES($1,'turn',$2,'call-1','crm.update','sha256:crm','call-1','non_idempotent_write','{"en":"Update CRM","pt":"Atualizar CRM"}','outcome_unknown')`,
		item.CurrentAttemptID, at.GetTurnId())
	require.NoError(t, err)
	w1.close()

	work := workstore.NewStore(env.pgpool)
	require.Eventually(t, func() bool {
		current, err := work.GetWorkItem(ctx, item.ID)
		return err == nil && current.State == workstore.StateFailed
	}, 5*time.Second, 20*time.Millisecond, "a turn with a write must stop the attempt")
	current, err := work.GetWorkItem(ctx, item.ID)
	require.NoError(t, err)
	assert.Contains(t, string(current.FailureSummary), workstore.StopInterruptedAfterWrite)

	w2 := env.connectWorker(t)
	defer w2.close()
	require.NoError(t, w2.ready())
	time.Sleep(300 * time.Millisecond) // several reconcile passes
	var turns int
	require.NoError(t, env.pgpool.QueryRow(ctx, `SELECT count(*) FROM runtime.turns WHERE run_id=$1`, item.CurrentAttemptID).Scan(&turns))
	assert.Equal(t, 1, turns, "no step is rerun after a write")
}

// TestDispatch_GraphWorkerDrainAbortIsWorkerLoss covers a pod termination: the
// draining worker aborts its turn and reports ABORTED with abort_reason
// worker_drain over a live stream. That is worker loss, so the read-only step
// reruns in a fresh session instead of failing the attempt.
func TestDispatch_GraphWorkerDrainAbortIsWorkerLoss(t *testing.T) {
	env := newDispatchEnv(t)
	ctx := context.Background()
	item := seedGraphWaitItem(t, env)

	w := env.connectWorker(t)
	defer w.close()
	require.NoError(t, w.ready())
	lost := w.recvControl(t, func(c *v1.ControlMessage) bool { return c.GetAssignTurn() != nil }).GetAssignTurn()
	require.NoError(t, w.sendEvent(&v1.TurnEvent{TurnId: lost.GetTurnId(), Sequence: 1,
		Kind: &v1.TurnEvent_WorkerOutcome{WorkerOutcome: &v1.WorkerOutcome{
			Outcome: v1.Outcome_OUTCOME_ABORTED, Message: "aborted [abort=worker_drain]", AbortReason: "worker_drain"}}}))
	w.recvControl(t, func(c *v1.ControlMessage) bool { return c.GetEventAck().GetThroughSequence() == 1 })

	require.NoError(t, w.ready())
	rerun := w.recvControl(t, func(c *v1.ControlMessage) bool { return c.GetAssignTurn() != nil }).GetAssignTurn()
	assert.Equal(t, "send", rerun.GetNodeKey())
	assert.NotEqual(t, lost.GetSessionId(), rerun.GetSessionId(), "the rerun uses a fresh session")
	var decision string
	require.NoError(t, env.pgpool.QueryRow(ctx, `SELECT decision FROM runtime.turn_interruptions WHERE turn_id=$1`, lost.GetTurnId()).Scan(&decision))
	assert.Equal(t, "rerun", decision)
	current, err := workstore.NewStore(env.pgpool).GetWorkItem(ctx, item.ID)
	require.NoError(t, err)
	assert.Equal(t, workstore.StateInProgress, current.State)
}
