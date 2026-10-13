package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	yaml "sigs.k8s.io/yaml"
)

// rbacManagerConfig applies the generated manager-role (config/rbac/role.yaml)
// and returns a rest.Config authenticated as a ServiceAccount bound to it. The
// reconciler runs under this config so the test behaviorally validates that
// role.yaml grants every verb the controller needs. envtest's API server
// enforces RBAC by default; the admin config would bypass it, so this is the
// only way to catch a missing permission.
func rbacManagerConfig(t *testing.T, ctx context.Context, adminCfg *rest.Config, scheme *runtime.Scheme) *rest.Config {
	t.Helper()
	adminClient, err := client.New(adminCfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	// Apply the generated ClusterRole (the artifact under test).
	roleBytes, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	require.NoError(t, err)
	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal(roleBytes, &role))
	require.NoError(t, adminClient.Create(ctx, &role))

	// ServiceAccount + ClusterRoleBinding -> manager-role.
	require.NoError(t, adminClient.Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-test", Namespace: "default"},
	}))
	require.NoError(t, adminClient.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-test-manager"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "cp-test", Namespace: "default"}},
		RoleRef:    rbacv1.RoleRef{Kind: "ClusterRole", Name: "manager-role", APIGroup: "rbac.authorization.k8s.io"},
	}))

	// Mint a short-lived token for the SA (modern K8s; no long-lived secret tokens).
	clientset, err := kubernetes.NewForConfig(adminCfg)
	require.NoError(t, err)
	tr, err := clientset.CoreV1().ServiceAccounts("default").CreateToken(ctx, "cp-test", &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
	require.NoError(t, err)

	// Authenticate as the SA, dropping the admin client cert (keep the CA so the
	// apiserver's TLS cert is still verified).
	saCfg := rest.CopyConfig(adminCfg)
	saCfg.BearerToken = tr.Status.Token
	saCfg.BearerTokenFile = ""
	saCfg.TLSClientConfig.CertData = nil
	saCfg.TLSClientConfig.CertFile = ""
	saCfg.TLSClientConfig.KeyData = nil
	saCfg.TLSClientConfig.KeyFile = ""
	return saCfg
}
