package workflow_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/workflow"
)

func label(en string) workflow.CanonicalLocalizedText {
	return workflow.CanonicalLocalizedText{EN: en, PT: en + " (pt)"}
}

// summaryGraph is entry -> {review (gate), research} in edge declaration
// order, research -> wait-reply -> finish, review loops back to entry.
func summaryGraph() workflow.CanonicalGraph {
	return workflow.CanonicalGraph{
		EntryNode: "intake",
		Nodes: []workflow.CanonicalNode{
			{Key: "finish", Label: label("Finishing"), Kind: workflow.NodeAgentTask, Prompt: "secret finish prompt"},
			{Key: "wait-reply", Label: label("Waiting for reply"), Kind: workflow.NodeWait},
			{Key: "research", Label: label("Researching"), Kind: workflow.NodeAgentTask, Prompt: "secret research prompt", ModelRef: "secret-model"},
			{Key: "review", Label: label("Reviewing"), Kind: workflow.NodeHumanGate, HumanGate: &workflow.CanonicalHumanGate{
				Type: "approval", Title: label("Approve the quote"),
			}},
			{Key: "intake", Label: label("Reading request"), Kind: workflow.NodeAgentTask, Prompt: "secret intake prompt"},
			{Key: "confirm", Label: label("Confirming"), Kind: workflow.NodeHumanGate, HumanGate: &workflow.CanonicalHumanGate{
				Type: "information", Title: label("Confirm the customer"),
			}},
		},
		Edges: []workflow.CanonicalEdge{
			{From: "intake", Outcome: "needs_review", To: "review"},
			{From: "intake", Outcome: "research", To: "research"},
			{From: "review", Outcome: "changes", To: "intake"},
			{From: "review", Outcome: "approved", To: "confirm"},
			{From: "research", Outcome: "sent", To: "wait-reply"},
			{From: "wait-reply", Outcome: "received", To: "finish"},
		},
	}
}

func TestBuildSummaryOrdersStepsBreadthFirstAndDerivesAlwaysAsks(t *testing.T) {
	presentation := workflow.CanonicalPresentation{
		Title: workflow.CanonicalText{EN: "Quotes", PT: "Cotações"},
		Coworker: workflow.CanonicalCoworker{
			Name: "Marco", Mark: workflow.CanonicalCoworkerMark{Shape: "round", Color: "blue"},
			Role: workflow.CanonicalText{EN: "Coordinator", PT: "Coordenador"},
		},
	}
	summary := workflow.BuildSummary("default/quotes", "2", presentation, summaryGraph())

	keys := make([]string, 0, len(summary.Steps))
	for _, step := range summary.Steps {
		keys = append(keys, step.Key)
	}
	assert.Equal(t, []string{"intake", "review", "research", "confirm", "wait-reply", "finish"}, keys)
	assert.Equal(t, "wait", summary.Steps[4].Kind)
	assert.Equal(t, workflow.CanonicalText{EN: "Reviewing", PT: "Reviewing (pt)"}, summary.Steps[1].Label)
	assert.Equal(t, []workflow.SummaryAsk{
		{NodeKey: "review", Type: "approval", Title: workflow.CanonicalText{EN: "Approve the quote", PT: "Approve the quote (pt)"}},
		{NodeKey: "confirm", Type: "information", Title: workflow.CanonicalText{EN: "Confirm the customer", PT: "Confirm the customer (pt)"}},
	}, summary.AlwaysAsks)

	raw, err := json.Marshal(summary)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "secret")
	assert.Contains(t, string(raw), `"onItsOwn":[]`, "absent lists serialize as empty arrays")
	assert.Contains(t, string(raw), `"connections":[]`)
}

func TestReadySummariesUseLatestValidDefinitionPerKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	store, pool := newTestStore(t)
	ctx := context.Background()

	register := func(key, version, coworker, status string, scopeID string) {
		t.Helper()
		def := sampleDefinition(key, version, scopeID)
		def.Digest = "digest-" + key + "-" + version
		def.ValidationStatus = status
		def.Presentation = []byte(`{"title":{"en":"` + key + `","pt":"` + key + `"},"coworker":{"name":"` + coworker +
			`","mark":{"shape":"square","color":"sun"},"role":{"en":"r","pt":"r"},"summary":{"en":"s","pt":"s"},"onItsOwn":[],"connections":[]}}`)
		_, err := store.RegisterDefinition(ctx, def)
		require.NoError(t, err)
	}
	quotes := insertWorkflowIdentity(t, pool, "workflow:default/quotes")
	invoices := insertWorkflowIdentity(t, pool, "workflow:default/invoices")
	register("default/quotes", "1", "Zoe", workflow.ValidationValid, quotes)
	register("default/quotes", "2", "Marco", workflow.ValidationValid, quotes)
	register("default/quotes", "3", "Broken", workflow.ValidationInvalid, quotes)
	register("default/invoices", "1", "Ana", workflow.ValidationValid, invoices)
	// Make creation order explicit rather than relying on clock resolution.
	_, err := pool.Exec(ctx, `UPDATE workflow.definitions SET created_at = now() - make_interval(hours => 10 - version::int)`)
	require.NoError(t, err)

	summaries, err := store.ListReadySummaries(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	assert.Equal(t, "Ana", summaries[0].Coworker.Name, "sorted by coworker name")
	assert.Equal(t, "default/quotes", summaries[1].Key)
	assert.Equal(t, "2", summaries[1].Version, "the latest valid version, never an invalid one")
	assert.Equal(t, "Marco", summaries[1].Coworker.Name)
	assert.Equal(t, []string{"write"}, []string{summaries[1].Steps[0].Key})

	one, err := store.GetReadySummary(ctx, "default/quotes")
	require.NoError(t, err)
	assert.Equal(t, summaries[1], one)
	_, err = store.GetReadySummary(ctx, "default/unknown")
	assert.ErrorIs(t, err, workflow.ErrNotFound)
}
