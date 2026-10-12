package workflow

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitLabel(en string) CanonicalLocalizedText {
	return CanonicalLocalizedText{EN: en, PT: en + " (pt)"}
}

// fanOutSpec is the DES-HOR-649-02 pattern: one agent step sends N requests
// and records N reply tokens, one wait collects the replies, and every wait
// outcome routes to a deciding agent step.
func fanOutSpec() CanonicalSpec {
	return CanonicalSpec{
		Key: "dev/quotes", ScopeIdentityKey: "workflow:default/dev", DefaultModelRef: "model-one",
		Graph: CanonicalGraph{EntryNode: "send", MaxTransitions: 20,
			Nodes: []CanonicalNode{
				{Key: "send", Label: waitLabel("Send requests"), Kind: NodeAgentTask, Prompt: "send", Outcomes: []string{"sent"},
					OutputSchema: json.RawMessage(`{"type":"object","properties":{"replyBy":{"type":"string"},"requests":{"type":"array","items":{"type":"object","properties":{"replyToken":{"type":"string"}}}}}}`)},
				{Key: "collect", Label: waitLabel("Wait for quotes"), Kind: NodeWait, Outcomes: append([]string(nil), WaitOutcomes...),
					Wait: &CanonicalWait{
						Events: []CanonicalWaitEvent{{Source: "email_reply", Correlation: CanonicalOutputReference{
							Node: "send", Path: []string{"requests", "*", "replyToken"}}}},
						Completion: WaitCompletionAll,
						Deadline:   CanonicalWaitDeadline{After: "72h"},
					}},
				{Key: "decide", Label: waitLabel("Decide"), Kind: NodeAgentTask, Prompt: "decide", Outcomes: []string{"done"},
					ResultPresentation: &CanonicalResultPresentation{Outcomes: []CanonicalResultOutcomePresentation{{Outcome: "done", Summary: waitLabel("Done")}}}},
			},
			Edges: []CanonicalEdge{
				{From: "send", Outcome: "sent", To: "collect"},
				{From: "collect", Outcome: WaitOutcomeReceived, To: "decide"},
				{From: "collect", Outcome: WaitOutcomeDeadline, To: "decide"},
				{From: "collect", Outcome: WaitOutcomeCancelled, To: "decide"},
			},
			TerminalOutcomes: []CanonicalTerminalOutcome{{Node: "decide", Outcome: "done"}},
		},
	}
}

func mutateWait(t *testing.T, mutate func(*CanonicalSpec)) error {
	t.Helper()
	raw, err := json.Marshal(fanOutSpec())
	require.NoError(t, err)
	var spec CanonicalSpec
	require.NoError(t, json.Unmarshal(raw, &spec))
	mutate(&spec)
	return ValidateGraph(spec)
}

func waitNode(spec *CanonicalSpec) *CanonicalNode { return &spec.Graph.Nodes[1] }

func TestValidateGraph_WaitFanOutAccepted(t *testing.T) {
	require.NoError(t, ValidateGraph(fanOutSpec()))
	require.NoError(t, mutateWait(t, func(s *CanonicalSpec) {
		waitNode(s).Wait.Completion = WaitCompletionAny
		waitNode(s).Wait.Deadline = CanonicalWaitDeadline{At: &CanonicalOutputReference{Node: "send", Path: []string{"replyBy"}}}
	}), "an absolute deadline from a dominating step's string output is valid")
}

func TestValidateGraph_WaitRejections(t *testing.T) {
	cases := map[string]struct {
		mutate func(*CanonicalSpec)
		want   string
	}{
		"unrouted reserved outcome": {func(s *CanonicalSpec) { s.Graph.Edges = s.Graph.Edges[:3] }, "collect/cancelled has no edge"},
		"custom outcomes":           {func(s *CanonicalSpec) { waitNode(s).Outcomes = []string{"received", "deadline"} }, "exactly received, deadline, and cancelled"},
		"missing wait spec":         {func(s *CanonicalSpec) { waitNode(s).Wait = nil }, "wait is required"},
		"agent fields":              {func(s *CanonicalSpec) { waitNode(s).Prompt = "x" }, "cannot declare agent prompt"},
		"output schema":             {func(s *CanonicalSpec) { waitNode(s).OutputSchema = json.RawMessage(`{"type":"object"}`) }, "outputSchema is forbidden"},
		"no events":                 {func(s *CanonicalSpec) { waitNode(s).Wait.Events = nil }, "1 to 16 events"},
		"bad source":                {func(s *CanonicalSpec) { waitNode(s).Wait.Events[0].Source = "Email-Reply" }, "source \"Email-Reply\" is invalid"},
		"bad completion":            {func(s *CanonicalSpec) { waitNode(s).Wait.Completion = "quorum" }, "must be all or any"},
		"no deadline":               {func(s *CanonicalSpec) { waitNode(s).Wait.Deadline = CanonicalWaitDeadline{} }, "exactly one of after or at"},
		"both deadlines": {func(s *CanonicalSpec) {
			waitNode(s).Wait.Deadline.At = &CanonicalOutputReference{Node: "send", Path: []string{"replyBy"}}
		}, "exactly one of after or at"},
		"deadline too short": {func(s *CanonicalSpec) { waitNode(s).Wait.Deadline.After = "30s" }, "between 1m0s and 2160h0m0s"},
		"deadline too long":  {func(s *CanonicalSpec) { waitNode(s).Wait.Deadline.After = "2161h" }, "between 1m0s and 2160h0m0s"},
		"unknown key node": {func(s *CanonicalSpec) {
			waitNode(s).Wait.Events[0].Correlation.Node = "nope"
		}, "does not reference a node"},
		"untyped key path": {func(s *CanonicalSpec) {
			waitNode(s).Wait.Events[0].Correlation.Path = []string{"requests", "*", "missing"}
		}, "property \"missing\" is not declared"},
		"non-string key": {func(s *CanonicalSpec) {
			waitNode(s).Wait.Events[0].Correlation.Path = []string{"requests"}
		}, "string or an array of strings"},
		"wildcard on object": {func(s *CanonicalSpec) {
			waitNode(s).Wait.Events[0].Correlation.Path = []string{"*"}
		}, "requires an array"},
		"absolute deadline from array": {func(s *CanonicalSpec) {
			waitNode(s).Wait.Deadline = CanonicalWaitDeadline{At: &CanonicalOutputReference{Node: "send", Path: []string{"requests", "*", "replyToken"}}}
		}, "must resolve to one string"},
		"source without schema": {func(s *CanonicalSpec) {
			s.Graph.Nodes[0].OutputSchema = nil
		}, "must declare an output schema"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.ErrorContains(t, mutateWait(t, tc.mutate), tc.want)
		})
	}
}

func TestValidateGraph_WaitSourceMustDominate(t *testing.T) {
	// entry branches: start -a-> send -> collect, start -b-> collect. "send"
	// does not run on the b path, so its keys may not exist.
	err := mutateWait(t, func(s *CanonicalSpec) {
		s.Graph.Nodes = append(s.Graph.Nodes, CanonicalNode{Key: "start", Label: waitLabel("Start"), Kind: NodeAgentTask, Prompt: "start", Outcomes: []string{"a", "b"}})
		s.Graph.EntryNode = "start"
		s.Graph.Edges = append(s.Graph.Edges,
			CanonicalEdge{From: "start", Outcome: "a", To: "send"},
			CanonicalEdge{From: "start", Outcome: "b", To: "collect"})
	})
	assert.ErrorContains(t, err, "does not run on every path before the wait")

	// With both paths through send, dominance holds even across a review cycle.
	require.NoError(t, mutateWait(t, func(s *CanonicalSpec) {
		s.Graph.Nodes = append(s.Graph.Nodes, CanonicalNode{Key: "start", Label: waitLabel("Start"), Kind: NodeAgentTask, Prompt: "start", Outcomes: []string{"a", "b"}})
		s.Graph.EntryNode = "start"
		s.Graph.Edges = append(s.Graph.Edges,
			CanonicalEdge{From: "start", Outcome: "a", To: "send"},
			CanonicalEdge{From: "start", Outcome: "b", To: "send"})
		s.Graph.Edges[3] = CanonicalEdge{From: "collect", Outcome: WaitOutcomeCancelled, To: "send"}
	}))
}

func TestValidateGraph_WaitCannotReadWaitOrItself(t *testing.T) {
	err := mutateWait(t, func(s *CanonicalSpec) {
		waitNode(s).Wait.Events[0].Correlation = CanonicalOutputReference{Node: "collect", Path: []string{"events"}}
	})
	assert.ErrorContains(t, err, "cannot read its own output")
}

func TestValidateGraph_WaitForbiddenOnOtherKinds(t *testing.T) {
	err := mutateWait(t, func(s *CanonicalSpec) {
		s.Graph.Nodes[0].Wait = &CanonicalWait{}
	})
	assert.ErrorContains(t, err, "wait is forbidden for agent_task")
}
