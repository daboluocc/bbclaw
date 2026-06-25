// Package projectstore owns adapter_v2's project allowlist — the set of
// directories the butler is told about so it knows what the user is working on
// (ADR-036). It is the v2 counterpart of v1's adapter/internal/projectstore,
// trimmed to what P1 needs: the loopback-only admin page is the source of truth
// from the first run (no BBCLAW_CWD_POOL env seed / legacy-format migration), and
// each entry carries two extra fields over v1 — Summary (one-line purpose) and
// CLIBin (the project's own CLI, e.g. bbclaw-buildhub) — that feed the system
// prompt project list and the cliReady status (ADR-036 §决策一/五).
//
// The persisted file <DataDir>/projects.json is the single source of truth; it is
// rewritten atomically (temp + rename) at 0600 inside a 0700 dir, matching
// settingsstore. List() reloads when the file's mtime changes so a future second
// reader (e.g. a dispatch subprocess) stays live, but P1 has a single reader.
//
// Security: registering a directory grants the butler authority to act in it, and
// CLIBin names a binary the butler may invoke — so Add validates the path
// (absolute, exists, directory) and the HTTP surface that calls it is loopback
// only (adminapi.LocalOnly).
package projectstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// Source records a project's provenance for display only.
const (
	SourceAdmin = "admin" // added at runtime through the admin page (the only P1 source)
	SourceEnv   = "env"   // reserved for a future env seed; sanitised through, never written by P1
)

// Project is one registered project directory the butler is told about.
type Project struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Source  string    `json:"source"`
	Summary string    `json:"summary,omitempty"` // one-line purpose; shown in the sysprompt list (ADR-036)
	CLIBin  string    `json:"cliBin,omitempty"`  // optional per-project CLI (path or name), e.g. bbclaw-buildhub
	AddedAt time.Time `json:"addedAt,omitempty"`
}

// persisted is the on-disk shape.
type persisted struct {
	Version  int       `json:"version"`
	Projects []Project `json:"projects"`
}

// Store is the project allowlist. Safe for concurrent use.
type Store struct {
	path  string
	mu    sync.RWMutex
	items []Project
	mtime time.Time // last-loaded file mtime, for reload-on-change
	dirty bool      // items not yet loaded from an existing file
}

// Open constructs a Store backed by path. A missing file is not an error (the
// pool is empty until the first Add writes it). A corrupt file is surfaced via the
// returned error but the Store stays usable (empty), so a bad file never blocks
// startup — mirroring settingsstore.Open.
func Open(path string) (*Store, error) {
	s := &Store{path: path, dirty: true}
	err := s.reload()
	if errors.Is(err, os.ErrNotExist) {
		s.mu.Lock()
		s.items, s.dirty = nil, false
		s.mu.Unlock()
		return s, nil
	}
	if err != nil {
		// Corrupt/unreadable file: degrade to an empty-but-usable store, surface err.
		s.mu.Lock()
		s.items, s.dirty = nil, false
		s.mu.Unlock()
		return s, err
	}
	return s, nil
}

// Path returns the file this store reads/writes.
func (s *Store) Path() string { return s.path }

// CLIReady reports whether a project's CLIBin currently resolves to an executable
// — an absolute path that exists and is executable, or a bare name found on PATH.
// An empty bin is "not configured" → false. Used by the admin view (so the user
// sees at a glance whether e.g. bbclaw-buildhub is installed) and the system-prompt
// project list (so the butler is told 就绪/未配置), preventing the screenshot
// failure where a missing BBCLAW_BUILDHUB_BIN left the butler guessing (ADR-036).
func CLIReady(bin string) bool {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		return false
	}
	if filepath.IsAbs(bin) {
		fi, err := os.Stat(bin)
		return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
	}
	_, err := exec.LookPath(bin)
	return err == nil
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
	s.items = sanitize(p.Projects)
	s.mtime = fi.ModTime()
	s.dirty = false
	s.mu.Unlock()
	return nil
}

// sanitize drops malformed entries and defaults an unknown source to admin.
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
		out = append(out, Project{
			Name:    name,
			Path:    path,
			Source:  src,
			Summary: strings.TrimSpace(p.Summary),
			CLIBin:  strings.TrimSpace(p.CLIBin),
			AddedAt: p.AddedAt,
		})
	}
	return out
}

// List returns the current pool, deduped by name and path (first occurrence
// wins). It reloads when the backing file changed. The returned slice is a copy.
func (s *Store) List() []Project {
	// Best-effort reload; ignore errors so a transient read failure degrades to the
	// last-known list rather than dropping projects.
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

// Resolve maps a (name, path) selection to a registered absolute path: a name must
// match a pool entry, or a raw path must equal a registered path exactly. Returns
// ("", false) otherwise. Kept for a future dispatch step (ADR-036 后续).
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
// exist, and be a directory — NO git requirement (ADR-036 §决策三: any directory).
// When in.Name is blank it is derived from the directory's base name and
// de-duplicated. The name must be free of the ',' / ':' delimiters (so it stays
// safe in the ASR_HOTWORDS comma list and the sysprompt line). Name and path must
// be unique against the current pool. The stored Project is returned.
func (s *Store) Add(in Project) (Project, error) {
	path := strings.TrimSpace(in.Path)
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

	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = s.uniqueName(filepath.Base(path))
	} else if strings.ContainsAny(name, ",:") {
		return Project{}, fmt.Errorf("name must not contain ',' or ':'")
	}

	for _, p := range s.List() {
		if p.Name == name {
			return Project{}, fmt.Errorf("a project named %q already exists", name)
		}
		if p.Path == path {
			return Project{}, fmt.Errorf("path already registered as %q", p.Name)
		}
	}

	proj := Project{
		Name:    name,
		Path:    path,
		Source:  SourceAdmin,
		Summary: strings.TrimSpace(in.Summary),
		CLIBin:  strings.TrimSpace(in.CLIBin),
		AddedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	s.items = append(append([]Project(nil), s.items...), proj)
	err = s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		return Project{}, err
	}
	return proj, nil
}

// uniqueName turns a raw base name into a pool-safe, currently-unused name: it
// replaces the ',' / ':' delimiters, falls back to "project" when empty, and
// appends -2, -3, … until the name is free.
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

// Remove deletes a project by name and persists. Returns (false, nil) when no such
// project exists.
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

// persistLocked writes the full list atomically at 0600. The caller holds s.mu. It
// refreshes the cached mtime so the next List() doesn't reload its own write.
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

// writeFile atomically serialises items (sorted by name for a stable file) to path
// at 0600, inside a 0700 dir (it may hold project paths the operator treats as
// sensitive) — matching settingsstore.writeAtomic.
func writeFile(path string, items []Project) error {
	sorted := append([]Project(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	data, err := json.MarshalIndent(persisted{Version: fileFormatVersion, Projects: sorted}, "", "  ")
	if err != nil {
		return fmt.Errorf("projectstore: marshal: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("projectstore: mkdir %s: %w", dir, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("projectstore: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("projectstore: rename: %w", err)
	}
	return nil
}
