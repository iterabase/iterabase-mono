package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
)

// CatalogueOutputEnv asks a compiled TestE2E entrypoint to emit registrations
// instead of executing infrastructure.
const CatalogueOutputEnv = "ITERABASE_E2E_CATALOGUE_OUTPUT"

// FixtureFactory resolves and validates the exact runtime fixture only when a
// scenario suite is actually executed. Catalogue mode never resolves fixtures.
type FixtureFactory func(*testing.T) Fixture

// Suite owns one compiled TestE2E registry.
type Suite struct {
	metadata       SuiteMetadata
	fixtureFactory FixtureFactory
	definitions    []Definition
}

// Catalogue is the deterministic merged representation of compiled suites.
type Catalogue struct {
	SchemaVersion int              `json:"schema_version"`
	Suites        []CatalogueSuite `json:"suites"`
}

// CatalogueSuite contains one suite and all of its compiled scenarios.
type CatalogueSuite struct {
	Suite     SuiteMetadata       `json:"suite"`
	Scenarios []CatalogueScenario `json:"scenarios"`
}

// CatalogueScenario combines owner identity, runtime metadata, and stage DAG.
type CatalogueScenario struct {
	ID       string           `json:"id"`
	Metadata ScenarioMetadata `json:"metadata"`
	Stages   []StageMetadata  `json:"stages"`
}

// NewSuite creates one owner-local suite. The fixture factory may be nil only
// for callers that never execute the suite.
func NewSuite(metadata SuiteMetadata, fixtureFactory FixtureFactory) *Suite {
	return &Suite{metadata: metadata, fixtureFactory: fixtureFactory}
}

// Add registers definitions in deterministic execution and catalogue order.
func (suite *Suite) Add(definitions ...Definition) {
	suite.definitions = append(suite.definitions, definitions...)
}

// Run emits the compiled catalogue when requested; otherwise it records the
// exact fixture and runs every scenario as a selectable Go subtest. With
// RequiredEnv set, the Go test result is the scenario verdict: any skipped,
// blocked, or not-run stage fails it.
func (suite *Suite) Run(t *testing.T) {
	t.Helper()
	if err := suite.validate(); err != nil {
		t.Fatalf("invalid E2E suite: %v", err)
	}
	if output := os.Getenv(CatalogueOutputEnv); output != "" {
		catalogue := Catalogue{SchemaVersion: 2, Suites: []CatalogueSuite{suite.catalogue()}}
		data, err := json.MarshalIndent(catalogue, "", "  ")
		if err != nil {
			t.Fatalf("marshal E2E catalogue: %v", err)
		}
		data = append(data, '\n')
		if err := os.WriteFile(output, data, 0o600); err != nil {
			t.Fatalf("write E2E catalogue %s: %v", output, err)
		}
		return
	}
	if suite.fixtureFactory == nil {
		t.Fatal("E2E suite has no fixture factory")
	}
	fixture := suite.fixtureFactory(t)
	if err := fixture.Validate(); err != nil {
		t.Fatalf("invalid E2E fixture: %v", err)
	}
	rendered, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("record E2E fixture: %v", err)
	}
	t.Logf("E2E fixture: %s", rendered)

	for _, definition := range suite.definitions {
		definition := definition
		t.Run(definition.metadata.Name, func(t *testing.T) {
			if !slices.Contains(definition.metadata.FixtureModes, fixture.Mode) {
				t.Fatalf("scenario does not support recorded fixture mode %q", fixture.Mode)
			}
			definition.run(t, prepareScenarioExecution())
		})
	}
}

func (suite *Suite) validate() error {
	if err := validateName("suite", suite.metadata.Name); err != nil {
		return err
	}
	if err := validateName("owner", suite.metadata.Owner); err != nil {
		return err
	}
	if suite.metadata.Entrypoint == "" {
		return fmt.Errorf("suite %q has no entrypoint", suite.metadata.Name)
	}
	seen := make(map[string]struct{}, len(suite.definitions))
	smoke := ""
	for _, definition := range suite.definitions {
		if err := validateScenario(definition); err != nil {
			return err
		}
		if definition.metadata.Smoke {
			if smoke != "" {
				return fmt.Errorf("suite %q has two smoke scenarios: %q and %q", suite.metadata.Name, smoke, definition.metadata.Name)
			}
			smoke = definition.metadata.Name
		}
		if _, exists := seen[definition.metadata.Name]; exists {
			return fmt.Errorf("suite %q repeats scenario %q", suite.metadata.Name, definition.metadata.Name)
		}
		seen[definition.metadata.Name] = struct{}{}
	}
	if len(suite.definitions) == 0 {
		return fmt.Errorf("suite %q has no scenarios", suite.metadata.Name)
	}
	return nil
}

func (suite *Suite) catalogue() CatalogueSuite {
	scenarios := make([]CatalogueScenario, 0, len(suite.definitions))
	for _, definition := range suite.definitions {
		scenarios = append(scenarios, CatalogueScenario{
			ID:       suite.metadata.Name + "/" + definition.metadata.Name,
			Metadata: cloneScenarioMetadata(definition.metadata),
			Stages:   cloneStages(definition.stages),
		})
	}
	return CatalogueSuite{Suite: suite.metadata, Scenarios: scenarios}
}

func cloneStages(stages []StageMetadata) []StageMetadata {
	cloned := make([]StageMetadata, 0, len(stages))
	for _, stage := range stages {
		cloned = append(cloned, StageMetadata{Name: stage.Name, DependsOn: slices.Clone(stage.DependsOn), Optional: stage.Optional})
	}
	return cloned
}

type stageStatus string

const (
	stagePassed  stageStatus = "passed"
	stageFailed  stageStatus = "failed"
	stageSkipped stageStatus = "skipped"
	stageBlocked stageStatus = "blocked"
	stageNotRun  stageStatus = "not-run"
	// stageNotSelected is an optional stage the affected selector did not choose;
	// it is complete, not skipped.
	stageNotSelected stageStatus = "not-selected"
)

type scenarioExecution struct {
	required bool
}

func prepareScenarioExecution() scenarioExecution {
	return scenarioExecution{required: os.Getenv(RequiredEnv) == "true"}
}

func runScenario[S any](t *testing.T, scenario Scenario[S], execution scenarioExecution) {
	t.Helper()
	var state S
	initialized := false
	failed := false
	incomplete := false
	statuses := make(map[string]stageStatus, len(scenario.Stages))
	for _, stage := range scenario.Stages {
		statuses[stage.Name] = stageNotRun
	}
	defer func() {
		failedBeforeCleanup := failed || t.Failed()
		if initialized {
			if failedBeforeCleanup || incomplete {
				runHooks(t, "diagnostics", state, scenario.Diagnostics)
			}
			if cleanupFailed := runHooks(t, "cleanup", state, scenario.Cleanup); cleanupFailed && !failedBeforeCleanup {
				runHooks(t, "diagnostics-after-cleanup-failure", state, scenario.Diagnostics)
			}
		}
	}()

	if scenario.NewState != nil {
		state = scenario.NewState(t)
	}
	initialized = true
	selectedOptional := map[string]bool{}
	for _, name := range strings.Split(os.Getenv(OptionalStagesEnv), ",") {
		selectedOptional[strings.TrimSpace(name)] = true
	}
	for _, stage := range scenario.Stages {
		if stage.Optional && !selectedOptional[stage.Name] {
			t.Run(stage.Name, func(t *testing.T) { t.Skipf("optional stage not selected (%s)", OptionalStagesEnv) })
			statuses[stage.Name] = stageNotSelected
			continue
		}
		blockedBy := make([]string, 0, len(stage.DependsOn))
		for _, dependency := range stage.DependsOn {
			if statuses[dependency] != stagePassed {
				blockedBy = append(blockedBy, dependency)
			}
		}
		if len(blockedBy) > 0 {
			t.Run(stage.Name, func(t *testing.T) {
				t.Skipf("blocked by prerequisite stage(s): %v", blockedBy)
			})
			statuses[stage.Name] = stageBlocked
			incomplete = true
			continue
		}

		skipped := false
		passed := t.Run(stage.Name, func(t *testing.T) {
			defer func() { skipped = t.Skipped() }()
			stage.Run(t, state)
		})
		switch {
		case skipped:
			statuses[stage.Name] = stageSkipped
			incomplete = true
		case !passed:
			statuses[stage.Name] = stageFailed
			failed = true
		default:
			statuses[stage.Name] = stagePassed
		}
	}
	if execution.required && incomplete {
		t.Errorf("required scenario has skipped, blocked, or not-run stages")
	}
}

func runHooks[S any](t *testing.T, group string, state S, hooks []Hook[S]) bool {
	t.Helper()
	failed := false
	for _, hook := range hooks {
		hook := hook
		if passed := t.Run(group+"/"+hook.Name, func(t *testing.T) { hook.Run(t, state) }); !passed {
			failed = true
		}
	}
	return failed
}

// MergeCatalogues validates and deterministically merges independently compiled
// suite catalogues.
func MergeCatalogues(catalogues ...Catalogue) (Catalogue, error) {
	merged := Catalogue{SchemaVersion: 2}
	seenSuites := make(map[string]struct{})
	seenScenarios := make(map[string]struct{})
	for _, catalogue := range catalogues {
		if catalogue.SchemaVersion != 2 {
			return Catalogue{}, fmt.Errorf("unsupported catalogue schema %d", catalogue.SchemaVersion)
		}
		for _, suite := range catalogue.Suites {
			if _, exists := seenSuites[suite.Suite.Name]; exists {
				return Catalogue{}, fmt.Errorf("duplicate suite %q", suite.Suite.Name)
			}
			seenSuites[suite.Suite.Name] = struct{}{}
			for _, scenario := range suite.Scenarios {
				if _, exists := seenScenarios[scenario.ID]; exists {
					return Catalogue{}, fmt.Errorf("duplicate scenario %q", scenario.ID)
				}
				seenScenarios[scenario.ID] = struct{}{}
			}
			merged.Suites = append(merged.Suites, suite)
		}
	}
	sort.Slice(merged.Suites, func(i, j int) bool {
		return merged.Suites[i].Suite.Name < merged.Suites[j].Suite.Name
	})
	for i := range merged.Suites {
		sort.Slice(merged.Suites[i].Scenarios, func(a, b int) bool {
			return merged.Suites[i].Scenarios[a].ID < merged.Suites[i].Scenarios[b].ID
		})
	}
	return merged, nil
}
