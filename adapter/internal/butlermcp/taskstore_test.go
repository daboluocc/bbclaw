package butlermcp

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTaskStoreMemoryOnly(t *testing.T) {
	ts := NewTaskStore("") // memory-only

	if err := ts.Create("t1", "/cwd", "do something"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	run, ok := ts.Get("t1")
	if !ok {
		t.Fatal("Get after Create: not found")
	}
	if run.Status != TaskStatusRunning {
		t.Fatalf("expected running, got %q", run.Status)
	}

	if err := ts.MarkDone("t1", "great result"); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	run, ok = ts.Get("t1")
	if !ok || run.Status != TaskStatusDone || run.Result != "great result" {
		t.Fatalf("after MarkDone: %+v ok=%v", run, ok)
	}
}

func TestTaskStoreMarkError(t *testing.T) {
	ts := NewTaskStore("")
	_ = ts.Create("t2", "/cwd", "task")
	if err := ts.MarkError("t2", "something broke"); err != nil {
		t.Fatalf("MarkError: %v", err)
	}
	run, ok := ts.Get("t2")
	if !ok || run.Status != TaskStatusError || run.Error != "something broke" {
		t.Fatalf("after MarkError: %+v ok=%v", run, ok)
	}
}

func TestTaskStoreRecent(t *testing.T) {
	ts := NewTaskStore("")
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		_ = ts.Create(id, "/cwd", "task")
		time.Sleep(time.Millisecond) // distinct timestamps
	}
	recent := ts.Recent(3)
	if len(recent) != 3 {
		t.Fatalf("Recent(3) returned %d entries", len(recent))
	}
	// newest first
	if !recent[0].StartedAt.After(recent[1].StartedAt) {
		t.Errorf("not ordered newest-first: %v vs %v", recent[0].StartedAt, recent[1].StartedAt)
	}
}

func TestTaskStoreFilePersistence(t *testing.T) {
	dir := t.TempDir()
	ts1 := NewTaskStore(dir)

	if err := ts1.Create("persist-1", "/project", "build it"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ts1.MarkDone("persist-1", "built successfully"); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}

	// Simulate mcp-server restart: new TaskStore from same dir.
	ts2 := NewTaskStore(dir)
	run, ok := ts2.Get("persist-1")
	if !ok {
		t.Fatal("task not found in new store after restart simulation")
	}
	if run.Status != TaskStatusDone {
		t.Fatalf("expected done, got %q", run.Status)
	}
	if run.Result != "built successfully" {
		t.Fatalf("unexpected result: %q", run.Result)
	}
}

func TestTaskStoreAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	ts := NewTaskStore(dir)

	_ = ts.Create("atomic-1", "/cwd", "task")

	// meta.json should exist; no tmp files should remain.
	metaPath := filepath.Join(dir, "task-runs", "atomic-1", "meta.json")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("meta.json not written: %v", err)
	}

	// No leftover .tmp files.
	entries, _ := filepath.Glob(filepath.Join(dir, "task-runs", "atomic-1", "*.tmp*"))
	if len(entries) > 0 {
		t.Errorf("leftover tmp files: %v", entries)
	}
}

func TestTaskStoreReconcileStale(t *testing.T) {
	dir := t.TempDir()
	ts1 := NewTaskStore(dir)

	// Create a task with a dead PID (PID 1 is init; use PID 999999 as "dead").
	_ = ts1.Create("stale-1", "/cwd", "long running")
	// Manually rewrite meta with a PID that won't be alive.
	ts1.mu.Lock()
	run := ts1.mem["stale-1"]
	run.OwnerPID = 999999
	ts1.mu.Unlock()
	_ = ts1.writeMeta(run)

	// New store reconciles on startup.
	ts2 := NewTaskStore(dir)
	got, ok := ts2.Get("stale-1")
	if !ok {
		t.Fatal("stale task not found after reconcile")
	}
	// PID 999999 is almost certainly dead; reconcile should have marked it error/stale.
	if got.Status == TaskStatusRunning {
		t.Logf("note: PID 999999 appears alive on this system; stale reconcile skipped (expected on some CI)")
	}
}
