package e2e

import (
	"os"
	"testing"
)

const (
	// PR-gating tests use a supplied monorepo-local chart when available and
	// otherwise fall back to pinned released baselines. Explicit version overrides
	// select a published chart. Baseline upgrades are intentional changes to this
	// file, not an unrelated release silently changing another PR's test matrix.
	pinnedPlatformChartVersion     = "0.3.11"
	pinnedControlPlaneChartVersion = "0.4.9" // release fixture authority; Forge no longer installs this chart directly
)

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
