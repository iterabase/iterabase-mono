package lifecycle

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/deployer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/fluxer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/k3s"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/overlayer"
	"golang.org/x/crypto/ssh"
)

// Flux sync resource names + constants. v1 single-node => one install per
// cluster, so fixed names in the flux-system namespace are unambiguous.
const (
	fluxNamespace       = "flux-system"
	fluxSourceName      = "overlay"          // GitRepository name (source-controller fetches the fork)
	fluxKustomizeName   = "overlay-crds"     // Kustomization name (reconciles crds/client)
	fluxTokenSecretName = "overlay-git-auth" //nolint:gosec // resource name, not a credential (GitRepository secretRef)
	fluxSSHSecretName   = "overlay-git-ssh"  //nolint:gosec // resource name, not a credential (identity + known_hosts for a file:// overlay)
	fluxGitUsername     = "git"              // generic https username (GitHub ignores it; password is the PAT)
	fluxInterval        = "1m"               // GitRepository + Kustomization poll interval
	fluxCRDPath         = "./crds/client"    // Kustomization path (the overlay's CRD instances)
)

// semverTagRe matches a flux2-style version tag (vX.Y.Z[-pre]); used to decide
// whether overlay.ref is a tag (Flux ref.tag) or a branch (Flux ref.branch).
// forge's overlay.ref accepts both (git clone --branch works for either); Flux's
// GitRepository distinguishes the two.
var (
	semverTagRe          = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z-.]+)?$`)
	canonicalSHA256HexRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// applyFluxSourcePhase establishes the exact source-controller artifact before
// Helm starts the tool runner. This breaks the bootstrap cycle in which Helm
// waits for a runner generation but Forge previously created the GitRepository
// only after the chart. Forge gates on metadata only and never downloads or
// parses the artifact.
func applyFluxSourcePhase(ctx context.Context, cfg *config.Cluster, o overlayer.Overlayer, f fluxer.Fluxer, d deployer.Deployer, opts ApplyOpts, res *Result, expectedCommit string) error {
	if !cfg.Spec.Flux.Enabled || opts.SkipFlux {
		return nil
	}
	if f == nil {
		return fmt.Errorf("flux.enabled is set but no fluxer is wired (internal error)")
	}
	if err := f.EnsureFlux(ctx, cfg.Spec.Flux.Version); err != nil {
		auditFail(cfg, "apply-flux", err)
		return fmt.Errorf("flux install: %w", err)
	}

	url, secretRef, err := applyFluxSourceAccess(ctx, cfg, o, d, opts)
	if err != nil {
		auditFail(cfg, "apply-flux", err)
		return err
	}
	repo := gitRepositoryManifest(fluxSourceName, fluxNamespace, url, cfg.Spec.Overlay.Ref, secretRef)
	if err := d.ApplyManifest(ctx, repo); err != nil {
		auditFail(cfg, "apply-flux", err)
		return fmt.Errorf("flux gitrepository: %w", err)
	}

	// source-controller's artifact server is default-deny. Authorize only the
	// chart's credential-split materializer before its pod starts.
	if cfg.Spec.Chart.Namespace != "" {
		policy := sourceArtifactNetworkPolicyManifest(fluxNamespace, cfg.Spec.Chart.Namespace)
		if err := d.ApplyManifest(ctx, policy); err != nil {
			auditFail(cfg, "apply-flux", err)
			return fmt.Errorf("flux artifact network policy: %w", err)
		}
	}

	artifact, err := waitForFluxArtifact(ctx, f, expectedCommit, opts.ReadyTimeout, opts.ReadyInterval)
	if err != nil {
		auditFail(cfg, "apply-flux", err)
		return err
	}
	res.FluxInstalled = true
	res.GitRepositoryStatus = fmt.Sprintf("ready=True revision=%s digest=%s", artifact.Revision, artifact.Digest)
	return nil
}

// applyFluxSourceAccess gives source-controller a way to read the overlay and
// returns the GitRepository URL and Secret. An https fork is read directly
// (with a token Secret when one was resolved; public repos clone anonymously).
// A file:// overlay is served from the node over read-only SSH with a fresh
// Forge-generated key (DES-HOR-632-01). Secrets go through stdin so no
// credential appears in a command string or process list.
func applyFluxSourceAccess(ctx context.Context, cfg *config.Cluster, o overlayer.Overlayer, d deployer.Deployer, opts ApplyOpts) (url, secretRef string, err error) {
	if !strings.HasPrefix(cfg.Spec.Overlay.Repo, "file://") {
		if len(opts.OverlayToken) == 0 {
			return cfg.Spec.Overlay.Repo, "", nil
		}
		sec := fluxTokenSecretManifest(fluxTokenSecretName, fluxNamespace, fluxGitUsername, opts.OverlayToken)
		if err := d.ApplyManifest(ctx, sec); err != nil {
			return "", "", fmt.Errorf("flux token secret: %w", err)
		}
		return cfg.Spec.Overlay.Repo, fluxTokenSecretName, nil
	}
	if o == nil {
		return "", "", fmt.Errorf("a file:// overlay with flux needs an overlayer (internal error)")
	}
	key, err := newFluxSSHKey()
	if err != nil {
		return "", "", err
	}
	source, err := o.ServeToFlux(ctx, cfg.Spec.Overlay.Repo, cfg.Spec.Overlay.Ref, key.publicKey, podCIDRs(cfg.Spec.K3s))
	if err != nil {
		return "", "", err
	}
	sec := fluxSSHSecretManifest(fluxSSHSecretName, fluxNamespace, key, source.KnownHosts)
	if err := d.ApplyManifest(ctx, sec); err != nil {
		return "", "", fmt.Errorf("flux ssh secret: %w", err)
	}
	return source.URL, fluxSSHSecretName, nil
}

// podCIDRs are the source addresses source-controller connects from.
func podCIDRs(k config.K3s) []string {
	return strings.Split(k3s.DesiredClusterCIDR(k), ",")
}

type fluxSSHKey struct {
	privateKey string // OpenSSH PEM
	publicKey  string // authorized_keys line
}

func newFluxSSHKey() (fluxSSHKey, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fluxSSHKey{}, fmt.Errorf("generate flux overlay key: %w", err)
	}
	const comment = "forge-flux-overlay"
	block, err := ssh.MarshalPrivateKey(private, comment)
	if err != nil {
		return fluxSSHKey{}, fmt.Errorf("encode flux overlay key: %w", err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		return fluxSSHKey{}, fmt.Errorf("encode flux overlay public key: %w", err)
	}
	return fluxSSHKey{
		privateKey: string(pem.EncodeToMemory(block)),
		publicKey:  strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublic))) + " " + comment,
	}, nil
}

// fluxSSHSecretManifest renders the Flux SSH auth Secret: identity,
// identity.pub and known_hosts (source-controller's documented keys).
func fluxSSHSecretManifest(name, namespace string, key fluxSSHKey, knownHosts string) string {
	m := fluxTokenSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Type:       "Opaque",
		Metadata:   fluxMeta{Name: name, Namespace: namespace},
		StringData: map[string]string{
			"identity":     key.privateKey,
			"identity.pub": key.publicKey,
			"known_hosts":  knownHosts,
		},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// applyFluxReconciliationPhase starts continuous reconciliation only after the
// chart has established the CRDs targeted by crds/client. The GitRepository was
// already made Ready by applyFluxSourcePhase.
func applyFluxReconciliationPhase(ctx context.Context, cfg *config.Cluster, d deployer.Deployer, opts ApplyOpts) error {
	if !cfg.Spec.Flux.Enabled || opts.SkipFlux {
		return nil
	}
	kust := kustomizationManifest(fluxKustomizeName, fluxNamespace, fluxSourceName, fluxCRDPath)
	if err := d.ApplyManifest(ctx, kust); err != nil {
		auditFail(cfg, "apply-flux", err)
		return fmt.Errorf("flux kustomization: %w", err)
	}
	return nil
}

// validateChartFluxSource prevents Helm from starting a 0.3+ tool runner with
// no generation source. A skipped/disabled Flux phase is supported only on
// re-entry when the exact source artifact is already established.
func validateChartFluxSource(ctx context.Context, cfg *config.Cluster, f fluxer.Fluxer, d deployer.Deployer, opts ApplyOpts, expectedCommit string) error {
	if d == nil || opts.SkipChart || cfg.Spec.Chart.Version == "" {
		return nil
	}
	requiresSource, err := chartVersionAtLeast(cfg.Spec.Chart.Version, fluxArtifactFirstVersion)
	if err != nil {
		return err
	}
	if !requiresSource || (cfg.Spec.Flux.Enabled && !opts.SkipFlux) {
		return nil
	}
	if f == nil {
		return fmt.Errorf("platform chart %q requires an exact Flux artifact before Helm; enable flux or provide an already-established source", cfg.Spec.Chart.Version)
	}
	artifact, err := f.GitRepositoryArtifact(ctx, fluxSourceName)
	if err != nil {
		return fmt.Errorf("read existing Flux GitRepository artifact: %w", err)
	}
	if fluxArtifactMatches(artifact, expectedCommit) {
		return nil
	}
	return fmt.Errorf("platform chart %q requires an exact Ready Flux artifact before Helm; enable flux and do not use --skip-flux for a fresh install", cfg.Spec.Chart.Version)
}

func fluxArtifactMatches(artifact fluxer.GitRepositoryArtifact, expectedCommit string) bool {
	return artifact.Ready && canonicalSHA256HexRe.MatchString(artifact.Digest) &&
		(expectedCommit == "" || strings.HasSuffix(artifact.Revision, ":"+expectedCommit))
}

func waitForFluxArtifact(ctx context.Context, f fluxer.Fluxer, expectedCommit string, timeout, interval time.Duration) (fluxer.GitRepositoryArtifact, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		artifact, err := f.GitRepositoryArtifact(ctx, fluxSourceName)
		if err != nil {
			return fluxer.GitRepositoryArtifact{}, fmt.Errorf("read Flux GitRepository artifact: %w", err)
		}
		if fluxArtifactMatches(artifact, expectedCommit) {
			return artifact, nil
		}

		select {
		case <-ctx.Done():
			return fluxer.GitRepositoryArtifact{}, ctx.Err()
		case <-deadline.C:
			return fluxer.GitRepositoryArtifact{}, fmt.Errorf("flux GitRepository %q did not publish Ready commit %q with a canonical sha256 digest within %s", fluxSourceName, expectedCommit, timeout)
		case <-ticker.C:
		}
	}
}

type networkPolicy struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   fluxMeta          `json:"metadata"`
	Spec       networkPolicySpec `json:"spec"`
}

type networkPolicySpec struct {
	PodSelector map[string]map[string]string `json:"podSelector"`
	PolicyTypes []string                     `json:"policyTypes"`
	Ingress     []networkPolicyIngress       `json:"ingress"`
}

type networkPolicyIngress struct {
	From  []networkPolicyPeer `json:"from"`
	Ports []networkPolicyPort `json:"ports"`
}

type networkPolicyPeer struct {
	NamespaceSelector map[string]map[string]string `json:"namespaceSelector"`
	PodSelector       map[string]map[string]string `json:"podSelector"`
}

type networkPolicyPort struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
}

func sourceArtifactNetworkPolicyManifest(namespace, consumerNamespace string) string {
	labels := func(values map[string]string) map[string]map[string]string {
		return map[string]map[string]string{"matchLabels": values}
	}
	policy := networkPolicy{
		APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy",
		Metadata: fluxMeta{Name: "allow-tool-materializer", Namespace: namespace},
		Spec: networkPolicySpec{
			// Flux v2.4 labels the source-controller Pod template with `app`,
			// while app.kubernetes.io/component is only present on its Service.
			// NetworkPolicy selects Pods, so use the workload label.
			PodSelector: labels(map[string]string{"app": "source-controller"}),
			PolicyTypes: []string{"Ingress"},
			Ingress: []networkPolicyIngress{{
				From: []networkPolicyPeer{{
					NamespaceSelector: labels(map[string]string{"kubernetes.io/metadata.name": consumerNamespace}),
					PodSelector:       labels(map[string]string{"app.kubernetes.io/component": "tool-runner"}),
				}},
				Ports: []networkPolicyPort{{Protocol: "TCP", Port: 9090}},
			}},
		},
	}
	b, _ := json.Marshal(policy)
	return string(b)
}

// fluxMeta is the shared metadata block for the Flux sync resources.
type fluxMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// fluxTokenSecret is a Secret holding the overlay git token (stringData) for
// Flux's GitRepository secretRef. Marshaled to JSON + piped via stdin (kubectl
// apply -f -) so the token never appears in a command string or ps — mirrors
// secretManifest in secrets.go.
type fluxTokenSecret struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Type       string            `json:"type"`
	Metadata   fluxMeta          `json:"metadata"`
	StringData map[string]string `json:"stringData"`
}

// fluxTokenSecretManifest renders the token Secret as JSON. The token is in
// stringData (kubectl stores it base64-encoded in .data).
func fluxTokenSecretManifest(name, namespace, username string, password []byte) string {
	m := fluxTokenSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Type:       "Opaque",
		Metadata:   fluxMeta{Name: name, Namespace: namespace},
		StringData: map[string]string{"username": username, "password": string(password)},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// gitRepository is a Flux source.toolkit.fluxcd.io/v1 GitRepository.
type gitRepository struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   fluxMeta    `json:"metadata"`
	Spec       gitRepoSpec `json:"spec"`
}

type gitRepoSpec struct {
	URL       string     `json:"url"`
	Ref       gitRepoRef `json:"ref"`
	Interval  string     `json:"interval"`
	SecretRef *fluxRef   `json:"secretRef,omitempty"`
}

// gitRepoRef selects branch or tag. A semver-looking ref (vX.Y.Z[-pre]) is a
// tag; otherwise a branch. (Flux also supports ref.semver/commit, not needed in
// v1.)
type gitRepoRef struct {
	Branch string `json:"branch,omitempty"`
	Tag    string `json:"tag,omitempty"`
}

type fluxRef struct {
	Name string `json:"name"`
}

// gitRepositoryManifest renders the GitRepository as JSON. secretRef is omitted
// (nil) for a public repo — Flux clones anonymously.
func gitRepositoryManifest(name, namespace, url, ref, secretRef string) string {
	r := gitRepository{
		APIVersion: "source.toolkit.fluxcd.io/v1",
		Kind:       "GitRepository",
		Metadata:   fluxMeta{Name: name, Namespace: namespace},
		Spec: gitRepoSpec{
			URL:      url,
			Ref:      fluxRefFor(ref),
			Interval: fluxInterval,
		},
	}
	if secretRef != "" {
		r.Spec.SecretRef = &fluxRef{Name: secretRef}
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// fluxRefFor maps overlay.ref to a Flux GitRepository ref: a semver tag
// (vX.Y.Z[-pre]) → ref.tag, else ref.branch.
func fluxRefFor(ref string) gitRepoRef {
	if semverTagRe.MatchString(ref) {
		return gitRepoRef{Tag: ref}
	}
	return gitRepoRef{Branch: ref}
}

// kustomization is a Flux kustomize.toolkit.fluxcd.io/v1 Kustomization.
type kustomization struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   fluxMeta          `json:"metadata"`
	Spec       kustomizationSpec `json:"spec"`
}

type kustomizationSpec struct {
	SourceRef kustomizationSource `json:"sourceRef"`
	Path      string              `json:"path"`
	Prune     bool                `json:"prune"`
	Wait      bool                `json:"wait"`
	Interval  string              `json:"interval"`
}

type kustomizationSource struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// kustomizationManifest renders the Kustomization as JSON. prune=true makes Flux
// the mirror authority (removals from the repo are GC'd); wait=true gates
// reconcile on health.
func kustomizationManifest(name, namespace, sourceName, path string) string {
	k := kustomization{
		APIVersion: "kustomize.toolkit.fluxcd.io/v1",
		Kind:       "Kustomization",
		Metadata:   fluxMeta{Name: name, Namespace: namespace},
		Spec: kustomizationSpec{
			SourceRef: kustomizationSource{Kind: "GitRepository", Name: sourceName},
			Path:      path,
			Prune:     true,
			Wait:      true,
			Interval:  fluxInterval,
		},
	}
	b, _ := json.Marshal(k)
	return string(b)
}
