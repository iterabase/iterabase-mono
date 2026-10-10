package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/artifacts"
)

// isolatedRoot is the package-wide temp directory that HOME and FORGE_HOME
// point at for every test in this package.
var isolatedRoot string

// TestMain points HOME and FORGE_HOME at a throwaway directory before any test
// runs. Lifecycle code audits failures through artifacts.AppendAudit, which
// falls back to the operator's real ~/.forge when FORGE_HOME is unset; a test
// that forgot useTempHome must never reach it (HOR-644).
func TestMain(m *testing.M) {
	os.Exit(runIsolated(m))
}

func runIsolated(m *testing.M) int {
	root, err := os.MkdirTemp("", "forge-lifecycle-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create isolated test home: %v\n", err)
		return 1
	}
	defer os.RemoveAll(root)
	isolatedRoot = root
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "create isolated test home: %v\n", err)
		return 1
	}
	for k, v := range map[string]string{"HOME": home, "FORGE_HOME": filepath.Join(root, "forge")} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "isolate %s: %v\n", k, err)
			return 1
		}
	}
	return m.Run()
}

// TestAuditNeverReachesOperatorHome proves a failing Apply that does not opt
// into useTempHome still audits inside the package's isolated root rather
// than the operator's real ~/.forge.
func TestAuditNeverReachesOperatorHome(t *testing.T) {
	root, err := artifacts.Root()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(root, isolatedRoot+string(filepath.Separator)),
		"state root %q escapes the isolated test root %q", root, isolatedRoot)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(home, isolatedRoot+string(filepath.Separator)),
		"HOME %q escapes the isolated test root %q", home, isolatedRoot)

	p := &fakeProv{pf: readyPf(), hostSwapErr: errors.New("swapoff failed")}
	_, err = Apply(context.Background(), testConfig(), p, nil, nil, nil, ApplyOpts{})
	require.Error(t, err)

	audit, err := os.ReadFile(filepath.Join(root, testConfig().Metadata.Name, "audit.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(audit), `"detail":"swapoff failed"`)
}
