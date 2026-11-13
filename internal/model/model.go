// Package model holds the Asana resource types we extract. Only the fields we
// need are decoded; unknown fields are ignored. Each type has a matching
// opt_fields constant, because Asana returns compact records (gid + name)
// unless you ask for more.
package model

import (
	"errors"
	"time"
)

var errMissingGID = errors.New("missing gid")

// Ref is Asana's compact reference to another object (owner, team, workspace).
type Ref struct {
	GID  string `json:"gid"`
	Name string `json:"name,omitempty"`
}

// WorkspaceOptFields lists the fields requested for GET /workspaces.
const WorkspaceOptFields = "name,is_organization"

// Workspace is an Asana workspace or organization; users and projects live in one.
type Workspace struct {
	GID            string `json:"gid"`
	Name           string `json:"name"`
	IsOrganization bool   `json:"is_organization"`
}

// Validate reports whether the record has the fields we rely on.
func (w Workspace) Validate() error {
	if w.GID == "" {
		return errMissingGID
	}
	return nil
}

// UserOptFields lists the fields requested for GET /users.
const UserOptFields = "name,email"

// User is an Asana user.
type User struct {
	GID   string `json:"gid"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"` // may be hidden, depending on the token's permissions
}

// Validate reports whether the record has the fields we rely on.
func (u User) Validate() error {
	if u.GID == "" {
		return errMissingGID
	}
	return nil
}

// ProjectOptFields lists the fields requested for GET /projects.
const ProjectOptFields = "name,archived,created_at,modified_at,owner.name,team.name,workspace.name,permalink_url"

// Project is an Asana project.
type Project struct {
	GID          string    `json:"gid"`
	Name         string    `json:"name"`
	Archived     bool      `json:"archived"`
	CreatedAt    time.Time `json:"created_at"`
	ModifiedAt   time.Time `json:"modified_at"`
	Owner        *Ref      `json:"owner"` // null when the project has no owner
	Team         *Ref      `json:"team"`  // null in personal (non-organization) workspaces
	Workspace    Ref       `json:"workspace"`
	PermalinkURL string    `json:"permalink_url"`
}

// Validate reports whether the record has the fields we rely on.
func (p Project) Validate() error {
	if p.GID == "" {
		return errMissingGID
	}
	return nil
}
