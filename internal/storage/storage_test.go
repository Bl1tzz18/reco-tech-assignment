package storage

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type rec struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func recID(r rec) string { return r.ID }

func listDir(t *testing.T, dir string) []string {
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

func TestReplaceDirWritesOneFilePerItem(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	st, err := ReplaceDir(s, "ws/users", []rec{{"1", "Ann"}, {"2", "Bob"}}, recID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Written != 2 {
		t.Errorf("stats = %+v, want 2 written", st)
	}
	got, err := os.ReadFile(filepath.Join(root, "ws", "users", "1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"id\": \"1\",\n  \"name\": \"Ann\"\n}\n"; string(got) != want {
		t.Errorf("file = %q, want pretty JSON %q", got, want)
	}
	if names := listDir(t, filepath.Join(root, "ws", "users")); !slices.Equal(names, []string{"1.json", "2.json"}) {
		t.Errorf("files = %v, want only the two records (no .tmp left behind)", names)
	}
}

func TestReplaceDirSkipsUnchangedAndPrunesDeleted(t *testing.T) {
	root := t.TempDir()
	s, _ := New(root)
	if _, err := ReplaceDir(s, "users", []rec{{"1", "Ann"}, {"2", "Bob"}}, recID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteJSON("users/"+MetaName, map[string]int{"count": 2}); err != nil {
		t.Fatal(err)
	}

	st, err := ReplaceDir(s, "users", []rec{{"1", "Ann"}, {"3", "Cy"}}, recID)
	if err != nil {
		t.Fatal(err)
	}
	if st != (Stats{Written: 1, Unchanged: 1, Removed: 1}) {
		t.Errorf("stats = %+v, want 1 written, 1 unchanged, 1 removed", st)
	}
	want := []string{"1.json", "3.json", MetaName + ".json"}
	if names := listDir(t, filepath.Join(root, "users")); !slices.Equal(names, want) {
		t.Errorf("files = %v, want %v (metadata must survive pruning)", names, want)
	}
}

func TestReplaceDirRejectsUnsafeNames(t *testing.T) {
	root := t.TempDir()
	s, _ := New(root)
	for _, bad := range []string{"../evil", "a/b", "", "_meta", strings.Repeat("x", maxNameLen+1)} {
		if _, err := ReplaceDir(s, "users", []rec{{bad, "x"}}, recID); err == nil {
			t.Errorf("ReplaceDir accepted unsafe name %q", bad)
		}
	}
	if _, err := ReplaceDir(s, "../outside", []rec{{"1", "x"}}, recID); err == nil {
		t.Error("ReplaceDir accepted an unsafe directory")
	}
}

func TestReplaceDirWithNoItemsAndNoDir(t *testing.T) {
	s, _ := New(t.TempDir())
	st, err := ReplaceDir(s, "projects", []rec{}, recID)
	if err != nil || st != (Stats{}) {
		t.Errorf("got %+v, %v; want empty stats and no error", st, err)
	}
}
