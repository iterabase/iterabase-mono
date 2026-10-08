package e2e

import (
	"fmt"
	"os"
	"regexp"
	"testing"
)

const (
	fixtureModeEnv        = "ITERABASE_E2E_FIXTURE_MODE"
	fixtureSourceSHAEnv   = "ITERABASE_E2E_SOURCE_SHA"
	fixtureSourceDirtyEnv = "ITERABASE_E2E_SOURCE_DIRTY"
	// RequiredEnv enables fail-closed execution: any skipped, blocked, or
	// not-run stage fails the scenario.
	RequiredEnv = "ITERABASE_E2E_REQUIRED"
)

// Fixture records the exact source used by one suite execution.
type Fixture struct {
	Mode      FixtureMode `json:"mode"`
	SourceSHA string      `json:"source_sha,omitempty"`
	Dirty     bool        `json:"dirty,omitempty"`
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Validate rejects incomplete fixture identities.
func (fixture Fixture) Validate() error {
	if fixture.Mode != FixtureSource {
		return fmt.Errorf("unsupported fixture mode %q", fixture.Mode)
	}
	if !fullSHA.MatchString(fixture.SourceSHA) {
		return fmt.Errorf("source fixture requires a full lowercase source SHA")
	}
	return nil
}

// FixtureFromEnv resolves the explicit suite mode. There is intentionally no
// default and no coordinated-ref or latest fallback.
func FixtureFromEnv(t *testing.T) Fixture {
	t.Helper()
	fixture := Fixture{
		Mode: FixtureMode(os.Getenv(fixtureModeEnv)), SourceSHA: os.Getenv(fixtureSourceSHAEnv),
		Dirty: os.Getenv(fixtureSourceDirtyEnv) == "true",
	}
	if err := fixture.Validate(); err != nil {
		t.Fatalf("invalid fixture (%s must be source): %v", fixtureModeEnv, err)
	}
	return fixture
}
