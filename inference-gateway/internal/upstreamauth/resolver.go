// Package upstreamauth resolves the credential an external ModelBackend
// presents to its provider (DES-HOR-597-01). The catalog carries only the
// authRef Secret *name*; the value is read from the Kubernetes API, held in
// gateway memory, and never logged or written anywhere else.
package upstreamauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// SecretKey is the authRef Secret key holding the provider credential (the
// ModelBackend CRD contract: "key API key/token").
const SecretKey = "key"

// ErrUnavailable is returned when the referenced credential cannot be used:
// the Secret is missing, empty, outside the platform namespace, or the wrong
// type. Callers classify it as upstream_auth_failed (DES-HOR-597-02).
var ErrUnavailable = errors.New("upstream credential unavailable")

// Resolver returns the credential for an external backend's authRef.
type Resolver interface {
	Credential(ctx context.Context, namespace, authRef string) (string, error)
}

type cached struct {
	value     string
	fetchedAt time.Time
	stale     bool
}

// K8sResolver reads authRef Secrets from the platform namespace. RBAC scopes
// the gateway to `get` on Secrets in that namespace only, mirroring the tool
// gateway (ARCH-008). Values are cached for ttl and marked stale on every
// catalog refresh; a transient API failure keeps serving the last value.
type K8sResolver struct {
	client    kubernetes.Interface
	namespace string
	ttl       time.Duration
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

// NewK8sResolver builds a resolver for Secrets in namespace.
func NewK8sResolver(client kubernetes.Interface, namespace string, ttl time.Duration) *K8sResolver {
	return &K8sResolver{client: client, namespace: namespace, ttl: ttl, now: time.Now, cache: map[string]cached{}}
}

// Invalidate marks every cached credential stale so the next request re-reads
// it; called on each snapshot refresh so a rotated key applies promptly.
func (r *K8sResolver) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, c := range r.cache {
		c.stale = true
		r.cache[name] = c
	}
}

// Credential returns the provider credential named by authRef. A backend
// outside the platform namespace is refused: its authRef names a Secret in its
// own namespace, which the gateway must not read from elsewhere.
func (r *K8sResolver) Credential(ctx context.Context, namespace, authRef string) (string, error) {
	if authRef == "" {
		return "", fmt.Errorf("%w: empty authRef", ErrUnavailable)
	}
	if namespace != r.namespace {
		return "", fmt.Errorf("%w: backend namespace %q is outside the platform namespace", ErrUnavailable, namespace)
	}

	r.mu.Lock()
	c, ok := r.cache[authRef]
	r.mu.Unlock()
	if ok && !c.stale && r.now().Sub(c.fetchedAt) < r.ttl {
		return c.value, nil
	}

	value, err := r.read(ctx, authRef)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case err == nil:
		r.cache[authRef] = cached{value: value, fetchedAt: r.now()}
		return value, nil
	case errors.Is(err, ErrUnavailable):
		delete(r.cache, authRef)
		return "", err
	case ok:
		// Transient API failure: keep routing with the last known value.
		return c.value, nil
	default:
		return "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
}

func (r *K8sResolver) read(ctx context.Context, name string) (string, error) {
	sec, err := r.client.CoreV1().Secrets(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			return "", fmt.Errorf("%w: secret %s/%s: %s", ErrUnavailable, r.namespace, name, apierrors.ReasonForError(err))
		}
		return "", fmt.Errorf("get secret %s/%s: %w", r.namespace, name, err)
	}
	if sec.Type == corev1.SecretTypeServiceAccountToken {
		return "", fmt.Errorf("%w: refusing service-account-token secret %s/%s", ErrUnavailable, r.namespace, name)
	}
	value := strings.TrimSpace(string(sec.Data[SecretKey]))
	if value == "" {
		return "", fmt.Errorf("%w: secret %s/%s has no %q value", ErrUnavailable, r.namespace, name, SecretKey)
	}
	return value, nil
}
