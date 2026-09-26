package e2e

import (
	"os"
	"regexp"
	"testing"
)

const (
	// PR-gating tests use a supplied monorepo-local chart when available and
	// otherwise fall back to pinned released baselines. Explicit version overrides
	// select a published chart. Baseline upgrades are intentional changes to this
	// file, not an unrelated release silently changing another PR's test matrix.
	pinnedPlatformChartVersion     = "0.3.11"
	pinnedControlPlaneChartVersion = "0.4.9" // release fixture authority; Forge no longer installs this chart directly

	// Release-fixture authority for the chart owner's preserved certificate
	// ownership history. Current Forge scenarios do not install this baseline:
	// `.github/scripts/release.py` regex-scrapes this literal out of this source
	// file and compares it to the pinned snapshot.
	certificateMigrationSourceVersion = "0.2.2"
)

// TestCertificateMigrationSourceVersionIsFixtureAuthority keeps that release
// fixture authority referenced from Go code. release.py fails the release
// contract when the literal is missing or is not semver-shaped, so a refactor
// that removes it as "unused" must fail here instead of in the candidate
// preflight.
func TestCertificateMigrationSourceVersionIsFixtureAuthority(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(certificateMigrationSourceVersion) {
		t.Fatalf("certificate migration source version %q is not a semver fixture identity", certificateMigrationSourceVersion)
	}
}

func platformChartVersion(t *testing.T, localChart string) string {
	t.Helper()
	if version := os.Getenv("ITERABASE_CHART_VERSION"); version != "" {
		return version
	}
	if localChart != "" {
		return ""
	}
	return pinnedPlatformChartVersion
}
