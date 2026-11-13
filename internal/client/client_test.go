package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"reco-tech-assignment/internal/obs"
)

const testToken = "test-token"

// fakeClock advances time on Sleep instead of waiting, so tests are instant.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sleeps = append(f.sleeps, d)
	f.now = f.now.Add(d)
	return ctx.Err()
}

func (f *fakeClock) Sleeps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sleeps)
}

func newTestClient(t *testing.T, baseURL string, opts Options) (*Client, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	opts.PageSize = max(opts.PageSize, 10)
	opts.BaseBackoff = time.Second
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	opts.Sleep = clock.Sleep
	opts.Now = clock.Now
	opts.Jitter = func(d time.Duration) time.Duration { return d }
	c, err := New(baseURL, testToken, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c, clock
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	fmt.Fprint(w, body)
}

func TestSendsBearerToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, `{"data":[{"gid":"1"}],"next_page":null}`)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, Options{MaxRetries: 2})

	items, err := c.FetchAll(t.Context(), "/users", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Errorf("got %d items, want 1", len(items))
	}
}

func TestFetchAllFollowsOffsetPagination(t *testing.T) {
	const total = 25
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.URL.Query().Get("workspace"); got != "ws1" {
			t.Errorf("workspace param = %q, want ws1 (params must be kept on every page)", got)
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		start, _ := strconv.Atoi(r.URL.Query().Get("offset")) // our fake offset token is just an index
		end := min(start+limit, total)
		data := ""
		for i := start; i < end; i++ {
			if i > start {
				data += ","
			}
			data += fmt.Sprintf(`{"gid":"%d"}`, i)
		}
		next := "null"
		if end < total {
			next = fmt.Sprintf(`{"offset":"%d","path":"/users?offset=%d","uri":"x"}`, end, end)
		}
		writeJSON(w, fmt.Sprintf(`{"data":[%s],"next_page":%s}`, data, next))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, Options{PageSize: 10})

	items, err := c.FetchAll(t.Context(), "/users", map[string][]string{"workspace": {"ws1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != total || requests.Load() != 3 {
		t.Errorf("got %d items in %d requests, want %d in 3", len(items), requests.Load(), total)
	}
}

func TestFetchAllRejectsRepeatedOffset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"data":[{"gid":"1"}],"next_page":{"offset":"same"}}`)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, Options{})

	if _, err := c.FetchAll(t.Context(), "/users", nil); err == nil {
		t.Fatal("expected an error for a repeated offset")
	}
}

func TestRetryAfter429(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter func(now time.Time) string
		want       time.Duration
	}{
		{"seconds", func(time.Time) string { return "7" }, 7 * time.Second},
		{"http-date", func(now time.Time) string { return now.Add(30 * time.Second).Format(http.TimeFormat) }, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			var clock *fakeClock
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					w.Header().Set("Retry-After", tt.retryAfter(clock.Now()))
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				writeJSON(w, `{"data":[],"next_page":null}`)
			}))
			defer srv.Close()
			var c *Client
			c, clock = newTestClient(t, srv.URL, Options{MaxRetries: 3})

			if _, err := c.FetchAll(t.Context(), "/users", nil); err != nil {
				t.Fatal(err)
			}
			if got := clock.Sleeps(); !slices.Equal(got, []time.Duration{tt.want}) {
				t.Errorf("sleeps = %v, want [%v]", got, tt.want)
			}
		})
	}
}

func Test429PausesOtherRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users" {
			w.Header().Set("Retry-After", "10")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(w, `{"data":[],"next_page":null}`)
	}))
	defer srv.Close()
	c, clock := newTestClient(t, srv.URL, Options{MaxRetries: 0})

	if _, err := c.FetchAll(t.Context(), "/users", nil); err == nil {
		t.Fatal("expected the 429 to be returned with MaxRetries 0")
	}
	if _, err := c.FetchAll(t.Context(), "/projects", nil); err != nil {
		t.Fatal(err)
	}
	if got := clock.Sleeps(); !slices.Equal(got, []time.Duration{10 * time.Second}) {
		t.Errorf("sleeps = %v, want [10s]: a request to another path must wait out the 429", got)
	}
}

func TestRetries5xxWithBackoff(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			writeJSON(w, `{"data":[],"next_page":null}`)
		}
	}))
	defer srv.Close()
	c, clock := newTestClient(t, srv.URL, Options{MaxRetries: 3})

	if _, err := c.FetchAll(t.Context(), "/users", nil); err != nil {
		t.Fatal(err)
	}
	if got := clock.Sleeps(); !slices.Equal(got, []time.Duration{time.Second, 2 * time.Second}) {
		t.Errorf("sleeps = %v, want [1s 2s]", got)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, Options{MaxRetries: 2})

	_, err := c.FetchAll(t.Context(), "/users", nil)
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusInternalServerError {
		t.Fatalf("err = %v, want a StatusError 500", err)
	}
	if requests.Load() != 3 {
		t.Errorf("requests = %d, want 3 (1 + 2 retries)", requests.Load())
	}
}

func TestDoesNotRetry401(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, `{"errors":[{"message":"Not Authorized"}]}`)
	}))
	defer srv.Close()
	c, clock := newTestClient(t, srv.URL, Options{MaxRetries: 3})

	_, err := c.FetchAll(t.Context(), "/users", nil)
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want a StatusError 401", err)
	}
	if requests.Load() != 1 || len(clock.Sleeps()) != 0 {
		t.Errorf("requests = %d, sleeps = %v; want 1 request and no sleeps", requests.Load(), clock.Sleeps())
	}
}

func TestRetriesNetworkErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused
	c, clock := newTestClient(t, srv.URL, Options{MaxRetries: 2})

	if _, err := c.FetchAll(t.Context(), "/users", nil); err == nil {
		t.Fatal("expected an error")
	}
	if got := len(clock.Sleeps()); got != 2 {
		t.Errorf("got %d retry sleeps, want 2", got)
	}
}

func TestRejectsNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>maintenance</html>")
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, Options{MaxRetries: 0})

	if _, err := c.FetchAll(t.Context(), "/users", nil); err == nil {
		t.Fatal("expected an error for an HTML body")
	}
}

func TestCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, Options{MaxRetries: 5})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := c.FetchAll(ctx, "/users", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestLimiterSpacesRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"data":[],"next_page":null}`)
	}))
	defer srv.Close()
	c, clock := newTestClient(t, srv.URL, Options{RequestsPerMinute: 60})

	for range 3 {
		if _, err := c.FetchAll(t.Context(), "/users", nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := clock.Sleeps(); !slices.Equal(got, []time.Duration{time.Second, time.Second}) {
		t.Errorf("sleeps = %v, want [1s 1s] at 60 rpm", got)
	}
}

type item struct {
	GID string `json:"gid"`
}

func (i item) Validate() error {
	if i.GID == "" {
		return errors.New("missing gid")
	}
	return nil
}

func TestFetchAllAsSkipsInvalidItems(t *testing.T) {
	bodies := map[string]string{
		"/mixed":   `{"data":[{"gid":"1"},{"name":"no gid"}],"next_page":null}`,
		"/invalid": `{"data":[{"name":"no gid"}],"next_page":null}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, bodies[r.URL.Path])
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, Options{})

	items, err := FetchAllAs[item](t.Context(), c, "/mixed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].GID != "1" {
		t.Errorf("items = %+v, want only gid 1", items)
	}
	if _, err := FetchAllAs[item](t.Context(), c, "/invalid", nil); err == nil {
		t.Error("expected an error when every item is invalid")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{"-3", 0},
		{"garbage", 0},
		{"99999", maxRetryAfter},
		{now.Add(time.Minute).Format(http.TimeFormat), time.Minute},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestTokenNeverLogged(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError) // exercise the retry WARN log too
			return
		}
		writeJSON(w, `{"data":[{"gid":"1"}],"next_page":null}`)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	log, err := obs.NewLogger(&buf, "text", "debug")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := newTestClient(t, srv.URL, Options{MaxRetries: 1, Logger: log})

	if _, err := c.FetchAll(t.Context(), "/users", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "http request") {
		t.Fatalf("expected debug request logs, got: %s", buf.String())
	}
	if strings.Contains(buf.String(), testToken) {
		t.Errorf("token leaked into logs: %s", buf.String())
	}
}

func TestMetricsRecordRequestsRetriesAnd429(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			writeJSON(w, `{"data":[],"next_page":null}`)
		}
	}))
	defer srv.Close()
	m := obs.NewMetrics()
	c, _ := newTestClient(t, srv.URL, Options{MaxRetries: 3, Metrics: m})

	if _, err := c.FetchAll(t.Context(), "/users", nil); err != nil {
		t.Fatal(err)
	}
	var got struct {
		ByStatus    map[string]int `json:"http_requests_by_status"`
		Retries     int            `json:"http_retries_total"`
		RateLimited int            `json:"http_rate_limited_total"`
	}
	if err := json.Unmarshal([]byte(m.String()), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"500": 1, "429": 1, "200": 1}
	if !maps.Equal(got.ByStatus, want) || got.Retries != 2 || got.RateLimited != 1 {
		t.Errorf("metrics = %+v, want by_status %v, 2 retries, 1 rate limited", got, want)
	}
}
