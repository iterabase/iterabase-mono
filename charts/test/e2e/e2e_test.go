package e2e_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
)

type exampleState struct{ events []string }

// TestE2E is the charts owner's single compiled suite entrypoint.
func TestE2E(t *testing.T) {
	suite := sharede2e.NewSuite(sharede2e.SuiteMetadata{
		Name: "charts", Owner: "charts", Entrypoint: "charts/test/e2e",
	}, chartFixtureFromEnv)
	suite.Add(
		hermeticExampleScenario(),
		freshInstallScenario(),
		nMinusOneUpgradeScenario(),
		observabilityScenario(),
		observabilityTLSScenario(),
	)
	suite.Run(t)
}

func chartFixtureFromEnv(t *testing.T) sharede2e.Fixture {
	t.Helper()
	return sharede2e.FixtureFromEnv(t)
}

func chartScenarioMetadata(name, description, makeTarget string, minutes int, references, targets []string, renders []sharede2e.RenderInput) sharede2e.ScenarioMetadata {
	artifacts := []string{
		"control-plane-image", "inference-gateway-image", "control-plane-chart", "inference-gateway-chart",
		"iterabase-platform-chart", "cert-manager-substrate-chart", "lvm-storage-substrate-chart",
	}
	if name == "observability" || name == "observability-tls" {
		artifacts = append(artifacts, "harness-image", "tool-runner-image")
	}
	return sharede2e.ScenarioMetadata{
		Name: name, Description: description, Tier: sharede2e.TierF2,
		References: references, ReleaseTargets: targets, RequiredArtifacts: artifacts,
		Intents:      []sharede2e.ExecutionIntent{sharede2e.IntentPR, sharede2e.IntentCandidate},
		FixtureModes: []sharede2e.FixtureMode{sharede2e.FixtureSource, sharede2e.FixtureCandidate},
		MakeTarget:   makeTarget, TimeoutMinutes: minutes, Renders: renders,
	}
}

func TestCurrentChartScenarioDoesNotAdvertiseIncompletePublishedLVMRuntime(t *testing.T) {
	metadata := chartScenarioMetadata("fresh-install", "test", "test-e2e-install", 45, nil, nil, nil)
	if slices.Contains(metadata.FixtureModes, sharede2e.FixturePublished) {
		t.Fatal("current chart scenarios advertise published mode before a same-version LVM storage companion exists")
	}
	if !slices.Contains(metadata.FixtureModes, sharede2e.FixtureSource) || !slices.Contains(metadata.FixtureModes, sharede2e.FixtureCandidate) {
		t.Fatalf("current chart fixture modes lost source/candidate coverage: %v", metadata.FixtureModes)
	}
}

func hermeticExampleScenario() sharede2e.Definition {
	return sharede2e.Define(sharede2e.Scenario[*exampleState]{
		Metadata: sharede2e.ScenarioMetadata{
			Name: "hermetic-example", Description: "Proves the charts suite composes typed dependent stages without infrastructure.",
			Tier: sharede2e.TierF0, References: []string{"HOR-476"},
			FixtureModes: []sharede2e.FixtureMode{sharede2e.FixtureSource, sharede2e.FixtureCandidate, sharede2e.FixturePublished},
		},
		NewState: func(*testing.T) *exampleState { return &exampleState{} },
		Stages: []sharede2e.Stage[*exampleState]{
			{Name: "render", Run: func(_ *testing.T, state *exampleState) { state.events = append(state.events, "rendered") }},
			{Name: "assert", DependsOn: []string{"render"}, Run: func(t *testing.T, state *exampleState) {
				if !slices.Equal(state.events, []string{"rendered"}) {
					t.Fatalf("events = %v", state.events)
				}
			}},
		},
	})
}

func TestUnitScenarioRendersNameRepositoryInputs(t *testing.T) {
	for name, values := range map[string]platformValues{
		"fresh-install":     freshInstallPlatform,
		"n-1-upgrade":       nMinusOnePlatform,
		"observability":     observabilityCandidateValues(observabilityPlatform, true, true),
		"observability-tls": observabilityCandidateValues(observabilityTLSPlatform, true, true),
	} {
		t.Run(name, func(t *testing.T) {
			renders := append(substrateRenders(testRelease, "values-tls.yaml"), values.render())
			for _, render := range renders {
				if _, err := os.Stat(filepath.Join("..", "..", "..", render.Chart, "Chart.yaml")); err != nil {
					t.Fatalf("render chart %s is not a repository chart: %v", render.Chart, err)
				}
				for _, file := range render.Values {
					if _, err := os.Stat(filepath.Join("..", "..", "..", file)); err != nil {
						t.Fatalf("render values %s does not exist: %v", file, err)
					}
				}
			}
			if got := values.render().Values; len(got) != len(values) {
				t.Fatalf("declared renders %v do not match installed values %v", got, values)
			}
		})
	}
}
