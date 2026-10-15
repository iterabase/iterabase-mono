package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MailboxProviderMicrosoft365 is the Microsoft 365 (Graph) mailbox provider.
const MailboxProviderMicrosoft365 = "microsoft365"

// MailboxSpec declares one Iterabase-managed mailbox (HOR-358,
// DES-HOR-358-03). The manager reconciles it into an overlay-owned
// `mailbox.sources` row; the tool gateway watches it. The spec is non-secret:
// it names a credential slot, never a Secret, because the slot's value is
// installation-owned and entered by a customer Admin (HOR-652).
type MailboxSpec struct {
	// provider is the mailbox provider.
	// +kubebuilder:validation:Enum=microsoft365
	Provider string `json:"provider"`

	// address is the mailbox's primary SMTP address.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=320
	// +kubebuilder:validation:Pattern=`^[^@\s]+@[^@\s]+\.[^@\s]+$`
	Address string `json:"address"`

	// folders are the mail folders watched for new messages: Graph well-known
	// names (for example `inbox`) or folder ids. Defaults to the inbox.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9=_-]{1,255}$`
	Folders []string `json:"folders,omitempty"`

	// credentialSlot names the installation-owned credential slot holding the
	// provider app credentials (Secret `iterabase-credential-<slot>`).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`
	CredentialSlot string `json:"credentialSlot"`

	// notifications enables provider change notifications to a publicly
	// reachable HTTPS endpoint (HOR-356). Without it the mailbox is
	// polling-only; delta catch-up always runs.
	// +optional
	Notifications *MailboxNotifications `json:"notifications,omitempty"`

	// pollInterval is the delta catch-up / polling interval (10s–1h).
	// Defaults to 60s.
	// +optional
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`
}

// MailboxNotifications configures provider change notifications.
type MailboxNotifications struct {
	// url is the public HTTPS notification endpoint.
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
}

// MailboxStatus reports the materialized mailbox.
type MailboxStatus struct {
	// +optional
	Ready bool `json:"ready,omitempty"`
	// +optional
	ValidationStatus string `json:"validationStatus,omitempty"`
	// +optional
	SourceID string `json:"sourceID,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=mailboxes,scope=Namespaced,shortName=mbx
// +kubebuilder:singular=mailbox
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.spec.address`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
//
// Mailbox is an Iterabase-managed mailbox declared by the customer overlay.
type Mailbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MailboxSpec   `json:"spec,omitempty"`
	Status            MailboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
//
// MailboxList is a list of Mailbox.
type MailboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Mailbox `json:"items"`
}
