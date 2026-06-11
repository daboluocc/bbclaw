package butler

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// TestPersistentDispatchRingSurvivesRestart is the core issue #138 P0 acceptance:
// dispatches recorded before an adapter restart are still readable afterwards.
func TestPersistentDispatchRingSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch_ring.json")

	// First "process": record a few dispatches into a persistent ring.
	r1 := NewPersistentDispatchRing(path, nil)
	r1.Record(&agent.DispatchStatus{Phase: "started", TaskID: "t1", Cwd: "proj", Title: "task one"})
	r1.Record(&agent.DispatchStatus{Phase: "done", TaskID: "t1", ElapsedMs: 1200})
	r1.Record(&agent.DispatchStatus{Phase: "error", TaskID: "t2", ErrorMsg: "boom"})

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file not written: %v", err)
	}

	// Second "process": a fresh ring loading the same snapshot.
	r2 := NewPersistentDispatchRing(path, nil)
	got := r2.Recent(20)
	if len(got) != 2 {
		t.Fatalf("after reload: want 2 entries, got %d", len(got))
	}
	// Recent is newest-first; t2 was recorded last.
	if got[0].TaskID != "t2" || got[0].Status != "error" || got[0].ErrorMsg != "boom" {
		t.Errorf("after reload: unexpected newest entry %+v", got[0])
	}
	if got[1].TaskID != "t1" || got[1].Status != "done" || got[1].ElapsedMs != 1200 {
		t.Errorf("after reload: upsert state not persisted, got %+v", got[1])
	}
}

func TestPersistentDispatchRingEmptyPathNoFile(t *testing.T) {
	// Empty path => in-memory only, no disk I/O, no panic.
	r := NewPersistentDispatchRing("", nil)
	r.Record(&agent.DispatchStatus{Phase: "started", TaskID: "t1"})
	if got := r.Recent(20); len(got) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got))
	}
}

func TestPersistentDispatchRingMissingFile(t *testing.T) {
	// A path that does not exist yet must degrade to an empty ring, not error.
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	r := NewPersistentDispatchRing(path, nil)
	if got := r.Recent(20); len(got) != 0 {
		t.Fatalf("missing file: want empty ring, got %d entries", len(got))
	}
	// And it becomes writable on first Record.
	r.Record(&agent.DispatchStatus{Phase: "started", TaskID: "t1"})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot not created on first record: %v", err)
	}
}

func TestPersistentDispatchRingCorruptFileDegrades(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch_ring.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Corrupt file must not fail construction — degrade to empty.
	r := NewPersistentDispatchRing(path, nil)
	if got := r.Recent(20); len(got) != 0 {
		t.Fatalf("corrupt file: want empty ring, got %d entries", len(got))
	}
	// Next record overwrites the corruption with a valid snapshot.
	r.Record(&agent.DispatchStatus{Phase: "started", TaskID: "t1"})
	r2 := NewPersistentDispatchRing(path, nil)
	if got := r2.Recent(20); len(got) != 1 {
		t.Fatalf("after overwrite: want 1 entry, got %d", len(got))
	}
}

func TestPersistentDispatchRingDropsEntriesWithoutTaskID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch_ring.json")
	// A hand-written snapshot containing one valid and one malformed entry.
	body := `[{"taskId":"","status":"done"},{"taskId":"t1","status":"done"}]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewPersistentDispatchRing(path, nil)
	got := r.Recent(20)
	if len(got) != 1 || got[0].TaskID != "t1" {
		t.Fatalf("want only the valid entry, got %+v", got)
	}
}

func TestPersistentDispatchRingEnforcesCapacityOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch_ring.json")
	// Pre-seed a snapshot that exceeds capacity (e.g. from a future build with a
	// larger cap). Loading must clamp to the newest dispatchRingCapacity entries.
	r1 := NewDispatchRing()
	for i := 0; i < dispatchRingCapacity+10; i++ {
		r1.entries = append(r1.entries, DispatchEntry{TaskID: "t" + string(rune('A'+i%26)) + string(rune('0'+i%10))})
	}
	r1.path = path
	r1.mu.Lock()
	r1.saveLocked()
	r1.mu.Unlock()

	r2 := NewPersistentDispatchRing(path, nil)
	if got := r2.Recent(1000); len(got) != dispatchRingCapacity {
		t.Fatalf("want capacity %d after load, got %d", dispatchRingCapacity, len(got))
	}
}
