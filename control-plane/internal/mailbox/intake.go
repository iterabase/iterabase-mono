package mailbox

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Intake defaults.
const (
	defaultTick        = 5 * time.Second
	defaultLease       = 2 * time.Minute
	defaultBatch       = 16
	maxBatchesPerRun   = 20
	credentialBackoff  = 5 * time.Minute
	transientBackoff   = time.Minute
	staleSweepPageSize = 32
)

// Intake runs delta catch-up, polling-only intake and the subscription
// lifecycle for every active mailbox (DES-HOR-358-01/02). It runs inside the
// tool gateway process, which alone holds mailbox credentials.
type Intake struct {
	Store   *Store
	Sources map[string]Source // by provider
	Log     *slog.Logger
	// Tick is how often due mailboxes are claimed. Each mailbox runs at its
	// own poll interval.
	Tick time.Duration
	// Lease bounds how long one replica holds a claimed mailbox.
	Lease time.Duration
	Now   func() time.Time
}

// Run claims and processes due mailboxes until ctx ends.
func (in *Intake) Run(ctx context.Context) {
	tick := in.Tick
	if tick <= 0 {
		tick = defaultTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		in.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce processes every currently due mailbox once, then sweeps stale
// subscriptions.
func (in *Intake) RunOnce(ctx context.Context) {
	lease := in.Lease
	if lease <= 0 {
		lease = defaultLease
	}
	for ctx.Err() == nil {
		due, err := in.Store.ClaimDue(ctx, lease, defaultBatch)
		if err != nil {
			in.log().Warn("mailbox intake: claim failed", "error", err)
			return
		}
		for _, mb := range due {
			next := in.process(ctx, mb)
			if err := in.Store.Reschedule(context.WithoutCancel(ctx), mb.ID, next); err != nil {
				in.log().Warn("mailbox intake: reschedule failed", "mailbox", mb.Name, "error", err)
			}
		}
		if len(due) < defaultBatch {
			break
		}
	}
	in.sweepStaleSubscriptions(ctx)
}

// process runs one intake pass for mb and returns the delay to its next run.
func (in *Intake) process(ctx context.Context, mb Mailbox) time.Duration {
	src, ok := in.Sources[mb.Provider]
	if !ok {
		in.log().Warn("mailbox intake: no source for provider", "mailbox", mb.Name, "provider", mb.Provider)
		return credentialBackoff
	}
	interval := mb.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	for _, folder := range mb.Folders {
		if mb.NotificationURL != "" {
			if err := in.subscribe(ctx, src, mb, folder); err != nil {
				// Notifications are an optimisation (HOR-356 decision): a
				// subscription failure never stops delta catch-up.
				if delay, stop := in.classify(ctx, mb, "subscribe", err); stop {
					return delay
				}
			}
		}
		if err := in.drain(ctx, src, mb, folder); err != nil {
			delay, _ := in.classify(ctx, mb, "delta", err)
			return delay
		}
	}
	if err := in.Store.RecordCredentialStatus(ctx, mb.CredentialSlot, false, ""); err != nil {
		in.log().Warn("mailbox intake: credential status", "mailbox", mb.Name, "error", err)
	}
	return interval
}

// classify records the outcome of a failed provider call and returns the
// retry delay, plus whether the pass should stop now.
func (in *Intake) classify(ctx context.Context, mb Mailbox, op string, err error) (time.Duration, bool) {
	var retry *RetryAfterError
	var cred *CredentialError
	switch {
	case errors.As(err, &retry):
		in.log().Info("mailbox intake: provider throttled", "mailbox", mb.Name, "op", op, "retry_after", retry.After)
		return max(retry.After, time.Second), true
	case errors.As(err, &cred):
		in.log().Warn("mailbox intake: credential failure", "mailbox", mb.Name, "op", op, "slot", mb.CredentialSlot, "reason", cred.Reason)
		if serr := in.Store.RecordCredentialStatus(ctx, mb.CredentialSlot, true, cred.Reason); serr != nil {
			in.log().Warn("mailbox intake: credential status", "mailbox", mb.Name, "error", serr)
		}
		return credentialBackoff, true
	case ctx.Err() != nil:
		return transientBackoff, true
	default:
		in.log().Warn("mailbox intake: provider call failed", "mailbox", mb.Name, "op", op, "error", err)
		return transientBackoff, op != "subscribe"
	}
}

// drain records every new message in folder, advancing the cursor only after
// the batch it covers is durable.
func (in *Intake) drain(ctx context.Context, src Source, mb Mailbox, folder string) error {
	resynced := false
	for range maxBatchesPerRun {
		cursor, baseline, err := in.Store.Cursor(ctx, mb.ID, folder)
		if err != nil {
			return err
		}
		started := in.now()
		changes, err := src.Changes(ctx, mb, folder, cursor, baseline)
		if errors.Is(err, ErrCursorExpired) && !resynced {
			in.log().Info("mailbox intake: cursor expired, resyncing", "mailbox", mb.Name, "folder", folder)
			if err := in.Store.ResetCursor(ctx, mb.ID, folder); err != nil {
				return err
			}
			resynced = true
			continue
		}
		if err != nil {
			return err
		}
		if err := in.record(ctx, src, mb, folder, changes.Messages); err != nil {
			return err
		}
		if err := in.Store.SaveCursor(ctx, mb.ID, folder, changes.Cursor, !changes.More, started); err != nil {
			return err
		}
		if !changes.More {
			return nil
		}
	}
	return nil
}

func (in *Intake) record(ctx context.Context, src Source, mb Mailbox, folder string, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ProviderMessageID
	}
	known, err := in.Store.KnownMessages(ctx, mb.ID, ids)
	if err != nil {
		return err
	}
	fresh := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if known[m.ProviderMessageID] {
			continue
		}
		known[m.ProviderMessageID] = true
		m.Folder = folder
		if err := src.ReplyHeaders(ctx, mb, &m); err != nil {
			return err
		}
		fresh = append(fresh, m)
	}
	n, err := in.Store.RecordMessages(ctx, mb.ID, fresh)
	if err != nil {
		return err
	}
	if n > 0 {
		in.log().Info("mailbox intake: recorded messages", "mailbox", mb.Name, "folder", folder, "count", n)
	}
	return nil
}

func (in *Intake) subscribe(ctx context.Context, src Source, mb Mailbox, folder string) error {
	current, err := in.Store.Subscription(ctx, mb.ID, folder)
	if err != nil {
		return err
	}
	if current != nil && current.NotificationURL != mb.NotificationURL {
		// The stale sweep removes it; create the new one now.
		current = nil
	}
	next, err := src.Subscribe(ctx, mb, folder, current, in.now())
	if err != nil {
		return err
	}
	if next == nil || (current != nil && *next == *current) {
		return nil
	}
	return in.Store.SaveSubscription(ctx, mb.ID, folder, *next)
}

func (in *Intake) sweepStaleSubscriptions(ctx context.Context) {
	stale, err := in.Store.StaleSubscriptions(ctx, staleSweepPageSize)
	if err != nil {
		in.log().Warn("mailbox intake: list stale subscriptions", "error", err)
		return
	}
	for _, st := range stale {
		src, ok := in.Sources[st.Mailbox.Provider]
		if !ok {
			continue
		}
		if err := src.Unsubscribe(ctx, st.Mailbox, st.Subscription); err != nil {
			// An expired subscription stops on its own; keep the row and retry
			// until the provider expiry, then forget it.
			if in.now().Before(st.Subscription.ExpiresAt) {
				in.log().Warn("mailbox intake: unsubscribe failed", "mailbox", st.Mailbox.Name, "error", err)
				continue
			}
		}
		if err := in.Store.DeleteSubscription(ctx, st.Subscription.ProviderID); err != nil {
			in.log().Warn("mailbox intake: forget subscription", "mailbox", st.Mailbox.Name, "error", err)
		}
	}
}

func (in *Intake) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

func (in *Intake) log() *slog.Logger {
	if in.Log != nil {
		return in.Log
	}
	return slog.Default()
}
