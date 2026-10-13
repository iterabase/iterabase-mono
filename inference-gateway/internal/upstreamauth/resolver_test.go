package upstreamauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const ns = "iterabase-system"

func secret(name, value string, typ corev1.SecretType) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       typ,
		Data:       map[string][]byte{SecretKey: []byte(value)},
	}
}

func TestCredential_ReadsSecretKey(t *testing.T) {
	client := fake.NewClientset(secret("llm", "sk-test\n", corev1.SecretTypeOpaque))
	r := NewK8sResolver(client, ns, time.Minute)

	got, err := r.Credential(context.Background(), ns, "llm")
	require.NoError(t, err)
	assert.Equal(t, "sk-test", got, "a trailing newline from --from-file is not part of the key")
}

func TestCredential_Unavailable(t *testing.T) {
	client := fake.NewClientset(
		secret("sa", "token", corev1.SecretTypeServiceAccountToken),
		secret("empty", "", corev1.SecretTypeOpaque),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "llm", Namespace: "other"}, Data: map[string][]byte{SecretKey: []byte("x")}},
	)
	r := NewK8sResolver(client, ns, time.Minute)

	for name, call := range map[string]struct{ namespace, ref string }{
		"missing":              {ns, "absent"},
		"service-account":      {ns, "sa"},
		"empty value":          {ns, "empty"},
		"empty ref":            {ns, ""},
		"foreign namespace":    {"other", "llm"},
		"no cross-ns fallback": {"other", "sa"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.Credential(context.Background(), call.namespace, call.ref)
			assert.ErrorIs(t, err, ErrUnavailable)
		})
	}
}

func TestCredential_CachesUntilInvalidated(t *testing.T) {
	client := fake.NewClientset(secret("llm", "old", corev1.SecretTypeOpaque))
	r := NewK8sResolver(client, ns, time.Hour)
	ctx := context.Background()

	got, err := r.Credential(ctx, ns, "llm")
	require.NoError(t, err)
	require.Equal(t, "old", got)

	_, err = client.CoreV1().Secrets(ns).Update(ctx, secret("llm", "rotated", corev1.SecretTypeOpaque), metav1.UpdateOptions{})
	require.NoError(t, err)
	got, _ = r.Credential(ctx, ns, "llm")
	assert.Equal(t, "old", got, "served from cache within the TTL")

	r.Invalidate()
	got, err = r.Credential(ctx, ns, "llm")
	require.NoError(t, err)
	assert.Equal(t, "rotated", got, "a catalog refresh picks up a rotated key")
}

func TestCredential_TTLExpiry(t *testing.T) {
	client := fake.NewClientset(secret("llm", "old", corev1.SecretTypeOpaque))
	r := NewK8sResolver(client, ns, time.Minute)
	now := time.Now()
	r.now = func() time.Time { return now }
	ctx := context.Background()

	_, err := r.Credential(ctx, ns, "llm")
	require.NoError(t, err)
	_, err = client.CoreV1().Secrets(ns).Update(ctx, secret("llm", "rotated", corev1.SecretTypeOpaque), metav1.UpdateOptions{})
	require.NoError(t, err)

	now = now.Add(2 * time.Minute)
	got, err := r.Credential(ctx, ns, "llm")
	require.NoError(t, err)
	assert.Equal(t, "rotated", got)
}

func TestCredential_TransientErrorServesLastValue(t *testing.T) {
	client := fake.NewClientset(secret("llm", "sk-test", corev1.SecretTypeOpaque))
	r := NewK8sResolver(client, ns, time.Minute)
	ctx := context.Background()

	_, err := r.Credential(ctx, ns, "llm")
	require.NoError(t, err)

	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	r.Invalidate()
	got, err := r.Credential(ctx, ns, "llm")
	require.NoError(t, err)
	assert.Equal(t, "sk-test", got)

	_, err = r.Credential(ctx, ns, "never-read")
	assert.ErrorIs(t, err, ErrUnavailable, "no last value to fall back to")
}

func TestCredential_DeletedSecretStopsServing(t *testing.T) {
	client := fake.NewClientset(secret("llm", "sk-test", corev1.SecretTypeOpaque))
	r := NewK8sResolver(client, ns, time.Minute)
	ctx := context.Background()

	_, err := r.Credential(ctx, ns, "llm")
	require.NoError(t, err)
	require.NoError(t, client.CoreV1().Secrets(ns).Delete(ctx, "llm", metav1.DeleteOptions{}))
	r.Invalidate()

	_, err = r.Credential(ctx, ns, "llm")
	assert.ErrorIs(t, err, ErrUnavailable, "a cleared credential is not served from cache")
}
