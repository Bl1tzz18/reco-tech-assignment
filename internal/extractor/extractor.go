// Package extractor pulls users and projects from Asana, per workspace, and
// stores each record in its own JSON file:
//
//	<data>/<workspace_gid>/users/<user_gid>.json
//	<data>/<workspace_gid>/projects/<project_gid>.json
//	<data>/<workspace_gid>/<resource>/_meta.json   (fetched_at, count)
package extractor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"time"

	"reco-tech-assignment/internal/client"
	"reco-tech-assignment/internal/model"
	"reco-tech-assignment/internal/storage"
)

const (
	usersResource    = "users"
	projectsResource = "projects"
)

// Meta describes the last successful sync of one resource in one workspace.
// It is written LAST, so its fetched_at always matches complete data.
type Meta struct {
	Resource  string    `json:"resource"`
	Workspace model.Ref `json:"workspace"`
	FetchedAt time.Time `json:"fetched_at"` // UTC, start of the fetch
	Count     int       `json:"count"`
}

// Extractor syncs Asana resources into a Store.
type Extractor struct {
	api          *client.Client
	store        *storage.Store
	log          *slog.Logger
	workspaceGID string // optional: limit extraction to one workspace
	now          func() time.Time
}

// New returns an Extractor. An empty workspaceGID means every workspace the token can see.
func New(api *client.Client, store *storage.Store, workspaceGID string, log *slog.Logger) *Extractor {
	return &Extractor{api: api, store: store, log: log, workspaceGID: workspaceGID, now: time.Now}
}

// SyncUsers extracts the users of every workspace.
func (e *Extractor) SyncUsers(ctx context.Context) error {
	return e.forEachWorkspace(ctx, func(ws model.Workspace) error {
		return syncResource(ctx, e, ws, usersResource, "/users", model.UserOptFields,
			func(u model.User) string { return u.GID })
	})
}

// SyncProjects extracts the projects of every workspace.
func (e *Extractor) SyncProjects(ctx context.Context) error {
	return e.forEachWorkspace(ctx, func(ws model.Workspace) error {
		return syncResource(ctx, e, ws, projectsResource, "/projects", model.ProjectOptFields,
			func(p model.Project) string { return p.GID })
	})
}

// forEachWorkspace runs fn for each workspace. One failing workspace doesn't
// stop the others; all errors are returned together.
func (e *Extractor) forEachWorkspace(ctx context.Context, fn func(model.Workspace) error) error {
	workspaces, err := e.workspaces(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, ws := range workspaces {
		if err := fn(ws); err != nil {
			if ctx.Err() != nil {
				return ctx.Err() // shutting down: not a per-workspace failure
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Extractor) workspaces(ctx context.Context) ([]model.Workspace, error) {
	if e.workspaceGID != "" {
		return []model.Workspace{{GID: e.workspaceGID}}, nil
	}
	params := url.Values{"opt_fields": {model.WorkspaceOptFields}}
	ws, err := client.FetchAllAs[model.Workspace](ctx, e.api, "/workspaces", params)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	return ws, nil
}

// syncResource fetches ALL pages of one resource, then writes one file per
// item, prunes deleted items and writes _meta.json. If any page fails nothing
// is written, so good data is never replaced by a partial fetch.
func syncResource[T any](ctx context.Context, e *Extractor, ws model.Workspace,
	resource, path, optFields string, gid func(T) string) error {
	start := e.now().UTC()
	params := url.Values{"workspace": {ws.GID}, "opt_fields": {optFields}}
	items, err := client.FetchAllAs[T](ctx, e.api, path, params)
	if err != nil {
		return fmt.Errorf("sync %s of workspace %s: %w", resource, ws.GID, err)
	}

	dir := filepath.Join(ws.GID, resource)
	stats, err := storage.ReplaceDir(e.store, dir, items, gid)
	if err != nil {
		return fmt.Errorf("store %s of workspace %s: %w", resource, ws.GID, err)
	}
	meta := Meta{
		Resource:  resource,
		Workspace: model.Ref{GID: ws.GID, Name: ws.Name},
		FetchedAt: start,
		Count:     len(items),
	}
	if _, err := e.store.WriteJSON(filepath.Join(dir, storage.MetaName), meta); err != nil {
		return fmt.Errorf("store %s metadata of workspace %s: %w", resource, ws.GID, err)
	}

	e.log.Info("sync ok", "resource", resource, "workspace", ws.GID, "items", len(items),
		"written", stats.Written, "unchanged", stats.Unchanged, "removed", stats.Removed,
		"took", e.now().Sub(start).Round(time.Millisecond))
	return nil
}
