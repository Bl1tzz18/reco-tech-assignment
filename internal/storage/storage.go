// Package storage writes pretty-printed JSON files atomically
// (temp file in the same directory + rename), one file per record.
package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	dirPerm     = 0o755
	filePerm    = 0o644
	ext         = ".json"
	maxNameLen  = 128
	metaPrefix  = "_" // files starting with "_" hold metadata and are never pruned
	MetaName    = metaPrefix + "meta"
	tempPattern = "-*.tmp"
)

// Store writes JSON files under a root directory.
type Store struct {
	dir string
}

// New creates the root directory if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Stats summarises a ReplaceDir call.
type Stats struct {
	Written   int // new or changed files
	Unchanged int // identical content, not rewritten
	Removed   int // files of items that no longer exist
}

// ReplaceDir makes <root>/<dir> hold exactly one <key>.json per item: it
// writes new or changed items and removes files of items that are gone (e.g. a
// deleted project). Removal runs only after every write succeeded, so a failed
// sync never deletes data. Metadata files (prefix "_") are left alone.
func ReplaceDir[T any](s *Store, dir string, items []T, key func(T) string) (Stats, error) {
	var st Stats
	full, err := s.path(dir)
	if err != nil {
		return st, err
	}
	keep := make(map[string]bool, len(items))
	for _, it := range items {
		k := key(it)
		if !validName(k) || strings.HasPrefix(k, metaPrefix) {
			return st, fmt.Errorf("invalid file name %q in %s", k, dir)
		}
		keep[k] = true
	}
	for _, it := range items {
		wrote, err := s.WriteJSON(filepath.Join(dir, key(it)), it)
		if err != nil {
			return st, err
		}
		if wrote {
			st.Written++
		} else {
			st.Unchanged++
		}
	}
	st.Removed, err = prune(full, keep)
	return st, err
}

// WriteJSON atomically writes v as pretty JSON to <root>/<rel>.json, creating
// parent directories. It skips the write when the file already holds the same
// bytes (cheap at scale: most records don't change between syncs) and reports
// whether it wrote.
func (s *Store) WriteJSON(rel string, v any) (bool, error) {
	final, err := s.path(rel)
	if err != nil {
		return false, err
	}
	final += ext
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode %s: %w", rel, err)
	}
	data = append(data, '\n')

	if old, err := os.ReadFile(final); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return false, fmt.Errorf("create dir %s: %w", dir, err)
	}
	if err := writeAtomic(dir, final, data); err != nil {
		return false, err
	}
	return true, nil
}

// writeAtomic writes to a temp file in the SAME directory, then renames it:
// rename is atomic within a filesystem, so readers never see half a file and
// a crash leaves the previous version intact.
func writeAtomic(dir, final string, data []byte) error {
	tmp, err := os.CreateTemp(dir, filepath.Base(final)+tempPattern)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name()) // cleanup on failure; a no-op after a successful rename

	werr := tmp.Chmod(filePerm) // CreateTemp uses 0600
	if werr == nil {
		_, werr = tmp.Write(data)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("write %s: %w", tmp.Name(), werr)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return fmt.Errorf("rename to %s: %w", final, err)
	}
	return nil
}

// prune removes <dir>/*.json files whose name is not in keep.
func prune(dir string, keep map[string]bool) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil // nothing was ever written here
	}
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", dir, err)
	}
	removed := 0
	for _, e := range entries {
		stem, isJSON := strings.CutSuffix(e.Name(), ext)
		if e.IsDir() || !isJSON || strings.HasPrefix(stem, metaPrefix) || keep[stem] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return removed, fmt.Errorf("remove stale %s: %w", e.Name(), err)
		}
		removed++
	}
	return removed, nil
}

// path validates every segment of rel and joins it to the root. Names come
// from API data (gids), so this blocks path traversal like "../../etc".
func (s *Store) path(rel string) (string, error) {
	for seg := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
		if !validName(seg) {
			return "", fmt.Errorf("invalid path %q", rel)
		}
	}
	return filepath.Join(s.dir, rel), nil
}

// validName allows only [A-Za-z0-9_-], 1..maxNameLen characters.
func validName(s string) bool {
	if s == "" || len(s) > maxNameLen {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}
