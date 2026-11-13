package obs

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWithAddsContextAttrsToEveryLog(t *testing.T) {
	var buf bytes.Buffer
	log, err := NewLogger(&buf, "json", "info")
	if err != nil {
		t.Fatal(err)
	}
	ctx := With(t.Context(), "job", "users")
	child := With(ctx, "run", 7)
	sibling := With(ctx, "run", 8) // must not leak into child (shared backing array)

	log.InfoContext(child, "hello", "k", "v")
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["job"] != "users" || got["run"] != float64(7) || got["k"] != "v" {
		t.Errorf("log = %v, want job=users run=7 k=v", got)
	}
	_ = sibling
}

func TestRedactsSensitiveKeys(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewLogger(&buf, "text", "info")
	log.Info("oops", "token", "s3cret", "Authorization", "Bearer s3cret")
	if strings.Contains(buf.String(), "s3cret") {
		t.Errorf("secret leaked: %s", buf.String())
	}
}

func TestJSONDurationsAreHumanReadable(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewLogger(&buf, "json", "info")
	log.Info("x", "took", 1500*time.Millisecond)
	if !strings.Contains(buf.String(), `"took":"1.5s"`) {
		t.Errorf("log = %s, want took as \"1.5s\"", buf.String())
	}
}

func TestNewLoggerRejectsBadConfig(t *testing.T) {
	if _, err := NewLogger(&bytes.Buffer{}, "xml", "info"); err == nil {
		t.Error("expected an error for format xml")
	}
	if _, err := NewLogger(&bytes.Buffer{}, "text", "loud"); err == nil {
		t.Error("expected an error for level loud")
	}
}

func TestMetricsCountersAndJSON(t *testing.T) {
	m := NewMetrics()
	m.ObserveRequest(200, 100*time.Millisecond)
	m.ObserveRequest(200, 50*time.Millisecond)
	m.ObserveRequest(429, 10*time.Millisecond)
	m.ObserveRequest(0, time.Millisecond)
	m.ObserveRetry()
	m.ObserveRateLimited()
	m.ObserveSync(SyncStats{Workspace: "1", Resource: "users", Items: 5, Written: 2, Unchanged: 3})

	var got struct {
		ByStatus    map[string]int       `json:"http_requests_by_status"`
		Retries     int                  `json:"http_retries_total"`
		RateLimited int                  `json:"http_rate_limited_total"`
		RequestMs   int                  `json:"http_request_ms_total"`
		Syncs       map[string]SyncStats `json:"syncs"`
	}
	if err := json.Unmarshal([]byte(m.String()), &got); err != nil {
		t.Fatalf("metrics are not valid JSON: %v\n%s", err, m)
	}
	if got.ByStatus["200"] != 2 || got.ByStatus["429"] != 1 || got.ByStatus["error"] != 1 {
		t.Errorf("by status = %v", got.ByStatus)
	}
	if got.Retries != 1 || got.RateLimited != 1 || got.RequestMs != 161 {
		t.Errorf("counters = %+v", got)
	}
	if got.Syncs["1/users"].Items != 5 {
		t.Errorf("syncs = %v", got.Syncs)
	}
}

func TestNilMetricsIsNoop(t *testing.T) {
	var m *Metrics
	m.ObserveRequest(200, time.Second)
	m.ObserveRetry()
	m.RegisterJob("x", time.Minute, 0)
	m.ObserveJobStart("x")
	m.ObserveJobAborted("x")
	m.ObserveJobRun("x", time.Second, nil)
	m.ObserveSync(SyncStats{})
}

func TestHealth(t *testing.T) {
	m := NewMetrics()
	now := m.startedAt
	m.now = func() time.Time { return now }
	m.RegisterJob("users", time.Minute, 0)

	if _, healthy := m.Health(); !healthy {
		t.Error("a job that just started must be healthy")
	}
	m.ObserveJobRun("users", time.Second, errors.New("boom"))
	if st, healthy := m.Health(); !healthy || st["users"] != "failing" {
		t.Errorf("one failure: status=%v healthy=%v, want failing but healthy", st, healthy)
	}
	now = now.Add(4 * time.Minute) // > 3 intervals without a success
	if st, healthy := m.Health(); healthy || st["users"] != "stale" {
		t.Errorf("status=%v healthy=%v, want stale and unhealthy", st, healthy)
	}
	m.ObserveJobRun("users", time.Second, nil)
	if st, healthy := m.Health(); !healthy || st["users"] != "ok" {
		t.Errorf("after success: status=%v healthy=%v", st, healthy)
	}
}

func TestHealthDetectsStuckRun(t *testing.T) {
	m := NewMetrics()
	now := m.startedAt
	m.now = func() time.Time { return now }
	m.RegisterJob("users", time.Hour, 10*time.Second)

	m.ObserveJobStart("users")
	now = now.Add(5 * time.Second)
	if st, healthy := m.Health(); !healthy || st["users"] != "ok" {
		t.Errorf("run within timeout: status=%v healthy=%v", st, healthy)
	}
	if m.Jobs()["users"].RunningSince.IsZero() {
		t.Error("running_since must be set while a run is in progress")
	}
	now = now.Add(10 * time.Second)
	if st, healthy := m.Health(); healthy || st["users"] != "stuck" {
		t.Errorf("run past its timeout: status=%v healthy=%v, want stuck", st, healthy)
	}
	m.ObserveJobRun("users", 15*time.Second, nil)
	if st, _ := m.Health(); st["users"] != "ok" || !m.Jobs()["users"].RunningSince.IsZero() {
		t.Errorf("after the run ended: status=%v, running_since must be cleared", st)
	}
}

func TestMetricsExposeGoroutines(t *testing.T) {
	var got struct {
		Goroutines int `json:"goroutines"`
	}
	if err := json.Unmarshal([]byte(NewMetrics().String()), &got); err != nil || got.Goroutines < 1 {
		t.Errorf("goroutines = %d, err = %v", got.Goroutines, err)
	}
}

func TestHealthzEndpoint(t *testing.T) {
	m := NewMetrics()
	m.RegisterJob("users", time.Minute, 0)
	srv := httptest.NewServer(Handler(m))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	m.now = func() time.Time { return m.startedAt.Add(time.Hour) }
	resp2, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 for a stale job", resp2.StatusCode)
	}
}
