package sshprovisioner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The HOR-601 sequence: revision 1 created the dispatch Deployment and then
// failed waiting for it; revision 2 disabled dispatch but failed on the
// admission webhook before Helm reached its deletions; revision 3 deployed.
// Helm diffed revision 3 against revision 2, so dispatch was never deleted.
// Revision 3's success marked revision 2 superseded, as real Helm does; only
// revision 1 keeps its failed status.
const hor601History = `[
  {"revision":1,"status":"failed"},
  {"revision":2,"status":"superseded"},
  {"revision":3,"status":"deployed"}
]`

const revision1Manifest = `---
# Source: iterabase-platform/charts/control-plane/templates/api.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: iterabase-control-plane-api
---
# Source: iterabase-platform/charts/control-plane/templates/dispatch.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: iterabase-control-plane-dispatch
---
# Source: iterabase-platform/charts/control-plane/templates/dispatch-rbac.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: iterabase-control-plane-dispatch
---
# Source: iterabase-platform/templates/retained.yaml
apiVersion: v1
kind: Secret
metadata:
  name: iterabase-retained
  namespace: iterabase-system
---
# Source: iterabase-platform/templates/handed-over.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: iterabase-handed-over
`

const revision2Manifest = `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: iterabase-control-plane-api
`

const revision3Manifest = `---
# Source: iterabase-platform/charts/control-plane/templates/api.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: iterabase-control-plane-api
`

func helmOwned(apiVersion, kind, namespace, name string, extra map[string]string) map[string]any {
	annotations := map[string]any{
		"meta.helm.sh/release-name":      "iterabase",
		"meta.helm.sh/release-namespace": "iterabase-system",
	}
	for k, v := range extra {
		annotations[k] = v
	}
	metadata := map[string]any{"name": name, "annotations": annotations}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	return map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": metadata}
}

func list(t *testing.T, items ...map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	require.NoError(t, err)
	return string(b)
}

func TestPruneFailedRevisionOrphans_DeletesWhatFailedRevisionsLeftBehind(t *testing.T) {
	implicitLive := list(t,
		helmOwned("apps/v1", "Deployment", "iterabase-system", "iterabase-control-plane-dispatch", nil),
		helmOwned("rbac.authorization.k8s.io/v1", "ClusterRole", "", "iterabase-control-plane-dispatch", nil),
		// Since adopted by another release: not this release's to delete.
		map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{
			"name": "iterabase-handed-over", "namespace": "iterabase-system",
			"annotations": map[string]any{"meta.helm.sh/release-name": "other", "meta.helm.sh/release-namespace": "iterabase-system"},
		}},
	)
	explicitLive := list(t,
		helmOwned("v1", "Secret", "iterabase-system", "iterabase-retained", map[string]string{"helm.sh/resource-policy": "keep"}),
	)
	p, obs := startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		switch {
		case strings.Contains(cmd, "'history'"):
			return sshCommandResult{stdout: hor601History}
		case strings.Contains(cmd, "'--revision' '1'"):
			return sshCommandResult{stdout: revision1Manifest}
		case strings.Contains(cmd, "'--revision' '2'"):
			return sshCommandResult{stdout: revision2Manifest}
		case strings.Contains(cmd, "'get' 'manifest'"):
			return sshCommandResult{stdout: revision3Manifest}
		case strings.Contains(cmd, "'get' '-f' '-'") && strings.Contains(cmd, "'-n' 'iterabase-system'"):
			return sshCommandResult{stdout: implicitLive}
		case strings.Contains(cmd, "'get' '-f' '-'"):
			return sshCommandResult{stdout: explicitLive}
		case strings.Contains(cmd, "'delete' '-f' '-'"):
			return sshCommandResult{}
		}
		return sshCommandResult{stderr: "unexpected command", code: 1}
	})

	deleted, err := p.pruneFailedRevisionOrphans(context.Background(), "iterabase", "iterabase-system")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"ClusterRole/iterabase-control-plane-dispatch",
		"Deployment/iterabase-system/iterabase-control-plane-dispatch",
	}, deleted)

	records := obs.records()
	assert.Equal(t, helmCmd("history", "iterabase", "-n", "iterabase-system", "-o", "json"), records[0].cmd)
	assert.Len(t, commandsMatching(records, "'get' 'manifest'"), 2, "the current revision and the one still failed")
	assert.Empty(t, commandsMatching(records, "'--revision' '2'"), "Helm already deleted against the superseded revision")

	gets := commandsMatching(records, "'get' '-f' '-'")
	require.Len(t, gets, 2)
	for _, g := range gets {
		assert.NotContains(t, g.stdin, "iterabase-control-plane-api", "objects the current revision renders are never candidates")
	}

	deletes := commandsMatching(records, "'delete' '-f' '-'")
	require.Len(t, deletes, 1)
	assert.Equal(t, kubectlCmd("delete", "-f", "-", "--ignore-not-found", "--timeout=2m"), deletes[0].cmd)
	assert.Contains(t, deletes[0].stdin, "iterabase-control-plane-dispatch")
	assert.NotContains(t, deletes[0].stdin, "iterabase-retained", "keep-policy objects survive")
	assert.NotContains(t, deletes[0].stdin, "iterabase-handed-over", "objects another release owns survive")
	assert.NotContains(t, deletes[0].stdin, "iterabase-control-plane-api")
}

func TestPruneFailedRevisionOrphans_NoFailedRevisionReadsOnlyHistory(t *testing.T) {
	p, obs := startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		if strings.Contains(cmd, "'history'") {
			return sshCommandResult{stdout: cleanHelmHistory}
		}
		return sshCommandResult{stderr: "unexpected command", code: 1}
	})
	deleted, err := p.pruneFailedRevisionOrphans(context.Background(), "iterabase", "iterabase-system")
	require.NoError(t, err)
	assert.Empty(t, deleted)
	assert.Len(t, obs.records(), 1)
}

func TestPruneFailedRevisionOrphans_FailedRevisionWithNothingLeftOverDeletesNothing(t *testing.T) {
	p, obs := startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		switch {
		case strings.Contains(cmd, "'history'"):
			return sshCommandResult{stdout: `[{"revision":1,"status":"failed"},{"revision":2,"status":"deployed"}]`}
		case strings.Contains(cmd, "'get' 'manifest'"):
			return sshCommandResult{stdout: revision3Manifest}
		}
		return sshCommandResult{stderr: "unexpected command", code: 1}
	})
	deleted, err := p.pruneFailedRevisionOrphans(context.Background(), "iterabase", "iterabase-system")
	require.NoError(t, err)
	assert.Empty(t, deleted)
	assert.Empty(t, commandsMatching(obs.records(), "kubectl"), "a failed revision whose objects are all still rendered needs no live read")
}

func TestPruneFailedRevisionOrphans_AlreadyGoneDeletesNothing(t *testing.T) {
	p, obs := startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		switch {
		case strings.Contains(cmd, "'history'"):
			return sshCommandResult{stdout: hor601History}
		case strings.Contains(cmd, "'--revision' '1'"):
			return sshCommandResult{stdout: revision1Manifest}
		case strings.Contains(cmd, "'get' 'manifest'"):
			return sshCommandResult{stdout: revision3Manifest}
		case strings.Contains(cmd, "'get' '-f' '-'"):
			return sshCommandResult{} // --ignore-not-found: nothing left
		}
		return sshCommandResult{stderr: "unexpected command", code: 1}
	})
	deleted, err := p.pruneFailedRevisionOrphans(context.Background(), "iterabase", "iterabase-system")
	require.NoError(t, err)
	assert.Empty(t, deleted)
	assert.Empty(t, commandsMatching(obs.records(), "'delete'"))
}

func TestPruneFailedRevisionOrphans_PropagatesFailures(t *testing.T) {
	for _, tc := range []struct {
		name, failOn, want string
	}{
		{"history", "'history'", "helm history iterabase-system/iterabase"},
		{"failed manifest", "'--revision' '1'", "helm get manifest iterabase-system/iterabase revision 1"},
		{"live read", "'get' '-f' '-'", "read objects from failed revisions"},
		{"delete", "'delete' '-f' '-'", "delete objects orphaned by failed revisions of iterabase-system/iterabase (ClusterRole/iterabase-control-plane-dispatch, Deployment/iterabase-system/iterabase-control-plane-dispatch)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := list(t,
				helmOwned("apps/v1", "Deployment", "iterabase-system", "iterabase-control-plane-dispatch", nil),
				helmOwned("rbac.authorization.k8s.io/v1", "ClusterRole", "", "iterabase-control-plane-dispatch", nil),
			)
			p, _ := startObservedFakeSSH(t, func(cmd string) sshCommandResult {
				if strings.Contains(cmd, tc.failOn) {
					return sshCommandResult{stderr: "boom", code: 1}
				}
				switch {
				case strings.Contains(cmd, "'history'"):
					return sshCommandResult{stdout: `[{"revision":1,"status":"failed"},{"revision":2,"status":"deployed"}]`}
				case strings.Contains(cmd, "'--revision' '1'"):
					return sshCommandResult{stdout: revision1Manifest}
				case strings.Contains(cmd, "'get' 'manifest'"):
					return sshCommandResult{stdout: revision3Manifest}
				case strings.Contains(cmd, "'get' '-f' '-'") && strings.Contains(cmd, "'-n'"):
					return sshCommandResult{stdout: live}
				}
				return sshCommandResult{}
			})
			_, err := p.pruneFailedRevisionOrphans(context.Background(), "iterabase", "iterabase-system")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestParseManifestRefs(t *testing.T) {
	refs, err := parseManifestRefs(revision1Manifest + "---\n# Source: empty.yaml\n---\n")
	require.NoError(t, err)
	require.Len(t, refs, 5)
	assert.Equal(t, manifestRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "iterabase-control-plane-dispatch"}, refs[1])
	assert.Equal(t, manifestRef{APIVersion: "v1", Kind: "Secret", Namespace: "iterabase-system", Name: "iterabase-retained"}, refs[3])

	_, err = parseManifestRefs("kind: [unterminated\n")
	require.Error(t, err)
}

func TestManifestRefKeyIgnoresAPIVersion(t *testing.T) {
	beta := manifestRef{APIVersion: "policy/v1beta1", Kind: "PodDisruptionBudget", Name: "api"}
	ga := manifestRef{APIVersion: "policy/v1", Kind: "PodDisruptionBudget", Name: "api"}
	core := manifestRef{APIVersion: "v1", Kind: "PodDisruptionBudget", Name: "api"}
	assert.Equal(t, beta.key(), ga.key())
	assert.NotEqual(t, ga.key(), core.key(), "the API group still distinguishes kinds")
}
