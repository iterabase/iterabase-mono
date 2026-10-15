package gateway_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	artifactstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/artifacttest"
	v1 "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/gatewayrpc/iterabase/gateway/v1"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/gatewayrpc/iterabase/gateway/v1/gatewayv1connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// admitClearedInput admits a cleared input artifact linked to the node.
func admitClearedInput(t *testing.T, env *testEnv, creator, workItemID, runID, nodeID, body string) artifactstore.Artifact {
	t.Helper()
	a, err := artifacttest.AdmitCleared(context.Background(), env.pgpool, env.artifacts, env.objects, artifactstore.AdmitInput{
		SourceType: artifactstore.SourceUpload, Attribution: artifactstore.Attribution{RequestActorID: creator},
		DisplayName: "input.txt", ClaimedMIME: "text/plain", ContainerDepth: 1,
		Scope: &artifactstore.Scope{WorkItemID: workItemID, AttemptID: runID, NodeExecutionID: nodeID, Role: "input"},
	}, []byte(body))
	require.NoError(t, err)
	require.True(t, a.Cleared())
	return a
}

func artifactClient(env *testEnv, cert tls.Certificate) gatewayv1connect.ArtifactServiceClient {
	return gatewayv1connect.NewArtifactServiceClient(mTLSClient(cert, env.caPool), env.srvURL, connect.WithGRPC())
}

func TestArtifactRPCSupervisorScopeAndStreaming(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	run1, turn1, work1, node1, creator1 := seedWorkAttempt(t, env)
	run2, turn2, _, _, _ := seedWorkAttempt(t, env)

	input := admitClearedInput(t, env, creator1, work1, run1, node1, "authorized input")

	client := artifactClient(env, env.supervisor)
	context1 := &v1.ArtifactCallerContext{AttemptId: run1, CallerScope: v1.CallerScope_CALLER_SCOPE_TURN, CallerScopeId: turn1, FencingGeneration: 1}
	stream, err := client.GetArtifact(ctx, connect.NewRequest(&v1.GetArtifactRequest{Context: context1, ArtifactId: input.ID}))
	require.NoError(t, err)
	var got bytes.Buffer
	metadata := false
	for stream.Receive() {
		msg := stream.Msg()
		if msg.GetMetadata() != nil {
			metadata = true
		}
		got.Write(msg.GetChunk())
	}
	require.NoError(t, stream.Err())
	assert.True(t, metadata)
	assert.Equal(t, "authorized input", got.String())

	// A valid supervisor assignment for another run cannot use a learned id.
	_, err = client.StatArtifact(ctx, connect.NewRequest(&v1.StatArtifactRequest{Context: &v1.ArtifactCallerContext{
		AttemptId: run2, CallerScope: v1.CallerScope_CALLER_SCOPE_TURN, CallerScopeId: turn2, FencingGeneration: 1,
	}, ArtifactId: input.ID}))
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))

	put := client.PutArtifact(ctx)
	require.NoError(t, put.Send(&v1.PutArtifactRequest{Kind: &v1.PutArtifactRequest_Init{Init: &v1.PutArtifactInit{Context: context1, MimeType: "application/json"}}}))
	require.NoError(t, put.Send(&v1.PutArtifactRequest{Kind: &v1.PutArtifactRequest_Chunk{Chunk: []byte(`{"new":true}`)}}))
	resp, err := put.CloseAndReceive()
	require.NoError(t, err)
	ref := resp.Msg.Metadata.Ref
	require.NotNil(t, ref)
	assert.NotEqual(t, input.ID, ref.ArtifactId)
	// DES-HOR-461-02: the output is quarantined, citable by its producer, and
	// unusable by any consumer until cleared.
	assert.Equal(t, artifactstore.StatusCheckingSecurity, resp.Msg.Metadata.State)
	admissible, err := env.artifacts.Store().OwnOutputAdmissible(ctx, ref.ArtifactId, run1)
	require.NoError(t, err)
	assert.True(t, admissible)
	linked, err := env.artifacts.Store().LinkedToAttempt(ctx, ref.ArtifactId, run1, node1)
	require.NoError(t, err)
	assert.False(t, linked, "a quarantined output is not readable through the workload boundary")
	_, err = client.StatArtifact(ctx, connect.NewRequest(&v1.StatArtifactRequest{Context: context1, ArtifactId: ref.ArtifactId}))
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))

	require.NoError(t, artifacttest.Drain(ctx, artifacttest.NewCoordinator(env.pgpool, env.objects, &artifacttest.Scanner{})))
	linked, err = env.artifacts.Store().LinkedToAttempt(ctx, ref.ArtifactId, run1, node1)
	require.NoError(t, err)
	assert.True(t, linked, "the cleared output becomes readable")
}

func TestArtifactRPCRunnerReadsCommittedInputAndCannotWriteWithoutDescriptorCapability(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	var runnerRead string
	var runnerPutCode connect.Code
	done := make(chan struct{}, 1)
	desc := echoDescriptor()
	desc.ArtifactCapabilities = &v1.ArtifactCapabilities{ReadsArtifacts: true, WritesArtifacts: false, AcceptedMimeTypes: []string{"text/plain"}}
	runner := startRefRunner(t, env, desc, func(inv *v1.Invoke) (*v1.InvokeResult, bool) {
		client := artifactClient(env, env.runner)
		stream, err := client.GetArtifact(ctx, connect.NewRequest(&v1.GetArtifactRequest{
			Context:    &v1.ArtifactCallerContext{InvocationId: inv.InvocationId},
			ArtifactId: inv.ArtifactInputRefs[0].ArtifactId,
		}))
		if err == nil {
			var body bytes.Buffer
			for stream.Receive() {
				body.Write(stream.Msg().GetChunk())
			}
			if stream.Err() == nil {
				runnerRead = body.String()
			}
		}
		put := client.PutArtifact(ctx)
		_ = put.Send(&v1.PutArtifactRequest{Kind: &v1.PutArtifactRequest_Init{Init: &v1.PutArtifactInit{
			Context: &v1.ArtifactCallerContext{InvocationId: inv.InvocationId}, MimeType: "text/plain",
		}}})
		_, putErr := put.CloseAndReceive()
		runnerPutCode = connect.CodeOf(putErr)
		done <- struct{}{}
		return &v1.InvokeResult{State: v1.InvokeState_INVOKE_STATE_SUCCEEDED, ResultJson: []byte(`{"ok":true}`)}, false
	})
	defer runner.close()

	runID, turnID, workItemID, nodeID, creatorID := seedWorkAttempt(t, env)
	_, err := env.pgpool.Exec(ctx, `UPDATE runtime.turn_assignments SET capability_request=$2 WHERE turn_id=$1`, turnID, []byte(`[{"tool":"echo","maxEffectClass":"read_only"}]`))
	require.NoError(t, err)
	require.NoError(t, env.store.SnapshotAttemptTools(ctx, runID, env.poolID, nil))
	input := admitClearedInput(t, env, creatorID, workItemID, runID, nodeID, "runner-readable")
	ref, err := input.Ref()
	require.NoError(t, err)

	resp, err := gatewayClient(env, env.supervisor).InvokeTool(ctx, connect.NewRequest(&v1.InvokeRequest{
		AttemptId: runID, CallerScope: v1.CallerScope_CALLER_SCOPE_TURN, CallerScopeId: turnID, FencingGeneration: 1,
		ToolCallId: "artifact-read", ToolName: desc.Name, ToolVersionDigest: desc.Digest, ArgumentsJson: []byte(`{}`),
		ArtifactInputRefs: []*v1.ArtifactRef{{ArtifactId: ref.ArtifactID, MimeType: ref.MIMEType, SizeBytes: ref.SizeBytes, Digest: ref.Digest}},
	}))
	require.NoError(t, err)
	assert.Equal(t, v1.InvokeState_INVOKE_STATE_SUCCEEDED, resp.Msg.State)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not execute artifact read/write checks")
	}
	assert.Equal(t, "runner-readable", runnerRead, "runner can read the exact input committed on its live invocation")
	assert.Equal(t, connect.CodePermissionDenied, runnerPutCode, "read-only descriptor cannot persist artifact outputs")
}

// TestArtifactRPCQuarantinedToolOutputIsNotOutcomeUnknown proves
// DES-HOR-461-02 at the gateway: a write tool whose output is still being
// scanned completes normally, and a rejected output is refused.
func TestArtifactRPCQuarantinedToolOutputIsNotOutcomeUnknown(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	desc := echoDescriptor()
	desc.ArtifactCapabilities = &v1.ArtifactCapabilities{WritesArtifacts: true}
	runner := startRefRunner(t, env, desc, func(inv *v1.Invoke) (*v1.InvokeResult, bool) {
		put := artifactClient(env, env.runner).PutArtifact(ctx)
		_ = put.Send(&v1.PutArtifactRequest{Kind: &v1.PutArtifactRequest_Init{Init: &v1.PutArtifactInit{
			Context: &v1.ArtifactCallerContext{InvocationId: inv.InvocationId}, MimeType: "text/csv",
		}}})
		_ = put.Send(&v1.PutArtifactRequest{Kind: &v1.PutArtifactRequest_Chunk{Chunk: []byte("a,b\n1,2\n")}})
		resp, err := put.CloseAndReceive()
		if err != nil {
			return &v1.InvokeResult{State: v1.InvokeState_INVOKE_STATE_FAILED}, false
		}
		return &v1.InvokeResult{State: v1.InvokeState_INVOKE_STATE_SUCCEEDED, ResultJson: []byte(`{}`),
			ArtifactOutputRefs: []*v1.ArtifactRef{resp.Msg.Metadata.Ref}}, false
	})
	defer runner.close()
	runID, turnID, _, _, _ := seedWorkAttempt(t, env)
	_, err := env.pgpool.Exec(ctx, `UPDATE runtime.turn_assignments SET capability_request=$2 WHERE turn_id=$1`, turnID, []byte(`[{"tool":"echo","maxEffectClass":"read_only"}]`))
	require.NoError(t, err)
	require.NoError(t, env.store.SnapshotAttemptTools(ctx, runID, env.poolID, nil))
	resp, err := gatewayClient(env, env.supervisor).InvokeTool(ctx, connect.NewRequest(&v1.InvokeRequest{
		AttemptId: runID, CallerScope: v1.CallerScope_CALLER_SCOPE_TURN, CallerScopeId: turnID, FencingGeneration: 1,
		ToolCallId: "artifact-write", ToolName: desc.Name, ToolVersionDigest: desc.Digest, ArgumentsJson: []byte(`{}`),
	}))
	require.NoError(t, err)
	assert.Equal(t, v1.InvokeState_INVOKE_STATE_SUCCEEDED, resp.Msg.State, "a pending scan is not an ambiguous outcome")
	require.Len(t, resp.Msg.ArtifactOutputRefs, 1)
	assert.Equal(t, "text/csv", resp.Msg.ArtifactOutputRefs[0].MimeType)
}

func seedWorkAttempt(t *testing.T, env *testEnv) (runID, turnID, workItemID, nodeID, identityID string) {
	t.Helper()
	ctx := context.Background()
	sid := shortID()
	require.NoError(t, env.pgpool.QueryRow(ctx, `INSERT INTO identity.identities (key,kind,source,display_name) VALUES ($1,'workflow','local',$1) RETURNING id::text`, "artifact-"+sid).Scan(&identityID))
	var definitionID string
	require.NoError(t, env.pgpool.QueryRow(ctx, `
		INSERT INTO workflow.definitions (key,version,digest,spec_json,scope_identity_id,source_type,pool_key)
		VALUES ($1,'1','sha256:test','{}',$2,'operator_artifact',$3) RETURNING id::text`, "artifact-"+sid, identityID, poolKey).Scan(&definitionID))
	require.NoError(t, env.pgpool.QueryRow(ctx, `
		INSERT INTO runtime.workflow_runs (kind,definition_key,scope_identity_id,session_id,session_dir,state,started_at)
		VALUES ('workflow',$1,$2,$3,$4,'running',now()) RETURNING id::text`, "artifact-"+sid+":1", identityID, sid, "/sessions/"+sid).Scan(&runID))
	require.NoError(t, env.store.UpsertRunPoolAssignment(ctx, runID, env.poolID))
	require.NoError(t, env.pgpool.QueryRow(ctx, `
		INSERT INTO work.work_items (workflow_key,scope_identity_id,title,start_identity_id,start_idempotency_key,start_payload_hash)
		VALUES ($1,$2,'Artifact work',$2,$3,$3) RETURNING id::text`, "artifact-"+sid, identityID, sid).Scan(&workItemID))
	_, err := env.pgpool.Exec(ctx, `
		INSERT INTO work.attempts (id,work_item_id,number,definition_id,definition_key,definition_version,definition_digest,graph_snapshot)
		VALUES ($1,$2,1,$3,$4,'1','sha256:test','{}')`, runID, workItemID, definitionID, "artifact-"+sid+":1")
	require.NoError(t, err)
	_, err = env.pgpool.Exec(ctx, `UPDATE work.work_items SET current_attempt_id=$1 WHERE id=$2`, runID, workItemID)
	require.NoError(t, err)
	require.NoError(t, env.pgpool.QueryRow(ctx, `
		INSERT INTO runtime.node_executions (attempt_id,node_key,visit,execution_seq,kind,state)
		VALUES ($1,'node',1,1,'agent_task','running') RETURNING id::text`, runID).Scan(&nodeID))
	require.NoError(t, env.pgpool.QueryRow(ctx, `
		INSERT INTO runtime.turns (run_id,session_id,node_execution_id,state,started_at)
		VALUES ($1,$2,$3,'running',now()) RETURNING id::text`, runID, sid, nodeID).Scan(&turnID))
	_, err = env.pgpool.Exec(ctx, `
		INSERT INTO runtime.turn_assignments
		(turn_id,run_id,pool_id,worker_id,fencing_generation,attempt_id,scope_identity_id,agent_pool_key,work_item_id,node_execution_id,state)
		VALUES ($1::uuid,$2::uuid,$3::uuid,'worker-1',1,$2::text,$4::uuid,$5,$6::uuid,$7::uuid,'active')`, turnID, runID, env.poolID, identityID, poolKey, workItemID, nodeID)
	require.NoError(t, err)
	return
}
