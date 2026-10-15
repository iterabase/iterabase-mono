// Package deployer defines the cluster-level manifest operations interface
// (Helm + kubectl) — the testability seam for installing/upgrading/uninstalling
// Helm releases (the iterabase-platform chart, the NVIDIA GPU Operator, etc.) and
// applying the operator-rendered overlay's owned objects. The real implementation
// lives in internal/sshprovisioner (helm + k3s kubectl run on the host over SSH,
// sharing the same transport as the k3s Provisioner); tests use fakes. Lifecycle
// logic never talks to SSH, helm, or kubectl directly.
package deployer

import "context"

// ChartState is the reconciled state of the platform chart release, read for
// status reporting. A release that does not exist is {Installed: false}.
type ChartState struct {
	Installed bool   // a helm release exists for this name/namespace
	Status    string // helm status: deployed/failed/pending-upgrade/pending-install/...
	Version   string // installed chart version (semver)
}

// ApplyOpts configures a Helm release install/upgrade (helm upgrade --install).
type ApplyOpts struct {
	Release    string   // helm release name
	Repository string   // chart ref (OCI URL, e.g. oci://.../iterabase-platform, or repo/name)
	Version    string   // chart version (semver)
	Checksum   string   // optional repository-reviewed archive SHA-256; required for mutable repo indexes
	Namespace  string   // target namespace (--create-namespace)
	Values     []string // --set inline values (e.g. GPU operator overrides)
	ValueFiles []string // -f value files, applied in order (later wins); overlay values
	Timeout    string   // Helm operation timeout (default 10m); long stateful conformance gates set an explicit bound
}

// ObjectRef identifies one Kubernetes object by kind, namespace and name.
type ObjectRef struct {
	Kind      string
	Namespace string
	Name      string
}

// Deployer abstracts cluster-level manifest operations (Helm + kustomize). One
// instance is bound to a single host (the same host the Provisioner bootstrapped
// k3s on); helm + k3s kubectl run there over SSH using the k3s kubeconfig.
type Deployer interface {
	// Apply idempotently reconciles CRDs bundled in the pinned chart artifact,
	// waits for them to become Established, then installs or upgrades the Helm
	// release (helm upgrade --install), applying -f value files (ValueFiles, in
	// order) then --set values (Values). It ensures helm is present first.
	// After a successful upgrade it deletes objects this release created in a
	// failed revision and no longer renders, which Helm's previous-revision
	// baseline never deletes (DES-HOR-677-02).
	Apply(ctx context.Context, opts ApplyOpts) error
	// EnsureRepo adds (or force-updates) a Helm repository on the host. Needed
	// for repo-based charts (e.g. the NVIDIA GPU Operator); a no-op concern for
	// OCI charts. Idempotent.
	EnsureRepo(ctx context.Context, name, url string) error
	// Status reads the helm release state. A missing release is not an error.
	Status(ctx context.Context, release, namespace string) (*ChartState, error)
	// TransferMetalLBHookOwnership adopts the IPAddressPool and L2Advertisement
	// resources the pre-DES-HOR-511 platform created as Helm post-install hooks
	// into the platform release before it renders them as ordinary resources.
	// It annotates only this release's objects, adding release-ownership metadata
	// and dropping the stale hook annotations so Helm upgrades them in place
	// (preserving object UIDs) instead of deleting and recreating them. It is
	// idempotent and non-destructive: it changes only ownership/hook metadata and
	// is a no-op when no such objects exist.
	TransferMetalLBHookOwnership(ctx context.Context, release, namespace string) error
	// UninstallChart removes the Helm release. A missing release is an
	// idempotent success; command, hook, and uninstall failures are propagated.
	UninstallChart(ctx context.Context, release, namespace string) error
	// ApplyOwned applies manifest (every object labelled owner=ownerValue) with
	// kubectl apply over SSH stdin. It first refuses the whole manifest if any
	// object already exists live without that owner label: overlay objects never
	// overwrite installation-owned objects (DES-HOR-593-03).
	ApplyOwned(ctx context.Context, manifest string, ownerLabel, ownerValue string) error
	// PruneOwned deletes live objects matching selector that are not in keep,
	// and returns what it deleted. kinds are kubectl resource names (e.g.
	// configmap); kinds unknown to the cluster are skipped. With no kinds it
	// searches every listable, deletable resource type.
	PruneOwned(ctx context.Context, kinds []string, selector string, keep []ObjectRef) ([]string, error)
	// ApplyManifest applies a Kubernetes manifest (JSON/YAML) via `kubectl apply
	// -f -` over SSH stdin, so secret values never appear in the command string
	// or ps. Used by the secret-sync phase to ensure namespaces + materialize
	// Secrets from operator-local env vars. Idempotent (kubectl apply).
	ApplyManifest(ctx context.Context, manifest string) error
}
