package butler

import (
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

const dispatchRingCap = 50

// DispatchEntry is one recorded dispatch task, surfaced by
// GET /v1/butler/dispatch/recent (ADR-021-firmware-ui §1.4).
type DispatchEntry struct {
	TaskID    string    `json:"taskId"`
	Cwd       string    `json:"cwd"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`    // "started" | "done" | "async" | "error"
	StartedAt time.Time `json:"startedAt"`
	ElapsedMs int64     `json:"elapsedMs,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// DispatchRecorder is a process-level in-memory ring buffer that tracks
// mcp__bbclaw__ dispatch events consumed from EvDispatchStatus events.
// It is safe for concurrent use (HTTP handler reads, driver goroutine writes).
//
// The recorder is intentionally a singleton per adapter process; it is reset
// on restart (v1 limitation — see ADR-021 §1.4).
type DispatchRecorder struct {
	mu      sync.RWMutex
	entries []*DispatchEntry        // ring: newest appended to tail
	byID    map[string]*DispatchEntry // taskId → entry for fast update
}

// NewDispatchRecorder creates an empty recorder.
func NewDispatchRecorder() *DispatchRecorder {
	return &DispatchRecorder{
		byID: make(map[string]*DispatchEntry, dispatchRingCap),
	}
}

// Record updates the ring buffer from an EvDispatchStatus event.
// "started" phase creates a new entry keyed by TaskID (the claude tool_use.id).
// Terminal phases (done/async/error) look up by TaskID and update status.
func (r *DispatchRecorder) Record(ev agent.Event) {
	if ev.Type != agent.EvDispatchStatus || ev.Dispatch == nil {
		return
	}
	d := ev.Dispatch
	r.mu.Lock()
	defer r.mu.Unlock()

	switch d.Phase {
	case "started":
		entry := &DispatchEntry{
			TaskID:    d.TaskID,
			Cwd:       d.Cwd,
			Title:     d.Title,
			Status:    "started",
			StartedAt: time.Now(),
		}
		r.entries = append(r.entries, entry)
		r.byID[d.TaskID] = entry
		// evict oldest when over capacity
		if len(r.entries) > dispatchRingCap {
			oldest := r.entries[0]
			r.entries = r.entries[1:]
			delete(r.byID, oldest.TaskID)
		}
	default:
		// terminal phase: update existing entry
		if entry, ok := r.byID[d.TaskID]; ok {
			entry.Status = d.Phase
			entry.ElapsedMs = d.ElapsedMs
			entry.Error = d.Error
		}
		// If no existing entry (e.g. recorder started after started event),
		// create a minimal entry so we don't lose the data entirely.
		if _, ok := r.byID[d.TaskID]; !ok {
			entry := &DispatchEntry{
				TaskID:    d.TaskID,
				Cwd:       d.Cwd,
				Title:     d.Title,
				Status:    d.Phase,
				StartedAt: time.Now(),
				ElapsedMs: d.ElapsedMs,
				Error:     d.Error,
			}
			r.entries = append(r.entries, entry)
			r.byID[d.TaskID] = entry
			if len(r.entries) > dispatchRingCap {
				oldest := r.entries[0]
				r.entries = r.entries[1:]
				delete(r.byID, oldest.TaskID)
			}
		}
	}
}

// Recent returns the most recent n entries in reverse-chronological order
// (newest first). If n <= 0 or n > len(entries), all entries are returned.
func (r *DispatchRecorder) Recent(n int) []DispatchEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := len(r.entries)
	if total == 0 {
		return []DispatchEntry{}
	}
	if n <= 0 || n > total {
		n = total
	}
	// entries is chronological (oldest first); return newest-first slice
	out := make([]DispatchEntry, n)
	for i := 0; i < n; i++ {
		out[i] = *r.entries[total-1-i]
	}
	return out
}
