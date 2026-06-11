package butler

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// dispatchRingCapacity is the maximum number of dispatch entries held in memory.
// Oldest entries are evicted when the buffer is full (ADR-021-firmware-ui §1.4).
const dispatchRingCapacity = 50

// DispatchEntry holds the state of one butler dispatch task.
// It is created on phase="started" and updated on subsequent phases for the
// same TaskID. The ring buffer is cleared on adapter restart (accepted limitation).
type DispatchEntry struct {
	TaskID    string    `json:"taskId"`
	Cwd       string    `json:"cwd"`
	Title     string    `json:"title"`
	Status    string    `json:"status"` // "started"|"done"|"running"|"async"|"error"
	ElapsedMs int64     `json:"elapsedMs,omitempty"`
	ErrorMsg  string    `json:"error,omitempty"`
	StartedAt time.Time `json:"startedAt"`
}

// DispatchRing is a thread-safe ring buffer for dispatch events.
// butler.Engine subscribes EvDispatchStatus events and calls Record to update it;
// GET /v1/butler/dispatch/recent reads from it.
//
// When constructed with NewPersistentDispatchRing the buffer is backed by a JSON
// snapshot file in BBCLAW_DATA_DIR (default ~/.bbclaw-adapter/dispatch_ring.json):
// entries are loaded on startup and the snapshot is rewritten atomically after
// every Record. This is the P0 persistence foundation for issue #138 — the firmware
// Task List and /admin survive an adapter restart instead of resetting to empty.
// NewDispatchRing keeps the legacy in-memory-only behaviour for tests and callers
// without a data dir (path == "" disables all disk I/O).
type DispatchRing struct {
	mu      sync.Mutex
	entries []DispatchEntry // ordered oldest→newest, capped at dispatchRingCapacity

	path string      // "" => in-memory only, no persistence
	log  *obs.Logger // optional
}

// NewDispatchRing returns an empty in-memory-only ring buffer (no persistence).
func NewDispatchRing() *DispatchRing { return &DispatchRing{} }

// NewPersistentDispatchRing returns a ring buffer backed by the JSON snapshot at
// path. Existing entries are loaded on construction; a missing, empty or corrupt
// file degrades to an empty ring rather than failing — the next Record overwrites
// it. Pass an empty path to get an in-memory-only ring (equivalent to
// NewDispatchRing).
func NewPersistentDispatchRing(path string, log *obs.Logger) *DispatchRing {
	r := &DispatchRing{path: path, log: log}
	r.load()
	return r
}

// load reads the snapshot file into r.entries. Called once from the constructor
// before the ring is shared, so it does not take the lock. Missing/corrupt files
// degrade to an empty ring (mirrors driverstate.Store.load).
func (r *DispatchRing) load() {
	if r.path == "" {
		return
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && r.log != nil {
			r.log.Warnf("dispatch-ring: read %s failed (%v), starting empty", r.path, err)
		}
		return
	}
	var entries []DispatchEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		if r.log != nil {
			r.log.Warnf("dispatch-ring: corrupt snapshot at %s (%v), starting empty", r.path, err)
		}
		return
	}
	// Drop malformed entries (no TaskID) and enforce capacity defensively.
	cleaned := entries[:0]
	for _, e := range entries {
		if e.TaskID != "" {
			cleaned = append(cleaned, e)
		}
	}
	if len(cleaned) > dispatchRingCapacity {
		cleaned = cleaned[len(cleaned)-dispatchRingCapacity:]
	}
	r.entries = cleaned
	if r.log != nil {
		r.log.Infof("dispatch-ring: loaded %d entry(ies) from %s", len(r.entries), r.path)
	}
}

// saveLocked writes the current entries to the snapshot file atomically
// (temp + rename). Caller must hold r.mu. No-op when persistence is disabled.
// Best-effort: a write error is logged but never propagated, so dispatch
// recording never fails because of a full/read-only disk.
func (r *DispatchRing) saveLocked() {
	if r.path == "" {
		return
	}
	body, err := json.MarshalIndent(r.entries, "", "  ")
	if err != nil {
		if r.log != nil {
			r.log.Warnf("dispatch-ring: marshal snapshot failed: %v", err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		if r.log != nil {
			r.log.Warnf("dispatch-ring: mkdir for %s failed: %v", r.path, err)
		}
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		if r.log != nil {
			r.log.Warnf("dispatch-ring: write %s failed: %v", tmp, err)
		}
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		if r.log != nil {
			r.log.Warnf("dispatch-ring: rename %s -> %s failed: %v", tmp, r.path, err)
		}
	}
}

// Record upserts a DispatchStatus into the ring.
// On phase="started" a new entry is created (or the existing entry for TaskID
// is updated in-place to avoid duplicates on retry). On any other phase the
// entry for TaskID is updated; if not found a new entry is created.
func (r *DispatchRing) Record(ds *agent.DispatchStatus) {
	if ds == nil || ds.TaskID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Try to update an existing entry (same TaskID).
	for i := range r.entries {
		if r.entries[i].TaskID == ds.TaskID {
			r.entries[i].Status = ds.Phase
			if ds.ElapsedMs > 0 {
				r.entries[i].ElapsedMs = ds.ElapsedMs
			}
			if ds.ErrorMsg != "" {
				r.entries[i].ErrorMsg = ds.ErrorMsg
			}
			if ds.Cwd != "" {
				r.entries[i].Cwd = ds.Cwd
			}
			if ds.Title != "" {
				r.entries[i].Title = ds.Title
			}
			r.saveLocked()
			return
		}
	}

	// New entry.
	entry := DispatchEntry{
		TaskID:    ds.TaskID,
		Cwd:       ds.Cwd,
		Title:     truncateDispatchTitle(ds.Title, 24),
		Status:    ds.Phase,
		ElapsedMs: ds.ElapsedMs,
		ErrorMsg:  ds.ErrorMsg,
		StartedAt: time.Now().UTC(),
	}
	r.entries = append(r.entries, entry)

	// Evict oldest when over capacity.
	if len(r.entries) > dispatchRingCapacity {
		r.entries = r.entries[len(r.entries)-dispatchRingCapacity:]
	}
	r.saveLocked()
}

// Recent returns up to n entries in reverse-chronological order (newest first).
func (r *DispatchRing) Recent(n int) []DispatchEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	src := r.entries
	if n <= 0 || n > len(src) {
		n = len(src)
	}
	// Copy the last n (most recent) and reverse.
	slice := make([]DispatchEntry, n)
	for i := 0; i < n; i++ {
		slice[i] = src[len(src)-1-i]
	}
	return slice
}

// truncateDispatchTitle truncates s to at most maxCJK runes, appending "…".
func truncateDispatchTitle(s string, maxCJK int) string {
	if utf8.RuneCountInString(s) <= maxCJK {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxCJK]) + "…"
}
