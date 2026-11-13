package client

import (
	"context"
	"sync"
	"time"
)

// limiter is a token bucket shared by every request of a Client. It refills
// at rpm tokens per minute and holds at most burst tokens. pause() blocks all
// requests until a deadline (used after a 429).
type limiter struct {
	mu          sync.Mutex
	perToken    time.Duration // time.Minute / rpm; 0 = unlimited
	burst       float64
	tokens      float64 // below 0 = reserved by requests that are waiting
	last        time.Time
	pausedUntil time.Time
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error
}

func newLimiter(rpm, burst int, now func() time.Time, sleep func(context.Context, time.Duration) error) *limiter {
	l := &limiter{burst: float64(max(burst, 1)), now: now, sleep: sleep}
	l.tokens = l.burst
	if rpm > 0 {
		l.perToken = time.Minute / time.Duration(rpm)
	}
	return l
}

// wait reserves a token under the lock, then sleeps WITHOUT holding it, so
// concurrent callers queue up fairly instead of blocking each other.
func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := l.now()
	var delay time.Duration
	if l.perToken > 0 {
		if !l.last.IsZero() {
			l.tokens = min(l.burst, l.tokens+float64(now.Sub(l.last))/float64(l.perToken))
		}
		l.last = now
		l.tokens--
		if l.tokens < 0 {
			delay = time.Duration(-l.tokens * float64(l.perToken))
		}
	}
	delay = max(delay, l.pausedUntil.Sub(now))
	l.mu.Unlock()

	if delay <= 0 {
		return ctx.Err()
	}
	return l.sleep(ctx, delay)
}

// pause stops all requests for d. It never shortens an existing pause.
func (l *limiter) pause(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until := l.now().Add(d); until.After(l.pausedUntil) {
		l.pausedUntil = until
	}
}
