package client

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

const (
	maxBackoff      = 30 * time.Second
	maxRetryAfter   = 5 * time.Minute // cap a bogus Retry-After so we never stall for hours
	maxBackoffShift = 30              // 1<<30 * base would overflow; maxBackoff applies anyway
)

// get runs doOnce with retries for transient failures (408, 429, 5xx, network).
func (c *Client) get(ctx context.Context, rawURL string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		body, err := c.doOnce(ctx, rawURL)
		if err == nil {
			return body, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retryable(err) {
			return nil, err
		}
		if attempt >= c.maxRetries {
			return nil, fmt.Errorf("giving up after %d attempts: %w", attempt+1, err)
		}
		wait, ownSleep := c.retryDelay(err, attempt)
		c.log.Warn("request failed, retrying", "url", rawURL, "attempt", attempt+1, "wait", wait, "err", err)
		if !ownSleep {
			continue // 429: doOnce already paused the shared limiter; the next attempt waits there
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// retryDelay returns how long to wait before the next attempt, and whether
// get must sleep itself (false when the shared limiter already holds the pause).
func (c *Client) retryDelay(err error, attempt int) (time.Duration, bool) {
	var se *StatusError
	if errors.As(err, &se) && se.RetryAfter > 0 {
		return se.RetryAfter, se.StatusCode != http.StatusTooManyRequests
	}
	return c.jitter(c.backoff(attempt)), true
}

// retryable: other 4xx (400/401/403/404) are our bug or a bad token; retrying won't help.
func retryable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.StatusCode == http.StatusRequestTimeout ||
			se.StatusCode == http.StatusTooManyRequests ||
			se.StatusCode >= http.StatusInternalServerError
	}
	return true // network error, timeout, unexpected content type
}

// backoff is exponential: base, 2*base, 4*base, ... capped at maxBackoff.
func (c *Client) backoff(attempt int) time.Duration {
	if attempt >= maxBackoffShift {
		return maxBackoff
	}
	return min(c.baseBackoff*(1<<attempt), maxBackoff)
}

// equalJitter returns a random duration in [d/2, d], so many clients
// don't retry in lockstep.
func equalJitter(d time.Duration) time.Duration {
	half := d / 2
	return half + rand.N(half+1)
}

// parseRetryAfter accepts both forms allowed by RFC 9110: seconds or an HTTP-date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return clampWait(time.Duration(secs) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return clampWait(t.Sub(now))
	}
	return 0
}

func clampWait(d time.Duration) time.Duration { return min(max(d, 0), maxRetryAfter) }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
