package poller

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"reco-tech-assignment/internal/obs"
)

const testTimeout = 5 * time.Second

var discard = slog.New(slog.DiscardHandler)

// runAsync starts Run and returns a channel closed when it returns.
func runAsync(ctx context.Context, job Job) <-chan struct{} {
	return runAsyncWith(ctx, Runner{Log: discard}, job)
}

func runAsyncWith(ctx context.Context, r Runner, job Job) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx, job)
	}()
	return done
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunRunsImmediatelyThenOnEveryTick(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0 // only touched by the Run goroutine; read after done (happens-before)
	done := runAsync(ctx, Job{Name: "t", Interval: time.Millisecond, Sync: func(context.Context) error {
		calls++
		if calls == 3 {
			cancel()
		}
		return nil
	}})
	waitDone(t, done)
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestRunSurvivesErrorsAndPanics(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	done := runAsync(ctx, Job{Name: "t", Interval: time.Millisecond, Sync: func(context.Context) error {
		calls++
		switch calls {
		case 1:
			return errors.New("boom")
		case 2:
			panic("bug")
		default:
			cancel()
			return nil
		}
	}})
	waitDone(t, done)
	if calls != 3 {
		t.Errorf("calls = %d, want 3: errors and panics must not stop the job", calls)
	}
}

func TestRunStopsDuringWaitOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	done := runAsync(ctx, Job{Name: "t", Interval: time.Hour, Sync: func(context.Context) error {
		close(started)
		return nil
	}})
	<-started // first run happens immediately, not after an hour
	cancel()
	waitDone(t, done) // must not wait for the 1h tick
}

func TestRunOnceTurnsPanicIntoError(t *testing.T) {
	err := RunOnce(t.Context(), Job{Name: "t", Sync: func(context.Context) error { panic("bug") }})
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("err = %v, want a panic error", err)
	}
}

func TestRunOnceWrapsError(t *testing.T) {
	sentinel := errors.New("boom")
	err := RunOnce(t.Context(), Job{Name: "users", Sync: func(context.Context) error { return sentinel }})
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "users") {
		t.Errorf("err = %v, want it to wrap the cause and name the job", err)
	}
}

func TestRunRecordsJobMetrics(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := obs.NewMetrics()
	calls := 0
	done := runAsyncWith(ctx, Runner{Log: discard, Metrics: m}, Job{Name: "users", Interval: time.Millisecond,
		Sync: func(context.Context) error {
			calls++
			switch calls {
			case 1:
				return errors.New("boom")
			case 2:
				return nil
			default:
				cancel() // this run is aborted by shutdown: must not count as a failure
				return context.Canceled
			}
		}})
	waitDone(t, done)

	j := m.Jobs()["users"]
	if j.Runs != 2 || j.Failures != 1 || j.ConsecutiveFailures != 0 || j.LastSuccess.IsZero() || j.Interval != "1ms" {
		t.Errorf("job stats = %+v, want 2 runs, 1 failure, recovered", j)
	}
}
