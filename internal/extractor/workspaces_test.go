package extractor

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"reco-tech-assignment/internal/client"
	"reco-tech-assignment/internal/obs"
	"reco-tech-assignment/internal/storage"
)

// multiAsana serves two workspaces with paginated users. Users of
// failWorkspace answer 500. It records every path it was asked for.
type multiAsana struct {
	usersPerWorkspace map[string]int
	failWorkspace     string

	mu    sync.Mutex
	paths []string
}

func (f *multiAsana) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path+"?workspace="+r.URL.Query().Get("workspace"))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	switch r.URL.Path {
	case "/workspaces":
		fmt.Fprint(w, `{"data":[{"gid":"100","name":"Acme"},{"gid":"200","name":"Beta"}],"next_page":null}`)
	case "/users":
		ws := r.URL.Query().Get("workspace")
		if ws == f.failWorkspace {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		total := f.usersPerWorkspace[ws]
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		start, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		end := min(start+limit, total)
		var items []string
		for i := start; i < end; i++ {
			items = append(items, fmt.Sprintf(`{"gid":"%s%d","name":"u%d"}`, ws, i, i))
		}
		next := "null"
		if end < total {
			next = fmt.Sprintf(`{"offset":"%d"}`, end)
		}
		fmt.Fprintf(w, `{"data":[%s],"next_page":%s}`, strings.Join(items, ","), next)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *multiAsana) requested(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.paths {
		if strings.HasPrefix(p, prefix) {
			n++
		}
	}
	return n
}

func newPagedExtractor(t *testing.T, api http.Handler, workspaceGID string) (*Extractor, string, *obs.Metrics) {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	log := slog.New(slog.DiscardHandler)
	c, err := client.New(srv.URL, "token", client.Options{Logger: log, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := obs.NewMetrics()
	return New(c, store, workspaceGID, log, m), dir, m
}

func TestSyncContinuesWhenOneWorkspaceFails(t *testing.T) {
	api := &multiAsana{usersPerWorkspace: map[string]int{"100": 5}, failWorkspace: "200"}
	ex, dir, m := newPagedExtractor(t, api, "")

	res, err := ex.SyncUsers(t.Context())

	if err == nil || !strings.Contains(err.Error(), "workspace 200") {
		t.Fatalf("err = %v, want the failure of workspace 200", err)
	}
	// Workspace 100 is complete (all 3 pages) despite 200 failing.
	if got := files(t, filepath.Join(dir, "100", "users")); len(got) != 6 { // 5 users + _meta.json
		t.Errorf("workspace 100 files = %v, want 5 users + _meta.json", got)
	}
	if api.requested("/users?workspace=100") != 3 {
		t.Errorf("workspace 100 pages = %d, want 3 (5 users, 2 per page)", api.requested("/users?workspace=100"))
	}
	// Nothing written for the failed workspace.
	if got := listIfExists(filepath.Join(dir, "200", "users")); len(got) != 0 {
		t.Errorf("workspace 200 files = %v, want none", got)
	}
	if res.Items != 5 {
		t.Errorf("result = %+v, want the 5 items of workspace 100", res)
	}
	if _, ok := m.Syncs()["200/users"]; ok {
		t.Error("a failed sync must not be recorded as a successful one")
	}
}

func TestSyncAllWorkspaces(t *testing.T) {
	api := &multiAsana{usersPerWorkspace: map[string]int{"100": 3, "200": 1}}
	ex, dir, _ := newPagedExtractor(t, api, "")

	res, err := ex.SyncUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(files(t, filepath.Join(dir, "100", "users"))); n != 4 {
		t.Errorf("workspace 100 files = %d, want 3 + _meta.json", n)
	}
	if n := len(files(t, filepath.Join(dir, "200", "users"))); n != 2 {
		t.Errorf("workspace 200 files = %d, want 1 + _meta.json", n)
	}
	// 1 /workspaces + 2 pages (ws 100) + 1 page (ws 200).
	if res != (Result{Items: 4, Requests: 4}) {
		t.Errorf("result = %+v, want 4 items in 4 requests", res)
	}
}

func TestSyncSingleConfiguredWorkspace(t *testing.T) {
	api := &multiAsana{usersPerWorkspace: map[string]int{"100": 1, "200": 1}}
	ex, dir, _ := newPagedExtractor(t, api, "200")

	if _, err := ex.SyncUsers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if api.requested("/workspaces") != 0 {
		t.Error("with -workspace set, workspaces must not be discovered")
	}
	if api.requested("/users?workspace=100") != 0 {
		t.Error("workspace 100 must not be fetched")
	}
	if n := len(files(t, filepath.Join(dir, "200", "users"))); n != 2 {
		t.Errorf("workspace 200 files = %d, want 1 + _meta.json", n)
	}
}

func listIfExists(dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, "*"))
	return matches
}
