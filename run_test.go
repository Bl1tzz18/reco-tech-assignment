package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"reco-tech-assignment/internal/obs"
)

// End-to-end tests of run(): flags -> client -> extractor -> storage -> metrics,
// against a fake Asana. Only the network is faked.

const testToken = "e2e-token"

// fakeAsana serves one workspace ("100") with paginated users and projects.
type fakeAsana struct {
	failProjects atomic.Bool
	requests     atomic.Int32
}

func (f *fakeAsana) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/workspaces":
		fmt.Fprint(w, `{"data":[{"gid":"100","name":"Acme","is_organization":true}],"next_page":null}`)
	case "/users":
		servePage(w, r, 5, func(i int) string { return fmt.Sprintf(`{"gid":"%d","name":"User %d"}`, 10+i, i) })
	case "/projects":
		if f.failProjects.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		servePage(w, r, 3, func(i int) string { return fmt.Sprintf(`{"gid":"%d","name":"Project %d"}`, 20+i, i) })
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// servePage serves total items with Asana's limit/offset pagination. The
// offset token is just the index of the next item.
func servePage(w http.ResponseWriter, r *http.Request, total int, item func(int) string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	start, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	end := min(start+limit, total)
	items := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		items = append(items, item(i))
	}
	next := "null"
	if end < total {
		next = fmt.Sprintf(`{"offset":"%d"}`, end)
	}
	fmt.Fprintf(w, `{"data":[%s],"next_page":%s}`, strings.Join(items, ","), next)
}

// testConfig parses real flags, so the tests also cover parseFlags + validate.
func testConfig(t *testing.T, baseURL string, extra ...string) config {
	t.Helper()
	args := append([]string{
		"-base-url", baseURL,
		"-data-dir", t.TempDir(),
		"-env-file", filepath.Join(t.TempDir(), "no.env"),
		"-rpm", "60000", // keep the limiter out of the way
		"-page-size", "2", // force several pages per resource
		"-max-retries", "0",
	}, extra...)
	cfg, err := parseFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func countJSON(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), "_") {
			n++
		}
	}
	return n
}

func TestRunOnceExtractsEverything(t *testing.T) {
	t.Setenv(tokenEnv, testToken)
	api := &fakeAsana{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	cfg := testConfig(t, srv.URL, "-once")
	m := obs.NewMetrics()

	if err := run(t.Context(), cfg, slog.New(slog.DiscardHandler), m); err != nil {
		t.Fatal(err)
	}

	if n := countJSON(t, filepath.Join(cfg.dataDir, "100", "users")); n != 5 {
		t.Errorf("users files = %d, want 5 (all 3 pages)", n)
	}
	if n := countJSON(t, filepath.Join(cfg.dataDir, "100", "projects")); n != 3 {
		t.Errorf("projects files = %d, want 3 (both pages)", n)
	}
	// /workspaces x2 (one per job) + users 3 pages + projects 2 pages = 7.
	var got struct {
		ByStatus map[string]int `json:"http_requests_by_status"`
	}
	if err := json.Unmarshal([]byte(m.String()), &got); err != nil {
		t.Fatal(err)
	}
	if got.ByStatus["200"] != 7 || api.requests.Load() != 7 {
		t.Errorf("requests: metrics %v, server saw %d; want 7", got.ByStatus, api.requests.Load())
	}
	if s := m.Syncs()["100/users"]; s.Items != 5 || s.Written != 5 {
		t.Errorf("users sync stats = %+v", s)
	}
}

func TestRunOncePartialFailure(t *testing.T) {
	t.Setenv(tokenEnv, testToken)
	api := &fakeAsana{}
	api.failProjects.Store(true)
	srv := httptest.NewServer(api)
	defer srv.Close()
	cfg := testConfig(t, srv.URL, "-once")

	err := run(t.Context(), cfg, slog.New(slog.DiscardHandler), obs.NewMetrics())
	if err == nil || !strings.Contains(err.Error(), "job projects") {
		t.Fatalf("err = %v, want the projects job failure", err)
	}
	// Users are independent of projects: they must still be stored.
	if n := countJSON(t, filepath.Join(cfg.dataDir, "100", "users")); n != 5 {
		t.Errorf("users files = %d, want 5 despite the projects failure", n)
	}
}

func TestRunRequiresToken(t *testing.T) {
	t.Setenv(tokenEnv, "")
	cfg := testConfig(t, "http://127.0.0.1:1", "-once")
	err := run(t.Context(), cfg, slog.New(slog.DiscardHandler), obs.NewMetrics())
	if err == nil || !strings.Contains(err.Error(), tokenEnv) {
		t.Errorf("err = %v, want a missing-token error", err)
	}
}

func TestRunReadsTokenFromEnvFile(t *testing.T) {
	t.Setenv(tokenEnv, "")
	os.Unsetenv(tokenEnv) // t.Setenv restores it after the test
	api := &fakeAsana{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte(tokenEnv+"="+testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, srv.URL, "-once", "-env-file", envFile)

	if err := run(t.Context(), cfg, slog.New(slog.DiscardHandler), obs.NewMetrics()); err != nil {
		t.Errorf("run with the token in .env: %v", err)
	}
}

func TestRunPeriodicUntilCancelled(t *testing.T) {
	t.Setenv(tokenEnv, testToken)
	srv := httptest.NewServer(&fakeAsana{})
	defer srv.Close()
	cfg := testConfig(t, srv.URL, "-interval", "1h", "-metrics-addr", "127.0.0.1:0")
	m := obs.NewMetrics()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, slog.New(slog.DiscardHandler), m) }()

	// Both jobs run immediately (not after 1h); wait for their first success.
	deadline := time.Now().Add(5 * time.Second)
	for {
		jobs := m.Jobs()
		if jobs["users"].Runs == 1 && jobs["projects"].Runs == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("jobs did not run: %+v", jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st, healthy := m.Health(); !healthy {
		t.Errorf("health = %v, want healthy", st)
	}

	cancel() // what Ctrl+C / SIGTERM does
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v after cancel, want nil (clean shutdown)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

func TestServeObservability(t *testing.T) {
	m := obs.NewMetrics()
	m.RegisterJob("users", time.Minute, 0)
	addr, stop, err := serveObservability("127.0.0.1:0", m, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/healthz", "/metrics", "/debug/vars"} {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}

	// A second server on the same port must fail at startup, not silently.
	if _, _, err := serveObservability(addr, m, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("expected an error for a port already in use")
	}

	stop()
	if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
		resp.Body.Close()
		t.Error("server still answering after stop")
	}
}
