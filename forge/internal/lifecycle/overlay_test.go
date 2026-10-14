package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/deployer"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/overlay"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/overlayer"
)

const overlayStagePath = "/var/lib/forge/overlay/opo1"

const renderedWorkflowManifest = `apiVersion: platform.iterabase.com/v1alpha1
kind: Workflow
metadata:
  labels:
    app.kubernetes.io/managed-by: forge
    iterabase.com/overlay-part: objects
    iterabase.com/owner: overlay
  name: triage
  namespace: acme
`

// overlaySecretsYAML is a minimal overlay secrets.yaml for the secret-sync tests.
func overlaySecretsYAML() string {
	return `secrets:
  - name: cloudflare-api-token
    namespace: iterabase-system
    key: api-token
    envVar: FORGE_CLOUDFLARE_API_TOKEN
`
}

// testRendered is an operator-rendered overlay with two value files, one
// Workflow instance, an empty tools archive and no secrets.
func testRendered(t *testing.T) *overlay.Rendered {
	t.Helper()
	tools, err := overlay.PackTools(nil)
	require.NoError(t, err)
	return &overlay.Rendered{
		Revision: "sha256:" + strings.Repeat("c", 64),
		ValueFiles: []overlay.ValueFile{
			{Name: "../../bases/hosted/values.yaml", Content: []byte("base: true\n")},
			{Name: "values.yaml", Content: []byte("customer: acme\n")},
		},
		Manifest: []byte(renderedWorkflowManifest),
		Objects: []overlay.ObjectRef{{
			APIVersion: "platform.iterabase.com/v1alpha1", Kind: "Workflow", Namespace: "acme", Name: "triage",
		}},
		Tools: tools,
	}
}

func overlayApplyOpts(rendered *overlay.Rendered) ApplyOpts {
	return ApplyOpts{
		ReadyTimeout: time.Second, ReadyInterval: 10 * time.Millisecond,
		Overlay: rendered,
	}
}

func newOverlayFakes() (*fakeProv, *fakeDeployer, *fakeOverlayer) {
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{order: &d.order}
	return p, d, o
}

func TestApplyOverlay_PhaseOrderAndContent(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	d.pruneOwnedResult = []string{"Workflow/acme/retired"}
	rendered := testRendered(t)
	opts := overlayApplyOpts(rendered)
	opts.ExtraValueFiles = []overlay.ValueFile{{Name: "aws", Content: []byte("hosted: aws\n")}}

	res, err := Apply(context.Background(), testConfigWithChart(), p, d, o, opts)
	require.NoError(t, err)

	// Value files staged once, overlay files first then Forge-derived facts.
	require.Len(t, o.stageCalls, 1)
	assert.Equal(t, overlayStagePath, o.stageCalls[0].dir)
	assert.Equal(t, []overlayer.File{
		{Name: "00-overlay.yaml", Content: []byte("base: true\n")},
		{Name: "01-overlay.yaml", Content: []byte("customer: acme\n")},
		{Name: "02-forge.yaml", Content: []byte("hosted: aws\n")},
	}, o.stageCalls[0].files)
	staged := []string{overlayStagePath + "/00-overlay.yaml", overlayStagePath + "/01-overlay.yaml", overlayStagePath + "/02-forge.yaml"}

	// Ordered delivery: stage → substrates → tools (namespace, archive,
	// pointer) → platform chart → owned objects → prune objects → prune tools.
	assert.Equal(t, []string{
		"stage",
		"metallb-hook-transfer",
		"apply",                            // certificate substrate
		"apply",                            // LVM substrate
		"manifest", "manifest", "manifest", // tools namespace, archive, pointer
		"apply", // platform
		"apply-owned",
		"prune-owned:" + overlay.ObjectsSelector,
		"prune-owned:" + overlay.ToolsSelector,
	}, d.order)

	require.Len(t, d.applyCalls, 3)
	assert.Equal(t, staged, d.applyCalls[0].valueFiles, "certificate substrate receives the staged values")
	platform := d.applyCalls[2]
	assert.Equal(t, "opo1", platform.release)
	assert.Equal(t, staged, platform.valueFiles)
	assert.Equal(t, []string{overlayToolsValue + "=true"}, platform.values)

	// Tools manifests: namespace, immutable digest-named archive, pointer.
	require.Len(t, d.applyManifestCalls, 3)
	assert.Contains(t, d.applyManifestCalls[0], `"kind":"Namespace"`)
	assert.Contains(t, d.applyManifestCalls[1], `"name":"`+rendered.Tools.ArchiveName()+`"`)
	assert.Contains(t, d.applyManifestCalls[1], `"immutable":true`)
	assert.Contains(t, d.applyManifestCalls[2], `"name":"`+overlay.ToolsPointerName+`"`)
	assert.Contains(t, d.applyManifestCalls[2], `"revision":"`+rendered.Revision+`"`)

	require.Len(t, d.applyOwnedCalls, 1)
	assert.Equal(t, applyOwnedCall{manifest: renderedWorkflowManifest, ownerLabel: overlay.OwnerLabel, ownerValue: overlay.OwnerOverlay}, d.applyOwnedCalls[0])

	require.Len(t, d.pruneOwnedCalls, 2)
	objects := d.pruneOwnedCalls[0]
	assert.Equal(t, overlay.ObjectsSelector, objects.selector)
	assert.Nil(t, objects.kinds, "object prune discovers every resource type")
	assert.Equal(t, []deployer.ObjectRef{{Kind: "Workflow", Namespace: "acme", Name: "triage"}}, objects.keep)
	tools := d.pruneOwnedCalls[1]
	assert.Equal(t, overlay.ToolsSelector, tools.selector)
	assert.Equal(t, []string{"configmap"}, tools.kinds)
	assert.ElementsMatch(t, []deployer.ObjectRef{
		{Kind: "ConfigMap", Namespace: overlay.ToolsNamespace, Name: overlay.ToolsPointerName},
		{Kind: "ConfigMap", Namespace: overlay.ToolsNamespace, Name: rendered.Tools.ArchiveName()},
	}, tools.keep)

	assert.True(t, res.OverlayApplied)
	assert.Equal(t, rendered.Revision, res.OverlayRevision)
	assert.Equal(t, rendered.Revision, res.Plan.OverlayRevision)
	assert.Equal(t, rendered.Tools.Digest, res.ToolsDigest)
	assert.Equal(t, []string{"Workflow/acme/retired"}, res.OverlayPruned)
	assert.False(t, res.SecretsApplied, "no secrets.yaml => no secrets")
}

func TestApplyOverlay_KeepsEveryRenderedKind(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	rendered := testRendered(t)
	rendered.Objects = append(rendered.Objects,
		overlay.ObjectRef{APIVersion: "v1", Kind: "ConfigMap", Namespace: "acme", Name: "prompts"},
		overlay.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "acme", Name: "sidecar"},
	)
	_, err := Apply(context.Background(), testConfigWithChart(), p, d, o, overlayApplyOpts(rendered))
	require.NoError(t, err)
	require.NotEmpty(t, d.pruneOwnedCalls)
	assert.Nil(t, d.pruneOwnedCalls[0].kinds, "object prune discovers every resource type")
	assert.Len(t, d.pruneOwnedCalls[0].keep, 3)
}

func TestApplyOverlay_NoObjectsStillPrunes(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	rendered := testRendered(t)
	rendered.Manifest, rendered.Objects = nil, nil
	res, err := Apply(context.Background(), testConfigWithChart(), p, d, o, overlayApplyOpts(rendered))
	require.NoError(t, err)
	assert.Empty(t, d.applyOwnedCalls, "nothing rendered => nothing applied")
	require.Len(t, d.pruneOwnedCalls, 2, "removing the last overlay object still prunes it live")
	assert.Equal(t, overlay.ObjectsSelector, d.pruneOwnedCalls[0].selector)
	assert.Empty(t, d.pruneOwnedCalls[0].keep)
	assert.Nil(t, d.pruneOwnedCalls[0].kinds)
	assert.True(t, res.OverlayApplied)
}

func TestApplyOverlay_SkipOverlayDeliversNothing(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	rendered := testRendered(t)
	rendered.Secrets = []byte(overlaySecretsYAML())
	opts := overlayApplyOpts(rendered)
	opts.SkipOverlay = true
	r := &fakeSecretResolver{value: "v"}
	opts.SecretResolver = r

	res, err := Apply(context.Background(), testConfigWithChart(), p, d, o, opts)
	require.NoError(t, err)
	assert.Empty(t, o.stageCalls, "no value files staged")
	assert.Empty(t, d.applyManifestCalls, "no tools archive and no secrets")
	assert.Empty(t, r.calls)
	assert.Empty(t, d.applyOwnedCalls)
	assert.Empty(t, d.pruneOwnedCalls, "a skipped overlay never prunes live overlay objects")
	require.Len(t, d.applyCalls, 3)
	assert.Empty(t, d.applyCalls[2].valueFiles)
	assert.Equal(t, []string{overlayToolsValue + "=false"}, d.applyCalls[2].values)
	assert.False(t, res.OverlayApplied)
	assert.Empty(t, res.OverlayRevision)
	assert.Empty(t, res.ToolsDigest)
}

func TestApplyOverlay_NoOverlayStagesOnlyForgeValues(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	opts := overlayApplyOpts(nil)
	opts.ExtraValueFiles = []overlay.ValueFile{{Name: "aws", Content: []byte("hosted: aws\n")}}
	res, err := Apply(context.Background(), testConfigWithChart(), p, d, o, opts)
	require.NoError(t, err)
	require.Len(t, o.stageCalls, 1)
	assert.Equal(t, []overlayer.File{{Name: "00-forge.yaml", Content: []byte("hosted: aws\n")}}, o.stageCalls[0].files)
	require.Len(t, d.applyCalls, 3)
	assert.Equal(t, []string{overlayStagePath + "/00-forge.yaml"}, d.applyCalls[2].valueFiles)
	assert.Equal(t, []string{overlayToolsValue + "=false"}, d.applyCalls[2].values)
	assert.Empty(t, d.applyManifestCalls)
	assert.Empty(t, d.applyOwnedCalls)
	// No overlay configured (and not skipped): every overlay-owned object and
	// tools ConfigMap is pruned, so removing the overlay removes them live.
	require.Len(t, d.pruneOwnedCalls, 2)
	assert.Equal(t, overlay.ObjectsSelector, d.pruneOwnedCalls[0].selector)
	assert.Nil(t, d.pruneOwnedCalls[0].kinds, "object prune discovers every resource type")
	assert.Empty(t, d.pruneOwnedCalls[0].keep)
	assert.Equal(t, overlay.ToolsSelector, d.pruneOwnedCalls[1].selector)
	assert.Equal(t, []string{"configmap"}, d.pruneOwnedCalls[1].kinds)
	assert.Empty(t, d.pruneOwnedCalls[1].keep)
	assert.Equal(t, []string{"stage", "metallb-hook-transfer", "apply", "apply", "apply",
		"prune-owned:" + overlay.ObjectsSelector, "prune-owned:" + overlay.ToolsSelector}, d.order,
		"prunes run only after the platform chart dropped the overlay tools source")
	assert.False(t, res.OverlayApplied)
}

func TestApplyOverlay_RemovedOverlayPruneFailureSurfaces(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	d.pruneOwnedErr = errors.New("api unavailable")
	_, err := Apply(context.Background(), testConfigWithChart(), p, d, o, overlayApplyOpts(nil))
	require.ErrorContains(t, err, "prune overlay objects: api unavailable")
}

func TestApplyOverlay_NoValuesNeedsNoOverlayer(t *testing.T) {
	useTempHome(t)
	p, d, _ := newOverlayFakes()
	rendered := testRendered(t)
	rendered.ValueFiles = nil
	_, err := Apply(context.Background(), testConfigWithChart(), p, d, nil, overlayApplyOpts(rendered))
	require.NoError(t, err)
	assert.Empty(t, d.applyCalls[2].valueFiles)

	rendered = testRendered(t)
	_, err = Apply(context.Background(), testConfigWithChart(), p, &fakeDeployer{}, nil, overlayApplyOpts(rendered))
	require.ErrorContains(t, err, "need an overlayer")
}

func TestApplyOverlay_StageFailureStopsBeforeChart(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	o.stageErr = errors.New("disk full")
	_, err := Apply(context.Background(), testConfigWithChart(), p, d, o, overlayApplyOpts(testRendered(t)))
	require.ErrorContains(t, err, "stage overlay values: disk full")
	assert.Empty(t, d.applyCalls)
	assert.Empty(t, d.applyManifestCalls)
}

func TestApplyOverlay_OwnershipConflictStopsBeforePrune(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	d.applyOwnedErr = errors.New("refusing to overwrite objects not owned by the overlay")
	_, err := Apply(context.Background(), testConfigWithChart(), p, d, o, overlayApplyOpts(testRendered(t)))
	require.ErrorContains(t, err, "overlay objects: refusing to overwrite")
	assert.Empty(t, d.pruneOwnedCalls, "never prune after a refused apply")
}

func TestApplyOverlay_PruneFailureSurfaces(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	d.pruneOwnedErr = errors.New("api unavailable")
	res, err := Apply(context.Background(), testConfigWithChart(), p, d, o, overlayApplyOpts(testRendered(t)))
	require.ErrorContains(t, err, "prune overlay objects: api unavailable")
	assert.False(t, res.OverlayApplied)
}

func TestApplyOverlay_ToolsFailureStopsBeforePlatform(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	d.applyManifestErr = errors.New("too large")
	_, err := Apply(context.Background(), testConfigWithChart(), p, d, o, overlayApplyOpts(testRendered(t)))
	require.ErrorContains(t, err, "overlay tools: too large")
	for _, c := range d.applyCalls {
		assert.NotEqual(t, "opo1", c.release, "platform chart must not apply without its tools generation")
	}
}

func TestApplyOverlay_RefusesFluxEraChartsBeforeMutation(t *testing.T) {
	for _, version := range []string{"0.3.0", "0.3.23", "0.4.0", "0.4.9"} {
		t.Run(version, func(t *testing.T) {
			useTempHome(t)
			p, d, o := newOverlayFakes()
			cfg := testConfigWithChart()
			cfg.Spec.Chart.Version = version
			_, err := Apply(context.Background(), cfg, p, d, o, overlayApplyOpts(nil))
			require.ErrorContains(t, err, "reads customer tools from Flux")
			require.ErrorContains(t, err, overlayToolsFirstVersion)
			assert.Empty(t, p.installs, "refused before k3s install")
			assert.Empty(t, p.mutationOrder)
			assert.Empty(t, d.applyCalls)
			assert.Empty(t, o.stageCalls)
		})
	}
}

func TestValidateOverlayChart(t *testing.T) {
	for _, tt := range []struct {
		version   string
		skipChart bool
		wantErr   string
	}{
		{version: ""},
		{version: "0.2.2"},
		{version: "0.3.0", wantErr: "reads customer tools from Flux"},
		{version: "v0.4.0-rc.1", wantErr: "reads customer tools from Flux"},
		{version: "0.4.99", wantErr: "reads customer tools from Flux"},
		{version: "0.3.0", skipChart: true},
		{version: overlayToolsFirstVersion},
		{version: "9.0.0"},
		{version: "not-a-version", wantErr: "invalid chart version"},
	} {
		t.Run(fmt.Sprintf("%s/skip=%t", tt.version, tt.skipChart), func(t *testing.T) {
			cfg := testConfigWithChart()
			cfg.Spec.Chart.Version = tt.version
			err := validateOverlayChart(cfg, ApplyOpts{SkipChart: tt.skipChart})
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestOverlayToolsValues(t *testing.T) {
	rendered := &overlay.Rendered{}
	for _, tt := range []struct {
		version string
		opts    ApplyOpts
		want    []string
	}{
		{version: "0.2.2", opts: ApplyOpts{Overlay: rendered}},
		{version: "0.3.0", opts: ApplyOpts{Overlay: rendered}},
		{version: "", opts: ApplyOpts{Overlay: rendered}},
		{version: overlayToolsFirstVersion, opts: ApplyOpts{Overlay: rendered}, want: []string{overlayToolsValue + "=true"}},
		{version: overlayToolsFirstVersion, opts: ApplyOpts{}, want: []string{overlayToolsValue + "=false"}},
		{version: overlayToolsFirstVersion, opts: ApplyOpts{Overlay: rendered, SkipOverlay: true}, want: []string{overlayToolsValue + "=false"}},
		{version: "9.1.0", opts: ApplyOpts{Overlay: rendered}, want: []string{overlayToolsValue + "=true"}},
	} {
		cfg := testConfigWithChart()
		cfg.Spec.Chart.Version = tt.version
		assert.Equal(t, tt.want, overlayToolsValues(cfg, tt.opts), "version %q opts %+v", tt.version, tt.opts)
	}
}

func TestPlan_RecordsOverlayRevision(t *testing.T) {
	p := &fakeProv{pf: readyPf()}
	plan, err := Plan(context.Background(), testConfig(), p, testRendered(t))
	require.NoError(t, err)
	assert.Equal(t, "sha256:"+strings.Repeat("c", 64), plan.OverlayRevision)

	plan, err = Plan(context.Background(), testConfig(), p, nil)
	require.NoError(t, err)
	assert.Empty(t, plan.OverlayRevision)
}

func TestDestroy_RemovesOverlayStage(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	o := &fakeOverlayer{}
	require.NoError(t, Destroy(context.Background(), testConfigWithChart(), p, &fakeDeployer{}, o))
	assert.Equal(t, []string{overlayStagePath}, o.removeCalls)
}

type fakeSecretResolver struct {
	value string
	unset bool
	calls []resolveCall
}

type resolveCall struct{ name, envVar string }

func (f *fakeSecretResolver) Resolve(_ context.Context, name, envVar string) (string, error) {
	f.calls = append(f.calls, resolveCall{name, envVar})
	if f.unset {
		return "", fmt.Errorf("secret %q: env var %q is unset (set it in the operator's gitignored .env)", name, envVar)
	}
	return f.value, nil
}

func renderedWithSecrets(t *testing.T) *overlay.Rendered {
	r := testRendered(t)
	r.Secrets = []byte(overlaySecretsYAML())
	return r
}

func TestApply_Secrets(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	r := &fakeSecretResolver{value: "supersecret-token"}
	opts := overlayApplyOpts(renderedWithSecrets(t))
	opts.SecretResolver = r
	res, err := Apply(context.Background(), testConfigWithChart(), p, d, o, opts)
	require.NoError(t, err)
	assert.True(t, res.SecretsApplied)

	// the resolver was called for the declared secret (env-or-prompt happens there).
	require.Len(t, r.calls, 1)
	assert.Equal(t, "cloudflare-api-token", r.calls[0].name)
	assert.Equal(t, "FORGE_CLOUDFLARE_API_TOKEN", r.calls[0].envVar)

	// namespace pre-create, then the secret manifest (stringData), via stdin;
	// the three tools manifests follow.
	require.Len(t, d.applyManifestCalls, 5)
	assert.Contains(t, d.applyManifestCalls[0], `"Namespace"`)
	assert.Contains(t, d.applyManifestCalls[0], `"iterabase-system"`)
	assert.Contains(t, d.applyManifestCalls[1], `"Secret"`)
	assert.Contains(t, d.applyManifestCalls[1], `"cloudflare-api-token"`)
	assert.Contains(t, d.applyManifestCalls[1], `"api-token"`)
	assert.Contains(t, d.applyManifestCalls[1], "supersecret-token", "value piped via stdin manifest")

	// the value never reaches helm values or staged value files.
	for _, c := range d.applyCalls {
		for _, v := range c.values {
			assert.NotContains(t, v, "supersecret-token")
		}
	}
	for _, f := range o.stageCalls[0].files {
		assert.NotContains(t, string(f.Content), "supersecret-token")
	}

	// secrets are applied BEFORE any chart so cert-manager finds them on
	// first reconcile.
	manifestIdx, applyIdx := -1, -1
	for i, op := range d.order {
		if op == "manifest" && manifestIdx == -1 {
			manifestIdx = i
		}
		if op == "apply" && applyIdx == -1 {
			applyIdx = i
		}
	}
	assert.GreaterOrEqual(t, manifestIdx, 0)
	assert.GreaterOrEqual(t, applyIdx, 0)
	assert.Less(t, manifestIdx, applyIdx, "secrets applied before chart")
}

func TestApply_Secrets_UnsetEnv(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	opts := overlayApplyOpts(renderedWithSecrets(t))
	opts.SecretResolver = &fakeSecretResolver{unset: true}
	_, err := Apply(context.Background(), testConfigWithChart(), p, d, o, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FORGE_CLOUDFLARE_API_TOKEN")
	assert.Contains(t, err.Error(), "unset")
	assert.Empty(t, d.applyCalls, "chart not applied when a secret value is missing")
	assert.Len(t, d.applyManifestCalls, 1, "namespace pre-created before the resolve fails")
}

func TestApply_SkipSecrets(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	r := &fakeSecretResolver{value: "v"}
	opts := overlayApplyOpts(renderedWithSecrets(t))
	opts.SkipSecrets = true
	opts.SecretResolver = r
	res, err := Apply(context.Background(), testConfigWithChart(), p, d, o, opts)
	require.NoError(t, err)
	assert.False(t, res.SecretsApplied)
	assert.Empty(t, r.calls)
	require.Len(t, d.applyManifestCalls, 3, "only the tools manifests")
	for _, m := range d.applyManifestCalls {
		assert.NotContains(t, m, `"Secret"`)
	}
	assert.True(t, res.OverlayApplied)
}

func TestApply_Secrets_InvalidDeclaration(t *testing.T) {
	useTempHome(t)
	p, d, o := newOverlayFakes()
	rendered := testRendered(t)
	rendered.Secrets = []byte("secrets: [unterminated\n")
	_, err := Apply(context.Background(), testConfigWithChart(), p, d, o, overlayApplyOpts(rendered))
	require.ErrorContains(t, err, "parse overlay secrets.yaml")
	assert.Empty(t, d.applyCalls)
}

// Compile-time proof the fakes satisfy the seams lifecycle orchestrates.
var (
	_ deployer.Deployer   = (*fakeDeployer)(nil)
	_ overlayer.Overlayer = (*fakeOverlayer)(nil)
)
