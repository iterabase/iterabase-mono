package sshprovisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Helm ownership metadata on every object a release manages.
const (
	helmReleaseNameAnnotation      = "meta.helm.sh/release-name"
	helmReleaseNamespaceAnnotation = "meta.helm.sh/release-namespace"
	helmResourcePolicyAnnotation   = "helm.sh/resource-policy"
	helmResourcePolicyKeep         = "keep"
)

// manifestRef identifies one object a Helm revision rendered.
type manifestRef struct {
	APIVersion string
	Kind       string
	Namespace  string // empty when the manifest leaves it to the release namespace
	Name       string
}

// key ignores the API version so an object rendered under different versions
// across revisions (for example v1beta1 then v1) is still one object.
func (r manifestRef) key() string {
	group := "" // the core group's apiVersion is just "v1"
	if g, _, ok := strings.Cut(r.APIVersion, "/"); ok {
		group = g
	}
	return group + "|" + r.Kind + "|" + r.Namespace + "|" + r.Name
}

// pruneFailedRevisionOrphans deletes objects that this release rendered in a
// failed revision but no longer renders now (DES-HOR-677-02). Helm computes an
// upgrade's deletions from the previous revision's manifest, even a failed one,
// so an object created by a failed revision and missing from the next attempt's
// manifest is never deleted: it survives every later successful revision.
// Called only after a successful upgrade, it deletes what Helm would have
// deleted from an accurate baseline: objects whose live ownership annotations
// still name this release and that do not ask Helm to keep them.
func (p *SSHProvisioner) pruneFailedRevisionOrphans(ctx context.Context, release, namespace string) ([]string, error) {
	candidates, err := p.failedRevisionLeftovers(ctx, release, namespace)
	if err != nil || len(candidates) == 0 {
		return nil, err
	}
	live, err := p.liveObjects(ctx, candidates, namespace)
	if err != nil {
		return nil, fmt.Errorf("read objects from failed revisions of %s/%s: %w", namespace, release, err)
	}
	orphans, deleted := releaseOwned(live, release, namespace)
	if len(orphans) == 0 {
		return nil, nil
	}
	if _, err := p.runStdin(ctx, kubectlCmd("delete", "-f", "-", "--ignore-not-found", "--timeout=2m"), objectList(orphans)); err != nil {
		return nil, fmt.Errorf("delete objects orphaned by failed revisions of %s/%s (%s): %w", namespace, release, strings.Join(deleted, ", "), err)
	}
	return deleted, nil
}

// failedRevisionLeftovers returns the objects the release's failed revisions
// rendered that its current revision does not.
func (p *SSHProvisioner) failedRevisionLeftovers(ctx context.Context, release, namespace string) ([]manifestRef, error) {
	out, err := p.run(ctx, helmCmd("history", release, "-n", namespace, "-o", "json"))
	if err != nil {
		return nil, fmt.Errorf("helm history %s/%s: %w", namespace, release, err)
	}
	failed, err := failedRevisions(out)
	if err != nil || len(failed) == 0 {
		return nil, err
	}
	current, err := p.revisionManifest(ctx, release, namespace, 0)
	if err != nil {
		return nil, err
	}
	rendered := make(map[string]bool, len(current))
	for _, ref := range current {
		rendered[ref.key()] = true
	}
	leftovers := map[string]manifestRef{}
	for _, revision := range failed {
		refs, err := p.revisionManifest(ctx, release, namespace, revision)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			if !rendered[ref.key()] {
				leftovers[ref.key()] = ref
			}
		}
	}
	refs := make([]manifestRef, 0, len(leftovers))
	for _, ref := range leftovers {
		refs = append(refs, ref)
	}
	return refs, nil
}

// liveObjects reads the refs that still exist. Refs without an explicit
// namespace live in the release namespace (namespaced kinds) or are
// cluster-scoped; kubectl resolves which from -n.
func (p *SSHProvisioner) liveObjects(ctx context.Context, refs []manifestRef, namespace string) ([]liveObject, error) {
	var explicit, implicit []manifestRef
	for _, ref := range refs {
		if ref.Namespace == "" {
			implicit = append(implicit, ref)
		} else {
			explicit = append(explicit, ref)
		}
	}
	var live []liveObject
	for _, group := range []struct {
		refs []manifestRef
		args []string
	}{
		{explicit, nil},
		{implicit, []string{"-n", namespace}},
	} {
		if len(group.refs) == 0 {
			continue
		}
		args := append([]string{"get", "-f", "-", "-o", "json", "--ignore-not-found"}, group.args...)
		raw, err := p.runStdin(ctx, kubectlCmd(args...), objectList(group.refs))
		if err != nil {
			return nil, err
		}
		objects, err := parseLiveObjects(raw)
		if err != nil {
			return nil, err
		}
		live = append(live, objects...)
	}
	return live, nil
}

// releaseOwned keeps the live objects whose Helm ownership annotations still
// name this release and that do not ask Helm to keep them. Objects since
// adopted by another owner are not this release's to delete.
func releaseOwned(live []liveObject, release, namespace string) ([]manifestRef, []string) {
	var owned []manifestRef
	var refs []string
	for _, obj := range live {
		a := obj.Metadata.Annotations
		if a[helmReleaseNameAnnotation] != release || a[helmReleaseNamespaceAnnotation] != namespace ||
			a[helmResourcePolicyAnnotation] == helmResourcePolicyKeep {
			continue
		}
		owned = append(owned, manifestRef{APIVersion: obj.APIVersion, Kind: obj.Kind, Namespace: obj.Metadata.Namespace, Name: obj.Metadata.Name})
		refs = append(refs, obj.ref())
	}
	sort.Strings(refs)
	return owned, refs
}

// failedRevisions returns the failed revisions in `helm history -o json`.
func failedRevisions(out string) ([]int, error) {
	var history []struct {
		Revision int    `json:"revision"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &history); err != nil {
		return nil, fmt.Errorf("parse helm history: %w", err)
	}
	var failed []int
	for _, h := range history {
		if h.Status == "failed" {
			failed = append(failed, h.Revision)
		}
	}
	return failed, nil
}

// revisionManifest reads the objects one revision rendered; revision 0 reads
// the current revision. Hooks are not part of a revision's manifest.
func (p *SSHProvisioner) revisionManifest(ctx context.Context, release, namespace string, revision int) ([]manifestRef, error) {
	args := []string{"get", "manifest", release, "-n", namespace}
	if revision > 0 {
		args = append(args, "--revision", strconv.Itoa(revision))
	}
	out, err := p.run(ctx, helmCmd(args...))
	if err != nil {
		return nil, fmt.Errorf("helm get manifest %s/%s revision %d: %w", namespace, release, revision, err)
	}
	refs, err := parseManifestRefs(out)
	if err != nil {
		return nil, fmt.Errorf("parse manifest of %s/%s revision %d: %w", namespace, release, revision, err)
	}
	return refs, nil
}

// parseManifestRefs reads the object identities in a multi-document manifest.
func parseManifestRefs(manifest string) ([]manifestRef, error) {
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	var refs []manifestRef
	for {
		var doc struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return refs, nil
			}
			return nil, err
		}
		if doc.Kind == "" || doc.Metadata.Name == "" {
			continue // empty document
		}
		refs = append(refs, manifestRef{APIVersion: doc.APIVersion, Kind: doc.Kind, Namespace: doc.Metadata.Namespace, Name: doc.Metadata.Name})
	}
}

// objectList renders identity-only objects as a kubectl List for stdin, so
// kubectl resolves each kind and scope itself.
func objectList(refs []manifestRef) string {
	sort.Slice(refs, func(i, j int) bool { return refs[i].key() < refs[j].key() })
	items := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		metadata := map[string]any{"name": ref.Name}
		if ref.Namespace != "" {
			metadata["namespace"] = ref.Namespace
		}
		items = append(items, map[string]any{"apiVersion": ref.APIVersion, "kind": ref.Kind, "metadata": metadata})
	}
	b, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	return string(b)
}
