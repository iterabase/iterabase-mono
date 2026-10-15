// Package overlay renders a customer overlay directory on the operator machine
// (DES-HOR-593-03). Forge ships only the rendered result to the installation:
// the ordered Helm value files, the Kustomize-built object instances carrying
// the overlay owner label, the customer tools archive (DES-HOR-593-05) and the
// non-secret secret declarations. Nothing else from the overlays repository
// leaves the operator machine, so one repository can hold many customers.
package overlay

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
)

// Ownership labels (DES-HOR-593-03). Every object Forge applies from an overlay
// carries both; forge apply re-asserts and prunes only objects with the overlay
// owner and never touches installation-owned objects.
const (
	OwnerLabel     = "iterabase.com/owner"
	OwnerOverlay   = "overlay"
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByForge = "forge"
	// PartLabel separates the two kinds of overlay-owned objects so each is
	// pruned only against its own desired set.
	PartLabel   = "iterabase.com/overlay-part"
	PartObjects = "objects"
	PartTools   = "tools"
)

// ObjectsSelector selects every overlay-owned object instance.
const ObjectsSelector = OwnerLabel + "=" + OwnerOverlay + "," + PartLabel + "=" + PartObjects

// ToolsSelector selects the overlay tools archive and pointer ConfigMaps.
const ToolsSelector = OwnerLabel + "=" + OwnerOverlay + "," + PartLabel + "=" + PartTools

// ValueFile is one Helm value file, in apply order.
type ValueFile struct {
	Name    string // overlay-relative path, for diagnostics
	Content []byte
}

// ObjectRef identifies one rendered object.
type ObjectRef struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

func (r ObjectRef) String() string {
	if r.Namespace == "" {
		return r.Kind + "/" + r.Name
	}
	return r.Kind + "/" + r.Namespace + "/" + r.Name
}

// Rendered is everything Forge ships to one installation.
type Rendered struct {
	// Revision is the sha256 of every rendered input below. It identifies the
	// exact overlay content an installation received.
	Revision   string
	ValueFiles []ValueFile
	// Manifest is the multi-document YAML of the Kustomize build, with the
	// ownership labels set on every object. Empty when the overlay has no
	// kustomization.
	Manifest []byte
	Objects  []ObjectRef
	Tools    ToolsArchive
	// Secrets is the optional secrets.yaml declaration file (names, keys and
	// operator environment variables; never values).
	Secrets []byte
}

// Render loads the overlay configured relative to configDir.
func Render(configDir string, ov config.Overlay) (*Rendered, error) {
	dir, err := overlayDir(configDir, ov.Path)
	if err != nil {
		return nil, err
	}
	root := repositoryRoot(dir)
	if err := rejectRemoteKustomizations(dir, map[string]bool{}); err != nil {
		return nil, err
	}

	out := &Rendered{}
	if out.ValueFiles, err = readValueFiles(root, dir, ov.ValueFiles); err != nil {
		return nil, err
	}
	if hasKustomization(dir) {
		if out.Manifest, out.Objects, err = build(dir); err != nil {
			return nil, err
		}
	}
	toolDirs, err := existingToolDirs(root, dir, ov.ToolDirs)
	if err != nil {
		return nil, err
	}
	if out.Tools, err = PackTools(toolDirs); err != nil {
		return nil, err
	}
	secrets, err := os.ReadFile(filepath.Join(dir, "secrets.yaml"))
	switch {
	case err == nil:
		out.Secrets = secrets
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("overlay secrets.yaml: %w", err)
	}

	out.Revision = revision(out)
	return out, nil
}

// repositoryRoot is the nearest ancestor of dir holding .git, or dir itself.
// Value files and tool directories must stay inside it.
func overlayDir(configDir, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(configDir, path)
	}
	dir, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("overlay.path: %w", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("overlay.path %q is not a directory", dir)
	}
	return dir, nil
}

func readValueFiles(root, dir string, names []string) ([]ValueFile, error) {
	out := make([]ValueFile, 0, len(names))
	for _, name := range names {
		path, err := within(root, filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("overlay value file %q: %w", name, err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("overlay value file %q: %w", name, err)
		}
		out = append(out, ValueFile{Name: name, Content: content})
	}
	return out, nil
}

// existingToolDirs resolves the configured tool directories inside root;
// missing ones are skipped.
func existingToolDirs(root, dir string, names []string) ([]string, error) {
	var out []string
	for _, name := range names {
		path, err := within(root, filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("overlay tool directory %q: %w", name, err)
		}
		out = append(out, path)
	}
	return out, nil
}

func repositoryRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// within resolves path (following symlinks) and requires it inside root.
func within(root, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the overlays repository %s", resolved, root)
	}
	return resolved, nil
}

func hasKustomization(dir string) bool {
	_, ok := kustomizationFile(dir)
	return ok
}

func kustomizationFile(dir string) (string, bool) {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path, true
		}
	}
	return "", false
}

// build runs Kustomize in-process with plugins and Helm inflation disabled,
// then sets the ownership labels on every object.
func build(dir string) ([]byte, []ObjectRef, error) {
	opts := krusty.MakeDefaultOptions()
	resources, err := krusty.MakeKustomizer(opts).Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return nil, nil, fmt.Errorf("overlay kustomize build %s: %w", dir, err)
	}
	var refs []ObjectRef
	seen := map[string]bool{}
	for _, r := range resources.Resources() {
		if r.GetKind() == "Secret" {
			return nil, nil, fmt.Errorf("overlay object Secret/%s: declare secrets in secrets.yaml (values come from operator environment variables), never as manifests", r.GetName())
		}
		labels := r.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		if owner, ok := labels[OwnerLabel]; ok && owner != OwnerOverlay {
			return nil, nil, fmt.Errorf("overlay object %s/%s sets %s=%q; overlay objects are always owned by the overlay", r.GetKind(), r.GetName(), OwnerLabel, owner)
		}
		labels[OwnerLabel] = OwnerOverlay
		labels[ManagedByLabel] = ManagedByForge
		labels[PartLabel] = PartObjects
		if err := r.SetLabels(labels); err != nil {
			return nil, nil, fmt.Errorf("label overlay object %s/%s: %w", r.GetKind(), r.GetName(), err)
		}
		ref := ObjectRef{APIVersion: r.GetApiVersion(), Kind: r.GetKind(), Namespace: r.GetNamespace(), Name: r.GetName()}
		if seen[ref.String()] {
			return nil, nil, fmt.Errorf("overlay renders %s twice", ref)
		}
		seen[ref.String()] = true
		refs = append(refs, ref)
	}
	manifest, err := resources.AsYaml()
	if err != nil {
		return nil, nil, fmt.Errorf("overlay manifest: %w", err)
	}
	return manifest, refs, nil
}

// rejectRemoteKustomizations refuses kustomizations that would fetch remote
// bases or inflate Helm charts: both shell out and reach the network, and the
// rendered overlay must come only from reviewed repository content. It checks
// the kustomization in dir and every local kustomization it reaches, so another
// customer's overlay in the same repository never affects this one.
func rejectRemoteKustomizations(dir string, seen map[string]bool) error {
	if seen[dir] {
		return nil
	}
	seen[dir] = true
	path, ok := kustomizationFile(dir)
	if !ok {
		return nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is the kustomization Forge itself resolved inside the overlay
	if err != nil {
		return err
	}
	var k struct {
		Resources                   []string `yaml:"resources"`
		Components                  []string `yaml:"components"`
		Bases                       []string `yaml:"bases"`
		Generators                  []string `yaml:"generators"`
		Transformers                []string `yaml:"transformers"`
		Validators                  []string `yaml:"validators"`
		Configurations              []string `yaml:"configurations"`
		CRDs                        []string `yaml:"crds"`
		OpenAPI                     any      `yaml:"openapi"`
		HelmCharts                  any      `yaml:"helmCharts"`
		HelmChartInflationGenerator any      `yaml:"helmChartInflationGenerator"`
	}
	if err := yaml.Unmarshal(data, &k); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if k.HelmCharts != nil || k.HelmChartInflationGenerator != nil {
		return fmt.Errorf("%s inflates Helm charts; overlay kustomizations must be local manifests only", path)
	}
	if k.OpenAPI != nil {
		return fmt.Errorf("%s sets openapi; overlay kustomizations use the built-in schema only", path)
	}
	for _, ref := range localRefCandidates(k.Generators, k.Transformers, k.Validators, k.Configurations, k.CRDs) {
		if isRemoteRef(ref) {
			return fmt.Errorf("%s references the remote file %q; overlay kustomizations must be local", path, ref)
		}
	}
	for _, ref := range append(append(k.Resources, k.Components...), k.Bases...) {
		if isRemoteRef(ref) {
			return fmt.Errorf("%s references the remote base %q; overlay kustomizations must be local", path, ref)
		}
		next := filepath.Join(dir, ref)
		if info, err := os.Stat(next); err == nil && info.IsDir() {
			if err := rejectRemoteKustomizations(next, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func localRefCandidates(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

func isRemoteRef(ref string) bool {
	if strings.Contains(ref, "://") || strings.HasPrefix(ref, "git@") {
		return true
	}
	for _, host := range []string{"github.com/", "gitlab.com/", "bitbucket.org/"} {
		if strings.HasPrefix(ref, host) {
			return true
		}
	}
	return false
}

func revision(r *Rendered) string {
	h := sha256.New()
	write := func(label string, data []byte) {
		fmt.Fprintf(h, "%s\x00%d\x00", label, len(data))
		h.Write(data)
	}
	for _, v := range r.ValueFiles {
		write("values:"+v.Name, v.Content)
	}
	write("manifest", r.Manifest)
	write("tools", []byte(r.Tools.Digest))
	write("secrets", r.Secrets)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// SortedRefs returns refs sorted for stable output.
func SortedRefs(refs []ObjectRef) []ObjectRef {
	out := append([]ObjectRef(nil), refs...)
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}
