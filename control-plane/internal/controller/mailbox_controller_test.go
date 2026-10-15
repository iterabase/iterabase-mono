package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/api/v1alpha1"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/mailbox"
)

type fakeMailboxStore struct {
	upserts  []mailbox.Mailbox
	disabled []string
	conflict bool
}

func (f *fakeMailboxStore) UpsertOverlay(_ context.Context, in mailbox.Mailbox) (mailbox.Mailbox, error) {
	if f.conflict {
		return mailbox.Mailbox{}, mailbox.ErrConflict
	}
	f.upserts = append(f.upserts, in)
	in.ID = "source-1"
	return in, nil
}

func (f *fakeMailboxStore) DisableOverlay(_ context.Context, ownerRef string) error {
	f.disabled = append(f.disabled, ownerRef)
	return nil
}

func mailboxScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}

func reconcileMailbox(t *testing.T, store *fakeMailboxStore, mb *v1alpha1.Mailbox) (client.Client, *v1alpha1.Mailbox) {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(mailboxScheme(t)).WithObjects(mb).WithStatusSubresource(mb).Build()
	r := &MailboxReconciler{Client: cl, Scheme: cl.Scheme(), Store: store}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mb.Namespace, Name: mb.Name}}
	for range 2 { // the first pass adds the finalizer
		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
	}
	var got v1alpha1.Mailbox
	require.NoError(t, cl.Get(context.Background(), req.NamespacedName, &got))
	return cl, &got
}

func validMailbox() *v1alpha1.Mailbox {
	return &v1alpha1.Mailbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "customer", Name: "quotes", Generation: 3},
		Spec: v1alpha1.MailboxSpec{
			Provider: v1alpha1.MailboxProviderMicrosoft365, Address: "Quotes@Customer.pt", CredentialSlot: "m365",
			Notifications: &v1alpha1.MailboxNotifications{URL: "https://acme.iterabase.app/hooks/graph"},
			PollInterval:  &metav1.Duration{Duration: 30 * time.Second},
		},
	}
}

func TestMailboxReconcilerMaterializesAnOverlayOwnedSource(t *testing.T) {
	store := &fakeMailboxStore{}
	_, got := reconcileMailbox(t, store, validMailbox())
	require.Len(t, store.upserts, 1)
	assert.Equal(t, mailbox.Mailbox{
		Name: "quotes", Owner: mailbox.OwnerOverlay, OwnerRef: "customer/quotes", Provider: mailbox.ProviderMicrosoft365,
		Address: "quotes@customer.pt", Folders: []string{"inbox"}, CredentialSlot: "m365",
		NotificationURL: "https://acme.iterabase.app/hooks/graph", PollInterval: 30 * time.Second,
	}, store.upserts[0])
	assert.True(t, got.Status.Ready)
	assert.Equal(t, "source-1", got.Status.SourceID)
	assert.Equal(t, v1alpha1.ValidationValid, got.Status.ValidationStatus)
	assert.Contains(t, got.Finalizers, mailboxFinalizer)
}

func TestMailboxReconcilerRefusesConflictsAndInvalidSpecs(t *testing.T) {
	store := &fakeMailboxStore{conflict: true}
	_, got := reconcileMailbox(t, store, validMailbox())
	assert.False(t, got.Status.Ready)
	assert.Equal(t, v1alpha1.ValidationInvalid, got.Status.ValidationStatus)
	assert.Contains(t, got.Status.Message, "already used by another mailbox")
	assert.Equal(t, []string{"customer/quotes"}, store.disabled, "a conflicting spec stops the previous declaration")

	for name, mutate := range map[string]func(*v1alpha1.Mailbox){
		"poll interval too short": func(m *v1alpha1.Mailbox) { m.Spec.PollInterval = &metav1.Duration{Duration: time.Second} },
		"duplicate folder":        func(m *v1alpha1.Mailbox) { m.Spec.Folders = []string{"inbox", "Inbox"} },
		"plain http":              func(m *v1alpha1.Mailbox) { m.Spec.Notifications.URL = "http://x" },
		"not an address":          func(m *v1alpha1.Mailbox) { m.Spec.Address = "quotes" },
		"non-label slot":          func(m *v1alpha1.Mailbox) { m.Spec.CredentialSlot = "M365_App" },
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeMailboxStore{}
			mb := validMailbox()
			mutate(mb)
			_, got := reconcileMailbox(t, store, mb)
			assert.Empty(t, store.upserts)
			assert.Equal(t, v1alpha1.ValidationInvalid, got.Status.ValidationStatus)
			assert.Equal(t, int64(3), got.Status.ObservedGeneration)
		})
	}
}

func TestMailboxReconcilerRetiresTheSourceOnDeletion(t *testing.T) {
	store := &fakeMailboxStore{}
	cl, got := reconcileMailbox(t, store, validMailbox())
	require.NoError(t, cl.Delete(context.Background(), got))
	r := &MailboxReconciler{Client: cl, Scheme: cl.Scheme(), Store: store}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "customer", Name: "quotes"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"customer/quotes"}, store.disabled)
}
