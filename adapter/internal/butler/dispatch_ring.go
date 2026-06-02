package butler

import (
	"sync"
	"time"
	"unicode/utf8"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
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

// DispatchRing is a thread-safe in-memory ring buffer for dispatch events.
// butler.Engine subscribes EvDispatchStatus events and calls Record to update it;
// GET /v1/butler/dispatch/recent reads from it.
type DispatchRing struct {
	mu      sync.Mutex
	entries []DispatchEntry // ordered oldest→newest, capped at dispatchRingCapacity
}

// NewDispatchRing returns an empty ring buffer.
func NewDispatchRing() *DispatchRing { return &DispatchRing{} }

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
