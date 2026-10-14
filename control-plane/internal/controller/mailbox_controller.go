package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/api/v1alpha1"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/mailbox"
)

const mailboxFinalizer = "platform.iterabase.com/mailbox-finalizer"

// Mailbox poll interval bounds (mirrors the mailbox.sources CHECK).
const (
	minMailboxPollInterval = 10 * time.Second
	maxMailboxPollInterval = time.Hour
)

// MailboxStore is the mailbox-store contract the Mailbox reconciler uses.
// *mailbox.Store implements it; tests may inject a fake.
type MailboxStore interface {
	UpsertOverlay(ctx context.Context, in mailbox.Mailbox) (mailbox.Mailbox, error)
	DisableOverlay(ctx context.Context, ownerRef string) error
}

// MailboxReconciler materializes overlay Mailbox CRs into overlay-owned
// `mailbox.sources` rows (HOR-358, DES-HOR-358-03). It re-asserts its own rows
// and retires them on CR deletion; installation-owned mailboxes are never
// modified, and a name or address they hold is refused.
type MailboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Store  MailboxStore
}

// +kubebuilder:rbac:groups=platform.iterabase.com,resources=mailboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.iterabase.com,resources=mailboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.iterabase.com,resources=mailboxes/finalizers,verbs=update

// Reconcile handles Mailbox create/update/delete events.
func (r *MailboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var mb v1alpha1.Mailbox
	if err := r.Get(ctx, req.NamespacedName, &mb); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	ownerRef := mb.Namespace + "/" + mb.Name

	if !mb.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&mb, mailboxFinalizer) {
			if err := r.Store.DisableOverlay(ctx, ownerRef); err != nil {
				logger.Error(err, "mailbox cleanup failed; keeping finalizer and requeuing")
				return ctrl.Result{Requeue: true}, nil
			}
			controllerutil.RemoveFinalizer(&mb, mailboxFinalizer)
			if err := r.Update(ctx, &mb); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&mb, mailboxFinalizer) {
		controllerutil.AddFinalizer(&mb, mailboxFinalizer)
		if err := r.Update(ctx, &mb); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	in, err := mailboxFromSpec(&mb)
	if err != nil {
		// An invalid declaration must not keep watching under an older spec.
		if derr := r.Store.DisableOverlay(ctx, ownerRef); derr != nil {
			return ctrl.Result{}, derr
		}
		return ctrl.Result{}, r.patchStatus(ctx, &mb, false, v1alpha1.ValidationInvalid, "", fmt.Sprintf("validation: %v", err), true)
	}
	out, err := r.Store.UpsertOverlay(ctx, in)
	if errors.Is(err, mailbox.ErrConflict) {
		// Stop watching under the previous spec: the CR now declares another
		// mailbox that cannot be materialized.
		if derr := r.Store.DisableOverlay(ctx, ownerRef); derr != nil {
			return ctrl.Result{}, derr
		}
		return ctrl.Result{}, r.patchStatus(ctx, &mb, false, v1alpha1.ValidationInvalid, "",
			fmt.Sprintf("mailbox name %q or address %q is already used by another mailbox", in.Name, in.Address), true)
	}
	if err != nil {
		_ = r.patchStatus(ctx, &mb, false, v1alpha1.ValidationValid, "", fmt.Sprintf("materialize mailbox: %v", err), false)
		return ctrl.Result{}, err
	}
	logger.Info("materialized mailbox", "name", out.Name, "source", out.ID)
	return ctrl.Result{}, r.patchStatus(ctx, &mb, true, v1alpha1.ValidationValid, out.ID, "", true)
}

// mailboxFromSpec validates the CR and maps it to an overlay-owned source.
func mailboxFromSpec(mb *v1alpha1.Mailbox) (mailbox.Mailbox, error) {
	if errs := utilvalidation.IsDNS1123Label(mb.Name); len(errs) > 0 {
		return mailbox.Mailbox{}, fmt.Errorf("metadata.name must be a DNS label: %s", strings.Join(errs, "; "))
	}
	if mb.Spec.Provider != v1alpha1.MailboxProviderMicrosoft365 {
		return mailbox.Mailbox{}, fmt.Errorf("spec.provider %q is not supported", mb.Spec.Provider)
	}
	address := strings.ToLower(strings.TrimSpace(mb.Spec.Address))
	if at := strings.IndexByte(address, '@'); at <= 0 || at != strings.LastIndexByte(address, '@') || at == len(address)-1 {
		return mailbox.Mailbox{}, fmt.Errorf("spec.address must be a plain email address")
	}
	if errs := utilvalidation.IsDNS1123Label(mb.Spec.CredentialSlot); len(errs) > 0 {
		return mailbox.Mailbox{}, fmt.Errorf("spec.credentialSlot must be a DNS label: %s", strings.Join(errs, "; "))
	}
	folders, err := mailboxFolders(mb.Spec.Folders)
	if err != nil {
		return mailbox.Mailbox{}, err
	}
	interval, err := mailboxPollInterval(mb.Spec.PollInterval)
	if err != nil {
		return mailbox.Mailbox{}, err
	}
	notificationURL := ""
	if mb.Spec.Notifications != nil {
		notificationURL = mb.Spec.Notifications.URL
		if !strings.HasPrefix(notificationURL, "https://") {
			return mailbox.Mailbox{}, fmt.Errorf("spec.notifications.url must be https")
		}
	}
	return mailbox.Mailbox{
		Name:            mb.Name,
		Owner:           mailbox.OwnerOverlay,
		OwnerRef:        mb.Namespace + "/" + mb.Name,
		Provider:        mailbox.ProviderMicrosoft365,
		Address:         address,
		Folders:         folders,
		CredentialSlot:  mb.Spec.CredentialSlot,
		NotificationURL: notificationURL,
		PollInterval:    interval,
	}, nil
}

// mailboxFolders defaults to the inbox and rejects case-insensitive repeats.
func mailboxFolders(in []string) ([]string, error) {
	if len(in) == 0 {
		return []string{"inbox"}, nil
	}
	seen := make(map[string]bool, len(in))
	for _, f := range in {
		if seen[strings.ToLower(f)] {
			return nil, fmt.Errorf("spec.folders contains %q twice", f)
		}
		seen[strings.ToLower(f)] = true
	}
	return in, nil
}

// mailboxPollInterval defaults the interval and enforces its bounds.
func mailboxPollInterval(in *metav1.Duration) (time.Duration, error) {
	interval := mailbox.DefaultPollInterval
	if in != nil {
		interval = in.Duration
	}
	if interval < minMailboxPollInterval || interval > maxMailboxPollInterval || interval%time.Second != 0 {
		return 0, fmt.Errorf("spec.pollInterval must be whole seconds between %s and %s", minMailboxPollInterval, maxMailboxPollInterval)
	}
	return interval, nil
}

func (r *MailboxReconciler) patchStatus(ctx context.Context, mb *v1alpha1.Mailbox, ready bool, validationStatus, sourceID, message string, recordObserved bool) error {
	base := mb.DeepCopy()
	mb.Status.Ready = ready
	mb.Status.ValidationStatus = validationStatus
	mb.Status.SourceID = sourceID
	mb.Status.Message = message
	if recordObserved {
		mb.Status.ObservedGeneration = mb.Generation
	}
	return r.Status().Patch(ctx, mb, client.MergeFrom(base))
}

// SetupWithManager registers the reconciler.
func (r *MailboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.Mailbox{}).Complete(r)
}
