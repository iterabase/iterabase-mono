// Package e2e provides Iterabase's shared deterministic end-to-end test mechanics.
package e2e

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Tier identifies the fixture boundary exercised by a scenario.
type Tier string

const (
	TierF0 Tier = "F0" // pure/static process
	TierF1 Tier = "F1" // local process or container integration
	TierF2 Tier = "F2" // fresh isolated Kind cluster
	TierF3 Tier = "F3" // ephemeral real machine
	TierP  Tier = "P"  // irreducibly production-only evidence
)

// FixtureMode identifies how runtime artifacts are supplied.
type FixtureMode string

const (
	FixtureSource    FixtureMode = "source"
	FixtureCandidate FixtureMode = "candidate"
	FixturePublished FixtureMode = "published"
)

// ExecutionIntent identifies the workflow route that may select a scenario.
type ExecutionIntent string

const (
	IntentPR        ExecutionIntent = "pr"
	IntentCandidate ExecutionIntent = "candidate"
)

// SuiteMetadata identifies one repository-owned TestE2E entrypoint.
type SuiteMetadata struct {
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Entrypoint string `json:"entrypoint"`
}

// ScenarioMetadata is compiled with the scenario that it describes.
type ScenarioMetadata struct {
	Name              string            `json:"name"`
	Description       string            `json:"description"`
	Tier              Tier              `json:"tier"`
	References        []string          `json:"references,omitempty"`
	ReleaseTargets    []string          `json:"release_targets,omitempty"`
	RequiredArtifacts []string          `json:"required_artifacts,omitempty"`
	Intents           []ExecutionIntent `json:"intents,omitempty"`
	FixtureModes      []FixtureMode     `json:"fixture_modes"`
	MakeTarget        string            `json:"make_target,omitempty"`
	TimeoutMinutes    int               `json:"timeout_minutes,omitempty"`
	Capacity          string            `json:"capacity,omitempty"`
	Mandatory         bool              `json:"mandatory_capacity,omitempty"`
	ProductionOnly    bool              `json:"production_only,omitempty"`
	// Smoke marks the one scenario per suite that CI-only changes run.
	Smoke bool `json:"smoke,omitempty"`
	// SelectedBy narrows which changed artifacts select this scenario on pull
	// requests; the merge queue ignores it (DES-HOR-590-02). Empty means all
	// RequiredArtifacts.
	SelectedBy []string `json:"selected_by,omitempty"`
	// Renders declares the chart renders this scenario installs, so a chart
	// change selects it only when its rendered manifests change.
	Renders []RenderInput `json:"renders,omitempty"`
}

// RenderInput is one `helm template` of a repository chart with the values a
// scenario installs. Paths are repository-relative.
type RenderInput struct {
	Chart  string            `json:"chart"`
	Values []string          `json:"values,omitempty"`
	Set    map[string]string `json:"set,omitempty"`
}

// StageMetadata describes one stage and its direct prerequisites.
type StageMetadata struct {
	Name      string   `json:"name"`
	DependsOn []string `json:"depends_on,omitempty"`
}

// Stage is one typed operation in a scenario.
type Stage[S any] struct {
	Name      string
	DependsOn []string
	Run       func(*testing.T, S)
}

// Hook is a named diagnostic or cleanup operation.
type Hook[S any] struct {
	Name string
	Run  func(*testing.T, S)
}

// Scenario defines a typed state factory, dependency graph, and lifecycle hooks.
type Scenario[S any] struct {
	Metadata    ScenarioMetadata
	NewState    func(*testing.T) S
	Stages      []Stage[S]
	Diagnostics []Hook[S]
	Cleanup     []Hook[S]
}

// Definition is the type-erased scenario retained by a Suite.
type Definition struct {
	metadata      ScenarioMetadata
	stages        []StageMetadata
	definitionErr error
	run           func(*testing.T, scenarioExecution)
}

// Define validates and erases a typed scenario so heterogeneous state types can
// coexist in one suite.
func Define[S any](scenario Scenario[S]) Definition {
	stages := make([]StageMetadata, 0, len(scenario.Stages))
	var definitionErr error
	for _, stage := range scenario.Stages {
		stages = append(stages, StageMetadata{Name: stage.Name, DependsOn: slices.Clone(stage.DependsOn)})
		if stage.Run == nil && definitionErr == nil {
			definitionErr = fmt.Errorf("stage %q has no run function", stage.Name)
		}
	}
	definition := Definition{metadata: cloneScenarioMetadata(scenario.Metadata), stages: stages, definitionErr: definitionErr}
	if definition.definitionErr == nil {
		definition.definitionErr = validateHooks(scenario.Diagnostics, scenario.Cleanup)
	}
	definition.run = func(t *testing.T, execution scenarioExecution) {
		runScenario(t, scenario, execution)
	}
	return definition
}

var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:[a-z0-9-]*[a-z0-9])?$`)

func validateName(kind, name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%s name %q must be lowercase kebab-case", kind, name)
	}
	return nil
}

func validateScenario(definition Definition) error {
	if definition.definitionErr != nil {
		return definition.definitionErr
	}
	metadata := definition.metadata
	if err := validateName("scenario", metadata.Name); err != nil {
		return err
	}
	if strings.TrimSpace(metadata.Description) == "" {
		return fmt.Errorf("scenario %q has no description", metadata.Name)
	}
	switch metadata.Tier {
	case TierF0, TierF1, TierF2, TierF3, TierP:
	default:
		return fmt.Errorf("scenario %q has invalid tier %q", metadata.Name, metadata.Tier)
	}
	if metadata.ProductionOnly != (metadata.Tier == TierP) {
		return fmt.Errorf("scenario %q production_only must match tier P", metadata.Name)
	}
	if len(metadata.FixtureModes) == 0 {
		return fmt.Errorf("scenario %q has no fixture modes", metadata.Name)
	}
	fixtureModes := make(map[FixtureMode]struct{}, len(metadata.FixtureModes))
	for _, mode := range metadata.FixtureModes {
		switch mode {
		case FixtureSource, FixtureCandidate, FixturePublished:
		default:
			return fmt.Errorf("scenario %q has invalid fixture mode %q", metadata.Name, mode)
		}
		if _, exists := fixtureModes[mode]; exists {
			return fmt.Errorf("scenario %q repeats fixture mode %q", metadata.Name, mode)
		}
		fixtureModes[mode] = struct{}{}
	}
	if metadata.TimeoutMinutes < 0 {
		return fmt.Errorf("scenario %q has a negative timeout", metadata.Name)
	}
	if metadata.MakeTarget != "" && metadata.TimeoutMinutes == 0 {
		return fmt.Errorf("scenario %q has a make target without a timeout", metadata.Name)
	}
	if metadata.Tier == TierF2 || metadata.Tier == TierF3 {
		if metadata.MakeTarget == "" || metadata.TimeoutMinutes == 0 {
			return fmt.Errorf("runnable scenario %q has no execution target and timeout", metadata.Name)
		}
		if len(metadata.RequiredArtifacts) == 0 {
			return fmt.Errorf("runnable scenario %q has no artifact requirements", metadata.Name)
		}
		if len(metadata.Intents) == 0 {
			return fmt.Errorf("runnable scenario %q has no workflow routing", metadata.Name)
		}
		intents := make(map[ExecutionIntent]struct{}, len(metadata.Intents))
		for _, intent := range metadata.Intents {
			switch intent {
			case IntentPR, IntentCandidate:
			default:
				return fmt.Errorf("scenario %q has invalid execution intent %q", metadata.Name, intent)
			}
			if _, exists := intents[intent]; exists {
				return fmt.Errorf("scenario %q repeats execution intent %q", metadata.Name, intent)
			}
			intents[intent] = struct{}{}
		}
		for _, required := range []ExecutionIntent{IntentPR, IntentCandidate} {
			if _, exists := intents[required]; !exists {
				return fmt.Errorf("runnable scenario %q has no %s routing", metadata.Name, required)
			}
		}
		artifacts := make(map[string]struct{}, len(metadata.RequiredArtifacts))
		for _, artifact := range metadata.RequiredArtifacts {
			if err := validateName("artifact", artifact); err != nil {
				return fmt.Errorf("scenario %q: %w", metadata.Name, err)
			}
			if _, exists := artifacts[artifact]; exists {
				return fmt.Errorf("scenario %q repeats artifact %q", metadata.Name, artifact)
			}
			artifacts[artifact] = struct{}{}
		}
		for _, artifact := range metadata.SelectedBy {
			if _, exists := artifacts[artifact]; !exists {
				return fmt.Errorf("scenario %q is selected by %q, which it does not require", metadata.Name, artifact)
			}
		}
		for _, render := range metadata.Renders {
			if !strings.HasPrefix(render.Chart, "charts/charts/") {
				return fmt.Errorf("scenario %q renders %q, which is not a repository chart", metadata.Name, render.Chart)
			}
		}
	} else if metadata.Smoke || len(metadata.SelectedBy) > 0 || len(metadata.Renders) > 0 {
		return fmt.Errorf("scenario %q declares selection metadata outside tiers F2 and F3", metadata.Name)
	}
	if metadata.Capacity != "" && metadata.Tier != TierF3 {
		return fmt.Errorf("scenario %q declares capacity outside tier F3", metadata.Name)
	}
	if metadata.Mandatory && metadata.Capacity == "" {
		return fmt.Errorf("scenario %q requires unnamed capacity", metadata.Name)
	}
	if len(definition.stages) == 0 {
		return fmt.Errorf("scenario %q has no stages", metadata.Name)
	}
	seen := make(map[string]struct{}, len(definition.stages))
	for _, stage := range definition.stages {
		if err := validateName("stage", stage.Name); err != nil {
			return fmt.Errorf("scenario %q: %w", metadata.Name, err)
		}
		if _, exists := seen[stage.Name]; exists {
			return fmt.Errorf("scenario %q repeats stage %q", metadata.Name, stage.Name)
		}
		for _, dependency := range stage.DependsOn {
			if _, exists := seen[dependency]; !exists {
				return fmt.Errorf("scenario %q stage %q has unknown or forward dependency %q", metadata.Name, stage.Name, dependency)
			}
		}
		seen[stage.Name] = struct{}{}
	}
	return nil
}

func validateHooks[S any](diagnostics, cleanup []Hook[S]) error {
	if err := validateHookList("diagnostic", diagnostics); err != nil {
		return err
	}
	return validateHookList("cleanup", cleanup)
}

func validateHookList[S any](kind string, hooks []Hook[S]) error {
	seen := make(map[string]struct{}, len(hooks))
	for _, hook := range hooks {
		if err := validateName(kind, hook.Name); err != nil {
			return err
		}
		if hook.Run == nil {
			return fmt.Errorf("%s %q has no run function", kind, hook.Name)
		}
		if _, exists := seen[hook.Name]; exists {
			return fmt.Errorf("duplicate %s %q", kind, hook.Name)
		}
		seen[hook.Name] = struct{}{}
	}
	return nil
}

func cloneScenarioMetadata(metadata ScenarioMetadata) ScenarioMetadata {
	metadata.References = slices.Clone(metadata.References)
	metadata.ReleaseTargets = slices.Clone(metadata.ReleaseTargets)
	metadata.RequiredArtifacts = slices.Clone(metadata.RequiredArtifacts)
	metadata.Intents = slices.Clone(metadata.Intents)
	metadata.FixtureModes = slices.Clone(metadata.FixtureModes)
	metadata.SelectedBy = slices.Clone(metadata.SelectedBy)
	renders := make([]RenderInput, 0, len(metadata.Renders))
	for _, render := range metadata.Renders {
		render.Values = slices.Clone(render.Values)
		render.Set = maps.Clone(render.Set)
		renders = append(renders, render)
	}
	if len(renders) == 0 {
		renders = nil
	}
	metadata.Renders = renders
	return metadata
}
