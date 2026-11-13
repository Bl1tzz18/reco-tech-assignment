package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"reco-tech-assignment/internal/extractor"
)

// budgetWarnFraction is the share of -rpm that polling may use on average.
// The rest is headroom for retries, 429 recovery and other clients of the token.
const budgetWarnFraction = 0.7

// rateBudget estimates the steady-state requests/minute of all jobs
// (requests per sync / interval) and warns when the total exceeds the budget.
// The limiter stays correct either way; this only tells you that syncs will
// queue behind it and take longer than their interval.
type rateBudget struct {
	mu     sync.Mutex
	rpm    int
	perJob map[string]float64
	log    *slog.Logger
}

func newRateBudget(rpm int, log *slog.Logger) *rateBudget {
	return &rateBudget{rpm: rpm, perJob: make(map[string]float64), log: log}
}

// track wraps a sync so every successful run updates the estimate.
func (b *rateBudget) track(job string, every time.Duration,
	sync func(context.Context) (extractor.Result, error)) func(context.Context) error {
	return func(ctx context.Context) error {
		res, err := sync(ctx)
		if err == nil {
			b.record(job, res.Requests, every)
		}
		return err
	}
}

func (b *rateBudget) record(job string, requests int, every time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.perJob[job] = float64(requests) / every.Minutes()
	total := 0.0
	for _, v := range b.perJob {
		total += v
	}
	limit := budgetWarnFraction * float64(b.rpm)
	attrs := []any{"job", job, "requests_per_sync", requests, "job_rpm", round1(b.perJob[job]),
		"total_rpm", round1(total), "budget_rpm", round1(limit)}
	if total > limit {
		b.log.Warn("polling exceeds the rate budget: use longer intervals or incremental sync", attrs...)
		return
	}
	b.log.Debug("rate budget", attrs...)
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
