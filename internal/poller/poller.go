// Package poller runs each job immediately and then on its own interval,
// until the context is cancelled. A failing (or panicking) run is logged and
// retried on the next tick; it never stops the job or the other jobs.
package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"reco-tech-assignment/internal/obs"
)

// failureAlertThreshold is the number of consecutive failures after which a
// job logs at ERROR instead of WARN (a production system would alert here).
const failureAlertThreshold = 3

// Job is one periodic unit of work.
type Job struct {
	Name     string
	Interval time.Duration
	Timeout  time.Duration // per run; 0 = no limit. A timed-out run is a failure.
	Sync     func(ctx context.Context) error
}

// Runner runs jobs with logging and metrics. A nil Metrics disables metrics.
type Runner struct {
	Log     *slog.Logger
	Metrics *obs.Metrics
}

// Run runs job now and then every job.Interval until ctx is cancelled.
// time.Ticker drops ticks while a run is in progress, so a slow run delays
// the next one instead of piling runs up.
//
// Every log line of a run (here, in the extractor and in the client) carries
// job=<name> and run=<n>, so a single run can be traced end to end.
func (r Runner) Run(ctx context.Context, job Job) {
	ctx = obs.With(ctx, "job", job.Name)
	r.Metrics.RegisterJob(job.Name, job.Interval, job.Timeout)
	ticker := time.NewTicker(job.Interval)
	defer ticker.Stop()

	failures := 0
	for run := 1; ; run++ {
		runCtx := obs.With(ctx, "run", run)
		r.Metrics.ObserveJobStart(job.Name)
		start := time.Now()
		err := RunOnce(runCtx, job)
		took := time.Since(start).Round(time.Millisecond)
		switch {
		case ctx.Err() != nil:
			// Shutting down: an aborted run is not a failure.
			r.Metrics.ObserveJobAborted(job.Name)
		case err == nil:
			r.Metrics.ObserveJobRun(job.Name, took, nil)
			if failures > 0 {
				r.Log.InfoContext(runCtx, "job recovered", "after_failures", failures)
			}
			failures = 0
			r.Log.InfoContext(runCtx, "run ok", "interval", job.Interval, "took", took)
		default:
			r.Metrics.ObserveJobRun(job.Name, took, err)
			failures++
			level := slog.LevelWarn
			if failures >= failureAlertThreshold {
				level = slog.LevelError
			}
			r.Log.Log(runCtx, level, "run failed", "interval", job.Interval,
				"consecutive_failures", failures, "took", took, "err", err)
		}
		if took > job.Interval && ctx.Err() == nil {
			r.Log.WarnContext(runCtx, "run took longer than its interval; the next run starts right away",
				"interval", job.Interval, "took", took)
		}

		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
		// Checked after the select, not as one of its cases: when both a tick
		// and the cancellation are ready, select picks at random, which could
		// start one more run after shutdown was requested.
		if ctx.Err() != nil {
			r.Log.InfoContext(ctx, "poller stopped")
			return
		}
	}
}

// RunOnce runs job a single time, bounded by job.Timeout. A panic becomes an
// error, so one buggy job can't take the whole process (and the other jobs) down.
func RunOnce(ctx context.Context, job Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("job %s panicked: %v", job.Name, r)
		}
	}()
	runCtx := ctx
	if job.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, job.Timeout)
		defer cancel()
	}
	if err := job.Sync(runCtx); err != nil {
		// The run's own deadline (not the parent's) expired: say so explicitly.
		if ctx.Err() == nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("job %s: timed out after %s: %w", job.Name, job.Timeout, err)
		}
		return fmt.Errorf("job %s: %w", job.Name, err)
	}
	return nil
}
