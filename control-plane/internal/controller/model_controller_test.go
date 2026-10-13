package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/api/v1alpha1"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/catalog"
)

// hor559ModelName is the Model used by the HOR-559 self-trigger regression
// test. The reconcile counter is keyed to it so fallback requeues of other
// Models in this test cannot perturb the count.
const hor559ModelName = "m-hor559"

// TestModelReconcile exercises the Model reconciler UNDER RBAC: materializing
// Model CRs into catalog.models, deriving availability from the referenced
// ModelBackend (via the ModelBackend watch), and exposing the effective_catalog
// view join (Model -> ModelBackend) the gateway (HOR-247) reads. The ModelBackend
// reconciler is not run here; backends are created as CRs with status patched +
// catalog.backends seeded directly. Requires Docker + KUBEBUILDER_ASSETS.
func TestModelReconcile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run envtest (make setup-envtest)")
	}

	store := newCatalogStore(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	testEnv := &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
		},
	}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = testEnv.Stop() })

	adminClient, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	saCfg := rbacManagerConfig(t, ctx, cfg, scheme)

	mgr, err := ctrl.NewManager(saCfg, ctrl.Options{Scheme: scheme})
	require.NoError(t, err)
	tracking := &primaryGetCounter[*v1alpha1.Model]{Client: mgr.GetClient(), name: hor559ModelName}
	require.NoError(t, (&ModelReconciler{
		Client: tracking,
		Scheme: scheme,
		Store:  store,
	}).SetupWithManager(mgr))

	mgrCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(mgrCtx) }()

	// Models are created BEFORE their backend so the ModelBackend watch (fired by
	// the backend create/status-patch) re-reconciles the model once the backend's
	// status is in the cache — avoiding a stale-cache race on the first pass.
	createModel := func(t *testing.T, name, modelID, backendRef string, thinking *bool) *v1alpha1.Model {
		t.Helper()
		m := &v1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: v1alpha1.ModelSpec{
				ModelID:         modelID,
				BackendRef:      backendRef,
				Transforms:      v1alpha1.ModelTransforms{RewriteModelName: true},
				ReasoningConfig: v1alpha1.ModelReasoningConfig{EnableThinking: thinking},
			},
		}
		require.NoError(t, adminClient.Create(ctx, m))
		return m
	}
	createBackend := func(t *testing.T, name, model string, deployed, healthy bool) {
		t.Helper()
		mb := &v1alpha1.ModelBackend{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       v1alpha1.ModelBackendSpec{Kind: "vLLM", Model: model},
		}
		require.NoError(t, adminClient.Create(ctx, mb))
		base := mb.DeepCopy()
		mb.Status.Deployed = deployed
		mb.Status.Healthy = healthy
		require.NoError(t, adminClient.Status().Patch(ctx, mb, client.MergeFrom(base)))
	}
	seedBackendRow := func(t *testing.T, name, model, serviceURL string, deployed, healthy bool) {
		t.Helper()
		_, err := store.UpsertBackend(ctx, catalog.Backend{
			Key: "default/" + name, Name: name, Namespace: "default", Kind: "vLLM",
			Model: model, ServiceURL: serviceURL, Deployed: deployed, Healthy: healthy,
		})
		require.NoError(t, err)
	}
	setBackendHealth := func(t *testing.T, name string, deployed, healthy bool) {
		t.Helper()
		var mb v1alpha1.ModelBackend
		require.NoError(t, adminClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &mb))
		base := mb.DeepCopy()
		mb.Status.Deployed = deployed
		mb.Status.Healthy = healthy
		require.NoError(t, adminClient.Status().Patch(ctx, &mb, client.MergeFrom(base)))
	}
	// waitReconciled waits until the reconciler has processed the current
	// generation AND availability matches want. Gating on observedGeneration
	// avoids matching the zero value for the unavailable case.
	waitReconciled := func(t *testing.T, name string, wantAvailable bool) {
		t.Helper()
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		require.Eventually(t, func() bool {
			var got v1alpha1.Model
			if err := adminClient.Get(ctx, nn, &got); err != nil {
				return false
			}
			return got.Status.ObservedGeneration == got.Generation && got.Status.Available == wantAvailable
		}, 30*time.Second, 200*time.Millisecond, "Model %s should be reconciled with available=%v", name, wantAvailable)
	}
	findEntry := func(modelID string) (catalog.CatalogEntry, bool) {
		entries, err := store.EffectiveCatalog(ctx)
		require.NoError(t, err)
		for _, e := range entries {
			if e.ModelID == modelID {
				return e, true
			}
		}
		return catalog.CatalogEntry{}, false
	}

	t.Run("available when backend healthy + view join", func(t *testing.T) {
		off := false
		createModel(t, "m-healthy", "qwen3-27b", "be-healthy", &off)
		createBackend(t, "be-healthy", "Qwen/Qwen3-27B", true, true)
		seedBackendRow(t, "be-healthy", "Qwen/Qwen3-27B", "be-healthy.default.svc:8000", true, true)
		waitReconciled(t, "m-healthy", true)

		// Materialized in catalog.models.
		m, err := store.GetModelByKey(ctx, "default/m-healthy")
		require.NoError(t, err)
		assert.Equal(t, "qwen3-27b", m.ModelID)
		assert.Equal(t, "be-healthy", m.BackendRef)
		assert.True(t, m.Available)

		// The view joins Model -> ModelBackend with the rewrite id + url.
		e, ok := findEntry("qwen3-27b")
		require.True(t, ok, "effective_catalog should expose the model")
		assert.Equal(t, "be-healthy.default.svc:8000", e.BackendURL)
		assert.Equal(t, "Qwen/Qwen3-27B", e.BackendModelID, "backend_model_id is the HF id the gateway rewrites to")
		assert.Equal(t, "vLLM", e.BackendKind)
		assert.True(t, e.Available)
		assert.Empty(t, e.BackendAuthRef, "an in-cluster backend carries no credential reference")

		// Per-alias config is carried through (reasoning off).
		var rc struct {
			EnableThinking *bool `json:"enable_thinking"`
		}
		require.NoError(t, json.Unmarshal(e.ReasoningConfig, &rc))
		require.NotNil(t, rc.EnableThinking)
		assert.False(t, *rc.EnableThinking)

		// HOR-559: a ModelBackend health change is a status-only update on the
		// backend CR, so the ModelBackend watch must stay unfiltered and still
		// propagate it to Model.status.available promptly.
		setBackendHealth(t, "be-healthy", true, false)
		waitReconciled(t, "m-healthy", false)
		setBackendHealth(t, "be-healthy", true, true)
		waitReconciled(t, "m-healthy", true)
	})

	t.Run("unavailable when backend not healthy", func(t *testing.T) {
		on := true
		createModel(t, "m-sick", "qwen3-27b-sick", "be-sick", &on)
		createBackend(t, "be-sick", "Qwen/Qwen3-27B", true, false)
		seedBackendRow(t, "be-sick", "Qwen/Qwen3-27B", "be-sick.default.svc:8000", true, false)
		waitReconciled(t, "m-sick", false)

		e, ok := findEntry("qwen3-27b-sick")
		require.True(t, ok, "view row exists (backend present) but unavailable")
		assert.False(t, e.Available, "available = Model.available AND backend.healthy")
	})

	t.Run("external backend exposes its credential Secret name", func(t *testing.T) {
		createModel(t, "m-external", "hosted-model", "be-external", nil)
		createBackend(t, "be-external", "hosted/model", true, true)
		_, err := store.UpsertBackend(ctx, catalog.Backend{
			Key: "default/be-external", Name: "be-external", Namespace: "default", Kind: "external",
			ServiceURL: "https://llm.example/v1", AuthRef: "external-llm", Deployed: true, Healthy: true,
		})
		require.NoError(t, err)
		waitReconciled(t, "m-external", true)

		// HOR-597: the gateway resolves the Secret by name; the value never
		// reaches Postgres.
		e, ok := findEntry("hosted-model")
		require.True(t, ok)
		assert.Equal(t, "external", e.BackendKind)
		assert.Equal(t, "external-llm", e.BackendAuthRef)
	})

	t.Run("aliases — two models, one backend", func(t *testing.T) {
		off := false
		on := true
		createModel(t, "m-alias-off", "qwen3-27b-alias", "be-alias", &off)
		createModel(t, "m-alias-on", "qwen3-27b-thinking-alias", "be-alias", &on)
		createBackend(t, "be-alias", "Qwen/Qwen3-27B", true, true)
		seedBackendRow(t, "be-alias", "Qwen/Qwen3-27B", "be-alias.default.svc:8000", true, true)
		waitReconciled(t, "m-alias-off", true)
		waitReconciled(t, "m-alias-on", true)

		eOff, ok := findEntry("qwen3-27b-alias")
		require.True(t, ok)
		eOn, ok := findEntry("qwen3-27b-thinking-alias")
		require.True(t, ok)
		// Same backend, different aliases.
		assert.Equal(t, eOff.BackendURL, eOn.BackendURL)
		assert.Equal(t, "be-alias.default.svc:8000", eOff.BackendURL)
		assert.NotEqual(t, eOff.ModelID, eOn.ModelID)
		assert.True(t, eOff.Available && eOn.Available)
	})

	t.Run("delete soft-deletes the catalog row", func(t *testing.T) {
		off := false
		m := createModel(t, "m-del", "qwen3-27b-del", "be-del", &off)
		createBackend(t, "be-del", "Qwen/Qwen3-27B", true, true)
		seedBackendRow(t, "be-del", "Qwen/Qwen3-27B", "be-del.default.svc:8000", true, true)
		waitReconciled(t, "m-del", true)
		_, ok := findEntry("qwen3-27b-del")
		require.True(t, ok)

		require.NoError(t, adminClient.Delete(ctx, m))
		require.Eventually(t, func() bool {
			var got v1alpha1.Model
			return errors.IsNotFound(adminClient.Get(ctx, types.NamespacedName{Name: "m-del", Namespace: "default"}, &got))
		}, 15*time.Second, 200*time.Millisecond, "Model should be deleted after finalizer cleanup")

		_, err := store.GetModelByKey(ctx, "default/m-del")
		assert.ErrorIs(t, err, catalog.ErrNotFound, "soft-deleted model should not be active")
		_, ok = findEntry("qwen3-27b-del")
		assert.False(t, ok, "soft-deleted model should drop out of effective_catalog")
	})

	// HOR-559 regression: the ~275/s self-triggered loop came from the
	// controller's own unconditional status write being delivered back as a
	// Model update event. A status-only update must not enqueue a reconcile;
	// a spec (generation) change still must.
	t.Run("status-only update does not trigger a reconcile", func(t *testing.T) {
		off := false
		createModel(t, hor559ModelName, "qwen3-27b-hor559", "be-hor559", &off)
		createBackend(t, "be-hor559", "Qwen/Qwen3-27B", true, true)
		seedBackendRow(t, "be-hor559", "Qwen/Qwen3-27B", "be-hor559.default.svc:8000", true, true)
		waitReconciled(t, hor559ModelName, true)
		nn := types.NamespacedName{Name: hor559ModelName, Namespace: "default"}

		// Drain any in-flight setup events so the baseline is steady.
		require.Eventually(t, func() bool {
			stable := tracking.count.Load()
			time.Sleep(250 * time.Millisecond)
			return tracking.count.Load() == stable
		}, 10*time.Second, 250*time.Millisecond, "reconcile count should quiesce before the status-only update")

		// Out-of-band status write: generation is unchanged, so the primary
		// watch predicate must filter the update event and no reconcile may be
		// enqueued. A reconcile would repair the message (and, before the fix,
		// the status write would re-trigger the loop).
		var m v1alpha1.Model
		require.NoError(t, adminClient.Get(ctx, nn, &m))
		base := m.DeepCopy()
		m.Status.Message = "out-of-band status write"
		require.NoError(t, adminClient.Status().Patch(ctx, &m, client.MergeFrom(base)))
		before := tracking.count.Load()
		require.Never(t, func() bool { return tracking.count.Load() != before },
			2*time.Second, 100*time.Millisecond, "status-only update must not trigger a reconcile")

		// Positive control: a spec (generation) change still reconciles, and the
		// controller repairs the out-of-band status on that pass.
		require.NoError(t, adminClient.Get(ctx, nn, &m))
		base = m.DeepCopy()
		m.Spec.DisplayName = "HOR-559 renamed"
		require.NoError(t, adminClient.Patch(ctx, &m, client.MergeFrom(base)))
		require.Eventually(t, func() bool { return tracking.count.Load() > before },
			10*time.Second, 100*time.Millisecond, "spec change must trigger a reconcile")
		require.Eventually(t, func() bool {
			var got v1alpha1.Model
			return adminClient.Get(ctx, nn, &got) == nil &&
				got.Status.ObservedGeneration == got.Generation && got.Status.Message == ""
		}, 10*time.Second, 100*time.Millisecond, "spec change must be observed and the status repaired")
	})
}

// TestModelStatusChanged pins the conditional status write (HOR-559): a
// steady-state reconcile must be a no-op, while every observed transition still
// patches the CR.
func TestModelStatusChanged(t *testing.T) {
	current := &v1alpha1.ModelStatus{Available: true, Healthy: true, ObservedGeneration: 3}
	assert.False(t, modelStatusChanged(current, 3, true, true, ""), "steady state must not patch status")
	assert.True(t, modelStatusChanged(current, 4, true, true, ""), "generation advance must patch")
	assert.True(t, modelStatusChanged(current, 3, false, true, ""), "availability change must patch")
	assert.True(t, modelStatusChanged(current, 3, true, false, ""), "health change must patch")
	assert.True(t, modelStatusChanged(current, 3, true, true, "backend not ready"), "message change must patch")
}

// TestModelRequeueInterval guards the HOR-559 requeue-unit bug:
// ctrl.Result.RequeueAfter is a time.Duration, so an untyped `30` would requeue
// every 30 nanoseconds and hot-loop the reconciler even with the watch
// predicate in place.
func TestModelRequeueInterval(t *testing.T) {
	assert.Equal(t, 30*time.Second, modelRequeueInterval, "idle Model reconciles at the 30s fallback cadence, not nanoseconds")
}
