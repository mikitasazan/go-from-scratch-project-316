package crawler

import (
	"context"
	"sync"
	"time"
)

// limiter spaces out requests across the whole run, not per worker: every
// caller takes the next slot in one shared schedule, so eight workers with a
// 200ms interval still make five requests a second between them.
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newLimiter(interval time.Duration) *limiter {
	return &limiter{interval: interval}
}

// wait blocks until this caller's slot comes up. Without an interval it does
// nothing at all, so an unlimited crawl is not slowed down by the bookkeeping.
// A cancelled context ends the wait immediately.
func (l *limiter) wait(ctx context.Context) error {
	if l == nil || l.interval <= 0 {
		return nil
	}

	l.mu.Lock()

	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}

	slot := l.next
	l.next = slot.Add(l.interval)

	l.mu.Unlock()

	pause := time.Until(slot)
	if pause <= 0 {
		return nil
	}

	timer := time.NewTimer(pause)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// interval turns the two speed settings into the one number the limiter uses.
// RPS wins when both are given, because it is the more specific instruction.
func interval(rps float64, delay time.Duration) time.Duration {
	if rps > 0 {
		return time.Duration(float64(time.Second) / rps)
	}

	if delay > 0 {
		return delay
	}

	return 0
}
