package butler

import (
	"fmt"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

func makeStarted(taskID, cwd, title string) agent.Event {
	return agent.Event{
		Type: agent.EvDispatchStatus,
		Dispatch: &agent.DispatchStatus{
			Phase:  "started",
			TaskID: taskID,
			Cwd:    cwd,
			Title:  title,
		},
	}
}

func makeDone(taskID string, elapsedMs int64) agent.Event {
	return agent.Event{
		Type: agent.EvDispatchStatus,
		Dispatch: &agent.DispatchStatus{
			Phase:     "done",
			TaskID:    taskID,
			ElapsedMs: elapsedMs,
		},
	}
}

// TestDispatchRecorder_Empty verifies that Recent returns [] when empty.
func TestDispatchRecorder_Empty(t *testing.T) {
	r := NewDispatchRecorder()
	got := r.Recent(20)
	if len(got) != 0 {
		t.Errorf("want empty slice, got %d entries", len(got))
	}
}

// TestDispatchRecorder_FiveEntries_ReverseChrono verifies newest-first ordering.
func TestDispatchRecorder_FiveEntries_ReverseChrono(t *testing.T) {
	r := NewDispatchRecorder()
	for i := 1; i <= 5; i++ {
		r.Record(makeStarted(fmt.Sprintf("task-%d", i), "proj", fmt.Sprintf("task %d", i)))
	}
	got := r.Recent(5)
	if len(got) != 5 {
		t.Fatalf("want 5 entries, got %d", len(got))
	}
	// Newest first: task-5 should be index 0
	if got[0].TaskID != "task-5" {
		t.Errorf("want task-5 first, got %q", got[0].TaskID)
	}
	if got[4].TaskID != "task-1" {
		t.Errorf("want task-1 last, got %q", got[4].TaskID)
	}
}

// TestDispatchRecorder_RingEvictsOldest verifies that entries beyond cap 50
// evict the oldest entry.
func TestDispatchRecorder_RingEvictsOldest(t *testing.T) {
	r := NewDispatchRecorder()
	// Insert 51 entries
	for i := 1; i <= 51; i++ {
		r.Record(makeStarted(fmt.Sprintf("task-%d", i), "proj", "t"))
	}
	// ring should hold exactly 50
	got := r.Recent(0) // 0 = all
	if len(got) != 50 {
		t.Fatalf("want 50 entries after cap eviction, got %d", len(got))
	}
	// oldest evicted should be task-1; task-2 should be the oldest remaining
	// (Recent returns newest-first, so last element is the oldest)
	oldest := got[len(got)-1]
	if oldest.TaskID != "task-2" {
		t.Errorf("want task-2 as oldest remaining, got %q", oldest.TaskID)
	}
	// task-1 should not be in the map
	r.mu.RLock()
	_, exists := r.byID["task-1"]
	r.mu.RUnlock()
	if exists {
		t.Error("task-1 should have been evicted from byID map")
	}
}

// TestDispatchRecorder_UpdateTerminalPhase verifies that done/async/error
// updates the status of an existing started entry.
func TestDispatchRecorder_UpdateTerminalPhase(t *testing.T) {
	r := NewDispatchRecorder()
	r.Record(makeStarted("task-x", "proj", "my task"))
	r.Record(makeDone("task-x", 3200))

	got := r.Recent(1)
	if len(got) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got))
	}
	if got[0].Status != "done" {
		t.Errorf("want status=done, got %q", got[0].Status)
	}
	if got[0].ElapsedMs != 3200 {
		t.Errorf("want elapsedMs=3200, got %d", got[0].ElapsedMs)
	}
}

// TestDispatchRecorder_LimitCap verifies that Recent respects the n limit.
func TestDispatchRecorder_LimitCap(t *testing.T) {
	r := NewDispatchRecorder()
	for i := 1; i <= 30; i++ {
		r.Record(makeStarted(fmt.Sprintf("task-%d", i), "proj", "t"))
	}
	got := r.Recent(20)
	if len(got) != 20 {
		t.Errorf("want 20 with limit=20, got %d", len(got))
	}
}
