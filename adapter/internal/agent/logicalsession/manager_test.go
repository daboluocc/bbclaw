package logicalsession

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// testLogger returns a logger safe to use from tests. obs.Logger has
// nil-receiver safety, but using NewLogger keeps test output realistic.
func testLogger() *obs.Logger {
	return obs.NewLogger()
}

func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	m, err := NewManager(path, "/tmp/default", testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, path
}

func TestCreateAndGet(t *testing.T) {
	m, _ := newTestManager(t)

	s, err := m.Create("dev-1", "claude-code", "/work/proj", "Refactor auth")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(string(s.ID), idPrefix) {
		t.Errorf("id %q missing %q prefix", s.ID, idPrefix)
	}
	if s.DeviceID != "dev-1" || s.Driver != "claude-code" || s.Cwd != "/work/proj" || s.Title != "Refactor auth" {
		t.Errorf("unexpected fields: %+v", s)
	}
	if s.CLISessionID != "" {
		t.Errorf("CLISessionID should start empty, got %q", s.CLISessionID)
	}
	if s.CreatedAt.IsZero() || s.LastUsedAt.IsZero() {
		t.Errorf("timestamps unset: %+v", s)
	}

	got, ok := m.Get(s.ID)
	if !ok {
		t.Fatalf("Get returned not found for id %s", s.ID)
	}
	if got.ID != s.ID || got.Title != s.Title {
		t.Errorf("Get returned divergent session: %+v vs %+v", got, s)
	}

	// Ensure caller's copy mutating doesn't affect storage.
	got.Title = "MUTATED"
	again, _ := m.Get(s.ID)
	if again.Title == "MUTATED" {
		t.Errorf("Get must return a copy, but storage was mutated")
	}
}

func TestPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	mA, err := NewManager(path, "/tmp/default", testLogger())
	if err != nil {
		t.Fatalf("NewManager A: %v", err)
	}
	created, err := mA.Create("dev-A", "opencode", "/proj/A", "session-A")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	mB, err := NewManager(path, "/tmp/default", testLogger())
	if err != nil {
		t.Fatalf("NewManager B: %v", err)
	}
	got, ok := mB.Get(created.ID)
	if !ok {
		t.Fatalf("session %s missing after restart", created.ID)
	}
	if got.DeviceID != "dev-A" || got.Driver != "opencode" || got.Cwd != "/proj/A" || got.Title != "session-A" {
		t.Errorf("session diverged after restart: %+v", got)
	}

	list := mB.List("dev-A", "", 0)
	if len(list) != 1 || list[0].ID != created.ID {
		t.Errorf("List after restart wrong: %+v", list)
	}
}

func TestUpdateCLISessionID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	mA, err := NewManager(path, "", testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	s, err := mA.Create("dev-1", "claude-code", "/proj", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const cliID = "8a352891-4f65-4fad-a04e-e8cfa8fc21d6"
	if err := mA.UpdateCLISessionID(s.ID, cliID); err != nil {
		t.Fatalf("UpdateCLISessionID: %v", err)
	}

	mB, err := NewManager(path, "", testLogger())
	if err != nil {
		t.Fatalf("NewManager B: %v", err)
	}
	got, ok := mB.Get(s.ID)
	if !ok {
		t.Fatalf("session missing after restart")
	}
	if got.CLISessionID != cliID {
		t.Errorf("cli session id not preserved: got %q want %q", got.CLISessionID, cliID)
	}
	if !got.LastUsedAt.After(s.LastUsedAt) && !got.LastUsedAt.Equal(s.LastUsedAt) {
		t.Errorf("LastUsedAt should have advanced: before=%s after=%s", s.LastUsedAt, got.LastUsedAt)
	}
}

func TestListSortAndLimit(t *testing.T) {
	m, _ := newTestManager(t)

	ids := make([]ID, 5)
	for i := 0; i < 5; i++ {
		s, err := m.Create("dev-1", "claude-code", "/p", "")
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids[i] = s.ID
		// Touch sessions in reverse order so their LastUsedAt order is
		// the inverse of creation order.
		time.Sleep(2 * time.Millisecond)
	}
	// Touch ids[0] last so it should sort first.
	time.Sleep(2 * time.Millisecond)
	if err := m.Touch(ids[0]); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	all := m.List("dev-1", "", 0)
	if len(all) != 5 {
		t.Fatalf("List returned %d, want 5", len(all))
	}
	if all[0].ID != ids[0] {
		t.Errorf("most recently used should be ids[0]=%s, got %s", ids[0], all[0].ID)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].LastUsedAt.Before(all[i].LastUsedAt) {
			t.Errorf("not sorted desc at %d: %s before %s",
				i, all[i-1].LastUsedAt, all[i].LastUsedAt)
		}
	}

	limited := m.List("dev-1", "", 2)
	if len(limited) != 2 {
		t.Errorf("limit=2 returned %d", len(limited))
	}

	// Driver filter excludes mismatched driver.
	if got := m.List("dev-1", "opencode", 0); len(got) != 0 {
		t.Errorf("driver filter returned %d, want 0", len(got))
	}

	// Device filter excludes mismatched device.
	if got := m.List("dev-other", "", 0); len(got) != 0 {
		t.Errorf("device filter returned %d, want 0", len(got))
	}

	// Empty deviceID matches any device.
	if got := m.List("", "", 0); len(got) != 5 {
		t.Errorf("empty deviceID returned %d, want 5", len(got))
	}
}

func TestDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	mA, err := NewManager(path, "/tmp", testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	keep, err := mA.Create("dev-1", "claude-code", "/p", "keep")
	if err != nil {
		t.Fatalf("Create keep: %v", err)
	}
	gone, err := mA.Create("dev-1", "claude-code", "/p", "gone")
	if err != nil {
		t.Fatalf("Create gone: %v", err)
	}
	if err := mA.Delete(gone.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := mA.Get(gone.ID); ok {
		t.Errorf("Get returned deleted session")
	}
	if err := mA.Delete(gone.ID); err == nil {
		t.Errorf("Delete on missing id should fail")
	}

	mB, err := NewManager(path, "/tmp", testLogger())
	if err != nil {
		t.Fatalf("NewManager B: %v", err)
	}
	if _, ok := mB.Get(gone.ID); ok {
		t.Errorf("deletion not persisted across restart")
	}
	if _, ok := mB.Get(keep.ID); !ok {
		t.Errorf("non-deleted session lost across restart")
	}
}

func TestConcurrentCreate(t *testing.T) {
	m, path := newTestManager(t)

	var wg sync.WaitGroup
	const n = 10
	errs := make(chan error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			if _, err := m.Create("dev-1", "claude-code", "/p", ""); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Create concurrent: %v", err)
	}

	all := m.List("dev-1", "", 0)
	if len(all) != n {
		t.Errorf("concurrent created %d sessions, want %d", len(all), n)
	}

	// File on disk must be valid JSON.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var shape fileShape
	if err := json.Unmarshal(data, &shape); err != nil {
		t.Fatalf("file is not valid JSON: %v\nbody=%s", err, string(data))
	}
	if shape.Version != fileFormatVersion {
		t.Errorf("file version=%d want %d", shape.Version, fileFormatVersion)
	}
	if len(shape.Sessions) != n {
		t.Errorf("file has %d sessions, want %d", len(shape.Sessions), n)
	}
}

func TestNewManagerOnMissingFile(t *testing.T) {
	dir := t.TempDir()
	// Path includes a not-yet-created subdirectory.
	path := filepath.Join(dir, "nested", "sessions.json")

	m, err := NewManager(path, "/tmp", testLogger())
	if err != nil {
		t.Fatalf("NewManager on missing path: %v", err)
	}
	if got := m.List("", "", 0); len(got) != 0 {
		t.Errorf("expected empty list, got %d", len(got))
	}
	// File must not yet exist; only created on first write.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should not exist before first write, got err=%v", err)
	}

	if _, err := m.Create("dev-1", "claude-code", "/p", ""); err != nil {
		t.Fatalf("Create after empty init: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file should exist after first write: %v", err)
	}

	info, err := os.Stat(path)
	if err == nil {
		// On Unix, mode bits should be 0600.
		if mode := info.Mode().Perm(); mode != fileMode {
			t.Errorf("file mode = %o, want %o", mode, fileMode)
		}
	}
}

func TestDefaultCwdFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	const defaultCwd = "/default/workspace"

	m, err := NewManager(path, defaultCwd, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	s, err := m.Create("dev-1", "claude-code", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Cwd != defaultCwd {
		t.Errorf("cwd=%q want fallback %q", s.Cwd, defaultCwd)
	}

	explicit, err := m.Create("dev-1", "claude-code", "/explicit", "")
	if err != nil {
		t.Fatalf("Create explicit: %v", err)
	}
	if explicit.Cwd != "/explicit" {
		t.Errorf("explicit cwd ignored: got %q", explicit.Cwd)
	}
}

func TestSetTitle(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create("dev-1", "claude-code", "/p", "old")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.SetTitle(s.ID, "new"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	got, _ := m.Get(s.ID)
	if got.Title != "new" {
		t.Errorf("title=%q want %q", got.Title, "new")
	}
	if err := m.SetTitle("ls-doesnotexist", "x"); err == nil {
		t.Errorf("SetTitle on missing id should fail")
	}
}
