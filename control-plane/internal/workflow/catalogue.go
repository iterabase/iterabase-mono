package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// Summary is the customer-safe projection of one Ready workflow definition
// (HOR-459): its title, AI coworker identity, business steps and the human
// gates the coworker always asks first. It deliberately carries no prompt,
// model, skill, tool, capability, pool, value model or schema.
type Summary struct {
	Key        string            `json:"key"`
	Version    string            `json:"version"`
	Title      CanonicalText     `json:"title"`
	Coworker   CanonicalCoworker `json:"coworker"`
	Steps      []SummaryStep     `json:"steps"`
	AlwaysAsks []SummaryAsk      `json:"alwaysAsks"`
}

// SummaryStep is one graph node as a customer-facing business step.
type SummaryStep struct {
	Key   string        `json:"key"`
	Kind  string        `json:"kind"`
	Label CanonicalText `json:"label"`
}

// SummaryAsk is one human gate: something the coworker always asks first.
type SummaryAsk struct {
	NodeKey string        `json:"nodeKey"`
	Type    string        `json:"type"`
	Title   CanonicalText `json:"title"`
}

// ListReadySummaries returns the latest valid, active definition of every
// workflow key as a customer-safe summary, sorted by coworker name (then key).
func (s *Store) ListReadySummaries(ctx context.Context) ([]Summary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (key) key, version, spec_json, presentation
		FROM workflow.definitions
		WHERE deleted_at IS NULL AND validation_status = $1
		ORDER BY key, created_at DESC, id DESC`, ValidationValid)
	if err != nil {
		return nil, fmt.Errorf("list workflow summaries: %w", err)
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		summary, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workflow summaries: %w", err)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Coworker.Name != out[j].Coworker.Name {
			return out[i].Coworker.Name < out[j].Coworker.Name
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// GetReadySummary returns the customer-safe summary of the latest valid,
// active definition of one workflow key, or ErrNotFound.
func (s *Store) GetReadySummary(ctx context.Context, key string) (Summary, error) {
	summary, err := scanSummary(s.pool.QueryRow(ctx, `
		SELECT key, version, spec_json, presentation
		FROM workflow.definitions
		WHERE key = $1 AND deleted_at IS NULL AND validation_status = $2
		ORDER BY created_at DESC, id DESC LIMIT 1`, key, ValidationValid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Summary{}, ErrNotFound
	}
	return summary, err
}

func scanSummary(row pgx.Row) (Summary, error) {
	var key, version string
	var specJSON, presentationJSON []byte
	if err := row.Scan(&key, &version, &specJSON, &presentationJSON); err != nil {
		return Summary{}, err
	}
	var spec struct {
		Graph CanonicalGraph `json:"graph"`
	}
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return Summary{}, fmt.Errorf("workflow summary %s:%s graph: %w", key, version, err)
	}
	var presentation CanonicalPresentation
	if err := json.Unmarshal(presentationJSON, &presentation); err != nil {
		return Summary{}, fmt.Errorf("workflow summary %s:%s presentation: %w", key, version, err)
	}
	return BuildSummary(key, version, presentation, spec.Graph), nil
}

// BuildSummary projects a definition into its customer-safe summary. Steps are
// the graph nodes in breadth-first order from the entry node, following edges
// in declaration order, so the order is deterministic; alwaysAsks are the
// human_gate steps in that same order.
func BuildSummary(key, version string, presentation CanonicalPresentation, graph CanonicalGraph) Summary {
	presentation = presentation.Normalized()
	summary := Summary{
		Key: key, Version: version,
		Title: presentation.Title, Coworker: presentation.Coworker,
		Steps: []SummaryStep{}, AlwaysAsks: []SummaryAsk{},
	}
	nodes := make(map[string]CanonicalNode, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.Key] = node
	}
	next := make(map[string][]string, len(graph.Nodes))
	for _, edge := range graph.Edges {
		next[edge.From] = append(next[edge.From], edge.To)
	}
	visited := map[string]bool{}
	queue := []string{graph.EntryNode}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		node, ok := nodes[current]
		if !ok || visited[current] {
			continue
		}
		visited[current] = true
		summary.Steps = append(summary.Steps, SummaryStep{Key: node.Key, Kind: node.Kind, Label: summaryText(node.Label)})
		if node.Kind == NodeHumanGate && node.HumanGate != nil {
			summary.AlwaysAsks = append(summary.AlwaysAsks, SummaryAsk{
				NodeKey: node.Key, Type: node.HumanGate.Type, Title: summaryText(node.HumanGate.Title),
			})
		}
		queue = append(queue, next[current]...)
	}
	return summary
}

func summaryText(in CanonicalLocalizedText) CanonicalText {
	return CanonicalText(in)
}
