package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// NodeWait is a durable wait for correlated external events (DES-HOR-649-02).
// It holds no worker, turn, or session while waiting.
const NodeWait = "wait"

// Reserved wait outcomes. They are implicit in the definition and each must be
// routed by one edge or terminal declaration.
const (
	WaitOutcomeReceived  = "received"
	WaitOutcomeDeadline  = "deadline"
	WaitOutcomeCancelled = "cancelled"

	WaitCompletionAll = "all"
	WaitCompletionAny = "any"

	// WaitMinDeadline and WaitMaxDeadline bound every deadline from the moment
	// the node is entered, whether relative or read from step output.
	WaitMinDeadline = time.Minute
	WaitMaxDeadline = 90 * 24 * time.Hour

	// WaitMaxExpectedEvents bounds the correlation keys one wait visit may
	// register across all of its declared events.
	WaitMaxExpectedEvents = 256

	// WaitOutputPathWildcard expands every item of an array in an output path.
	WaitOutputPathWildcard = "*"
)

// WaitOutcomes is the exact reserved outcome set, in canonical order.
var WaitOutcomes = []string{WaitOutcomeReceived, WaitOutcomeDeadline, WaitOutcomeCancelled}

var waitSourcePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// validateWaitNodeShape checks the wait-only fields of graph.nodes[i]. Graph
// references (dominance and output typing) are checked once the whole graph
// is indexed, by validateWaitReferences.
func validateWaitNodeShape(i int, n CanonicalNode) error {
	if n.Prompt != "" || n.ModelRef != "" || len(n.Skills) > 0 || len(n.Capabilities) > 0 || n.WorkspaceTools || n.Timeout != "" {
		return fmt.Errorf("graph.nodes[%d] wait cannot declare agent prompt/model/skills/capabilities/workspaceTools/timeout", i)
	}
	if n.HumanGate != nil {
		return fmt.Errorf("graph.nodes[%d].humanGate is forbidden for wait", i)
	}
	if len(bytes.TrimSpace(n.OutputSchema)) > 0 {
		return fmt.Errorf("graph.nodes[%d].outputSchema is forbidden for wait; its output is the collected events", i)
	}
	if n.Wait == nil {
		return fmt.Errorf("graph.nodes[%d].wait is required for wait", i)
	}
	if !sameStringSet(n.Outcomes, WaitOutcomes) {
		return fmt.Errorf("graph.nodes[%d] wait outcomes must be exactly received, deadline, and cancelled", i)
	}
	if err := validateWaitEvents(i, n.Wait); err != nil {
		return err
	}
	return validateWaitDeadline(i, n.Wait.Deadline)
}

func validateWaitEvents(i int, w *CanonicalWait) error {
	if len(w.Events) == 0 || len(w.Events) > 16 {
		return fmt.Errorf("graph.nodes[%d].wait.events must declare 1 to 16 events", i)
	}
	for j, event := range w.Events {
		if !waitSourcePattern.MatchString(event.Source) {
			return fmt.Errorf("graph.nodes[%d].wait.events[%d].source %q is invalid", i, j, event.Source)
		}
		if err := validateOutputReferenceShape(event.Correlation); err != nil {
			return fmt.Errorf("graph.nodes[%d].wait.events[%d].correlation: %w", i, j, err)
		}
	}
	switch w.Completion {
	case WaitCompletionAll, WaitCompletionAny:
		return nil
	default:
		return fmt.Errorf("graph.nodes[%d].wait.completion %q must be all or any", i, w.Completion)
	}
}

func validateWaitDeadline(i int, deadline CanonicalWaitDeadline) error {
	switch {
	case deadline.After != "" && deadline.At != nil, deadline.After == "" && deadline.At == nil:
		return fmt.Errorf("graph.nodes[%d].wait.deadline requires exactly one of after or at", i)
	case deadline.After != "":
		d, err := time.ParseDuration(deadline.After)
		if err != nil || d < WaitMinDeadline || d > WaitMaxDeadline {
			return fmt.Errorf("graph.nodes[%d].wait.deadline.after must be a duration between %s and %s", i, WaitMinDeadline, WaitMaxDeadline)
		}
		return nil
	default:
		if err := validateOutputReferenceShape(*deadline.At); err != nil {
			return fmt.Errorf("graph.nodes[%d].wait.deadline.at: %w", i, err)
		}
		return nil
	}
}

func validateOutputReferenceShape(ref CanonicalOutputReference) error {
	if ref.Node == "" {
		return fmt.Errorf("node is required")
	}
	if len(ref.Path) == 0 {
		return fmt.Errorf("path is required")
	}
	for _, segment := range ref.Path {
		if segment == "" {
			return fmt.Errorf("path cannot contain an empty segment")
		}
	}
	return nil
}

// validateWaitReferences proves every wait's correlation and absolute-deadline
// source has always run before the wait (static dominance from entryNode) and
// that its declared output schema types the referenced path.
func validateWaitReferences(entry string, nodes map[string]CanonicalNode, adj map[string][]string) error {
	for key, node := range nodes {
		if node.Kind != NodeWait {
			continue
		}
		for j, event := range node.Wait.Events {
			if err := validateOutputReference(entry, key, event.Correlation, true, nodes, adj); err != nil {
				return fmt.Errorf("graph node %q wait.events[%d].correlation: %w", key, j, err)
			}
		}
		if at := node.Wait.Deadline.At; at != nil {
			if err := validateOutputReference(entry, key, *at, false, nodes, adj); err != nil {
				return fmt.Errorf("graph node %q wait.deadline.at: %w", key, err)
			}
		}
	}
	return nil
}

func validateOutputReference(entry, waitKey string, ref CanonicalOutputReference, allowArrays bool,
	nodes map[string]CanonicalNode, adj map[string][]string) error {
	source, ok := nodes[ref.Node]
	if !ok {
		return fmt.Errorf("node %q does not reference a node", ref.Node)
	}
	if ref.Node == waitKey {
		return fmt.Errorf("a wait cannot read its own output")
	}
	var schema json.RawMessage
	switch source.Kind {
	case NodeAgentTask:
		schema = source.OutputSchema
	case NodeHumanGate:
		schema = source.HumanGate.ResponseSchema
	default:
		return fmt.Errorf("node %q must be an agent_task or human_gate", ref.Node)
	}
	if !dominates(entry, ref.Node, waitKey, adj) {
		return fmt.Errorf("node %q does not run on every path before the wait", ref.Node)
	}
	arrays, err := schemaStringPath(schema, ref.Path)
	if err != nil {
		return fmt.Errorf("node %q output path %s: %w", ref.Node, resultPathLabel(ref.Path), err)
	}
	if !allowArrays && arrays > 0 {
		return fmt.Errorf("node %q output path %s must resolve to one string", ref.Node, resultPathLabel(ref.Path))
	}
	return nil
}

// dominates reports whether every path from entry to target passes through
// dominator. A node trivially dominates itself.
func dominates(entry, dominator, target string, adj map[string][]string) bool {
	if dominator == target || dominator == entry {
		return true
	}
	seen := map[string]struct{}{entry: {}}
	queue := []string{entry}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == target {
			return false
		}
		for _, next := range adj[cur] {
			if next == dominator {
				continue
			}
			if _, ok := seen[next]; ok {
				continue
			}
			seen[next] = struct{}{}
			queue = append(queue, next)
		}
	}
	return true
}

// schemaStringPath walks a declared JSON Schema along path and requires the
// leaf to be a string, or an array of strings (implicitly expanded). It
// returns how many arrays the path expands.
func schemaStringPath(raw json.RawMessage, path []string) (int, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("{}")) {
		return 0, fmt.Errorf("the source node must declare an output schema typing this path")
	}
	arrays := 0
	current := raw
	for _, segment := range path {
		var schema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Items      json.RawMessage            `json:"items"`
		}
		if err := json.Unmarshal(current, &schema); err != nil {
			return 0, fmt.Errorf("schema is not an object")
		}
		if segment == WaitOutputPathWildcard {
			if schema.Type != "array" || len(schema.Items) == 0 {
				return 0, fmt.Errorf("%q requires an array with declared items", WaitOutputPathWildcard)
			}
			arrays++
			current = schema.Items
			continue
		}
		next, ok := schema.Properties[segment]
		if schema.Type != "object" || !ok {
			return 0, fmt.Errorf("property %q is not declared", segment)
		}
		current = next
	}
	leafArrays, err := schemaStringLeaf(current)
	if err != nil {
		return 0, err
	}
	return arrays + leafArrays, nil
}

// schemaStringLeaf accepts a string (0 arrays) or an array of strings (1).
func schemaStringLeaf(raw json.RawMessage) (int, error) {
	var leaf struct {
		Type  string          `json:"type"`
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &leaf); err != nil {
		return 0, fmt.Errorf("leaf schema is not an object")
	}
	switch leaf.Type {
	case "string":
		return 0, nil
	case "array":
		var item struct {
			Type string `json:"type"`
		}
		if len(leaf.Items) > 0 && json.Unmarshal(leaf.Items, &item) == nil && item.Type == "string" {
			return 1, nil
		}
	}
	return 0, fmt.Errorf("must resolve to a string or an array of strings")
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]struct{}, len(got))
	for _, v := range got {
		seen[v] = struct{}{}
	}
	for _, v := range want {
		if _, ok := seen[v]; !ok {
			return false
		}
	}
	return len(seen) == len(want)
}
