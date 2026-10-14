package sshprovisioner

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/deployer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/overlayer"
)

const (
	testOwnerLabel = "iterabase.com/owner"
	testOwnerValue = "overlay"
	testSelector   = "iterabase.com/owner=overlay,iterabase.com/overlay-part=objects"
)

const ownedManifest = `apiVersion: platform.iterabase.com/v1alpha1
kind: Workflow
metadata:
  name: triage
  namespace: acme
  labels:
    iterabase.com/owner: overlay
spec:
  prompt: classify-secretive-input
`

// startObservedFakeSSH starts the fake server with an observer that records
// every exec request and its stdin.
func startObservedFakeSSH(t *testing.T, h resultHandler) (*SSHProvisioner, *fakeSSHObserver) {
	t.Helper()
	obs := &fakeSSHObserver{}
	addr, _, _, cfg, cleanup := startFakeSSHCore(t, h, obs)
	t.Cleanup(cleanup)
	p := newProvisioner(t, addr, cfg)
	t.Cleanup(func() { _ = p.Close() })
	return p, obs
}

func commandsMatching(records []stdinRecord, substr string) []stdinRecord {
	var out []stdinRecord
	for _, r := range records {
		if strings.Contains(r.cmd, substr) {
			out = append(out, r)
		}
	}
	return out
}

func TestStageFiles_ReplacesDirAndWritesContentOverStdin(t *testing.T) {
	p, obs := startObservedFakeSSH(t, func(string) sshCommandResult { return sshCommandResult{} })
	paths, err := p.StageFiles(context.Background(), "/var/lib/forge/overlay/opo1", []overlayer.File{
		{Name: "00-overlay.yaml", Content: []byte("customer: acme\n")},
		{Name: "01-forge.yaml", Content: []byte("hosted: aws\n")},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"/var/lib/forge/overlay/opo1/00-overlay.yaml", "/var/lib/forge/overlay/opo1/01-forge.yaml"}, paths)

	records := obs.records()
	require.Len(t, records, 3)
	assert.Equal(t, "sudo rm -rf '/var/lib/forge/overlay/opo1' && sudo install -d -m 0700 -o $(id -un) -g $(id -gn) '/var/lib/forge/overlay/opo1'", records[0].cmd)
	assert.Equal(t, "umask 077 && cat > '/var/lib/forge/overlay/opo1/00-overlay.yaml'", records[1].cmd)
	assert.Equal(t, "customer: acme\n", records[1].stdin)
	assert.Equal(t, "umask 077 && cat > '/var/lib/forge/overlay/opo1/01-forge.yaml'", records[2].cmd)
	assert.Equal(t, "hosted: aws\n", records[2].stdin)
	for _, r := range records {
		assert.NotContains(t, r.cmd, "acme\n", "content never appears in a command string")
	}
}

func TestStageFiles_EmptyListLeavesEmptyDir(t *testing.T) {
	p, obs := startObservedFakeSSH(t, func(string) sshCommandResult { return sshCommandResult{} })
	paths, err := p.StageFiles(context.Background(), "/var/lib/forge/overlay/opo1", nil)
	require.NoError(t, err)
	assert.Empty(t, paths)
	require.Len(t, obs.records(), 1, "stale staged files are always removed")
}

func TestStageFiles_RejectsNonBaseNames(t *testing.T) {
	for _, name := range []string{"", "../escape.yaml", "nested/values.yaml", `back\slash.yaml`} {
		p, obs := startObservedFakeSSH(t, func(string) sshCommandResult { return sshCommandResult{} })
		_, err := p.StageFiles(context.Background(), "/var/lib/forge/overlay/opo1", []overlayer.File{{Name: name, Content: []byte("x")}})
		require.ErrorContains(t, err, "must be a base name", name)
		assert.Empty(t, commandsMatching(obs.records(), "cat >"), "nothing written for %q", name)
	}
}

func TestStageFiles_PropagatesFailures(t *testing.T) {
	p, _ := startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		if strings.HasPrefix(cmd, "sudo rm -rf") {
			return sshCommandResult{stderr: "read-only file system", code: 1}
		}
		return sshCommandResult{}
	})
	_, err := p.StageFiles(context.Background(), "/var/lib/forge/overlay/opo1", []overlayer.File{{Name: "a.yaml"}})
	require.ErrorContains(t, err, "overlay stage directory")
	require.ErrorContains(t, err, "read-only file system")

	p, _ = startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		if strings.Contains(cmd, "cat >") {
			return sshCommandResult{stderr: "no space left", code: 1}
		}
		return sshCommandResult{}
	})
	_, err = p.StageFiles(context.Background(), "/var/lib/forge/overlay/opo1", []overlayer.File{{Name: "a.yaml"}})
	require.ErrorContains(t, err, "overlay stage a.yaml")
}

func TestOverlayRemove(t *testing.T) {
	p, obs := startObservedFakeSSH(t, func(string) sshCommandResult { return sshCommandResult{} })
	require.NoError(t, p.Remove(context.Background(), "/var/lib/forge/overlay/opo1"))
	records := obs.records()
	require.Len(t, records, 1)
	assert.Equal(t, "sudo rm -rf '/var/lib/forge/overlay/opo1'", records[0].cmd)
}

func TestApplyOwned_RefusesUnownedLiveObjectsWithoutApplying(t *testing.T) {
	live := `{"kind":"List","items":[
	  {"kind":"Workflow","metadata":{"name":"triage","namespace":"acme","labels":{"iterabase.com/owner":"overlay"}}},
	  {"kind":"Workflow","metadata":{"name":"support","namespace":"acme","labels":{"iterabase.com/owner":"installation"}}},
	  {"kind":"AgentPool","metadata":{"name":"default","namespace":"acme"}},
	  {"kind":"ClusterRole","metadata":{"name":"reader","labels":{"app":"x"}}}
	]}`
	p, obs := startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		if strings.Contains(cmd, "'get' '-f' '-'") {
			return sshCommandResult{stdout: live}
		}
		return sshCommandResult{}
	})
	err := p.ApplyOwned(context.Background(), ownedManifest, testOwnerLabel, testOwnerValue)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to overwrite objects not owned by the overlay")
	assert.Contains(t, err.Error(), "AgentPool/acme/default, ClusterRole/reader, Workflow/acme/support", "conflicts listed sorted")
	assert.NotContains(t, err.Error(), "Workflow/acme/triage", "owned objects are not conflicts")

	records := obs.records()
	require.Len(t, records, 1, "kubectl apply never runs after a conflict")
	assert.Equal(t, kubectlCmd("get", "-f", "-", "-o", "json", "--ignore-not-found"), records[0].cmd)
	assert.Equal(t, ownedManifest, records[0].stdin, "the manifest travels over stdin")
	assert.NotContains(t, records[0].cmd, "classify-secretive-input")
}

func TestApplyOwned_AppliesWhenEverythingLiveIsOwned(t *testing.T) {
	for name, live := range map[string]string{
		"nothing live":       "",
		"single owned":       `{"kind":"Workflow","metadata":{"name":"triage","namespace":"acme","labels":{"iterabase.com/owner":"overlay"}}}`,
		"owned list":         `{"kind":"List","items":[{"kind":"Workflow","metadata":{"name":"triage","namespace":"acme","labels":{"iterabase.com/owner":"overlay"}}}]}`,
		"empty typed list":   `{"kind":"WorkflowList","items":[]}`,
		"whitespace no list": "\n  \n",
	} {
		t.Run(name, func(t *testing.T) {
			p, obs := startObservedFakeSSH(t, func(cmd string) sshCommandResult {
				if strings.Contains(cmd, "'get'") {
					return sshCommandResult{stdout: live}
				}
				return sshCommandResult{}
			})
			require.NoError(t, p.ApplyOwned(context.Background(), ownedManifest, testOwnerLabel, testOwnerValue))
			records := obs.records()
			require.Len(t, records, 2)
			assert.Equal(t, kubectlCmd("apply", "-f", "-"), records[1].cmd)
			assert.Equal(t, ownedManifest, records[1].stdin)
			assert.NotContains(t, records[1].cmd, "classify-secretive-input")
		})
	}
}

func TestApplyOwned_ReadOrApplyFailure(t *testing.T) {
	p, obs := startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		if strings.Contains(cmd, "'get'") {
			return sshCommandResult{stderr: "connection refused", code: 1}
		}
		return sshCommandResult{}
	})
	err := p.ApplyOwned(context.Background(), ownedManifest, testOwnerLabel, testOwnerValue)
	require.ErrorContains(t, err, "read live overlay objects")
	assert.Len(t, obs.records(), 1, "no apply after a failed ownership read")

	p, _ = startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		if strings.Contains(cmd, "'get'") {
			return sshCommandResult{stdout: "not json"}
		}
		return sshCommandResult{}
	})
	require.ErrorContains(t, p.ApplyOwned(context.Background(), ownedManifest, testOwnerLabel, testOwnerValue), "parse kubectl output")

	p, _ = startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		if strings.Contains(cmd, "'apply'") {
			return sshCommandResult{stderr: "admission denied", code: 1}
		}
		return sshCommandResult{}
	})
	err = p.ApplyOwned(context.Background(), ownedManifest, testOwnerLabel, testOwnerValue)
	require.ErrorContains(t, err, "kubectl apply -f -")
	require.ErrorContains(t, err, "admission denied")
}

func TestPruneOwned_DeletesOnlyUnkeptAndSkipsUnknownTypes(t *testing.T) {
	p, obs := startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		switch {
		case strings.Contains(cmd, "'get' 'workflow.platform.iterabase.com'"):
			return sshCommandResult{stdout: `{"kind":"List","items":[
			  {"kind":"Workflow","metadata":{"name":"triage","namespace":"acme"}},
			  {"kind":"Workflow","metadata":{"name":"retired","namespace":"acme"}},
			  {"kind":"Workflow","metadata":{"name":"triage","namespace":"other"}}
			]}`}
		case strings.Contains(cmd, "'get' 'agentpool.platform.iterabase.com'"):
			return sshCommandResult{stderr: `error: the server doesn't have a resource type "agentpool"`, code: 1}
		case strings.Contains(cmd, "'get' 'clusterrole.rbac.authorization.k8s.io'"):
			return sshCommandResult{stdout: `{"kind":"List","items":[{"kind":"ClusterRole","metadata":{"name":"old-reader"}}]}`}
		case strings.Contains(cmd, "'get' 'configmap'"):
			return sshCommandResult{stdout: `{"kind":"List","items":[]}`}
		}
		return sshCommandResult{}
	})
	deleted, err := p.PruneOwned(context.Background(),
		[]string{"workflow.platform.iterabase.com", "agentpool.platform.iterabase.com", "clusterrole.rbac.authorization.k8s.io", "configmap"},
		testSelector,
		[]deployer.ObjectRef{{Kind: "Workflow", Namespace: "acme", Name: "triage"}},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"Workflow/acme/retired", "Workflow/other/triage", "ClusterRole/old-reader"}, deleted)

	records := obs.records()
	gets := commandsMatching(records, "'get'")
	require.Len(t, gets, 4)
	assert.Equal(t, kubectlCmd("get", "workflow.platform.iterabase.com", "--all-namespaces", "-l", testSelector, "-o", "json", "--ignore-not-found"), gets[0].cmd)
	deletes := commandsMatching(records, "'delete'")
	require.Len(t, deletes, 3)
	assert.Equal(t, kubectlCmd("delete", "workflow.platform.iterabase.com", "retired", "--ignore-not-found", "--timeout=2m", "-n", "acme"), deletes[0].cmd)
	assert.Equal(t, kubectlCmd("delete", "workflow.platform.iterabase.com", "triage", "--ignore-not-found", "--timeout=2m", "-n", "other"), deletes[1].cmd, "keep matches namespace too")
	assert.Equal(t, kubectlCmd("delete", "clusterrole.rbac.authorization.k8s.io", "old-reader", "--ignore-not-found", "--timeout=2m"), deletes[2].cmd, "cluster-scoped objects are deleted without a namespace")
	assert.Empty(t, commandsMatching(records, "'delete' 'agentpool"), "unknown resource types are skipped")
}

func TestPruneOwned_KeepIsCaseInsensitiveOnKind(t *testing.T) {
	p, obs := startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		if strings.Contains(cmd, "'get'") {
			return sshCommandResult{stdout: `{"kind":"List","items":[{"kind":"ConfigMap","metadata":{"name":"overlay-tools","namespace":"iterabase-overlay"}}]}`}
		}
		return sshCommandResult{}
	})
	deleted, err := p.PruneOwned(context.Background(), []string{"configmap"}, testSelector,
		[]deployer.ObjectRef{{Kind: "configmap", Namespace: "iterabase-overlay", Name: "overlay-tools"}})
	require.NoError(t, err)
	assert.Empty(t, deleted)
	assert.Empty(t, commandsMatching(obs.records(), "'delete'"))
}

func TestPruneOwned_Failures(t *testing.T) {
	p, obs := startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		if strings.Contains(cmd, "'get' 'workflow") {
			return sshCommandResult{stderr: "Unable to connect to the server", code: 1}
		}
		return sshCommandResult{}
	})
	_, err := p.PruneOwned(context.Background(), []string{"workflow.platform.iterabase.com", "configmap"}, testSelector, nil)
	require.ErrorContains(t, err, "list workflow.platform.iterabase.com")
	assert.Len(t, obs.records(), 1, "pruning stops at the first failed listing")

	p, _ = startObservedFakeSSH(t, func(cmd string) sshCommandResult {
		switch {
		case strings.Contains(cmd, "'get'"):
			return sshCommandResult{stdout: `{"kind":"List","items":[
			  {"kind":"Workflow","metadata":{"name":"a","namespace":"acme"}},
			  {"kind":"Workflow","metadata":{"name":"b","namespace":"acme"}}
			]}`}
		case strings.Contains(cmd, "'delete' 'workflow.platform.iterabase.com' 'b'"):
			return sshCommandResult{stderr: "forbidden", code: 1}
		}
		return sshCommandResult{}
	})
	deleted, err := p.PruneOwned(context.Background(), []string{"workflow.platform.iterabase.com"}, testSelector, nil)
	require.ErrorContains(t, err, "delete Workflow/acme/b")
	assert.Equal(t, []string{"Workflow/acme/a"}, deleted, "already-deleted objects are still reported")
}
