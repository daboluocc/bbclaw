// Package driverstate persists user-mutable driver preferences across
// adapter restarts: the active driver name and, per driver, the active
// model id. The state file lives next to the logical-session table in
// BBCLAW_DATA_DIR (default ~/.bbclaw-adapter/driver_state.json).
//
// The store is concurrency-safe and file-backed. Writes are atomic via
// write-to-temp + rename. Reads are served from an in-memory snapshot so
// the HTTP hot path never touches disk.
//
// Schema (JSON):
//
//	{
//	  "active_driver": "claude-code",
//	  "active_models": {
//	    "claude-code": "claude-sonnet-4-6",
//	    "ollama":      "llama3.1:8b"
//	  }
//	}
//
// Missing file / unparseable file / older-format file all degrade to "empty
// state" rather than failing startup — the caller can repopulate by writing.
package driverstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// Store is the file-backed driver preference cache.
type Store struct {
	path string
	log  *obs.Logger

	mu           sync.RWMutex
	activeDriver string
	activeModels map[string]string // driverName -> modelID
}

// fileShape is the on-disk JSON envelope. Kept separate from Store so we
// can evolve the file format without rewriting every caller.
type fileShape struct {
	ActiveDriver string            `json:"active_driver,omitempty"`
	ActiveModels map[string]string `json:"active_models,omitempty"`
}

// NewStore loads state from path. A missing file is not an error — the
// returned store is simply empty and ready to be written to.
func NewStore(path string, log *obs.Logger) (*Store, error) {
	s := &Store{
		path:         path,
		log:          log,
		activeModels: make(map[string]string),
	}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("driverstate: load %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if s.log != nil {
				s.log.Infof("driverstate: no state file at %s, starting empty", s.path)
			}
			return nil
		}
		return err
	}
	var f fileShape
	if err := json.Unmarshal(data, &f); err != nil {
		// Don't fail startup on a corrupt file — the adapter can still run,
		// and the next successful write will overwrite the corruption.
		if s.log != nil {
			s.log.Warnf("driverstate: corrupt state at %s (%v), starting empty", s.path, err)
		}
		return nil
	}
	s.activeDriver = f.ActiveDriver
	if f.ActiveModels != nil {
		s.activeModels = f.ActiveModels
	}
	if s.log != nil {
		s.log.Infof("driverstate: loaded path=%s active_driver=%q models=%d",
			s.path, s.activeDriver, len(s.activeModels))
	}
	return nil
}

// save writes the current snapshot atomically (tmp + rename).
// Caller must hold s.mu (read lock is enough — we snapshot fields).
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	body, err := json.MarshalIndent(fileShape{
		ActiveDriver: s.activeDriver,
		ActiveModels: s.activeModels,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ActiveDriver returns the persisted active driver name, or "" when unset.
func (s *Store) ActiveDriver() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeDriver
}

// ActiveModel returns the persisted model id for the given driver, or ""
// when unset.
func (s *Store) ActiveModel(driver string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeModels[driver]
}

// SetActiveDriver persists driver as the active one. Empty string clears it.
// The caller is expected to validate that the driver is actually registered
// — Store has no view of the router.
func (s *Store) SetActiveDriver(driver string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeDriver = driver
	return s.saveLocked()
}

// SetActiveModel persists modelID under driver. Empty modelID removes the
// entry so the driver falls back to its own default.
func (s *Store) SetActiveModel(driver, modelID string) error {
	if driver == "" {
		return errors.New("driverstate: empty driver name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if modelID == "" {
		delete(s.activeModels, driver)
	} else {
		s.activeModels[driver] = modelID
	}
	return s.saveLocked()
}

// Snapshot returns the full state in a single struct, useful for callers
// that want a consistent view (e.g. the HTTP handler enumerating drivers).
func (s *Store) Snapshot() (activeDriver string, activeModels map[string]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.activeModels))
	for k, v := range s.activeModels {
		out[k] = v
	}
	return s.activeDriver, out
}
