// Persistence model for the logical session table.
//
// Single-process assumption: the adapter is a single-process daemon, so the
// manager protects state with one in-process sync.RWMutex and does NOT take
// a cross-process file lock. Running two adapter instances against the same
// sessions.json is unsupported.
//
// Atomic writes: every persistence call writes to "<path>.tmp", fsyncs, then
// os.Renames over the destination. This avoids torn files across crashes
// or restarts.

package logicalsession

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// fileFormatVersion is the JSON schema version persisted at the top level.
// Bump alongside any breaking shape change and add a migration path.
const fileFormatVersion = 1

// idPrefix marks BBClaw-minted ids so they can be told apart from raw CLI
// session ids on the wire (ADR-014 §6 phase A).
const idPrefix = "ls-"

// fileMode is intentionally restrictive: sessions.json contains cwds which
// can leak workspace structure on shared hosts.
const fileMode = 0o600

// dirMode for the parent directory created on first write.
const dirMode = 0o700

// fileShape is the on-disk JSON layout. The map key duplicates the inner
// LogicalSession.ID for O(1) lookup on load.
type fileShape struct {
	Version  int                        `json:"version"`
	Sessions map[ID]*LogicalSession     `json:"sessions"`
}

// Manager owns the in-memory logical session table and its persistence.
type Manager struct {
	path       string
	defaultCwd string
	log        *obs.Logger

	mu       sync.RWMutex
	sessions map[ID]*LogicalSession
}

// NewManager loads existing sessions from path (creates empty file if
// missing). defaultCwd is used when callers create sessions without
// specifying a cwd. Returns error only on unrecoverable IO/JSON errors;
// missing file is fine.
func NewManager(path, defaultCwd string, log *obs.Logger) (*Manager, error) {
	if path == "" {
		return nil, errors.New("logicalsession: path must not be empty")
	}
	m := &Manager{
		path:       path,
		defaultCwd: defaultCwd,
		log:        log,
		sessions:   make(map[ID]*LogicalSession),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Infof("logicalsession: no existing file at %s, starting empty", path)
			return m, nil
		}
		return nil, fmt.Errorf("logicalsession: read %s: %w", path, err)
	}

	if len(data) == 0 {
		log.Infof("logicalsession: empty file at %s, starting empty", path)
		return m, nil
	}

	var shape fileShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return nil, fmt.Errorf("logicalsession: parse %s: %w", path, err)
	}
	if shape.Sessions != nil {
		m.sessions = shape.Sessions
	}
	log.Infof("logicalsession: loaded %d sessions from %s (version=%d)",
		len(m.sessions), path, shape.Version)
	return m, nil
}

// Create mints a new logical session with a fresh "ls-<short-uuid>" id.
// cwd may be "" → fall back to manager's defaultCwd. title may be "".
// Returns the persisted session.
func (m *Manager) Create(deviceID, driver, cwd, title string) (*LogicalSession, error) {
	if driver == "" {
		return nil, errors.New("logicalsession: driver must not be empty")
	}
	if cwd == "" {
		cwd = m.defaultCwd
	}

	now := time.Now().UTC()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Loop in the (astronomically unlikely) event of a collision.
	var id ID
	for {
		next, err := newID()
		if err != nil {
			return nil, fmt.Errorf("logicalsession: mint id: %w", err)
		}
		if _, exists := m.sessions[next]; !exists {
			id = next
			break
		}
	}

	s := &LogicalSession{
		ID:         id,
		DeviceID:   deviceID,
		Driver:     driver,
		Cwd:        cwd,
		Title:      title,
		CreatedAt:  now,
		LastUsedAt: now,
	}
	m.sessions[id] = s

	if err := m.persistLocked(); err != nil {
		// Roll back the in-memory addition so the caller can retry.
		delete(m.sessions, id)
		return nil, err
	}

	m.log.Infof("logicalsession: created id=%s device=%s driver=%s cwd=%s",
		id, deviceID, driver, cwd)
	// Return a copy so callers can't mutate the stored session by accident.
	out := *s
	return &out, nil
}

// Get returns the session by id, or (nil, false) if not found.
func (m *Manager) Get(id ID) (*LogicalSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, false
	}
	out := *s
	return &out, true
}

// List returns sessions for a device (any driver if driver==""), sorted
// by LastUsedAt desc. Cap at limit (0 = no cap).
func (m *Manager) List(deviceID, driver string, limit int) []*LogicalSession {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*LogicalSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		if deviceID != "" && s.DeviceID != deviceID {
			continue
		}
		if driver != "" && s.Driver != driver {
			continue
		}
		cp := *s
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastUsedAt.After(out[j].LastUsedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// UpdateCLISessionID writes back the latest CLI session id (called when
// adapter spawns a new CLI conversation, e.g., after SESSION_NOT_FOUND
// retry). Also bumps LastUsedAt. Persists synchronously.
func (m *Manager) UpdateCLISessionID(id ID, cliSessionID string) error {
	return m.mutate(id, func(s *LogicalSession) {
		s.CLISessionID = cliSessionID
		s.LastUsedAt = time.Now().UTC()
	})
}

// Touch updates LastUsedAt without changing other fields. Persists.
func (m *Manager) Touch(id ID) error {
	return m.mutate(id, func(s *LogicalSession) {
		s.LastUsedAt = time.Now().UTC()
	})
}

// SetTitle updates the title. Persists.
func (m *Manager) SetTitle(id ID, title string) error {
	return m.mutate(id, func(s *LogicalSession) {
		s.Title = title
	})
}

// Delete removes a session. Persists.
func (m *Manager) Delete(id ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	prev, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("logicalsession: id %s not found", id)
	}
	delete(m.sessions, id)
	if err := m.persistLocked(); err != nil {
		// Restore on failure so caller's view stays consistent.
		m.sessions[id] = prev
		return err
	}
	m.log.Infof("logicalsession: deleted id=%s", id)
	return nil
}

// mutate applies fn to the session under the write lock and persists. If
// persistence fails, the prior session state is restored.
func (m *Manager) mutate(id ID, fn func(*LogicalSession)) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("logicalsession: id %s not found", id)
	}
	prev := *s
	fn(s)
	if err := m.persistLocked(); err != nil {
		*s = prev
		return err
	}
	return nil
}

// persistLocked writes the current map to disk atomically. Caller must
// hold m.mu (write).
func (m *Manager) persistLocked() error {
	if dir := filepath.Dir(m.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return fmt.Errorf("logicalsession: mkdir %s: %w", dir, err)
		}
	}

	shape := fileShape{
		Version:  fileFormatVersion,
		Sessions: m.sessions,
	}
	data, err := json.MarshalIndent(shape, "", "  ")
	if err != nil {
		return fmt.Errorf("logicalsession: marshal: %w", err)
	}

	tmp := m.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return fmt.Errorf("logicalsession: open tmp %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("logicalsession: write tmp %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("logicalsession: fsync tmp %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("logicalsession: close tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("logicalsession: rename %s -> %s: %w", tmp, m.path, err)
	}
	return nil
}

// newID mints a fresh logical session id of the form "ls-<16 hex chars>".
// 8 random bytes give 64 bits of entropy, ample for the lifetime device
// fleet.
func newID() (ID, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return ID(idPrefix + hex.EncodeToString(buf[:])), nil
}
