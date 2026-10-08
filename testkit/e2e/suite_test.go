package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSuiteRejectsDuplicateScenarioAndForwardDependency(t *testing.T) {
	t.Parallel()
	validMetadata := ScenarioMetadata{
		Name: "example", Description: "example", Tier: TierF0, FixtureModes: []FixtureMode{FixtureSource},
	}
	forward := Define(Scenario[struct{}]{
		Metadata: validMetadata,
		Stages: []Stage[struct{}]{
			{Name: "dependent", DependsOn: []string{"later"}, Run: func(*testing.T, struct{}) {}},
			{Name: "later", Run: func(*testing.T, struct{}) {}},
		},
	})
	suite := NewSuite(SuiteMetadata{Name: "owner", Owner: "owner", Entrypoint: "owner/test/e2e"}, nil)
	suite.Add(forward)
	if err := suite.validate(); err == nil || !strings.Contains(err.Error(), "forward dependency") {
		t.Fatalf("forward dependency error = %v", err)
	}

	valid := Define(Scenario[struct{}]{
		Metadata: validMetadata,
		Stages:   []Stage[struct{}]{{Name: "run", Run: func(*testing.T, struct{}) {}}},
	})
	duplicate := NewSuite(SuiteMetadata{Name: "owner", Owner: "owner", Entrypoint: "owner/test/e2e"}, nil)
	duplicate.Add(valid, valid)
	if err := duplicate.validate(); err == nil || !strings.Contains(err.Error(), "repeats scenario") {
		t.Fatalf("duplicate scenario error = %v", err)
	}
}

func TestSuiteValidatesSelectionMetadata(t *testing.T) {
	t.Parallel()
	runnable := func(mutate func(*ScenarioMetadata)) Definition {
		metadata := ScenarioMetadata{
			Name: "deployed", Description: "deployed", Tier: TierF2, FixtureModes: []FixtureMode{FixtureSource},
			MakeTarget: "test-e2e", TimeoutMinutes: 10, RequiredArtifacts: []string{"control-plane-image", "forge-binary"},
		}
		mutate(&metadata)
		return Define(Scenario[struct{}]{Metadata: metadata, Stages: []Stage[struct{}]{{Name: "run", Run: func(*testing.T, struct{}) {}}}})
	}
	validate := func(definitions ...Definition) error {
		suite := NewSuite(SuiteMetadata{Name: "owner", Owner: "owner", Entrypoint: "owner/test/e2e"}, nil)
		suite.Add(definitions...)
		return suite.validate()
	}
	valid := runnable(func(m *ScenarioMetadata) {
		m.Smoke = true
		m.SelectedBy = []string{"forge-binary"}
		m.Renders = []RenderInput{{Chart: "charts/charts/iterabase-platform", Values: []string{"charts/charts/iterabase-platform/values-tls.yaml"}}}
	})
	if err := validate(valid); err != nil {
		t.Fatalf("valid selection metadata: %v", err)
	}
	for name, test := range map[string]struct {
		definitions []Definition
		want        string
	}{
		"selected by an artifact it does not require": {
			[]Definition{runnable(func(m *ScenarioMetadata) { m.SelectedBy = []string{"harness-image"} })}, "does not require"},
		"render outside the repository charts": {
			[]Definition{runnable(func(m *ScenarioMetadata) { m.Renders = []RenderInput{{Chart: "/tmp/chart"}} })}, "not a repository chart"},
		"two smoke scenarios": {
			[]Definition{valid, runnable(func(m *ScenarioMetadata) { m.Name, m.Smoke = "other", true })}, "two smoke scenarios"},
		"selection metadata on a hermetic scenario": {
			[]Definition{Define(Scenario[struct{}]{
				Metadata: ScenarioMetadata{Name: "hermetic", Description: "hermetic", Tier: TierF0, FixtureModes: []FixtureMode{FixtureSource}, Smoke: true},
				Stages:   []Stage[struct{}]{{Name: "run", Run: func(*testing.T, struct{}) {}}},
			})}, "outside tiers F2 and F3"},
	} {
		if err := validate(test.definitions...); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want %q", name, err, test.want)
		}
	}
}

func TestOptionalStageRunsOnlyWhenSelected(t *testing.T) {
	for _, selected := range []string{"", "driver-upgrade", "other, driver-upgrade"} {
		t.Run("selected="+selected, func(t *testing.T) {
			t.Setenv(OptionalStagesEnv, selected)
			ran := false
			scenario := Scenario[struct{}]{
				Metadata: ScenarioMetadata{Name: "gpu", Description: "gpu", Tier: TierF0, FixtureModes: []FixtureMode{FixtureSource}},
				Stages: []Stage[struct{}]{
					{Name: "serve", Run: func(*testing.T, struct{}) {}},
					{Name: "driver-upgrade", DependsOn: []string{"serve"}, Optional: true, Run: func(*testing.T, struct{}) { ran = true }},
				},
			}
			// Required execution still passes: an unselected optional stage is
			// recorded not-selected, which is complete rather than skipped.
			runScenario(t, scenario, scenarioExecution{required: true})
			if want := selected != ""; ran != want {
				t.Fatalf("optional stage ran=%v with %s=%q", ran, OptionalStagesEnv, selected)
			}
		})
	}

	dependent := Define(Scenario[struct{}]{
		Metadata: ScenarioMetadata{Name: "gpu", Description: "gpu", Tier: TierF0, FixtureModes: []FixtureMode{FixtureSource}},
		Stages: []Stage[struct{}]{
			{Name: "driver-upgrade", Optional: true, Run: func(*testing.T, struct{}) {}},
			{Name: "after", DependsOn: []string{"driver-upgrade"}, Run: func(*testing.T, struct{}) {}},
		},
	})
	suite := NewSuite(SuiteMetadata{Name: "owner", Owner: "owner", Entrypoint: "owner/test/e2e"}, nil)
	suite.Add(dependent)
	if err := suite.validate(); err == nil || !strings.Contains(err.Error(), "depends on optional stage") {
		t.Fatalf("dependency on an optional stage error = %v", err)
	}
}

func TestStageFailureKeepsIndependentWorkAndLifecycleHooks(t *testing.T) {
	if os.Getenv("ITERABASE_E2E_FAILURE_HELPER") == "1" {
		runFailureHelper(t)
		return
	}
	directory := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestStageFailureKeepsIndependentWorkAndLifecycleHooks$")
	command.Env = append(os.Environ(), "ITERABASE_E2E_FAILURE_HELPER=1", "ITERABASE_E2E_HELPER_DIR="+directory)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("forced stage failure unexpectedly passed:\n%s", output)
	}
	for _, name := range []string{"independent", "diagnostics", "cleanup"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatalf("%s hook/stage did not run: %v\n%s", name, err, output)
		}
	}
	if _, err := os.Stat(filepath.Join(directory, "dependent")); !os.IsNotExist(err) {
		t.Fatalf("dependent stage ran despite failed prerequisite: %v", err)
	}
}

func TestRequiredSkipIsIncompleteAndStillRunsIndependentWorkAndCleanup(t *testing.T) {
	if os.Getenv("ITERABASE_E2E_SKIP_HELPER") == "1" {
		suite := helperSuite([]Stage[*helperState]{
			{Name: "skips", Run: func(t *testing.T, _ *helperState) { t.Skip("forced skip") }},
			{Name: "blocked", DependsOn: []string{"skips"}, Run: func(t *testing.T, state *helperState) { writeMarker(t, state.directory, "blocked") }},
			{Name: "independent", Run: func(t *testing.T, state *helperState) { writeMarker(t, state.directory, "skip-independent") }},
		}, []Hook[*helperState]{{Name: "record", Run: func(t *testing.T, state *helperState) { writeMarker(t, state.directory, "skip-cleanup") }}}, nil)
		suite.Run(t)
		return
	}
	directory := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestRequiredSkipIsIncompleteAndStillRunsIndependentWorkAndCleanup$")
	command.Env = append(os.Environ(),
		"ITERABASE_E2E_SKIP_HELPER=1",
		"ITERABASE_E2E_HELPER_DIR="+directory,
		RequiredEnv+"=true",
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("required forced skip unexpectedly passed:\n%s", output)
	}
	for _, name := range []string{"skip-independent", "skip-cleanup"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatalf("%s did not run: %v\n%s", name, err, output)
		}
	}
	if _, err := os.Stat(filepath.Join(directory, "blocked")); !os.IsNotExist(err) {
		t.Fatalf("blocked stage ran: %v", err)
	}
	if !strings.Contains(string(output), "required scenario has skipped, blocked, or not-run stages") {
		t.Fatalf("required forced skip did not fail as incomplete:\n%s", output)
	}
}

func TestCleanupFailureDoesNotSuppressLaterCleanup(t *testing.T) {
	if os.Getenv("ITERABASE_E2E_CLEANUP_HELPER") == "1" {
		suite := helperSuite([]Stage[*helperState]{{Name: "pass", Run: func(*testing.T, *helperState) {}}}, []Hook[*helperState]{
			{Name: "forced-failure", Run: func(t *testing.T, _ *helperState) { t.Fatal("forced cleanup failure") }},
			{Name: "still-runs", Run: func(t *testing.T, state *helperState) { writeMarker(t, state.directory, "cleanup-after-failure") }},
		}, []Hook[*helperState]{{Name: "record", Run: func(t *testing.T, state *helperState) { writeMarker(t, state.directory, "cleanup-diagnostics") }}})
		suite.Run(t)
		return
	}
	directory := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestCleanupFailureDoesNotSuppressLaterCleanup$")
	command.Env = append(os.Environ(), "ITERABASE_E2E_CLEANUP_HELPER=1", "ITERABASE_E2E_HELPER_DIR="+directory)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("forced cleanup failure unexpectedly passed:\n%s", output)
	}
	for _, name := range []string{"cleanup-after-failure", "cleanup-diagnostics"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatalf("%s did not run: %v\n%s", name, err, output)
		}
	}
}

type helperState struct{ directory string }

func runFailureHelper(t *testing.T) {
	stages := []Stage[*helperState]{
		{Name: "fails", Run: func(t *testing.T, _ *helperState) { t.Fatal("forced stage failure") }},
		{Name: "dependent", DependsOn: []string{"fails"}, Run: func(t *testing.T, state *helperState) { writeMarker(t, state.directory, "dependent") }},
		{Name: "independent", Run: func(t *testing.T, state *helperState) { writeMarker(t, state.directory, "independent") }},
	}
	suite := helperSuite(stages,
		[]Hook[*helperState]{{Name: "record", Run: func(t *testing.T, state *helperState) { writeMarker(t, state.directory, "cleanup") }}},
		[]Hook[*helperState]{{Name: "record", Run: func(t *testing.T, state *helperState) { writeMarker(t, state.directory, "diagnostics") }}},
	)
	suite.Run(t)
}

func helperSuite(stages []Stage[*helperState], cleanup, diagnostics []Hook[*helperState]) *Suite {
	suite := NewSuite(SuiteMetadata{Name: "helper", Owner: "helper", Entrypoint: "helper/test/e2e"}, func(t *testing.T) Fixture {
		return Fixture{Mode: FixtureSource, SourceSHA: strings.Repeat("a", 40)}
	})
	suite.Add(Define(Scenario[*helperState]{
		Metadata: ScenarioMetadata{
			Name: "lifecycle", Description: "forced lifecycle fixture", Tier: TierF0, FixtureModes: []FixtureMode{FixtureSource},
		},
		NewState:    func(*testing.T) *helperState { return &helperState{directory: os.Getenv("ITERABASE_E2E_HELPER_DIR")} },
		Stages:      stages,
		Cleanup:     cleanup,
		Diagnostics: diagnostics,
	}))
	return suite
}

func writeMarker(t *testing.T, directory, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte("ran\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
