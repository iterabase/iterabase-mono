package overlay

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
)

// writeTree writes files (slash-separated paths relative to root).
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
}

// overlaysRepo builds a multi-customer overlays repository:
//
//	.git/                         repository root marker
//	bases/hosted/                 shared base: values + a Workflow
//	components/support/           Kustomize Component: an AgentPool + shared tools
//	customers/acme/               the customer overlay composing both
//
// It returns the repository root.
func overlaysRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	writeTree(t, root, map[string]string{
		"bases/hosted/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- workflow.yaml\n",
		"bases/hosted/workflow.yaml": `apiVersion: platform.iterabase.com/v1alpha1
kind: Workflow
metadata:
  name: triage
  namespace: acme
  labels:
    team: support
spec:
  steps: 1
`,
		"bases/hosted/values.yaml":              "hosted: true\n",
		"components/support/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\nresources:\n- agentpool.yaml\n",
		"components/support/agentpool.yaml": `apiVersion: platform.iterabase.com/v1alpha1
kind: AgentPool
metadata:
  name: support
  namespace: acme
spec:
  replicas: 1
`,
		"components/support/tools/support-lookup/index.ts": "export const lookup = 1;\n",
		"customers/acme/kustomization.yaml": `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../../bases/hosted
components:
- ../../components/support
`,
		"customers/acme/values.yaml":               "customer: acme\n",
		"customers/acme/secrets.yaml":              "secrets:\n- name: crm-token\n  namespace: iterabase-system\n  key: token\n  envVar: FORGE_ACME_CRM_TOKEN\n",
		"customers/acme/tools/acme-crm/tool.json":  `{"name":"acme-crm"}`,
		"customers/acme/tools/acme-crm/lib/sub.ts": "export const sub = 2;\n",
	})
	return root
}

func acmeOverlay() config.Overlay {
	return config.Overlay{
		Path:       "customers/acme",
		ValueFiles: []string{"../../bases/hosted/values.yaml", "values.yaml"},
		ToolDirs:   []string{"../../components/support/tools", "tools"},
	}
}

func manifestDocs(t *testing.T, manifest []byte) []map[string]any {
	t.Helper()
	var docs []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs
}

func TestRender_ComposesBaseComponentValuesToolsAndSecrets(t *testing.T) {
	root := overlaysRepo(t)
	r, err := Render(root, acmeOverlay())
	require.NoError(t, err)

	assert.Equal(t, []ValueFile{
		{Name: "../../bases/hosted/values.yaml", Content: []byte("hosted: true\n")},
		{Name: "values.yaml", Content: []byte("customer: acme\n")},
	}, r.ValueFiles, "value files keep configured apply order")

	assert.ElementsMatch(t, []ObjectRef{
		{APIVersion: "platform.iterabase.com/v1alpha1", Kind: "Workflow", Namespace: "acme", Name: "triage"},
		{APIVersion: "platform.iterabase.com/v1alpha1", Kind: "AgentPool", Namespace: "acme", Name: "support"},
	}, r.Objects)

	docs := manifestDocs(t, r.Manifest)
	require.Len(t, docs, 2)
	for _, doc := range docs {
		labels := doc["metadata"].(map[string]any)["labels"].(map[string]any)
		assert.Equal(t, OwnerOverlay, labels[OwnerLabel], "%v", doc["kind"])
		assert.Equal(t, ManagedByForge, labels[ManagedByLabel], "%v", doc["kind"])
		assert.Equal(t, PartObjects, labels[PartLabel], "%v", doc["kind"])
		if doc["kind"] == "Workflow" {
			assert.Equal(t, "support", labels["team"], "existing labels are preserved")
		}
	}

	assert.Equal(t, 3, r.Tools.Files)
	names := archiveNames(t, r.Tools.Bytes)
	assert.Equal(t, []string{
		"tools/client/acme-crm/lib/sub.ts",
		"tools/client/acme-crm/tool.json",
		"tools/client/support-lookup/index.ts",
	}, names)

	assert.Contains(t, string(r.Secrets), "FORGE_ACME_CRM_TOKEN")
	assert.True(t, strings.HasPrefix(r.Revision, "sha256:"))
	assert.Len(t, r.Revision, len("sha256:")+64)
}

func TestRender_RelativeToConfigDirOrAbsolute(t *testing.T) {
	root := overlaysRepo(t)
	configDir := filepath.Join(root, "installs", "acme-prod")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	ov := acmeOverlay()
	ov.Path = "../../customers/acme"
	fromConfig, err := Render(configDir, ov)
	require.NoError(t, err)

	ov.Path = filepath.Join(root, "customers", "acme")
	absolute, err := Render("/nonexistent-config-dir", ov)
	require.NoError(t, err)
	assert.Equal(t, fromConfig.Revision, absolute.Revision)
}

func TestRender_IsDeterministicAndRevisionTracksEveryInput(t *testing.T) {
	root := overlaysRepo(t)
	base, err := Render(root, acmeOverlay())
	require.NoError(t, err)
	again, err := Render(root, acmeOverlay())
	require.NoError(t, err)
	assert.Equal(t, base.Revision, again.Revision)
	assert.Equal(t, base.Tools.Digest, again.Tools.Digest)
	assert.Equal(t, base.Manifest, again.Manifest)

	for name, mutate := range map[string]func(string){
		"shared value file": func(r string) { writeTree(t, r, map[string]string{"bases/hosted/values.yaml": "hosted: false\n"}) },
		"customer values":   func(r string) { writeTree(t, r, map[string]string{"customers/acme/values.yaml": "customer: acme2\n"}) },
		"base object": func(r string) {
			writeTree(t, r, map[string]string{"bases/hosted/workflow.yaml": strings.Replace(readFile(t, r, "bases/hosted/workflow.yaml"), "steps: 1", "steps: 2", 1)})
		},
		"component tool": func(r string) {
			writeTree(t, r, map[string]string{"components/support/tools/support-lookup/index.ts": "export const lookup = 2;\n"})
		},
		"new customer tool":  func(r string) { writeTree(t, r, map[string]string{"customers/acme/tools/acme-erp/index.ts": "x"}) },
		"secret declaration": func(r string) { writeTree(t, r, map[string]string{"customers/acme/secrets.yaml": "secrets: []\n"}) },
		"removed secrets":    func(r string) { require.NoError(t, os.Remove(filepath.Join(r, "customers/acme/secrets.yaml"))) },
	} {
		t.Run(name, func(t *testing.T) {
			r := overlaysRepo(t)
			mutate(r)
			changed, err := Render(r, acmeOverlay())
			require.NoError(t, err)
			assert.NotEqual(t, base.Revision, changed.Revision)
		})
	}

	t.Run("value file order", func(t *testing.T) {
		ov := acmeOverlay()
		ov.ValueFiles = []string{"values.yaml", "../../bases/hosted/values.yaml"}
		swapped, err := Render(overlaysRepo(t), ov)
		require.NoError(t, err)
		assert.NotEqual(t, base.Revision, swapped.Revision)
	})
}

func readFile(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	require.NoError(t, err)
	return string(b)
}

func TestRender_RejectsSecretManifests(t *testing.T) {
	root := overlaysRepo(t)
	writeTree(t, root, map[string]string{
		"bases/hosted/kustomization.yaml": "resources:\n- workflow.yaml\n- secret.yaml\n",
		"bases/hosted/secret.yaml":        "apiVersion: v1\nkind: Secret\nmetadata:\n  name: leaked\n  namespace: acme\nstringData:\n  token: nope\n",
	})
	_, err := Render(root, acmeOverlay())
	require.ErrorContains(t, err, "overlay object Secret/leaked")
	require.ErrorContains(t, err, "secrets.yaml")
}

func TestRender_RejectsForeignOwnerLabelAndAcceptsOverlayOwner(t *testing.T) {
	root := overlaysRepo(t)
	writeTree(t, root, map[string]string{
		"components/support/agentpool.yaml": "apiVersion: platform.iterabase.com/v1alpha1\nkind: AgentPool\nmetadata:\n  name: support\n  namespace: acme\n  labels:\n    iterabase.com/owner: installation\n",
	})
	_, err := Render(root, acmeOverlay())
	require.ErrorContains(t, err, `sets iterabase.com/owner="installation"`)

	writeTree(t, root, map[string]string{
		"components/support/agentpool.yaml": "apiVersion: platform.iterabase.com/v1alpha1\nkind: AgentPool\nmetadata:\n  name: support\n  namespace: acme\n  labels:\n    iterabase.com/owner: overlay\n",
	})
	_, err = Render(root, acmeOverlay())
	require.NoError(t, err)
}

func TestRender_RejectsRemoteBasesAndHelmCharts(t *testing.T) {
	for name, kustomization := range map[string]string{
		"https resource":        "resources:\n- https://github.com/example/overlays//bases/hosted?ref=v1\n",
		"github shorthand":      "resources:\n- github.com/example/overlays/bases/hosted\n",
		"gitlab shorthand":      "resources:\n- gitlab.com/example/overlays/bases/hosted\n",
		"ssh git":               "resources:\n- git@github.com:example/overlays.git//bases\n",
		"remote component":      "resources:\n- ../../bases/hosted\ncomponents:\n- https://example.com/components/support\n",
		"legacy remote base":    "bases:\n- ssh://git@example.com/overlays.git\n",
		"helm charts":           "resources:\n- ../../bases/hosted\nhelmCharts:\n- name: redis\n  repo: https://charts.example.com\n",
		"helm inflation legacy": "helmChartInflationGenerator:\n- chartName: redis\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := overlaysRepo(t)
			writeTree(t, root, map[string]string{"customers/acme/kustomization.yaml": kustomization})
			_, err := Render(root, acmeOverlay())
			require.Error(t, err)
			if strings.Contains(name, "helm") {
				assert.Contains(t, err.Error(), "inflates Helm charts")
			} else {
				assert.Contains(t, err.Error(), "references the remote base")
			}
		})
	}
}

func TestRender_ValueFilesAndToolDirsStayInsideRepository(t *testing.T) {
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{
		"values.yaml":            "stolen: true\n",
		"tools/evil/index.ts":    "x",
		"other-tools/x/index.ts": "x",
	})

	t.Run("relative escape", func(t *testing.T) {
		root := overlaysRepo(t)
		rel, err := filepath.Rel(filepath.Join(root, "customers", "acme"), filepath.Join(outside, "values.yaml"))
		require.NoError(t, err)
		ov := acmeOverlay()
		ov.ValueFiles = []string{rel}
		_, err = Render(root, ov)
		require.ErrorContains(t, err, "outside the overlays repository")
	})
	t.Run("symlinked value file", func(t *testing.T) {
		root := overlaysRepo(t)
		link := filepath.Join(root, "customers", "acme", "values.yaml")
		require.NoError(t, os.Remove(link))
		require.NoError(t, os.Symlink(filepath.Join(outside, "values.yaml"), link))
		_, err := Render(root, acmeOverlay())
		require.ErrorContains(t, err, `overlay value file "values.yaml"`)
		require.ErrorContains(t, err, "outside the overlays repository")
	})
	t.Run("symlinked tool dir", func(t *testing.T) {
		root := overlaysRepo(t)
		require.NoError(t, os.RemoveAll(filepath.Join(root, "customers", "acme", "tools")))
		require.NoError(t, os.Symlink(filepath.Join(outside, "tools"), filepath.Join(root, "customers", "acme", "tools")))
		_, err := Render(root, acmeOverlay())
		require.ErrorContains(t, err, `overlay tool directory "tools"`)
		require.ErrorContains(t, err, "outside the overlays repository")
	})
	t.Run("symlink inside repository is allowed", func(t *testing.T) {
		root := overlaysRepo(t)
		link := filepath.Join(root, "customers", "acme", "shared.yaml")
		require.NoError(t, os.Symlink(filepath.Join(root, "bases", "hosted", "values.yaml"), link))
		ov := acmeOverlay()
		ov.ValueFiles = []string{"shared.yaml"}
		r, err := Render(root, ov)
		require.NoError(t, err)
		assert.Equal(t, "hosted: true\n", string(r.ValueFiles[0].Content))
	})
	t.Run("without a git root the overlay directory is the boundary", func(t *testing.T) {
		root := overlaysRepo(t)
		require.NoError(t, os.Remove(filepath.Join(root, ".git")))
		_, err := Render(root, acmeOverlay())
		require.ErrorContains(t, err, "outside the overlays repository")
	})
}

func TestRender_OptionalPartsAndMissingInputs(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"acme/values.yaml": "a: 1\n"})
	r, err := Render(root, config.Overlay{Path: "acme", ValueFiles: []string{"values.yaml"}, ToolDirs: []string{"tools"}})
	require.NoError(t, err)
	assert.Empty(t, r.Manifest, "no kustomization => no objects")
	assert.Empty(t, r.Objects)
	assert.Nil(t, r.Secrets)
	assert.Zero(t, r.Tools.Files, "a missing tool dir is skipped")
	assert.NotEmpty(t, r.Tools.Bytes, "an overlay without tools still ships a valid empty archive")

	_, err = Render(root, config.Overlay{Path: "acme", ValueFiles: []string{"missing.yaml"}})
	require.ErrorContains(t, err, `overlay value file "missing.yaml"`)

	_, err = Render(root, config.Overlay{Path: "nope"})
	require.ErrorContains(t, err, "overlay.path")

	writeTree(t, root, map[string]string{"file": "x"})
	_, err = Render(root, config.Overlay{Path: "file"})
	require.ErrorContains(t, err, "is not a directory")
}

func TestRender_KustomizeBuildErrorsSurface(t *testing.T) {
	root := overlaysRepo(t)
	writeTree(t, root, map[string]string{"customers/acme/kustomization.yaml": "resources:\n- does-not-exist.yaml\n"})
	_, err := Render(root, acmeOverlay())
	require.ErrorContains(t, err, "overlay kustomize build")
}

func TestSortedRefs(t *testing.T) {
	in := []ObjectRef{{Kind: "Workflow", Namespace: "b", Name: "x"}, {Kind: "AgentPool", Namespace: "a", Name: "y"}, {Kind: "ClusterRole", Name: "z"}}
	out := SortedRefs(in)
	assert.Equal(t, []string{"AgentPool/a/y", "ClusterRole/z", "Workflow/b/x"}, []string{out[0].String(), out[1].String(), out[2].String()})
	assert.Equal(t, "Workflow", in[0].Kind, "input is not reordered")
}

func TestRender_RemoteRefsOnlyCheckedWhereReachable(t *testing.T) {
	t.Run("unreached sibling customer does not block", func(t *testing.T) {
		root := overlaysRepo(t)
		writeTree(t, root, map[string]string{
			"customers/other/kustomization.yaml": "resources:\n- https://github.com/example/overlays//bases?ref=v1\nhelmCharts:\n- name: redis\n",
			"bases/unused/kustomization.yaml":    "resources:\n- github.com/example/unused\n",
		})
		r, err := Render(root, acmeOverlay())
		require.NoError(t, err)
		assert.Len(t, r.Objects, 2)
	})
	for name, files := range map[string]map[string]string{
		"through a local component": {
			"components/support/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\nresources:\n- agentpool.yaml\n- https://example.com/remote.yaml\n",
		},
		"through a local base": {
			"bases/hosted/kustomization.yaml": "resources:\n- workflow.yaml\n- ../nested\n",
			"bases/nested/kustomization.yaml": "resources:\n- git@github.com:example/overlays.git//x\n",
		},
		"helm charts in a reached base": {
			"bases/hosted/kustomization.yaml": "resources:\n- workflow.yaml\nhelmCharts:\n- name: redis\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := overlaysRepo(t)
			writeTree(t, root, files)
			_, err := Render(root, acmeOverlay())
			require.Error(t, err)
			assert.Regexp(t, "references the remote base|inflates Helm charts", err.Error())
		})
	}
	t.Run("cyclic local references terminate", func(t *testing.T) {
		root := overlaysRepo(t)
		writeTree(t, root, map[string]string{
			"bases/a/kustomization.yaml":        "resources:\n- ../b\n",
			"bases/b/kustomization.yaml":        "resources:\n- ../a\n- https://example.com/x.yaml\n",
			"customers/acme/kustomization.yaml": "resources:\n- ../../bases/a\n",
		})
		_, err := Render(root, acmeOverlay())
		require.ErrorContains(t, err, "references the remote base")
	})
}
