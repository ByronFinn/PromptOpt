package provider

import (
	"context"
	"sync"
	"time"
)

// rateLimiter is a minimal token-bucket pacing gate with a one-token
// capacity: Wait grants one token per interval (1/rps), so consecutive
// requests leave at least that far apart. It is deliberately tiny —
// golang.org/x/time/rate would violate the standard-library-first rule
// — and safe for concurrent use. Zero configuration (rps <= 0) yields
// no limiter at all: pacing stays off until asked for.
type rateLimiter struct {
	mu       sync.Mutex
	next     time.Time     // earliest instant the next token is granted
	interval time.Duration // 1/rps, the spacing between consecutive grants
	now      func() time.Time
}

// newRateLimiter returns a limiter pacing at rps requests per second,
// or nil when rps <= 0 (off, the default).
func newRateLimiter(rps float64, now func() time.Time) *rateLimiter {
	if rps <= 0 {
		return nil
	}
	return &rateLimiter{
		next:     now(),
		interval: time.Duration(float64(time.Second) / rps),
		now:      now,
	}
}

// Wait blocks until the next token is granted or ctx is done. The
// reservation happens under the lock — the n-th concurrent caller
// reserves the n-th slot — so concurrent Chats keep their spacing
// instead of bursting through. Canceled callers release nothing: their
// slot simply lapses and later callers re-anchor on now.
func (l *rateLimiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	now := l.now()
	start := l.next
	if start.Before(now) {
		start = now
	}
	l.next = start.Add(l.interval)
	l.mu.Unlock()

	delay := start.Sub(now)
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
