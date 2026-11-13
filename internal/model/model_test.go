package model

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// testdata/*_page.json are REAL Asana responses (GET /users and GET /projects
// with our opt_fields), anonymised: names, emails and gids replaced, the
// structure and value formats untouched. If Asana changes a field's type or
// format, decoding these fails here first.

func decodePage[T any](t *testing.T, file string) []T {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Data []T `json:"data"`
	}
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	if len(page.Data) == 0 {
		t.Fatalf("%s has no items", file)
	}
	return page.Data
}

func TestDecodeRealUsersResponse(t *testing.T) {
	for _, u := range decodePage[User](t, "testdata/users_page.json") {
		if err := u.Validate(); err != nil {
			t.Errorf("user %+v: %v", u, err)
		}
		if u.Name == "" || u.Email == "" {
			t.Errorf("user %+v: name and email were requested and must be decoded", u)
		}
	}
}

func TestDecodeRealProjectsResponse(t *testing.T) {
	for _, p := range decodePage[Project](t, "testdata/projects_page.json") {
		if err := p.Validate(); err != nil {
			t.Errorf("project %+v: %v", p, err)
		}
		if p.Name == "" || p.PermalinkURL == "" || p.Workspace.GID == "" {
			t.Errorf("project %+v: requested fields missing", p)
		}
		// Asana sends RFC 3339 with milliseconds in UTC; it must parse to a real time.
		if p.CreatedAt.IsZero() || p.ModifiedAt.Before(p.CreatedAt) || p.CreatedAt.Location() != time.UTC {
			t.Errorf("project %s: created_at=%v modified_at=%v", p.GID, p.CreatedAt, p.ModifiedAt)
		}
		if p.Owner == nil || p.Owner.Name == "" {
			t.Errorf("project %s: owner.name was requested and must be decoded", p.GID)
		}
	}
}

// Shapes our account doesn't have but the API documents: no owner, no team
// (personal workspace), and fields we didn't ask for.
func TestDecodeProjectWithNullsAndUnknownFields(t *testing.T) {
	raw := `{"gid":"1","name":"P","archived":true,"owner":null,"team":null,
		"workspace":{"gid":"9","name":"W"},"resource_type":"project","color":"light-green",
		"created_at":"2020-01-02T03:04:05.678Z","modified_at":"2020-01-02T03:04:05.678Z"}`
	var p Project
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if p.Owner != nil || p.Team != nil || !p.Archived || p.Workspace.Name != "W" {
		t.Errorf("project = %+v", p)
	}
}

func TestValidateRequiresGID(t *testing.T) {
	for name, v := range map[string]interface{ Validate() error }{
		"workspace": Workspace{}, "user": User{}, "project": Project{},
	} {
		if v.Validate() == nil {
			t.Errorf("%s without gid passed validation", name)
		}
	}
}
