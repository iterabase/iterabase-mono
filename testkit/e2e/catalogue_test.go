package e2e

import (
	"os"
	"slices"
	"testing"
)

func TestCatalogueSelectionIsDeterministic(t *testing.T) {
	t.Parallel()
	catalogue := fixtureCatalogue()
	selected := catalogue.Select(Selection{Tiers: []Tier{TierF2}, References: []string{"HOR-478", "HOR-475"}})
	ids := make([]string, 0, len(selected))
	for _, item := range selected {
		ids = append(ids, item.Scenario.ID)
	}
	if want := []string{"charts/install", "control-plane/identity"}; !slices.Equal(ids, want) {
		t.Fatalf("selected IDs = %v, want %v", ids, want)
	}
}

func TestCatalogueMarkdownGolden(t *testing.T) {
	t.Parallel()
	got := fixtureCatalogue().Markdown()
	golden := "testdata/catalogue.md.golden"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("catalogue Markdown differs from golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func fixtureCatalogue() Catalogue {
	return Catalogue{SchemaVersion: 1, Suites: []CatalogueSuite{
		{
			Suite: SuiteMetadata{Name: "control-plane", Owner: "control-plane", Entrypoint: "control-plane/test/e2e"},
			Scenarios: []CatalogueScenario{{
				ID: "control-plane/identity",
				Metadata: ScenarioMetadata{Name: "identity", Description: "identity", Tier: TierF2,
					References: []string{"HOR-478"}, RequiredArtifacts: []string{"control-plane-image"}, FixtureModes: []FixtureMode{FixtureSource}},
				Stages: []StageMetadata{{Name: "install"}, {Name: "assert", DependsOn: []string{"install"}}},
			}},
		},
		{
			Suite: SuiteMetadata{Name: "charts", Owner: "charts", Entrypoint: "charts/test/e2e"},
			Scenarios: []CatalogueScenario{{
				ID: "charts/install",
				Metadata: ScenarioMetadata{Name: "install", Description: "install", Tier: TierF2,
					References: []string{"HOR-475"}, RequiredArtifacts: []string{"iterabase-platform-chart"}, FixtureModes: []FixtureMode{FixtureSource}},
				Stages: []StageMetadata{{Name: "cluster"}},
			}},
		},
	}}
}
