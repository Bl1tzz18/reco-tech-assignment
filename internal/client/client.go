// Package client talks to the Asana REST API: Bearer auth, a shared
// client-side rate limiter, retries with backoff and jitter,
// 429/Retry-After handling, and offset pagination.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"reco-tech-assignment/internal/obs"
)

const (
	defaultTimeout     = 10 * time.Second
	defaultBaseBackoff = 500 * time.Millisecond
	defaultPageSize    = 100
	maxPageSize        = 100      // Asana's documented maximum for `limit`
	maxBodyBytes       = 10 << 20 // 10 MiB: guard against a runaway response
	maxErrorSnippet    = 200      // bytes of an error body kept in StatusError
)

// Options configures a Client. Zero values pick sensible defaults.
type Options struct {
	RequestsPerMinute int // 0 disables the client-side limit
	Burst             int // 0 = 1 (requests evenly spaced)
	MaxRetries        int // retries after the first attempt
	PageSize          int // items per page, 1..100
	Timeout           time.Duration
	BaseBackoff       time.Duration
	Logger            *slog.Logger
	Metrics           *obs.Metrics // nil = no metrics

	// Test hooks; nil = real implementations.
	Sleep  func(ctx context.Context, d time.Duration) error
	Now    func() time.Time
	Jitter func(d time.Duration) time.Duration
}

// Client is safe for concurrent use. Share ONE Client between all jobs so
// they share one rate limit, as Asana's limit is per token.
type Client struct {
	baseURL     *url.URL
	token       string
	http        *http.Client
	limiter     *limiter
	maxRetries  int
	pageSize    int
	baseBackoff time.Duration
	log         *slog.Logger
	metrics     *obs.Metrics
	sleep       func(ctx context.Context, d time.Duration) error
	now         func() time.Time
	jitter      func(d time.Duration) time.Duration
}

// New returns a Client for baseURL (e.g. https://app.asana.com/api/1.0).
func New(baseURL, token string, opts Options) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("base URL %q must be an absolute http(s) URL", baseURL)
	}
	if token == "" {
		return nil, errors.New("empty API token")
	}
	if opts.PageSize <= 0 || opts.PageSize > maxPageSize {
		opts.PageSize = defaultPageSize
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.BaseBackoff <= 0 {
		opts.BaseBackoff = defaultBaseBackoff
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Jitter == nil {
		opts.Jitter = equalJitter
	}
	return &Client{
		baseURL:     u,
		token:       token,
		http:        &http.Client{Timeout: opts.Timeout},
		limiter:     newLimiter(opts.RequestsPerMinute, opts.Burst, opts.Now, opts.Sleep),
		maxRetries:  max(opts.MaxRetries, 0),
		pageSize:    opts.PageSize,
		baseBackoff: opts.BaseBackoff,
		log:         opts.Logger,
		metrics:     opts.Metrics,
		sleep:       opts.Sleep,
		now:         opts.Now,
		jitter:      opts.Jitter,
	}, nil
}

// PageSize is the number of items requested per page.
func (c *Client) PageSize() int { return c.pageSize }

// StatusError is returned for any non-2xx response.
type StatusError struct {
	StatusCode int
	RetryAfter time.Duration // parsed Retry-After header; 0 if absent
	Body       string        // first bytes of the body (Asana puts the reason in errors[].message)
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("unexpected status %d", e.StatusCode)
	switch e.StatusCode {
	case http.StatusUnauthorized:
		msg += " (check that API_TOKEN is a valid Asana personal access token)"
	case http.StatusForbidden:
		msg += " (the token has no access to this resource)"
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// doOnce performs a single GET: wait for the limiter, send, read, classify.
func (c *Client) doOnce(ctx context.Context, rawURL string) ([]byte, error) {
	waited, err := c.limiter.wait(ctx)
	c.metrics.ObserveLimiterWait(waited)
	if waited > 0 {
		c.log.DebugContext(ctx, "rate limiter wait", "wait", waited)
	}
	if err != nil {
		return nil, fmt.Errorf("rate limiter: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	start := c.now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.metrics.ObserveRequest(0, c.now().Sub(start))
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	took := c.now().Sub(start)
	c.metrics.ObserveRequest(resp.StatusCode, took)
	// Path only: no headers (Authorization) and no query noise.
	c.log.DebugContext(ctx, "http request", "method", req.Method, "path", req.URL.Path,
		"status", resp.StatusCode, "bytes", len(body), "took", took)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := &StatusError{
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), c.now()),
			Body:       snippet(body),
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			c.metrics.ObserveRateLimited()
			c.log.WarnContext(ctx, "rate limited by the API", "path", req.URL.Path, "retry_after", se.RetryAfter)
			if se.RetryAfter > 0 {
				c.limiter.pause(se.RetryAfter) // the limit is per token: every goroutine backs off
			}
		}
		return nil, se
	}
	// A proxy or maintenance page can answer 200 with HTML; don't decode that as data.
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "json") {
		return nil, fmt.Errorf("unexpected content type %q: %s", ct, snippet(body))
	}
	return body, nil
}

func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > maxErrorSnippet {
		s = strings.ToValidUTF8(s[:maxErrorSnippet], "") + "..."
	}
	return s
}
