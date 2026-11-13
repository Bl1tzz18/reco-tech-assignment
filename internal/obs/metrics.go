package obs

import (
	"expvar"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// staleAfterIntervals: a job is unhealthy when it hasn't succeeded for this
// many intervals (one failed run is normal; three in a row needs attention).
const staleAfterIntervals = 3

// Metrics are process-wide counters and per-job / per-sync state. They are
// expvar values (stdlib), served as JSON on /metrics and /debug/vars. Moving
// to Prometheus or OpenTelemetry later only changes this file.
//
// All methods are safe for concurrent use and are no-ops on a nil *Metrics,
// so instrumented code works without metrics (e.g. in tests).
type Metrics struct {
	root          *expvar.Map
	requests      *expvar.Map // by status code, or "error" for network errors
	retries       *expvar.Int
	rateLimited   *expvar.Int
	requestMs     *expvar.Int
	limiterWaitMs *expvar.Int
	startedAt     time.Time
	now           func() time.Time

	mu    sync.Mutex
	jobs  map[string]*JobStats
	syncs map[string]*SyncStats // key: <workspace>/<resource>
}

// JobStats is the state of one periodic job.
type JobStats struct {
	Interval            string    `json:"interval"`
	Runs                int64     `json:"runs"`
	Failures            int64     `json:"failures"`
	ConsecutiveFailures int64     `json:"consecutive_failures"`
	LastDurationMs      int64     `json:"last_duration_ms"`
	LastSuccess         time.Time `json:"last_success,omitzero"`
	LastError           string    `json:"last_error,omitempty"`
	interval            time.Duration
}

// SyncStats is the result of the last successful sync of one resource in one workspace.
type SyncStats struct {
	Workspace string    `json:"workspace"`
	Resource  string    `json:"resource"`
	Items     int       `json:"items"`
	Written   int       `json:"written"`
	Unchanged int       `json:"unchanged"`
	Removed   int       `json:"removed"`
	FetchedAt time.Time `json:"fetched_at"`
}

// NewMetrics returns an empty, unpublished Metrics. Call expvar.Publish once
// (in main) to also expose it on /debug/vars.
func NewMetrics() *Metrics {
	m := &Metrics{
		root:          new(expvar.Map).Init(),
		requests:      new(expvar.Map).Init(),
		retries:       new(expvar.Int),
		rateLimited:   new(expvar.Int),
		requestMs:     new(expvar.Int),
		limiterWaitMs: new(expvar.Int),
		startedAt:     time.Now(),
		now:           time.Now,
		jobs:          make(map[string]*JobStats),
		syncs:         make(map[string]*SyncStats),
	}
	m.root.Set("http_requests_by_status", m.requests)
	m.root.Set("http_retries_total", m.retries)
	m.root.Set("http_rate_limited_total", m.rateLimited)
	m.root.Set("http_request_ms_total", m.requestMs)
	m.root.Set("rate_limiter_wait_ms_total", m.limiterWaitMs)
	m.root.Set("uptime_s", expvar.Func(func() any { return int64(m.now().Sub(m.startedAt).Seconds()) }))
	m.root.Set("jobs", expvar.Func(func() any { return m.Jobs() }))
	m.root.Set("syncs", expvar.Func(func() any { return m.Syncs() }))
	return m
}

// String renders all metrics as JSON; it makes *Metrics an expvar.Var.
func (m *Metrics) String() string { return m.root.String() }

// LogValue makes the headline counters loggable: log.Info("...", "metrics", m).
func (m *Metrics) LogValue() slog.Value {
	if m == nil {
		return slog.Value{}
	}
	var byStatus []slog.Attr
	m.requests.Do(func(kv expvar.KeyValue) { // sorted by key
		if v, ok := kv.Value.(*expvar.Int); ok {
			byStatus = append(byStatus, slog.Int64(kv.Key, v.Value()))
		}
	})
	return slog.GroupValue(
		slog.Attr{Key: "requests_by_status", Value: slog.GroupValue(byStatus...)},
		slog.Int64("retries", m.retries.Value()),
		slog.Int64("rate_limited", m.rateLimited.Value()),
		slog.Int64("request_ms_total", m.requestMs.Value()),
		slog.Int64("limiter_wait_ms_total", m.limiterWaitMs.Value()),
	)
}

// ObserveRequest records one HTTP attempt. status is 0 for a network error.
func (m *Metrics) ObserveRequest(status int, d time.Duration) {
	if m == nil {
		return
	}
	key := "error"
	if status > 0 {
		key = strconv.Itoa(status)
	}
	m.requests.Add(key, 1)
	m.requestMs.Add(d.Milliseconds())
}

// ObserveRetry records one retried request.
func (m *Metrics) ObserveRetry() {
	if m != nil {
		m.retries.Add(1)
	}
}

// ObserveRateLimited records one 429 from the API.
func (m *Metrics) ObserveRateLimited() {
	if m != nil {
		m.rateLimited.Add(1)
	}
}

// ObserveLimiterWait records time spent waiting on the client-side rate limiter.
func (m *Metrics) ObserveLimiterWait(d time.Duration) {
	if m != nil {
		m.limiterWaitMs.Add(d.Milliseconds())
	}
}

// RegisterJob declares a periodic job, so /healthz can check it even before its first run.
func (m *Metrics) RegisterJob(name string, interval time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[name] = &JobStats{Interval: interval.String(), interval: interval}
}

// ObserveJobRun records the outcome of one run of a registered job.
func (m *Metrics) ObserveJobRun(name string, took time.Duration, err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[name]
	if !ok {
		j = &JobStats{}
		m.jobs[name] = j
	}
	j.Runs++
	j.LastDurationMs = took.Milliseconds()
	if err != nil {
		j.Failures++
		j.ConsecutiveFailures++
		j.LastError = err.Error()
		return
	}
	j.ConsecutiveFailures = 0
	j.LastSuccess = m.now().UTC()
	j.LastError = ""
}

// ObserveSync records the result of a successful sync of one resource.
func (m *Metrics) ObserveSync(s SyncStats) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncs[s.Workspace+"/"+s.Resource] = &s
}

// Jobs returns a snapshot of every job's state.
func (m *Metrics) Jobs() map[string]JobStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]JobStats, len(m.jobs))
	for k, v := range m.jobs {
		out[k] = *v
	}
	return out
}

// Syncs returns a snapshot of the last successful sync per workspace/resource.
func (m *Metrics) Syncs() map[string]SyncStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]SyncStats, len(m.syncs))
	for k, v := range m.syncs {
		out[k] = *v
	}
	return out
}

// Health reports each registered job as "ok", "failing" (recent failures,
// still within its window) or "stale" (no success for staleAfterIntervals
// intervals; counted from process start before the first success).
// healthy is false when any job is stale.
func (m *Metrics) Health() (status map[string]string, healthy bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	status = make(map[string]string, len(m.jobs))
	healthy = true
	for name, j := range m.jobs {
		since := j.LastSuccess
		if since.IsZero() {
			since = m.startedAt
		}
		switch {
		case j.interval > 0 && now.Sub(since) > staleAfterIntervals*j.interval:
			status[name] = "stale"
			healthy = false
		case j.ConsecutiveFailures > 0:
			status[name] = "failing"
		default:
			status[name] = "ok"
		}
	}
	return status, healthy
}
