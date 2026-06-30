package reminder

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// fileFormatVersion is the on-disk schema version. Bump with any breaking shape
// change and add a migration.
const fileFormatVersion = 1

type persisted struct {
	Version   int        `json:"version"`
	Reminders []Reminder `json:"reminders"`
}

// Store persists reminders at <DataDir>/reminders.json. Safe for concurrent use.
// Mirrors projectstore: atomic temp+rename at 0600 inside a 0700 dir, a missing
// file is an empty store, a corrupt file degrades to empty-but-usable + surfaced
// error (never blocks startup).
type Store struct {
	path  string
	mu    sync.RWMutex
	items []Reminder
	seq   uint64 // monotonic id source within a process run
}

// Open loads the store at path. A missing file yields an empty store.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("reminder: read %s: %w", path, err)
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return s, fmt.Errorf("reminder: parse %s: %w", path, err)
	}
	s.items = p.Reminders
	// Seed the id sequence past any persisted id so a restart never re-mints a
	// colliding id within the same wall-clock second.
	s.seq = uint64(len(p.Reminders))
	return s, nil
}

// Path returns the backing file.
func (s *Store) Path() string { return s.path }

// Add validates and persists a new reminder, returning the stored copy (with id,
// state, createdAt filled). RunAt must be set and in the future relative to now.
func (s *Store) Add(r Reminder, now time.Time) (Reminder, error) {
	if r.Prompt == "" {
		return Reminder{}, errors.New("reminder: empty prompt")
	}
	if !r.RunAt.After(now) {
		return Reminder{}, errors.New("reminder: runAt must be in the future")
	}
	if r.Kind == "" {
		r.Kind = KindOnce
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	r.ID = fmt.Sprintf("rem_%d_%d", now.Unix(), s.seq)
	r.State = StateScheduled
	r.CreatedAt = now
	s.items = append(s.items, r)
	if err := s.persistLocked(); err != nil {
		// Roll back the in-memory append so a failed write doesn't leave a ghost.
		s.items = s.items[:len(s.items)-1]
		return Reminder{}, err
	}
	return r, nil
}

// List returns a copy of all reminders, soonest RunAt first.
func (s *Store) List() []Reminder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]Reminder(nil), s.items...)
	sort.Slice(out, func(i, j int) bool { return out[i].RunAt.Before(out[j].RunAt) })
	return out
}

// Pending returns scheduled reminders due at or before now, soonest first. Used
// by the scheduler tick.
func (s *Store) Pending(now time.Time) []Reminder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var due []Reminder
	for _, r := range s.items {
		if r.State == StateScheduled && !r.RunAt.After(now) {
			due = append(due, r)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].RunAt.Before(due[j].RunAt) })
	return due
}

// SetState transitions a reminder by id and persists. Unknown id is an error.
func (s *Store) SetState(id, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].State = state
			return s.persistLocked()
		}
	}
	return fmt.Errorf("reminder: no such id %q", id)
}

// Cancel marks a scheduled reminder canceled. Already-fired reminders are left
// as-is (idempotent no-op error for clarity).
func (s *Store) Cancel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			if s.items[i].State != StateScheduled {
				return fmt.Errorf("reminder: %q is not scheduled (%s)", id, s.items[i].State)
			}
			s.items[i].State = StateCanceled
			return s.persistLocked()
		}
	}
	return fmt.Errorf("reminder: no such id %q", id)
}

func (s *Store) persistLocked() error {
	sorted := append([]Reminder(nil), s.items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RunAt.Before(sorted[j].RunAt) })
	data, err := json.MarshalIndent(persisted{Version: fileFormatVersion, Reminders: sorted}, "", "  ")
	if err != nil {
		return fmt.Errorf("reminder: marshal: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("reminder: mkdir %s: %w", dir, err)
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("reminder: write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("reminder: rename: %w", err)
	}
	return nil
}
