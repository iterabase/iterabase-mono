package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Selection filters compiled catalogue entries. Non-empty fields are ANDed;
// values inside one field are ORed.
type Selection struct {
	IDs        []string
	Tiers      []Tier
	References []string
}

// SelectedScenario retains suite ownership alongside one scenario.
type SelectedScenario struct {
	Suite    SuiteMetadata
	Scenario CatalogueScenario
}

// Select returns a stable suite/name-ordered selection.
func (catalogue Catalogue) Select(selection Selection) []SelectedScenario {
	var selected []SelectedScenario
	for _, suite := range catalogue.Suites {
		for _, scenario := range suite.Scenarios {
			metadata := scenario.Metadata
			if len(selection.IDs) > 0 && !slices.Contains(selection.IDs, scenario.ID) {
				continue
			}
			if len(selection.Tiers) > 0 && !slices.Contains(selection.Tiers, metadata.Tier) {
				continue
			}
			if len(selection.References) > 0 && !overlaps(selection.References, metadata.References) {
				continue
			}
			selected = append(selected, SelectedScenario{Suite: suite.Suite, Scenario: scenario})
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		return selected[i].Scenario.ID < selected[j].Scenario.ID
	})
	return selected
}

func overlaps(left, right []string) bool {
	for _, value := range left {
		if slices.Contains(right, value) {
			return true
		}
	}
	return false
}

// JSON renders the catalogue in a stable human-reviewable form.
func (catalogue Catalogue) JSON() ([]byte, error) {
	data, err := json.MarshalIndent(catalogue, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Markdown renders the same compiled registrations without a parallel source.
func (catalogue Catalogue) Markdown() []byte {
	var output bytes.Buffer
	output.WriteString("# Compiled E2E scenario catalogue\n\n")
	output.WriteString("Generated from owner `TestE2E` registrations. Do not edit generated output by hand.\n\n")
	for _, suite := range catalogue.Suites {
		fmt.Fprintf(&output, "## %s\n\n", suite.Suite.Name)
		fmt.Fprintf(&output, "Owner: `%s` · Entrypoint: `%s`\n\n", suite.Suite.Owner, suite.Suite.Entrypoint)
		output.WriteString("| Scenario | Tier | Stages | References | Required artifacts | Fixture modes |\n")
		output.WriteString("| --- | --- | --- | --- | --- | --- |\n")
		for _, scenario := range suite.Scenarios {
			stages := make([]string, 0, len(scenario.Stages))
			for _, stage := range scenario.Stages {
				value := "`" + stage.Name + "`"
				if len(stage.DependsOn) > 0 {
					value += " ← " + strings.Join(stage.DependsOn, ", ")
				}
				stages = append(stages, value)
			}
			modes := make([]string, 0, len(scenario.Metadata.FixtureModes))
			for _, mode := range scenario.Metadata.FixtureModes {
				modes = append(modes, string(mode))
			}
			fmt.Fprintf(&output, "| `%s` | %s | %s | %s | %s | %s |\n",
				scenario.ID,
				scenario.Metadata.Tier,
				strings.Join(stages, "<br>"),
				strings.Join(scenario.Metadata.References, ", "),
				strings.Join(scenario.Metadata.RequiredArtifacts, ", "),
				strings.Join(modes, ", "),
			)
		}
		output.WriteByte('\n')
	}
	return append(bytes.TrimRight(output.Bytes(), "\n"), '\n')
}
