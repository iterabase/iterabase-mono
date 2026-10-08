package e2e

import (
	"strings"
	"testing"
)

func TestFixtureValidationRejectsIncompleteOrUnsupportedFixtures(t *testing.T) {
	t.Parallel()
	fixtures := []Fixture{
		{Mode: FixtureSource, SourceSHA: "short"},
		{Mode: FixtureSource},
		{Mode: "candidate", SourceSHA: strings.Repeat("a", 40)},
		{Mode: "published", SourceSHA: strings.Repeat("a", 40)},
	}
	for _, fixture := range fixtures {
		if err := fixture.Validate(); err == nil {
			t.Fatalf("fixture unexpectedly valid: %+v", fixture)
		}
	}
}

func TestFixtureFromEnvRecordsSourceMode(t *testing.T) {
	t.Setenv(fixtureModeEnv, string(FixtureSource))
	t.Setenv(fixtureSourceSHAEnv, strings.Repeat("a", 40))
	t.Setenv(fixtureSourceDirtyEnv, "true")
	fixture := FixtureFromEnv(t)
	if fixture.Mode != FixtureSource || !fixture.Dirty || fixture.SourceSHA != strings.Repeat("a", 40) {
		t.Fatalf("source fixture = %+v", fixture)
	}
}
