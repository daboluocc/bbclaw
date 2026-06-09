// Package projectstore owns the butler's project allowlist — the set of
// directories the butler may dispatch worker agents into (the "cwd pool").
//
// The persisted file <DataDir>/projects.json is the single source of truth, so
// projects can be added and removed at runtime through the local admin page
// without editing .env or restarting. BBCLAW_CWD_POOL is only a one-time
// bootstrap: SeedIfMissing writes the env-defined projects into a fresh file the
// first time the adapter runs; once the file exists, the environment is ignored
// and every project — including the originally seeded ones — is editable and
// removable. This makes the admin page the web-first home for project config.
//
// Both the main process and each `mcp-server` subprocess Open a Store against the
// same file; List() reloads when the file's mtime changes, so an admin add takes
// effect in the subprocess on its next list_projects/dispatch without a restart
// (single-process-daemon assumption: no cross-process lock, last-writer-wins).
//
// Security note: adding a directory here grants the butler authority to run
// agentic tasks — including command/file execution — in that directory. Add()
// therefore validates the path (absolute, exists, is a directory); the HTTP
// surface that calls it is bound to localhost (see httpapi admin routes).
package projectstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fileFormatVersion is the JSON schema version persisted at the top level. Bump
// alongside any breaking shape change and add a migration path.
const fileFormatVersion = 1

// Source records a project's provenance for display only — both values are fully
// editable and removable through the admin page.
const (
	SourceEnv   = "env"   // seeded once from BBCLAW_CWD_POOL on first run
	SourceAdmin = "admin" // added at runtime through the admin page
)

// Project is one allow-listed project directory.
type Project struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Source  string    `json:"source"`
	AddedAt time.Time `json:"added_at,omitempty"`
}

// persisted is the on-disk shape: the full project list (the source of truth).
// Added is read only for backward compatibility with the pre-release delta
// format ({"added":[...]}); new files always write Projects.
type persisted struct {
	Version  int       `json:"version"`
	Projects []Project `json:"projects"`
	Added    []Project `json:"added,omitempty"` // legacy key, read-only
}

// entries returns the effective project list from a parsed file, preferring the
// current Projects key and falling back to the legacy Added key.
func (p persisted) entries() []Project {
	if len(p.Projects) > 0 {
		return p.Projects
	}
	return p.Added
}

// isLegacy reports whether the file used the old delta format (Added present,
// Projects absent) and therefore still needs the one-time env merge.
func (p persisted) isLegacy() bool {
	return len(p.Projects) == 0 && len(p.Added) > 0
}

// Store is the project allowlist. Safe for concurrent use.
type Store struct {
	path  string
	mu    sync.RWMutex
	items []Project // the full persisted list
	mtime time.Time // last-loaded file mtime, for reload-on-change
	dirty bool      // items not yet loaded from an existing file
}

// Bootstrap status values returned by Bootstrap, for the caller to log.
const (
	BootstrapNoop     = "noop"     // current-format file present → env ignored
	BootstrapSeeded   = "seeded"   // fresh file written from the env seed
	BootstrapMigrated = "migrated" // legacy delta file upgraded + env merged in
	BootstrapEmpty    = "empty"    // no file and nothing to seed
)

// Bootstrap reconciles the env-defined seed with the on-disk store ONCE, so the
// admin page can become the source of truth without losing anything:
//
//   - no file        → write the seed as a fresh current-format file (seeded)
//   - legacy file     → upgrade {"added":...} to {"projects":...} and merge in any
//     seed entries not already present, so projects that were previously env-only
//     survive the switch and become editable (migrated)
//   - current file    → leave untouched; the environment is ignored from now on
//     (noop)
//
// After Bootstrap, every project lives in the file and BBCLAW_CWD_POOL is a spent
// bootstrap that can be removed from .env. Safe and idempotent across processes
// (atomic write; a current-format file is never rewritten).
func Bootstrap(path string, seed []Project) (string, error) {
	seedClean := normalizeSource(seed, SourceEnv)

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if len(seedClean) == 0 {
			return BootstrapEmpty, nil // let the file appear lazily on first Add
		}
		return BootstrapSeeded, writeFile(path, seedClean)
	}
	if err != nil {
		return "", fmt.Errorf("projectstore: read %s: %w", path, err)
	}

	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", fmt.Errorf("projectstore: parse %s: %w", path, err)
	}
	if !p.isLegacy() {
		return BootstrapNoop, nil // current format → env ignored
	}

	// Legacy: keep existing entries, append seed entries not already present.
	merged := sanitize(p.Added)
	haveName := make(map[string]struct{}, len(merged))
	havePath := make(map[string]struct{}, len(merged))
	for _, e := range merged {
		haveName[e.Name] = struct{}{}
		havePath[e.Path] = struct{}{}
	}
	for _, sd := range seedClean {
		if _, dup := haveName[sd.Name]; dup {
			continue
		}
		if _, dup := havePath[sd.Path]; dup {
			continue
		}
		merged = append(merged, sd)
	}
	return BootstrapMigrated, writeFile(path, merged)
}

// normalizeSource trims and stamps source + timestamp onto seed entries,
// dropping blanks.
func normalizeSource(seed []Project, source string) []Project {
	now := time.Now().UTC()
	out := make([]Project, 0, len(seed))
	for _, p := range seed {
		name := strings.TrimSpace(p.Name)
		dir := strings.TrimSpace(p.Path)
		if name == "" || dir == "" {
			continue
		}
		out = append(out, Project{Name: name, Path: dir, Source: source, AddedAt: now})
	}
	return out
}

// Open constructs a Store backed by path. A missing file is not an error (the
// pool is simply empty until SeedIfMissing or Add writes it). A corrupt file is
// surfaced via the returned error but the Store stays usable (empty), so a bad
// file never blocks startup.
func Open(path string) (*Store, error) {
	s := &Store{path: path, dirty: true}
	err := s.reload()
	if errors.Is(err, os.ErrNotExist) {
		s.mu.Lock()
		s.items, s.dirty = nil, false
		s.mu.Unlock()
		return s, nil
	}
	return s, err
}

// reload reads the file into s.items when its mtime differs from the last load.
func (s *Store) reload() error {
	fi, statErr := os.Stat(s.path)
	if statErr != nil {
		return statErr
	}
	s.mu.RLock()
	unchanged := fi.ModTime().Equal(s.mtime) && !s.dirty
	s.mu.RUnlock()
	if unchanged {
		return nil
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("projectstore: read %s: %w", s.path, err)
	}
	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("projectstore: parse %s: %w", s.path, err)
	}
	s.mu.Lock()
	s.items = sanitize(p.entries())
	s.mtime = fi.ModTime()
	s.dirty = false
	s.mu.Unlock()
	return nil
}

// sanitize drops malformed entries and defaults an empty source to admin.
func sanitize(in []Project) []Project {
	out := make([]Project, 0, len(in))
	for _, p := range in {
		name := strings.TrimSpace(p.Name)
		path := strings.TrimSpace(p.Path)
		if name == "" || path == "" {
			continue
		}
		src := p.Source
		if src != SourceEnv && src != SourceAdmin {
			src = SourceAdmin
		}
		out = append(out, Project{Name: name, Path: path, Source: src, AddedAt: p.AddedAt})
	}
	return out
}

// List returns the current pool, deduped by name and path (first occurrence
// wins). It reloads when the backing file changed, so subprocess callers stay
// live without a restart. The returned slice is a copy the caller may mutate.
func (s *Store) List() []Project {
	// Best-effort reload; ignore errors so a transient read failure degrades to
	// the last-known list rather than dropping projects.
	_ = s.reload()

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Project, 0, len(s.items))
	seenName := make(map[string]struct{}, len(s.items))
	seenPath := make(map[string]struct{}, len(s.items))
	for _, p := range s.items {
		if _, dup := seenName[p.Name]; dup {
			continue
		}
		if _, dup := seenPath[p.Path]; dup {
			continue
		}
		seenName[p.Name] = struct{}{}
		seenPath[p.Path] = struct{}{}
		out = append(out, p)
	}
	return out
}

// Resolve maps a (name, path) selection to an allow-listed absolute cwd, mirroring
// the butlermcp security contract: a name must match a pool entry, or a raw path
// must equal an allow-listed path exactly. Returns ("", false) for anything else.
func (s *Store) Resolve(name, path string) (string, bool) {
	name = strings.TrimSpace(name)
	path = strings.TrimSpace(path)
	for _, p := range s.List() {
		if name != "" && p.Name == name {
			return p.Path, true
		}
		if path != "" && p.Path == path {
			return p.Path, true
		}
	}
	return "", false
}

// Add validates and appends a project, then persists. The path must be absolute,
// exist, and be a directory. The name must be non-empty and free of the pool
// delimiters (',' ':') so it stays addressable; both name and path must be unique
// against the current pool. The added Project is returned on success.
func (s *Store) Add(name, path string) (Project, error) {
	name = strings.TrimSpace(name)
	path = strings.TrimSpace(path)
	if name == "" {
		return Project{}, fmt.Errorf("name must not be empty")
	}
	if strings.ContainsAny(name, ",:") {
		return Project{}, fmt.Errorf("name must not contain ',' or ':'")
	}
	if path == "" {
		return Project{}, fmt.Errorf("path must not be empty")
	}
	if !filepath.IsAbs(path) {
		return Project{}, fmt.Errorf("path must be absolute: %q", path)
	}
	path = filepath.Clean(path)
	fi, err := os.Stat(path)
	if err != nil {
		return Project{}, fmt.Errorf("path not accessible: %w", err)
	}
	if !fi.IsDir() {
		return Project{}, fmt.Errorf("path is not a directory: %q", path)
	}

	for _, p := range s.List() {
		if p.Name == name {
			return Project{}, fmt.Errorf("a project named %q already exists", name)
		}
		if p.Path == path {
			return Project{}, fmt.Errorf("path already registered as %q", p.Name)
		}
	}

	proj := Project{Name: name, Path: path, Source: SourceAdmin, AddedAt: time.Now().UTC()}
	s.mu.Lock()
	s.items = append(append([]Project(nil), s.items...), proj)
	err = s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		return Project{}, err
	}
	return proj, nil
}

// AddPath registers a project by directory alone, deriving a unique name from the
// directory's base name (the admin UI no longer asks for a name). The base is
// sanitized of the pool delimiters and de-duplicated with a numeric suffix. All
// of Add's path validation still applies.
func (s *Store) AddPath(path string) (Project, error) {
	base := filepath.Base(filepath.Clean(strings.TrimSpace(path)))
	return s.Add(s.uniqueName(base), path)
}

// uniqueName turns a raw base name into a pool-safe, currently-unused name. It
// strips the ',' / ':' delimiters, falls back to "project" when empty, and
// appends -2, -3, … until the name is free in the current pool.
func (s *Store) uniqueName(base string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == ',' || r == ':' {
			return '-'
		}
		return r
	}, strings.TrimSpace(base))
	cleaned = strings.Trim(cleaned, "-. ")
	if cleaned == "" {
		cleaned = "project"
	}
	taken := make(map[string]struct{})
	for _, p := range s.List() {
		taken[p.Name] = struct{}{}
	}
	if _, dup := taken[cleaned]; !dup {
		return cleaned
	}
	for i := 2; ; i++ {
		candidate := cleaned + "-" + strconv.Itoa(i)
		if _, dup := taken[candidate]; !dup {
			return candidate
		}
	}
}

// Remove deletes a project by name and persists. Any project may be removed —
// including ones originally seeded from the environment, which by now live in the
// file like the rest. Returns (false, nil) when no such project exists.
func (s *Store) Remove(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, fmt.Errorf("name must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]Project, 0, len(s.items))
	found := false
	for _, p := range s.items {
		if p.Name == name {
			found = true
			continue
		}
		next = append(next, p)
	}
	if !found {
		return false, nil
	}
	s.items = next
	if err := s.persistLocked(); err != nil {
		return false, err
	}
	return true, nil
}

// persistLocked writes the full list atomically (tmp + rename) at 0600. The
// caller must hold s.mu. It refreshes the cached mtime so the next List() on this
// Store doesn't needlessly reload its own write.
func (s *Store) persistLocked() error {
	if err := writeFile(s.path, s.items); err != nil {
		return err
	}
	if fi, statErr := os.Stat(s.path); statErr == nil {
		s.mtime = fi.ModTime()
		s.dirty = false
	}
	return nil
}

// writeFile atomically serialises items (sorted by name for a stable file) to
// path at 0600.
func writeFile(path string, items []Project) error {
	sorted := append([]Project(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	data, err := json.MarshalIndent(persisted{Version: fileFormatVersion, Projects: sorted}, "", "  ")
	if err != nil {
		return fmt.Errorf("projectstore: marshal: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("projectstore: mkdir %s: %w", dir, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("projectstore: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("projectstore: rename: %w", err)
	}
	return nil
}
