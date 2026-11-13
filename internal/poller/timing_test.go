package poller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"reco-tech-assignment/internal/obs"
)

// These tests run inside a synctest bubble: time is fake and only advances
// when every goroutine in the bubble is blocked, so "wait 95s" is instant and
// exact. synctest.Test also fails if any goroutine is still running or
// blocked when the test ends, which doubles as a goroutine-leak check.

// recorder logs when each run started, relative to the start of the test.
type recorder struct {
	mu     sync.Mutex
	start  time.Time
	starts []time.Duration
}

func (r *recorder) mark() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, time.Since(r.start))
}

func (r *recorder) got() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.starts)
}

// runFor runs job for d of fake time, then cancels and waits for Run to return.
func runFor(t *testing.T, r Runner, job Job, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx, job)
	}()
	time.Sleep(d)
	cancel()
	<-done // Run must return promptly after cancel; a leaked goroutine fails synctest
}

func TestRunsExactlyOnInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{start: time.Now()}
		job := Job{Name: "users", Interval: 30 * time.Second, Sync: func(context.Context) error {
			rec.mark()
			return nil
		}}
		runFor(t, Runner{Log: discard}, job, 95*time.Second)

		want := []time.Duration{0, 30 * time.Second, 60 * time.Second, 90 * time.Second}
		if got := rec.got(); !slices.Equal(got, want) {
			t.Errorf("runs started at %v, want %v", got, want)
		}
	})
}

func TestSlowRunsDoNotPileUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{start: time.Now()}
		job := Job{Name: "users", Interval: 30 * time.Second, Sync: func(ctx context.Context) error {
			rec.mark()
			time.Sleep(45 * time.Second) // each run is slower than the interval
			return nil
		}}
		runFor(t, Runner{Log: discard}, job, 100*time.Second)

		// Back to back, never concurrent, no burst of queued ticks.
		want := []time.Duration{0, 45 * time.Second, 90 * time.Second}
		if got := rec.got(); !slices.Equal(got, want) {
			t.Errorf("runs started at %v, want %v", got, want)
		}
	})
}

func TestTimeoutCancelsAHungRunAndTheJobContinues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{start: time.Now()}
		m := obs.NewMetrics()
		job := Job{Name: "users", Interval: 30 * time.Second, Timeout: 10 * time.Second,
			Sync: func(ctx context.Context) error {
				rec.mark()
				<-ctx.Done() // hangs until the run's timeout cancels it
				return ctx.Err()
			}}
		runFor(t, Runner{Log: discard, Metrics: m}, job, 45*time.Second)

		want := []time.Duration{0, 30 * time.Second}
		if got := rec.got(); !slices.Equal(got, want) {
			t.Errorf("runs started at %v, want %v: a hung run must not block the next one", got, want)
		}
		j := m.Jobs()["users"]
		if j.Failures != 2 || !strings.Contains(j.LastError, "timed out after 10s") {
			t.Errorf("job stats = %+v, want 2 timeout failures", j)
		}
		if !j.RunningSince.IsZero() {
			t.Error("running_since must be cleared after the run ended")
		}
	})
}

func TestShutdownDuringARunIsNotAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := obs.NewMetrics()
		job := Job{Name: "users", Interval: time.Minute, Sync: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}}
		runFor(t, Runner{Log: discard, Metrics: m}, job, 5*time.Second)

		j := m.Jobs()["users"]
		if j.Failures != 0 || !j.RunningSince.IsZero() {
			t.Errorf("job stats = %+v, want no failures and no run in progress", j)
		}
	})
}

func TestRunOnceTimeoutError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		err := RunOnce(t.Context(), Job{Name: "users", Timeout: time.Second, Sync: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}})
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out after 1s") {
			t.Errorf("err = %v, want a timeout error", err)
		}
	})
}
