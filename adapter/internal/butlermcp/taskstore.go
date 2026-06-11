package butlermcp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TaskStatus constants for a task run entry.
const (
	TaskStatusRunning = "running"
	TaskStatusDone    = "done"
	TaskStatusError   = "error"
	TaskStatusStale   = "stale"
)

// TaskRun is the persisted state of one dispatched task.
type TaskRun struct {
	TaskID    string    `json:"taskId"`
	Cwd       string    `json:"cwd"`
	Prompt    string    `json:"prompt"`
	Status    string    `json:"status"` // running|done|error|stale
	OwnerPID  int       `json:"ownerPid"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
	ElapsedMs int64     `json:"elapsedMs,omitempty"`
	Result    string    `json:"result,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// TaskStore persists task runs under <dataDir>/task-runs/<taskId>/meta.json.
// All writes are atomic (write-to-temp then os.Rename). A zero-value TaskStore
// (no dataDir) is valid and operates in memory-only mode so that tests without
// a real filesystem still work.
type TaskStore struct {
	dataDir string // empty → memory-only
	mu      sync.RWMutex
	mem     map[string]*TaskRun // hot cache (always up-to-date in-process)
}

// NewTaskStore creates a TaskStore backed by <dataDir>/task-runs/.
// Pass an empty string to run memory-only (for tests).
func NewTaskStore(dataDir string) *TaskStore {
	ts := &TaskStore{
		dataDir: dataDir,
		mem:     make(map[string]*TaskRun),
	}
	if dataDir != "" {
		ts.reconcile()
	}
	return ts
}

// taskRunsDir returns the base directory for task run files.
func (ts *TaskStore) taskRunsDir() string {
	return filepath.Join(ts.dataDir, "task-runs")
}

// metaPath returns the path to the meta.json for a given taskId.
func (ts *TaskStore) metaPath(taskID string) string {
	return filepath.Join(ts.taskRunsDir(), taskID, "meta.json")
}

// Create persists a new task in "running" state.
func (ts *TaskStore) Create(taskID, cwd, prompt string) error {
	run := &TaskRun{
		TaskID:    taskID,
		Cwd:       cwd,
		Prompt:    prompt,
		Status:    TaskStatusRunning,
		OwnerPID:  os.Getpid(),
		StartedAt: time.Now(),
	}
	ts.mu.Lock()
	ts.mem[taskID] = run
	ts.mu.Unlock()
	return ts.writeMeta(run)
}

// MarkDone updates a task to "done" with the given result.
func (ts *TaskStore) MarkDone(taskID, result string) error {
	ts.mu.Lock()
	run := ts.getOrLoad(taskID)
	if run == nil {
		ts.mu.Unlock()
		return errors.New("task not found: " + taskID)
	}
	now := time.Now()
	// Clone so we can write to disk without holding the lock.
	updated := *run
	updated.Status = TaskStatusDone
	updated.Result = result
	updated.EndedAt = now
	updated.ElapsedMs = now.Sub(run.StartedAt).Milliseconds()
	ts.mu.Unlock()
	// Write to disk first, then update the in-memory cache. This ensures that
	// any concurrent Get (even from the same process) that re-reads the disk
	// will see the final state after the memory cache is updated.
	if err := ts.writeMeta(&updated); err != nil {
		return err
	}
	ts.mu.Lock()
	ts.mem[taskID] = &updated
	ts.mu.Unlock()
	return nil
}

// MarkError updates a task to "error" with the given message.
func (ts *TaskStore) MarkError(taskID, msg string) error {
	ts.mu.Lock()
	run := ts.getOrLoad(taskID)
	if run == nil {
		ts.mu.Unlock()
		return errors.New("task not found: " + taskID)
	}
	now := time.Now()
	// Clone so we can write to disk without holding the lock.
	updated := *run
	updated.Status = TaskStatusError
	updated.Error = msg
	updated.EndedAt = now
	updated.ElapsedMs = now.Sub(run.StartedAt).Milliseconds()
	ts.mu.Unlock()
	// Write disk first, then memory (same order as MarkDone).
	if err := ts.writeMeta(&updated); err != nil {
		return err
	}
	ts.mu.Lock()
	ts.mem[taskID] = &updated
	ts.mu.Unlock()
	return nil
}

// Get returns the TaskRun for the given taskId, loading from disk on cache miss.
// For tasks still in "running" state, the on-disk record is always re-read:
// MarkDone/MarkError write to disk before updating the in-memory cache, so
// the disk is the authoritative source of truth for the final status.
// Returns (TaskRun{}, false) when not found.
func (ts *TaskStore) Get(taskID string) (TaskRun, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	run := ts.getOrLoad(taskID)
	if run == nil {
		return TaskRun{}, false
	}
	// Always re-read from disk for running tasks. MarkDone/MarkError write the
	// disk before updating the memory cache, so a disk refresh is safe and
	// ensures both cross-process and same-process callers see the latest state.
	if run.Status == TaskStatusRunning && ts.dataDir != "" {
		if fresh, err := ts.loadMeta(taskID); err == nil {
			ts.mem[taskID] = fresh
			run = fresh
		}
	}
	return *run, true
}

// getOrLoad returns the in-memory TaskRun for taskID, loading from disk when
// absent from the cache. Caller MUST hold ts.mu.
func (ts *TaskStore) getOrLoad(taskID string) *TaskRun {
	if run, ok := ts.mem[taskID]; ok {
		return run
	}
	if ts.dataDir == "" {
		return nil
	}
	run, err := ts.loadMeta(taskID)
	if err != nil {
		return nil
	}
	ts.mem[taskID] = run
	return run
}

// Recent returns up to n task runs ordered newest-first.
func (ts *TaskStore) Recent(n int) []TaskRun {
	ts.mu.RLock()
	out := make([]TaskRun, 0, len(ts.mem))
	for _, r := range ts.mem {
		out = append(out, *r)
	}
	ts.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// writeMeta atomically writes the meta.json for the task run.
// No-op when ts.dataDir is empty (memory-only mode).
func (ts *TaskStore) writeMeta(run *TaskRun) error {
	if ts.dataDir == "" {
		return nil
	}
	dir := filepath.Join(ts.taskRunsDir(), run.TaskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(run)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "meta.json.tmp."+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "meta.json"))
}

// loadMeta reads the meta.json for the given taskId from disk.
func (ts *TaskStore) loadMeta(taskID string) (*TaskRun, error) {
	data, err := os.ReadFile(ts.metaPath(taskID))
	if err != nil {
		return nil, err
	}
	var run TaskRun
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// reconcile scans task-runs/ on startup and marks any "running" tasks whose
// owner process is no longer alive as "stale". This prevents tasks orphaned
// by a crash from being stuck in "running" forever.
func (ts *TaskStore) reconcile() {
	base := ts.taskRunsDir()
	entries, err := os.ReadDir(base)
	if err != nil {
		return // directory doesn't exist yet; nothing to reconcile
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		taskID := e.Name()
		// skip obviously invalid names
		if strings.ContainsAny(taskID, "/\\") {
			continue
		}
		run, err := ts.loadMeta(taskID)
		if err != nil {
			continue
		}
		ts.mu.Lock()
		ts.mem[taskID] = run
		ts.mu.Unlock()
		if run.Status == TaskStatusRunning && !pidAlive(run.OwnerPID) {
			_ = ts.MarkError(taskID, "stale: owner process "+strconv.Itoa(run.OwnerPID)+" no longer running")
		}
	}
}

// pidAlive reports whether the process with the given PID is currently running.
// The platform-specific checkPidLive (pid_unix.go / pid_windows.go) does the
// actual probe; this function adds a simple pid <= 0 guard.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return checkPidLive(pid)
}
