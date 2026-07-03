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
//
//	v1 → v2 (ADR-042 §10): Reminder.Target → Origin (json key "target" → "origin",
//	CwdName dropped); added FiredAt / Outcome. Migration is read-side + lenient
//	(see loadReminder) so an old file loads without a flag day.
const fileFormatVersion = 2

type persisted struct {
	Version   int               `json:"version"`
	Reminders []json.RawMessage `json:"reminders"`
}

// loadReminder unmarshals one persisted reminder, migrating the legacy v1 shape:
// the old `target` object (deviceId/sessionId/cwdName) backfills Origin when the
// new `origin` key is absent. cwdName is silently dropped (unused for firing).
func loadReminder(raw json.RawMessage) (Reminder, error) {
	var probe struct {
		Reminder
		LegacyTarget *Origin `json:"target"` // v1 key; nil in v2 files
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Reminder{}, err
	}
	r := probe.Reminder
	if (r.Origin == Origin{}) && probe.LegacyTarget != nil {
		r.Origin = *probe.LegacyTarget
	}
	return r, nil
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
	s.items = make([]Reminder, 0, len(p.Reminders))
	for _, raw := range p.Reminders {
		r, err := loadReminder(raw)
		if err != nil {
			// Skip a single corrupt record rather than blocking startup; surface it.
			return s, fmt.Errorf("reminder: parse record in %s: %w", path, err)
		}
		s.items = append(s.items, r)
	}
	// Seed the id sequence past any persisted id so a restart never re-mints a
	// colliding id within the same wall-clock second.
	s.seq = uint64(len(s.items))
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
	if r.Mode == "" {
		r.Mode = ModeNotify
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

// Retention bounds for fired (non-scheduled) reminders — the history (ADR-042
// §10.3). Scheduled/running are never pruned. Whichever bound trims first wins.
const (
	maxHistory = 200
	maxHistAge = 30 * 24 * time.Hour
)

// isHistory reports whether a reminder has left the scheduled/running lifecycle
// (i.e. it is a fired record: done / failed / canceled).
func isHistory(state string) bool {
	return state == StateDone || state == StateFailed || state == StateCanceled
}

// histTime is the ordering key for history — FiredAt, falling back to CreatedAt
// for records that never fired (canceled before RunAt).
func histTime(r Reminder) time.Time {
	if !r.FiredAt.IsZero() {
		return r.FiredAt
	}
	return r.CreatedAt
}

// List returns a copy of all reminders, soonest RunAt first.
func (s *Store) List() []Reminder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]Reminder(nil), s.items...)
	sort.Slice(out, func(i, j int) bool { return out[i].RunAt.Before(out[j].RunAt) })
	return out
}

// Scheduled returns still-scheduled reminders (upcoming, cancelable), soonest
// RunAt first — the "即将" view (ADR-042 §10.3).
func (s *Store) Scheduled() []Reminder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Reminder
	for _, r := range s.items {
		if r.State == StateScheduled {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunAt.Before(out[j].RunAt) })
	return out
}

// History returns fired reminders (done/failed/canceled), most-recently-fired
// first — the "已提醒" view (ADR-042 §10.3).
func (s *Store) History() []Reminder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Reminder
	for _, r := range s.items {
		if isHistory(r.State) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return histTime(out[i]).After(histTime(out[j])) })
	return out
}

// Complete transitions a reminder to a terminal state (done/failed) with its
// outcome + fire time, and persists (ADR-042 §10.3 history). Used by the
// scheduler after firing. Unknown id is an error.
func (s *Store) Complete(id, state, outcome string, firedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].State = state
			s.items[i].Outcome = outcome
			s.items[i].FiredAt = firedAt
			return s.persistLocked()
		}
	}
	return fmt.Errorf("reminder: no such id %q", id)
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

// pruneHistoryLocked caps the fired-reminder history in place (ADR-042 §10.3):
// scheduled/running are kept as-is; history records are kept only if within the
// most-recent maxHistory AND newer than maxHistAge. Mutates s.items.
func (s *Store) pruneHistoryLocked(now time.Time) {
	var live, hist []Reminder
	for _, r := range s.items {
		if isHistory(r.State) {
			hist = append(hist, r)
		} else {
			live = append(live, r)
		}
	}
	if len(hist) <= maxHistory {
		// Still enforce the age bound even under the count cap.
		kept := hist[:0]
		for _, r := range hist {
			if now.Sub(histTime(r)) <= maxHistAge {
				kept = append(kept, r)
			}
		}
		hist = kept
	} else {
		sort.Slice(hist, func(i, j int) bool { return histTime(hist[i]).After(histTime(hist[j])) })
		kept := hist[:0]
		for i, r := range hist {
			if i >= maxHistory || now.Sub(histTime(r)) > maxHistAge {
				break
			}
			kept = append(kept, r)
		}
		hist = kept
	}
	s.items = append(live, hist...)
}

func (s *Store) persistLocked() error {
	s.pruneHistoryLocked(time.Now())
	sorted := append([]Reminder(nil), s.items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RunAt.Before(sorted[j].RunAt) })
	raws := make([]json.RawMessage, 0, len(sorted))
	for _, r := range sorted {
		b, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("reminder: marshal record %s: %w", r.ID, err)
		}
		raws = append(raws, b)
	}
	data, err := json.MarshalIndent(persisted{Version: fileFormatVersion, Reminders: raws}, "", "  ")
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
