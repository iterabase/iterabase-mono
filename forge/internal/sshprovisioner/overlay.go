package sshprovisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/deployer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/overlayer"
)

var _ overlayer.Overlayer = (*SSHProvisioner)(nil)

// StageFiles implements overlayer.Overlayer: it replaces dir with exactly the
// given files, written over SSH stdin so their content never appears in a
// command string.
func (p *SSHProvisioner) StageFiles(ctx context.Context, dir string, files []overlayer.File) ([]string, error) {
	for _, f := range files {
		if f.Name == "" || strings.ContainsAny(f.Name, "/\\") {
			return nil, fmt.Errorf("overlay stage file name %q must be a base name", f.Name)
		}
	}
	if _, err := p.run(ctx, "sudo rm -rf "+shellQuote(dir)+" && sudo install -d -m 0700 -o $(id -un) -g $(id -gn) "+shellQuote(dir)); err != nil {
		return nil, fmt.Errorf("overlay stage directory: %w", err)
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		dest := path.Join(dir, f.Name)
		if _, err := p.runStdin(ctx, "umask 077 && cat > "+shellQuote(dest), string(f.Content)); err != nil {
			return nil, fmt.Errorf("overlay stage %s: %w", f.Name, err)
		}
		paths = append(paths, dest)
	}
	return paths, nil
}

// Remove implements overlayer.Overlayer.
func (p *SSHProvisioner) Remove(ctx context.Context, dir string) error {
	if _, err := p.run(ctx, "sudo rm -rf "+shellQuote(dir)); err != nil {
		return fmt.Errorf("overlay remove: %w", err)
	}
	return nil
}

// liveObject is the subset of a live object ApplyOwned and PruneOwned read.
type liveObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Items []liveObject `json:"items"`
}

func (o liveObject) ref() string {
	if o.Metadata.Namespace == "" {
		return o.Kind + "/" + o.Metadata.Name
	}
	return o.Kind + "/" + o.Metadata.Namespace + "/" + o.Metadata.Name
}

// parseLiveObjects reads kubectl get -o json output: a List, one object, or
// nothing.
func parseLiveObjects(out string) ([]liveObject, error) {
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	var obj liveObject
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		return nil, fmt.Errorf("parse kubectl output: %w", err)
	}
	if strings.HasSuffix(obj.Kind, "List") {
		return obj.Items, nil
	}
	return []liveObject{obj}, nil
}

// ApplyOwned implements deployer.Deployer.
func (p *SSHProvisioner) ApplyOwned(ctx context.Context, manifest, ownerLabel, ownerValue string) error {
	out, err := p.runStdin(ctx, kubectlCmd("get", "-f", "-", "-o", "json", "--ignore-not-found"), manifest)
	if err != nil {
		return fmt.Errorf("read live overlay objects: %w", err)
	}
	live, err := parseLiveObjects(out)
	if err != nil {
		return err
	}
	var conflicts []string
	for _, obj := range live {
		if obj.Metadata.Labels[ownerLabel] != ownerValue {
			conflicts = append(conflicts, obj.ref())
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return fmt.Errorf("refusing to overwrite objects not owned by the overlay (%s!=%s): %s; rename the overlay objects or remove the installation-owned ones deliberately", ownerLabel, ownerValue, strings.Join(conflicts, ", "))
	}
	if _, err := p.runStdin(ctx, kubectlCmd("apply", "-f", "-"), manifest); err != nil {
		return fmt.Errorf("kubectl apply -f -: %w", err)
	}
	return nil
}

// pruneMarker separates the per-resource listings of a discovery prune.
const pruneMarker = "### forge-prune-resource "

// PruneOwned implements deployer.Deployer. With no kinds it discovers every
// listable, deletable resource type on the cluster and lists them all in one
// host-side command, so an owned object is found whatever its kind.
func (p *SSHProvisioner) PruneOwned(ctx context.Context, kinds []string, selector string, keep []deployer.ObjectRef) ([]string, error) {
	listings, err := p.ownedListings(ctx, kinds, selector)
	if err != nil {
		return nil, err
	}
	kept := make(map[string]bool, len(keep))
	for _, k := range keep {
		kept[strings.ToLower(k.Kind)+"/"+k.Namespace+"/"+k.Name] = true
	}
	var deleted []string
	for _, l := range listings {
		for _, obj := range l.objects {
			if kept[strings.ToLower(obj.Kind)+"/"+obj.Metadata.Namespace+"/"+obj.Metadata.Name] {
				continue
			}
			args := []string{"delete", l.resource, obj.Metadata.Name, "--ignore-not-found", "--timeout=2m"}
			if obj.Metadata.Namespace != "" {
				args = append(args, "-n", obj.Metadata.Namespace)
			}
			if _, err := p.run(ctx, kubectlCmd(args...)); err != nil {
				return deleted, fmt.Errorf("delete %s: %w", obj.ref(), err)
			}
			deleted = append(deleted, obj.ref())
		}
	}
	return deleted, nil
}

type ownedListing struct {
	resource string
	objects  []liveObject
}

func (p *SSHProvisioner) ownedListings(ctx context.Context, kinds []string, selector string) ([]ownedListing, error) {
	if len(kinds) == 0 {
		return p.discoverOwned(ctx, selector)
	}
	var out []ownedListing
	for _, kind := range kinds {
		raw, err := p.run(ctx, kubectlCmd("get", kind, "--all-namespaces", "-l", selector, "-o", "json", "--ignore-not-found"))
		if err != nil {
			if strings.Contains(err.Error(), "the server doesn't have a resource type") {
				continue
			}
			return nil, fmt.Errorf("list %s: %w", kind, err)
		}
		objects, err := parseLiveObjects(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, ownedListing{resource: kind, objects: objects})
	}
	return out, nil
}

// discoverOwned lists objects matching selector across every listable,
// deletable resource type in one SSH round trip. A type whose listing fails
// (for example an unavailable aggregated API) fails the prune: an owned object
// must never be silently left behind.
func (p *SSHProvisioner) discoverOwned(ctx context.Context, selector string) ([]ownedListing, error) {
	k := kubectlCmd()
	script := "set -eu\n" +
		"for r in $(" + k + " api-resources --verbs=list,delete -o name); do\n" +
		"  printf '%s%s\\n' " + shellQuote(pruneMarker) + " \"$r\"\n" +
		"  " + k + " get \"$r\" --all-namespaces -l " + shellQuote(selector) + " -o json --ignore-not-found\n" +
		"done\n"
	raw, err := p.run(ctx, "bash -c "+shellQuote(script))
	if err != nil {
		return nil, fmt.Errorf("discover owned objects: %w", err)
	}
	return parseDiscovered(raw)
}

// parseDiscovered splits the discovery output into per-resource listings,
// keeping only resources with matching objects.
func parseDiscovered(raw string) ([]ownedListing, error) {
	var out []ownedListing
	for _, chunk := range strings.Split(raw, pruneMarker)[1:] {
		resource, body, _ := strings.Cut(chunk, "\n")
		objects, err := parseLiveObjects(body)
		if err != nil {
			return nil, fmt.Errorf("discover owned %s: %w", resource, err)
		}
		if len(objects) > 0 {
			out = append(out, ownedListing{resource: strings.TrimSpace(resource), objects: objects})
		}
	}
	return out, nil
}

// runStdin runs cmd on the host with stdin supplied. The command string itself
// never contains the stdin data, so secrets can be written to files without
// appearing in ps/history. Mirrors run's context + session handling.
func (p *SSHProvisioner) runStdin(ctx context.Context, cmd, stdin string) (string, error) {
	client, err := p.ensureClient(ctx)
	if err != nil {
		return "", err
	}
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	sess.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if err := sess.Start(cmd); err != nil {
		return "", fmt.Errorf("ssh start %q: %w", cmd, err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return stdout.String(), fmt.Errorf("ssh run %q: %w; stderr: %s", cmd, err, stderr.String())
		}
		return stdout.String(), nil
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return "", ctx.Err()
	}
}
