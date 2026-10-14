package mailbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when no active mailbox matches.
var ErrNotFound = errors.New("mailbox: not found")

// Store is the Postgres-backed mailbox registry, intake state and event inbox.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const sourceColumns = `id, name, owner, owner_ref, provider, address, folders, credential_slot, notification_url, poll_interval_seconds`

func scanMailbox(row pgx.Row) (Mailbox, error) {
	var mb Mailbox
	var seconds int
	if err := row.Scan(&mb.ID, &mb.Name, &mb.Owner, &mb.OwnerRef, &mb.Provider, &mb.Address, &mb.Folders,
		&mb.CredentialSlot, &mb.NotificationURL, &seconds); err != nil {
		return Mailbox{}, err
	}
	mb.PollInterval = time.Duration(seconds) * time.Second
	return mb, nil
}

// UpsertOverlay re-asserts the overlay-owned mailbox declared by ownerRef
// (`<namespace>/<name>`). Changing the provider, address or credential slot
// retires the old source and creates a new one, so recorded events keep the
// identity they were received under. A name or address held by any other
// active mailbox, including an installation-owned one, returns ErrConflict;
// installation-owned rows are never modified here.
func (s *Store) UpsertOverlay(ctx context.Context, in Mailbox) (Mailbox, error) {
	in.Address = strings.ToLower(in.Address)
	if in.PollInterval <= 0 {
		in.PollInterval = DefaultPollInterval
	}
	seconds := int(in.PollInterval / time.Second)
	var out Mailbox
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		current, err := scanMailbox(tx.QueryRow(ctx, `
			SELECT `+sourceColumns+` FROM mailbox.sources
			WHERE owner = 'overlay' AND owner_ref = $1 AND disabled_at IS NULL
			FOR UPDATE`, in.OwnerRef))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		case current.Provider != in.Provider || current.Address != in.Address || current.CredentialSlot != in.CredentialSlot:
			if _, err := tx.Exec(ctx, `UPDATE mailbox.sources SET disabled_at = now(), lease_until = NULL WHERE id = $1`, current.ID); err != nil {
				return err
			}
		default:
			out, err = scanMailbox(tx.QueryRow(ctx, `
				UPDATE mailbox.sources
				SET name = $2, folders = $3, notification_url = $4, poll_interval_seconds = $5
				WHERE id = $1
				RETURNING `+sourceColumns, current.ID, in.Name, in.Folders, in.NotificationURL, seconds))
			if err != nil {
				return err
			}
			// A folder no longer declared keeps no cursor; the intake's
			// subscription sweep removes its provider subscription.
			_, err = tx.Exec(ctx, `
				DELETE FROM mailbox.folder_cursors WHERE source_id = $1 AND NOT (folder = ANY($2))`, current.ID, in.Folders)
			return err
		}
		out, err = scanMailbox(tx.QueryRow(ctx, `
			INSERT INTO mailbox.sources
			    (name, owner, owner_ref, provider, address, folders, credential_slot, notification_url, poll_interval_seconds)
			VALUES ($1, 'overlay', $2, $3, $4, $5, $6, $7, $8)
			RETURNING `+sourceColumns,
			in.Name, in.OwnerRef, in.Provider, in.Address, in.Folders, in.CredentialSlot, in.NotificationURL, seconds))
		return err
	})
	if isUniqueViolation(err) {
		return Mailbox{}, ErrConflict
	}
	if err != nil {
		return Mailbox{}, fmt.Errorf("upsert overlay mailbox: %w", err)
	}
	return out, nil
}

// DisableOverlay retires the overlay-owned mailbox declared by ownerRef. It is
// idempotent and never touches installation-owned rows.
func (s *Store) DisableOverlay(ctx context.Context, ownerRef string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE mailbox.sources SET disabled_at = now(), lease_until = NULL
		WHERE owner = 'overlay' AND owner_ref = $1 AND disabled_at IS NULL`, ownerRef)
	if err != nil {
		return fmt.Errorf("disable overlay mailbox: %w", err)
	}
	return nil
}

// GetActive returns the active mailbox with the given name.
func (s *Store) GetActive(ctx context.Context, name string) (Mailbox, error) {
	mb, err := scanMailbox(s.pool.QueryRow(ctx, `
		SELECT `+sourceColumns+` FROM mailbox.sources WHERE name = $1 AND disabled_at IS NULL`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mailbox{}, ErrNotFound
	}
	return mb, err
}

// Permitted reports whether address is an active mailbox of provider whose
// credential slot is slot. It backs the gateway's mailbox tool scope check
// (DES-HOR-358-03).
func (s *Store) Permitted(ctx context.Context, provider, address, slot string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM mailbox.sources
		WHERE provider = $1 AND address = $2 AND credential_slot = $3 AND disabled_at IS NULL)`,
		provider, strings.ToLower(address), slot).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("mailbox scope: %w", err)
	}
	return ok, nil
}

// ClaimDue leases up to limit active mailboxes whose next intake run is due.
// Concurrent gateway replicas never process the same mailbox at once.
func (s *Store) ClaimDue(ctx context.Context, lease time.Duration, limit int) ([]Mailbox, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE mailbox.sources SET lease_until = now() + $1::interval
		WHERE id IN (
		    SELECT id FROM mailbox.sources
		    WHERE disabled_at IS NULL AND next_poll_at <= now()
		      AND (lease_until IS NULL OR lease_until < now())
		    ORDER BY next_poll_at
		    LIMIT $2
		    FOR UPDATE SKIP LOCKED)
		RETURNING `+sourceColumns, lease.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim due mailboxes: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Mailbox, error) { return scanMailbox(row) })
}

// Reschedule releases a claimed mailbox and sets its next intake run.
func (s *Store) Reschedule(ctx context.Context, id string, after time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE mailbox.sources
		SET next_poll_at = now() + $2::interval, last_polled_at = now(), lease_until = NULL
		WHERE id = $1`, id, after.String())
	if err != nil {
		return fmt.Errorf("reschedule mailbox: %w", err)
	}
	return nil
}

// Cursor returns the folder's delta cursor and baseline, creating the folder
// state on first observation. Mail received before the baseline is history.
func (s *Store) Cursor(ctx context.Context, sourceID, folder string) (string, time.Time, error) {
	var cursor string
	var baseline time.Time
	err := s.pool.QueryRow(ctx, `
		WITH ins AS (
		    INSERT INTO mailbox.folder_cursors (source_id, folder) VALUES ($1, $2)
		    ON CONFLICT (source_id, folder) DO NOTHING
		    RETURNING cursor, baseline_at)
		SELECT cursor, baseline_at FROM ins
		UNION ALL
		SELECT cursor, baseline_at FROM mailbox.folder_cursors WHERE source_id = $1 AND folder = $2
		LIMIT 1`, sourceID, folder).Scan(&cursor, &baseline)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read folder cursor: %w", err)
	}
	return cursor, baseline, nil
}

// SaveCursor records the folder's next delta cursor.
func (s *Store) SaveCursor(ctx context.Context, sourceID, folder, cursor string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE mailbox.folder_cursors SET cursor = $3, updated_at = now()
		WHERE source_id = $1 AND folder = $2`, sourceID, folder, cursor)
	if err != nil {
		return fmt.Errorf("save folder cursor: %w", err)
	}
	return nil
}

// resyncOverlap re-reads a window before the newest recorded message when a
// cursor expires, so mail that arrived around the reset is not missed.
// Deduplication absorbs the repeats.
const resyncOverlap = 15 * time.Minute

// ResetCursor clears an expired cursor. The next sync starts shortly before
// the newest recorded message in the folder, never before the baseline.
func (s *Store) ResetCursor(ctx context.Context, sourceID, folder string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE mailbox.folder_cursors c
		SET cursor = '', updated_at = now(),
		    baseline_at = GREATEST(c.baseline_at, COALESCE(
		        (SELECT max(received_at) FROM mailbox.messages m WHERE m.source_id = c.source_id AND m.folder = c.folder)
		            - $3::interval,
		        c.baseline_at))
		WHERE source_id = $1 AND folder = $2`, sourceID, folder, resyncOverlap.String())
	if err != nil {
		return fmt.Errorf("reset folder cursor: %w", err)
	}
	return nil
}

// KnownMessages returns which provider message ids are already recorded.
func (s *Store) KnownMessages(ctx context.Context, sourceID string, ids []string) (map[string]bool, error) {
	known := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return known, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT provider_message_id FROM mailbox.messages
		WHERE source_id = $1 AND provider_message_id = ANY($2)`, sourceID, ids)
	if err != nil {
		return nil, fmt.Errorf("known messages: %w", err)
	}
	ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("known messages: %w", err)
	}
	for _, id := range ids {
		known[id] = true
	}
	return known, nil
}

// RecordMessages records new messages exactly once per provider identity and
// signals [NotifyChannel] when any was new. It returns the number recorded.
func (s *Store) RecordMessages(ctx context.Context, sourceID string, msgs []Message) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	inserted := 0
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, m := range msgs {
			refs := m.References
			if refs == nil {
				refs = []string{}
			}
			tag, err := tx.Exec(ctx, `
				INSERT INTO mailbox.messages
				    (source_id, provider_message_id, folder, internet_message_id, conversation_id, in_reply_to,
				     "references", sender_address, sender_name, subject, has_attachments, received_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
				ON CONFLICT (source_id, provider_message_id) DO NOTHING`,
				sourceID, m.ProviderMessageID, m.Folder, m.InternetMessageID, m.ConversationID, m.InReplyTo,
				refs, strings.ToLower(m.SenderAddress), m.SenderName, m.Subject, m.HasAttachments, m.ReceivedAt)
			if err != nil {
				return err
			}
			inserted += int(tag.RowsAffected())
		}
		if inserted > 0 {
			_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, sourceID)
			return err
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("record mailbox messages: %w", err)
	}
	return inserted, nil
}

// Subscription returns the folder's persisted subscription, or nil.
func (s *Store) Subscription(ctx context.Context, sourceID, folder string) (*Subscription, error) {
	var sub Subscription
	err := s.pool.QueryRow(ctx, `
		SELECT provider_subscription_id, notification_url, client_state_sha256, expires_at
		FROM mailbox.subscriptions WHERE source_id = $1 AND folder = $2`, sourceID, folder).Scan(
		&sub.ProviderID, &sub.NotificationURL, &sub.ClientStateSHA256, &sub.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read subscription: %w", err)
	}
	return &sub, nil
}

// SaveSubscription upserts the folder's subscription.
func (s *Store) SaveSubscription(ctx context.Context, sourceID, folder string, sub Subscription) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mailbox.subscriptions
		    (source_id, folder, provider_subscription_id, notification_url, client_state_sha256, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (source_id, folder) DO UPDATE SET
		    provider_subscription_id = EXCLUDED.provider_subscription_id,
		    notification_url = EXCLUDED.notification_url,
		    client_state_sha256 = EXCLUDED.client_state_sha256,
		    expires_at = EXCLUDED.expires_at,
		    renewed_at = now()`,
		sourceID, folder, sub.ProviderID, sub.NotificationURL, sub.ClientStateSHA256, sub.ExpiresAt)
	if err != nil {
		return fmt.Errorf("save subscription: %w", err)
	}
	return nil
}

// DeleteSubscription forgets a subscription.
func (s *Store) DeleteSubscription(ctx context.Context, providerID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM mailbox.subscriptions WHERE provider_subscription_id = $1`, providerID); err != nil {
		return fmt.Errorf("delete subscription: %w", err)
	}
	return nil
}

// StaleSubscription is a persisted subscription that is no longer wanted: its
// mailbox was retired, the folder was removed, or notifications were turned
// off or moved to another URL.
type StaleSubscription struct {
	Mailbox      Mailbox
	Subscription Subscription
}

// StaleSubscriptions lists up to limit unwanted subscriptions.
func (s *Store) StaleSubscriptions(ctx context.Context, limit int) ([]StaleSubscription, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.name, s.owner, s.owner_ref, s.provider, s.address, s.folders, s.credential_slot,
		       s.notification_url, s.poll_interval_seconds,
		       b.provider_subscription_id, b.notification_url, b.client_state_sha256, b.expires_at
		FROM mailbox.subscriptions b JOIN mailbox.sources s ON s.id = b.source_id
		WHERE s.disabled_at IS NOT NULL OR NOT (b.folder = ANY(s.folders)) OR s.notification_url <> b.notification_url
		ORDER BY b.renewed_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("stale subscriptions: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (StaleSubscription, error) {
		var st StaleSubscription
		var seconds int
		err := row.Scan(&st.Mailbox.ID, &st.Mailbox.Name, &st.Mailbox.Owner, &st.Mailbox.OwnerRef, &st.Mailbox.Provider,
			&st.Mailbox.Address, &st.Mailbox.Folders, &st.Mailbox.CredentialSlot, &st.Mailbox.NotificationURL, &seconds,
			&st.Subscription.ProviderID, &st.Subscription.NotificationURL, &st.Subscription.ClientStateSHA256, &st.Subscription.ExpiresAt)
		st.Mailbox.PollInterval = time.Duration(seconds) * time.Second
		return st, err
	})
}

// RecordCredentialStatus records the latest outcome for a credential slot.
// reason must be customer-safe; it is empty for a healthy slot.
func (s *Store) RecordCredentialStatus(ctx context.Context, slot string, failing bool, reason string) error {
	state := "ok"
	if failing {
		state = "failing"
	} else {
		reason = ""
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mailbox.credential_status (credential_slot, state, reason) VALUES ($1, $2, $3)
		ON CONFLICT (credential_slot) DO UPDATE SET state = $2, reason = $3, observed_at = now()`, slot, state, reason)
	if err != nil {
		return fmt.Errorf("record credential status: %w", err)
	}
	return nil
}

// CredentialStatus is the latest outcome for a credential slot.
type CredentialStatus struct {
	Slot       string
	Failing    bool
	Reason     string
	ObservedAt time.Time
}

// GetCredentialStatus returns a slot's latest outcome, or ErrNotFound.
func (s *Store) GetCredentialStatus(ctx context.Context, slot string) (CredentialStatus, error) {
	st := CredentialStatus{Slot: slot}
	var state string
	err := s.pool.QueryRow(ctx, `
		SELECT state, reason, observed_at FROM mailbox.credential_status WHERE credential_slot = $1`, slot).Scan(
		&state, &st.Reason, &st.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialStatus{}, ErrNotFound
	}
	if err != nil {
		return CredentialStatus{}, fmt.Errorf("read credential status: %w", err)
	}
	st.Failing = state == "failing"
	return st, nil
}

// Event is one recorded message delivered to the mailbox consumer.
type Event struct {
	ID          string
	SourceID    string
	MailboxName string
	Provider    string
	Address     string
	Message
	RecordedAt time.Time
}

// Events is the consumer side of the mailbox source interface (HOR-595 routes
// mailbox starts and reply continuation through it). Delivery is
// at-least-once: an event claimed but not acknowledged before its lease ends
// is claimed again, so consumers must be idempotent by event ID.
type Events interface {
	ClaimEvents(ctx context.Context, limit int, lease time.Duration) ([]Event, error)
	AckEvent(ctx context.Context, id string) error
}

var _ Events = (*Store)(nil)

// ClaimEvents leases up to limit unacknowledged events, oldest first.
func (s *Store) ClaimEvents(ctx context.Context, limit int, lease time.Duration) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		WITH claimed AS (
		    UPDATE mailbox.messages SET claimed_until = now() + $2::interval
		    WHERE id IN (
		        SELECT id FROM mailbox.messages
		        WHERE acknowledged_at IS NULL AND (claimed_until IS NULL OR claimed_until < now())
		        ORDER BY recorded_at
		        LIMIT $1
		        FOR UPDATE SKIP LOCKED)
		    RETURNING *)
		SELECT c.id, c.source_id, s.name, s.provider, s.address, c.provider_message_id, c.folder,
		       c.internet_message_id, c.conversation_id, c.in_reply_to, c."references",
		       c.sender_address, c.sender_name, c.subject, c.has_attachments, c.received_at, c.recorded_at
		FROM claimed c JOIN mailbox.sources s ON s.id = c.source_id
		ORDER BY c.recorded_at`, limit, lease.String())
	if err != nil {
		return nil, fmt.Errorf("claim mailbox events: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.ID, &e.SourceID, &e.MailboxName, &e.Provider, &e.Address, &e.ProviderMessageID, &e.Folder,
			&e.InternetMessageID, &e.ConversationID, &e.InReplyTo, &e.References,
			&e.SenderAddress, &e.SenderName, &e.Subject, &e.HasAttachments, &e.ReceivedAt, &e.RecordedAt)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("claim mailbox events: %w", err)
	}
	return events, nil
}

// AckEvent marks an event handled. Acknowledging twice is harmless.
func (s *Store) AckEvent(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE mailbox.messages SET acknowledged_at = COALESCE(acknowledged_at, now()), claimed_until = NULL
		WHERE id = $1`, id); err != nil {
		return fmt.Errorf("ack mailbox event: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
