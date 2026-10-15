package mailbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/mailbox"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
)

// fakeSource is a scripted provider. Changes pops the next scripted result
// for the folder; an empty script returns no changes at the same cursor.
type fakeSource struct {
	mu            sync.Mutex
	changes       map[string][]func(cursor string, since time.Time) (mailbox.Changes, error)
	cursors       []string
	sinces        []time.Time
	headerCalls   []string
	subscribeErr  error
	subscriptions int
	unsubscribed  []string
}

func newFakeSource() *fakeSource {
	return &fakeSource{changes: map[string][]func(string, time.Time) (mailbox.Changes, error){}}
}

func (f *fakeSource) script(folder string, steps ...func(cursor string, since time.Time) (mailbox.Changes, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes[folder] = append(f.changes[folder], steps...)
}

func (f *fakeSource) Changes(_ context.Context, _ mailbox.Mailbox, folder, cursor string, since time.Time) (mailbox.Changes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursors = append(f.cursors, cursor)
	f.sinces = append(f.sinces, since)
	steps := f.changes[folder]
	if len(steps) == 0 {
		return mailbox.Changes{Cursor: cursor}, nil
	}
	f.changes[folder] = steps[1:]
	return steps[0](cursor, since)
}

func (f *fakeSource) ReplyHeaders(_ context.Context, _ mailbox.Mailbox, msg *mailbox.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headerCalls = append(f.headerCalls, msg.ProviderMessageID)
	msg.InReplyTo = "<parent@" + msg.ProviderMessageID + ">"
	msg.References = []string{"<root@x>", msg.InReplyTo}
	return nil
}

func (f *fakeSource) Subscribe(_ context.Context, mb mailbox.Mailbox, folder string, current *mailbox.Subscription, now time.Time) (*mailbox.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subscribeErr != nil {
		return nil, f.subscribeErr
	}
	if current != nil {
		return current, nil
	}
	f.subscriptions++
	return &mailbox.Subscription{ProviderID: mb.Name + "-" + folder, NotificationURL: mb.NotificationURL,
		ClientStateSHA256: "0000000000000000000000000000000000000000000000000000000000000000", ExpiresAt: now.Add(48 * time.Hour)}, nil
}

func (f *fakeSource) Unsubscribe(_ context.Context, _ mailbox.Mailbox, sub mailbox.Subscription) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsubscribed = append(f.unsubscribed, sub.ProviderID)
	return nil
}

func message(id string, received time.Time) mailbox.Message {
	return mailbox.Message{ProviderMessageID: id, InternetMessageID: "<" + id + "@acme.com>", ConversationID: "conv-" + id,
		SenderAddress: "Supplier@Acme.com", SenderName: "Supplier", Subject: "RFQ " + id, ReceivedAt: received}
}

func overlay(name, address string) mailbox.Mailbox {
	return mailbox.Mailbox{Name: name, Owner: mailbox.OwnerOverlay, OwnerRef: "customer/" + name, Provider: mailbox.ProviderMicrosoft365,
		Address: address, Folders: []string{"inbox"}, CredentialSlot: "m365", PollInterval: time.Minute}
}

func intake(store *mailbox.Store, src mailbox.Source) *mailbox.Intake {
	return &mailbox.Intake{Store: store, Sources: map[string]mailbox.Source{mailbox.ProviderMicrosoft365: src}}
}

// makeDue moves every mailbox's next run into the past.
func makeDue(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE mailbox.sources SET next_poll_at = now() - interval '1 second'`)
	require.NoError(t, err)
}

//nolint:gocyclo // One sequential intake proof against real Postgres.
func TestIntakeRecordsEachNewMessageExactlyOnce(t *testing.T) {
	pool, _ := testutil.NewPostgres(t)
	ctx := context.Background()
	store := mailbox.NewStore(pool)
	mb, err := store.UpsertOverlay(ctx, overlay("quotes", "Quotes@Customer.pt"))
	require.NoError(t, err)
	assert.Equal(t, "quotes@customer.pt", mb.Address)

	now := time.Now().UTC()
	src := newFakeSource()
	// First run: two pages; the second page repeats m1 (delta replays).
	src.script("inbox",
		func(cursor string, _ time.Time) (mailbox.Changes, error) {
			assert.Empty(t, cursor)
			return mailbox.Changes{Messages: []mailbox.Message{message("m1", now), message("m2", now)}, Cursor: "page-2", More: true}, nil
		},
		func(cursor string, _ time.Time) (mailbox.Changes, error) {
			assert.Equal(t, "page-2", cursor)
			return mailbox.Changes{Messages: []mailbox.Message{message("m1", now), message("m3", now)}, Cursor: "delta-1"}, nil
		},
	)
	in := intake(store, src)
	in.RunOnce(ctx)

	events, err := store.ClaimEvents(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, events, 3, "a repeated message is recorded once")
	assert.Equal(t, []string{"m1", "m2", "m3"}, []string{events[0].ProviderMessageID, events[1].ProviderMessageID, events[2].ProviderMessageID})
	first := events[0]
	assert.Equal(t, "quotes", first.MailboxName)
	assert.Equal(t, "quotes@customer.pt", first.Address)
	assert.Equal(t, "supplier@acme.com", first.SenderAddress)
	assert.Equal(t, "<parent@m1>", first.InReplyTo)
	assert.Equal(t, []string{"<root@x>", "<parent@m1>"}, first.References)
	assert.Equal(t, "inbox", first.Folder)
	assert.Equal(t, []string{"m1", "m2", "m3"}, src.headerCalls, "reply headers are fetched only for new messages")

	// A lease hides claimed events; acknowledging removes them for good.
	again, err := store.ClaimEvents(ctx, 10, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, again)
	for _, e := range events[:2] {
		require.NoError(t, store.AckEvent(ctx, e.ID))
	}
	_, err = pool.Exec(ctx, `UPDATE mailbox.messages SET claimed_until = now() - interval '1 second'`)
	require.NoError(t, err)
	redelivered, err := store.ClaimEvents(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, redelivered, 1, "an expired claim is delivered again")
	assert.Equal(t, "m3", redelivered[0].ProviderMessageID)

	// Not due yet: nothing runs.
	in.RunOnce(ctx)
	assert.Len(t, src.cursors, 2)

	// Second run resumes from the saved cursor; a later replay of m2 records nothing.
	makeDue(t, pool)
	src.script("inbox", func(cursor string, _ time.Time) (mailbox.Changes, error) {
		assert.Equal(t, "delta-1", cursor)
		return mailbox.Changes{Messages: []mailbox.Message{message("m2", now), message("m4", now)}, Cursor: "delta-2"}, nil
	})
	in.RunOnce(ctx)
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM mailbox.messages`).Scan(&count))
	assert.Equal(t, 4, count)

	// An expired cursor resyncs from the last completed round's start minus
	// the overlap, never before the folder's own baseline. The rounds above
	// completed seconds after the folder was first watched, so the baseline
	// (about now) is the restart point.
	makeDue(t, pool)
	src.script("inbox",
		func(string, time.Time) (mailbox.Changes, error) { return mailbox.Changes{}, mailbox.ErrCursorExpired },
		func(cursor string, since time.Time) (mailbox.Changes, error) {
			assert.Empty(t, cursor)
			assert.WithinDuration(t, now, since, 5*time.Second)
			return mailbox.Changes{Messages: []mailbox.Message{message("m4", now), message("m5", now)}, Cursor: "delta-3"}, nil
		},
	)
	in.RunOnce(ctx)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM mailbox.messages`).Scan(&count))
	assert.Equal(t, 5, count)
	cursor, _, err := store.Cursor(ctx, mb.ID, "inbox")
	require.NoError(t, err)
	assert.Equal(t, "delta-3", cursor)

	status, err := store.GetCredentialStatus(ctx, "m365")
	require.NoError(t, err)
	assert.False(t, status.Failing)
}

// TestIntakeResyncNeverSkipsAnUnfinishedRound reproduces unordered delta
// pages: page one of a round records a message received a minute ago, then the
// cursor expires while a 30-minute-old message is still unseen. The restart
// must cover it, so it starts from the last completed round, not from what the
// partial round recorded.
func TestIntakeResyncNeverSkipsAnUnfinishedRound(t *testing.T) {
	pool, _ := testutil.NewPostgres(t)
	ctx := context.Background()
	store := mailbox.NewStore(pool)
	mb, err := store.UpsertOverlay(ctx, overlay("quotes", "quotes@customer.pt"))
	require.NoError(t, err)
	_, _, err = store.Cursor(ctx, mb.ID, "inbox")
	require.NoError(t, err)
	// The folder has been watched for two hours; nothing completed since.
	_, err = pool.Exec(ctx, `UPDATE mailbox.folder_cursors SET baseline_at = now() - interval '2 hours', synced_through = now() - interval '2 hours'`)
	require.NoError(t, err)

	now := time.Now().UTC()
	src := newFakeSource()
	src.script("inbox",
		func(string, time.Time) (mailbox.Changes, error) {
			return mailbox.Changes{Messages: []mailbox.Message{message("recent", now.Add(-time.Minute))}, Cursor: "page-2", More: true}, nil
		},
		func(string, time.Time) (mailbox.Changes, error) { return mailbox.Changes{}, mailbox.ErrCursorExpired },
		func(cursor string, since time.Time) (mailbox.Changes, error) {
			assert.Empty(t, cursor)
			assert.WithinDuration(t, now.Add(-2*time.Hour), since, 5*time.Second, "restart from the last completed round")
			older := message("older", now.Add(-30*time.Minute))
			return mailbox.Changes{Messages: []mailbox.Message{older, message("recent", now.Add(-time.Minute))}, Cursor: "delta-1"}, nil
		},
	)
	intake(store, src).RunOnce(ctx)

	var ids []string
	rows, err := pool.Query(ctx, `SELECT provider_message_id FROM mailbox.messages ORDER BY seq`)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	assert.Equal(t, []string{"recent", "older"}, ids, "the unseen message is recovered and the seen one is not repeated")

	// Completing the resynced round moves the checkpoint forward.
	var synced time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT synced_through FROM mailbox.folder_cursors WHERE source_id = $1`, mb.ID).Scan(&synced))
	assert.WithinDuration(t, now.Add(-15*time.Minute), synced, 5*time.Second)
}

func TestIntakeFailuresAndSubscriptions(t *testing.T) {
	pool, _ := testutil.NewPostgres(t)
	ctx := context.Background()
	store := mailbox.NewStore(pool)
	in := overlay("quotes", "quotes@customer.pt")
	in.NotificationURL = "https://hooks.example.com/graph"
	mb, err := store.UpsertOverlay(ctx, in)
	require.NoError(t, err)

	src := newFakeSource()
	run := intake(store, src)

	// Credential failure: recorded for the slot with its customer-safe reason,
	// retried later, and cleared by the next success.
	src.script("inbox", func(string, time.Time) (mailbox.Changes, error) {
		return mailbox.Changes{}, &mailbox.CredentialError{Reason: "Microsoft 365 rejected the credentials"}
	})
	run.RunOnce(ctx)
	status, err := store.GetCredentialStatus(ctx, "m365")
	require.NoError(t, err)
	assert.True(t, status.Failing)
	assert.Equal(t, "Microsoft 365 rejected the credentials", status.Reason)
	var delay float64
	require.NoError(t, pool.QueryRow(ctx, `SELECT extract(epoch FROM next_poll_at - now()) FROM mailbox.sources WHERE id = $1`, mb.ID).Scan(&delay))
	assert.InDelta(t, 300, delay, 5, "credential failures back off")

	// Throttling honours Retry-After.
	makeDue(t, pool)
	src.script("inbox", func(string, time.Time) (mailbox.Changes, error) {
		return mailbox.Changes{}, &mailbox.RetryAfterError{After: 42 * time.Second}
	})
	run.RunOnce(ctx)
	require.NoError(t, pool.QueryRow(ctx, `SELECT extract(epoch FROM next_poll_at - now()) FROM mailbox.sources WHERE id = $1`, mb.ID).Scan(&delay))
	assert.InDelta(t, 42, delay, 5)

	// A subscription failure (HOR-356 endpoint not reachable) never stops delta.
	makeDue(t, pool)
	src.subscribeErr = errors.New("validation request to notification URL failed")
	src.script("inbox", func(string, time.Time) (mailbox.Changes, error) {
		return mailbox.Changes{Messages: []mailbox.Message{message("m1", time.Now())}, Cursor: "d1"}, nil
	})
	run.RunOnce(ctx)
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM mailbox.messages`).Scan(&count))
	assert.Equal(t, 1, count)
	status, err = store.GetCredentialStatus(ctx, "m365")
	require.NoError(t, err)
	assert.False(t, status.Failing)

	// Subscriptions are created once and persisted with only a state hash.
	makeDue(t, pool)
	src.subscribeErr = nil
	run.RunOnce(ctx)
	makeDue(t, pool)
	run.RunOnce(ctx)
	assert.Equal(t, 1, src.subscriptions)
	sub, err := store.Subscription(ctx, mb.ID, "inbox")
	require.NoError(t, err)
	require.NotNil(t, sub)
	assert.Equal(t, "quotes-inbox", sub.ProviderID)

	// Turning notifications off makes the subscription stale; it is removed.
	in.NotificationURL = ""
	_, err = store.UpsertOverlay(ctx, in)
	require.NoError(t, err)
	run.RunOnce(ctx)
	assert.Equal(t, []string{"quotes-inbox"}, src.unsubscribed)
	sub, err = store.Subscription(ctx, mb.ID, "inbox")
	require.NoError(t, err)
	assert.Nil(t, sub)
}

func TestOverlayOwnershipAndScope(t *testing.T) {
	pool, _ := testutil.NewPostgres(t)
	ctx := context.Background()
	store := mailbox.NewStore(pool)

	quotes, err := store.UpsertOverlay(ctx, overlay("quotes", "quotes@customer.pt"))
	require.NoError(t, err)

	// An installation-owned mailbox (HOR-674) holds its own name and address.
	_, err = pool.Exec(ctx, `
		INSERT INTO mailbox.sources (name, owner, provider, address, folders, credential_slot)
		VALUES ('ops', 'installation', 'microsoft365', 'ops@customer.pt', '{inbox}', 'm365')`)
	require.NoError(t, err)
	_, err = store.UpsertOverlay(ctx, overlay("ops", "other@customer.pt"))
	assert.ErrorIs(t, err, mailbox.ErrConflict, "the overlay cannot take an installation-owned name")
	_, err = store.UpsertOverlay(ctx, overlay("ops-overlay", "OPS@customer.pt"))
	assert.ErrorIs(t, err, mailbox.ErrConflict, "nor its address")
	require.NoError(t, store.DisableOverlay(ctx, "customer/ops"))
	ops, err := store.GetActive(ctx, "ops")
	require.NoError(t, err)
	assert.Equal(t, mailbox.OwnerInstallation, ops.Owner, "disabling an overlay ref never touches installation rows")

	// Folder and interval changes keep the source; an address change replaces it.
	changed := overlay("quotes", "quotes@customer.pt")
	changed.Folders = []string{"inbox", "archive"}
	same, err := store.UpsertOverlay(ctx, changed)
	require.NoError(t, err)
	assert.Equal(t, quotes.ID, same.ID)
	moved := overlay("quotes", "sales@customer.pt")
	replaced, err := store.UpsertOverlay(ctx, moved)
	require.NoError(t, err)
	assert.NotEqual(t, quotes.ID, replaced.ID)

	// Scope: an active mailbox on the slot, case-insensitively; nothing else.
	for _, tc := range []struct {
		address, slot string
		want          bool
	}{
		{"SALES@customer.pt", "m365", true},
		{"ops@customer.pt", "m365", true},
		{"quotes@customer.pt", "m365", false}, // retired by the address change
		{"sales@customer.pt", "other-slot", false},
		{"ceo@customer.pt", "m365", false},
	} {
		ok, err := store.Permitted(ctx, mailbox.ProviderMicrosoft365, tc.address, tc.slot)
		require.NoError(t, err)
		assert.Equal(t, tc.want, ok, "%s via %s", tc.address, tc.slot)
	}

	require.NoError(t, store.DisableOverlay(ctx, "customer/quotes"))
	_, err = store.GetActive(ctx, "quotes")
	assert.ErrorIs(t, err, mailbox.ErrNotFound)
}
