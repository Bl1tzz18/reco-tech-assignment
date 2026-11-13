package extractor

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"reco-tech-assignment/internal/client"
	"reco-tech-assignment/internal/model"
	"reco-tech-assignment/internal/storage"
)

// fakeAsana serves one workspace with the given users and projects.
// When failProjects is set, /projects answers 500.
type fakeAsana struct {
	users, projects string
	failProjects    atomic.Bool
}

func (f *fakeAsana) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/workspaces":
		fmt.Fprint(w, `{"data":[{"gid":"100","name":"Acme","is_organization":true}],"next_page":null}`)
	case "/users":
		fmt.Fprintf(w, `{"data":[%s],"next_page":null}`, f.users)
	case "/projects":
		if f.failProjects.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"data":[%s],"next_page":null}`, f.projects)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newTestExtractor(t *testing.T, api http.Handler) (*Extractor, string) {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	log := slog.New(slog.DiscardHandler)
	c, err := client.New(srv.URL, "token", client.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return New(c, store, "", log), dir
}

func readJSON[T any](t *testing.T, path string) T {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSyncWritesOneFilePerRecord(t *testing.T) {
	api := &fakeAsana{
		users: `{"gid":"1","name":"Ann","email":"ann@example.com"},{"gid":"2","name":"Bob"}`,
		projects: `{"gid":"10","name":"Launch","archived":false,"created_at":"2026-01-02T03:04:05.000Z",
			"modified_at":"2026-01-03T03:04:05.000Z","owner":{"gid":"1","name":"Ann"},"team":null,
			"workspace":{"gid":"100","name":"Acme"},"permalink_url":"https://app.asana.com/0/10"}`,
	}
	ex, dir := newTestExtractor(t, api)

	if err := ex.SyncUsers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := ex.SyncProjects(t.Context()); err != nil {
		t.Fatal(err)
	}

	ann := readJSON[model.User](t, filepath.Join(dir, "100", "users", "1.json"))
	if ann != (model.User{GID: "1", Name: "Ann", Email: "ann@example.com"}) {
		t.Errorf("user 1 = %+v", ann)
	}
	p := readJSON[model.Project](t, filepath.Join(dir, "100", "projects", "10.json"))
	if p.Name != "Launch" || p.Owner == nil || p.Owner.Name != "Ann" || p.Team != nil || p.CreatedAt.Year() != 2026 {
		t.Errorf("project 10 = %+v", p)
	}
	meta := readJSON[Meta](t, filepath.Join(dir, "100", "users", storage.MetaName+".json"))
	if meta.Count != 2 || meta.Workspace.Name != "Acme" || meta.FetchedAt.IsZero() || meta.FetchedAt.Location().String() != "UTC" {
		t.Errorf("users meta = %+v", meta)
	}
}

func TestSyncPrunesDeletedAndKeepsDataOnFailure(t *testing.T) {
	api := &fakeAsana{users: `{"gid":"1","name":"Ann"}`, projects: `{"gid":"10","name":"A"},{"gid":"11","name":"B"}`}
	ex, dir := newTestExtractor(t, api)
	projectsDir := filepath.Join(dir, "100", "projects")
	if err := ex.SyncProjects(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Project 11 is deleted in Asana: its file must go.
	api.projects = `{"gid":"10","name":"A"}`
	if err := ex.SyncProjects(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := files(t, projectsDir); !slices.Equal(got, []string{"10.json", "_meta.json"}) {
		t.Errorf("after delete: files = %v", got)
	}

	// The API fails: the sync errors and the previous files stay intact.
	api.failProjects.Store(true)
	if err := ex.SyncProjects(t.Context()); err == nil {
		t.Fatal("expected an error when /projects fails")
	}
	if got := files(t, projectsDir); !slices.Equal(got, []string{"10.json", "_meta.json"}) {
		t.Errorf("after failure: files = %v, want the previous data untouched", got)
	}
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
