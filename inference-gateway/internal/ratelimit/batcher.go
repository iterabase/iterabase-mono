package ratelimit

import (
	"context"
	"time"
)

// TPMBatcher accumulates token counts and flushes them to the limiter
// periodically. Used during streaming to avoid a database write per SSE chunk.
type TPMBatcher struct {
	limiter  Limiter
	keyID    string
	interval time.Duration

	pending  int
	ticker   *time.Ticker
	stopCh   chan struct{}
	tokensCh chan int
	done     chan struct{}
}

// NewTPMBatcher creates a batcher that flushes accumulated tokens to the
// limiter every `interval`.
func NewTPMBatcher(limiter Limiter, keyID string, interval time.Duration) *TPMBatcher {
	return &TPMBatcher{
		limiter:  limiter,
		keyID:    keyID,
		interval: interval,
		tokensCh: make(chan int, 256),
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins the background flush loop.
func (b *TPMBatcher) Start() {
	b.ticker = time.NewTicker(b.interval)
	go b.loop()
}

// Add queues tokens to be flushed on the next interval.
func (b *TPMBatcher) Add(tokens int) {
	select {
	case b.tokensCh <- tokens:
	default:
		// Channel full — accumulate will happen on next flush.
	}
}

// Stop flushes any remaining tokens and stops the background loop.
func (b *TPMBatcher) Stop(ctx context.Context) {
	close(b.stopCh)
	<-b.done

	// Final flush of anything remaining.
	if b.pending > 0 {
		b.limiter.IncrementTPM(ctx, b.keyID, b.pending) //nolint:errcheck
		b.pending = 0
	}
}

func (b *TPMBatcher) loop() {
	defer close(b.done)
	defer b.ticker.Stop()

	for {
		select {
		case <-b.stopCh:
			// Drain the tokens channel.
			for {
				select {
				case tokens := <-b.tokensCh:
					b.pending += tokens
				default:
					return
				}
			}
		case tokens := <-b.tokensCh:
			b.pending += tokens
		case <-b.ticker.C:
			b.flush()
		}
	}
}

func (b *TPMBatcher) flush() {
	// Drain all pending tokens from the channel.
	for {
		select {
		case tokens := <-b.tokensCh:
			b.pending += tokens
		default:
			goto done
		}
	}
done:
	if b.pending > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		b.limiter.IncrementTPM(ctx, b.keyID, b.pending) //nolint:errcheck
		cancel()
		b.pending = 0
	}
}
