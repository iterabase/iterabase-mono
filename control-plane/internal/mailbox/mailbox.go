// Package mailbox is the provider-neutral mailbox source boundary (HOR-358,
// DES-HOR-358-02/03).
//
// Postgres `mailbox.sources` is the authoritative mailbox list. Each mailbox
// has exactly one owner: the overlay `Mailbox` CRD (owner "overlay") or the
// installation (owner "installation", HOR-674). Intake runs in the tool
// gateway process: it watches every active mailbox through its provider
// [Source], records each new message exactly once in `mailbox.messages`, and
// hands it to consumers through [Store.ClaimEvents] / [Store.AckEvent].
//
// No credential value is stored or logged here. A mailbox names a credential
// slot; the provider resolves the slot to its installation-owned Secret
// ([CredentialSecretName]) inside the gateway.
package mailbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Providers.
const (
	ProviderMicrosoft365 = "microsoft365"
)

// Owners (DES-HOR-593-03).
const (
	OwnerOverlay      = "overlay"
	OwnerInstallation = "installation"
)

// DefaultPollInterval is the delta catch-up / polling-only interval.
const DefaultPollInterval = 60 * time.Second

// NotifyChannel is the pg_notify channel signalled when new events are
// recorded. The payload is the source id. Consumers must still poll: a lost
// notification only delays processing.
const NotifyChannel = "mailbox_messages"

// slotObjectPrefix names the installation-owned Secret holding a credential
// slot's values (DES-HOR-358-03). HOR-652's write-only API writes the same
// Secret.
const slotObjectPrefix = "iterabase-credential-"

// CredentialSecretName returns the Secret that holds credential slot values.
func CredentialSecretName(slot string) string { return slotObjectPrefix + slot }

// SlotFromCredentialSecretName reverses [CredentialSecretName].
func SlotFromCredentialSecretName(name string) (string, bool) {
	slot, ok := strings.CutPrefix(name, slotObjectPrefix)
	return slot, ok && slot != ""
}

// Mailbox is one active `mailbox.sources` row.
type Mailbox struct {
	ID              string
	Name            string
	Owner           string
	OwnerRef        string
	Provider        string
	Address         string
	Folders         []string
	CredentialSlot  string
	NotificationURL string // empty: polling-only, no provider subscription
	PollInterval    time.Duration
}

// Message is a provider-neutral new message. It carries identity and routing
// metadata only; bodies and attachments are read through the bounded product
// tools.
type Message struct {
	ProviderMessageID string
	Folder            string
	InternetMessageID string
	ConversationID    string
	InReplyTo         string
	References        []string
	SenderAddress     string
	SenderName        string
	Subject           string
	HasAttachments    bool
	ReceivedAt        time.Time
}

// Changes is one bounded batch of provider changes for a folder.
type Changes struct {
	// Messages are new, non-draft messages. Updates and removals are dropped by
	// the provider; deduplication makes any repeat harmless.
	Messages []Message
	// Cursor resumes after this batch. It is opaque and provider-owned.
	Cursor string
	// More reports that the provider has further changes ready now.
	More bool
}

// Subscription is a provider change-notification subscription for a folder.
type Subscription struct {
	ProviderID        string
	NotificationURL   string
	ClientStateSHA256 string
	ExpiresAt         time.Time
}

// Source is the provider-neutral mailbox connector. Microsoft 365 implements
// it in package graph (HOR-358); Google Workspace implements it in HOR-653.
type Source interface {
	// Changes returns messages received in folder after cursor. An empty cursor
	// starts a new sync covering mail received at or after since.
	Changes(ctx context.Context, mb Mailbox, folder, cursor string, since time.Time) (Changes, error)
	// ReplyHeaders completes a new message's In-Reply-To and References.
	ReplyHeaders(ctx context.Context, mb Mailbox, msg *Message) error
	// Subscribe creates, renews or recreates the folder's subscription. It
	// returns the subscription to persist, or current unchanged when nothing
	// was due.
	Subscribe(ctx context.Context, mb Mailbox, folder string, current *Subscription, now time.Time) (*Subscription, error)
	// Unsubscribe deletes a subscription; an already-gone one is not an error.
	Unsubscribe(ctx context.Context, mb Mailbox, sub Subscription) error
}

// ErrCursorExpired reports that the provider no longer accepts a cursor and
// the folder must be resynchronized.
var ErrCursorExpired = errors.New("mailbox: provider cursor expired")

// ErrConflict reports a mailbox name or address already owned by another
// active mailbox.
var ErrConflict = errors.New("mailbox: name or address already in use")

// RetryAfterError reports provider throttling or a transient outage.
type RetryAfterError struct{ After time.Duration }

func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("mailbox provider asked to retry after %s", e.After)
}

// CredentialError is a credential or authorization failure. Reason is
// customer-safe: it never echoes provider text or any credential value.
type CredentialError struct{ Reason string }

func (e *CredentialError) Error() string { return e.Reason }
