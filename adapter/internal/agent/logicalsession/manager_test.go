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

func TestDefaultTitleFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	m, err := NewManager(path, "", testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	t.Run("cwd provided → filepath.Base(cwd)", func(t *testing.T) {
		s, err := m.Create("dev-1", "claude-code", "/Users/mikas/github/bbclaw", "")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if s.Title != "bbclaw" {
			t.Errorf("title=%q want %q", s.Title, "bbclaw")
		}
	})

	t.Run("cwd and defaultCwd both empty → session-<short id>", func(t *testing.T) {
		s, err := m.Create("dev-1", "claude-code", "", "")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if !strings.HasPrefix(s.Title, "session-") {
			t.Errorf("title=%q want prefix %q", s.Title, "session-")
		}
		suffix := strings.TrimPrefix(s.Title, "session-")
		if len(suffix) != 6 {
			t.Errorf("title suffix %q should be 6 chars, got %d", suffix, len(suffix))
		}
		// Suffix must match the last 6 chars of the id.
		idStr := string(s.ID)
		wantSuffix := idStr[len(idStr)-6:]
		if suffix != wantSuffix {
			t.Errorf("title suffix %q does not match id tail %q", suffix, wantSuffix)
		}
	})

	t.Run("explicit title is not overwritten", func(t *testing.T) {
		s, err := m.Create("dev-1", "claude-code", "/Users/mikas/github/bbclaw", "my custom title")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if s.Title != "my custom title" {
			t.Errorf("title=%q want %q", s.Title, "my custom title")
		}
	})

	t.Run("defaultCwd fallback used for title when cwd arg is empty", func(t *testing.T) {
		m2, err := NewManager(filepath.Join(dir, "s2.json"), "/home/user/my-project", testLogger())
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		s, err := m2.Create("dev-1", "claude-code", "", "")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// cwd arg "" → falls back to defaultCwd "/home/user/my-project" → Base = "my-project"
		if s.Title != "my-project" {
			t.Errorf("title=%q want %q", s.Title, "my-project")
		}
	})

	t.Run("degenerate cwd '/' falls back to session-<id>", func(t *testing.T) {
		s, err := m.Create("dev-1", "claude-code", "/", "")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if !strings.HasPrefix(s.Title, "session-") {
			t.Errorf("title=%q want prefix %q for degenerate cwd", s.Title, "session-")
		}
	})
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

func TestUpdateCwd(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create("dev-1", "claude-code", "/old/path", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.UpdateCwd(s.ID, "/new/path"); err != nil {
		t.Fatalf("UpdateCwd: %v", err)
	}
	got, _ := m.Get(s.ID)
	if got.Cwd != "/new/path" {
		t.Errorf("cwd=%q want %q", got.Cwd, "/new/path")
	}
	// Empty cwd is written verbatim — UpdateCwd does not fall back to
	// defaultCwd the way Create does.
	if err := m.UpdateCwd(s.ID, ""); err != nil {
		t.Fatalf("UpdateCwd empty: %v", err)
	}
	got, _ = m.Get(s.ID)
	if got.Cwd != "" {
		t.Errorf("cwd=%q want empty", got.Cwd)
	}
	if err := m.UpdateCwd("ls-doesnotexist", "/x"); err == nil {
		t.Errorf("UpdateCwd on missing id should fail")
	}
}

func TestFindRecent(t *testing.T) {
	m, _ := newTestManager(t)

	// Create sessions for different devices and drivers.
	s1, err := m.Create("dev-1", "claude-code", "/p", "session1")
	if err != nil {
		t.Fatalf("Create s1: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	s2, err := m.Create("dev-1", "claude-code", "/p", "session2")
	if err != nil {
		t.Fatalf("Create s2: %v", err)
	}
	// s2 is more recent for dev-1 + claude-code.
	_ = s1

	// FindRecent should return s2 (most recent for dev-1 + claude-code).
	got := m.FindRecent("dev-1", "claude-code", 1*time.Minute)
	if got == nil {
		t.Fatal("FindRecent returned nil, expected s2")
	}
	if got.ID != s2.ID {
		t.Errorf("FindRecent returned %s, want %s", got.ID, s2.ID)
	}

	// Different driver → no match.
	got = m.FindRecent("dev-1", "opencode", 1*time.Minute)
	if got != nil {
		t.Errorf("FindRecent should return nil for unmatched driver, got %s", got.ID)
	}

	// Different device → no match.
	got = m.FindRecent("dev-2", "claude-code", 1*time.Minute)
	if got != nil {
		t.Errorf("FindRecent should return nil for unmatched device, got %s", got.ID)
	}

	// Empty device matches any device.
	got = m.FindRecent("", "claude-code", 1*time.Minute)
	if got == nil {
		t.Fatal("FindRecent with empty deviceID returned nil")
	}
	if got.ID != s2.ID {
		t.Errorf("FindRecent with empty deviceID returned %s, want %s", got.ID, s2.ID)
	}

	// Zero-duration window → nothing qualifies (sessions are at least a few ms old).
	got = m.FindRecent("dev-1", "claude-code", 0)
	if got != nil {
		t.Errorf("FindRecent with 0 window should return nil, got %s", got.ID)
	}

	// Empty driver → always nil.
	got = m.FindRecent("dev-1", "", 1*time.Minute)
	if got != nil {
		t.Errorf("FindRecent with empty driver should return nil, got %s", got.ID)
	}
}

func TestSweep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	m, err := NewManager(path, "/tmp", testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Create 3 sessions.
	s1, err := m.Create("dev-1", "claude-code", "/p", "old1")
	if err != nil {
		t.Fatalf("Create s1: %v", err)
	}
	s2, err := m.Create("dev-1", "claude-code", "/p", "old2")
	if err != nil {
		t.Fatalf("Create s2: %v", err)
	}
	s3, err := m.Create("dev-1", "claude-code", "/p", "recent")
	if err != nil {
		t.Fatalf("Create s3: %v", err)
	}

	// Manually backdate s1 and s2 to simulate old sessions.
	m.mu.Lock()
	oldTime := time.Now().UTC().Add(-8 * 24 * time.Hour) // 8 days ago
	m.sessions[s1.ID].LastUsedAt = oldTime
	m.sessions[s2.ID].LastUsedAt = oldTime
	m.mu.Unlock()
	// Persist the backdated state.
	m.mu.Lock()
	_ = m.persistLocked()
	m.mu.Unlock()

	// Sweep with 7-day max age should remove s1 and s2.
	n := m.Sweep(7 * 24 * time.Hour)
	if n != 2 {
		t.Errorf("Sweep returned %d, want 2", n)
	}

	// s3 should still exist.
	if _, ok := m.Get(s3.ID); !ok {
		t.Error("s3 should survive sweep")
	}
	if _, ok := m.Get(s1.ID); ok {
		t.Error("s1 should be swept")
	}
	if _, ok := m.Get(s2.ID); ok {
		t.Error("s2 should be swept")
	}

	// Verify persistence — reload from disk.
	m2, err := NewManager(path, "/tmp", testLogger())
	if err != nil {
		t.Fatalf("NewManager reload: %v", err)
	}
	if _, ok := m2.Get(s1.ID); ok {
		t.Error("s1 should not survive reload after sweep")
	}
	if _, ok := m2.Get(s3.ID); !ok {
		t.Error("s3 should survive reload after sweep")
	}

	// Sweep with nothing to remove returns 0.
	n = m.Sweep(7 * 24 * time.Hour)
	if n != 0 {
		t.Errorf("Sweep on clean state returned %d, want 0", n)
	}
}

// TestSweepRollbackOnPersistFailure verifies that when the post-delete persist
// fails, Sweep restores the removed sessions to the in-memory map so memory
// stays consistent with what's on disk (regression for the old "can't restore"
// path that silently dropped sessions).
func TestSweepRollbackOnPersistFailure(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(filepath.Join(dir, "sessions.json"), "/tmp", testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	s, err := m.Create("dev-1", "claude-code", "/p", "old")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	m.mu.Lock()
	m.sessions[s.ID].LastUsedAt = time.Now().UTC().Add(-30 * 24 * time.Hour)
	m.mu.Unlock()

	// Break persistence: point the store dir at a regular file so the MkdirAll
	// in persistLocked fails (ENOTDIR).
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	m.path = filepath.Join(blocker, "sessions.json")

	if n := m.Sweep(7 * 24 * time.Hour); n != 0 {
		t.Errorf("Sweep returned %d, want 0 on persist failure", n)
	}
	if _, ok := m.Get(s.ID); !ok {
		t.Error("session dropped from map after failed sweep persist (no rollback)")
	}
}

// TestCreateDefaultRoleEmpty verifies the legacy Create entrypoint mints a
// session with the empty role (RoleNone) — backward compatible.
func TestCreateDefaultRoleEmpty(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create("dev-1", "claude-code", "/p", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Role != RoleNone {
		t.Errorf("Create should default to RoleNone, got %q", s.Role)
	}
}

// TestCreateWithRolePersistAndReject verifies role validation and that the
// role survives a write→reload cycle.
func TestCreateWithRolePersistAndReject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	mA, err := NewManager(path, "/tmp/default", testLogger())
	if err != nil {
		t.Fatalf("NewManager A: %v", err)
	}

	butler, err := mA.CreateWithRole("dev-1", "claude-code", "/p", "butler", RoleButler)
	if err != nil {
		t.Fatalf("CreateWithRole butler: %v", err)
	}
	worker, err := mA.CreateWithRole("dev-1", "claude-code", "/p", "worker", RoleWorker)
	if err != nil {
		t.Fatalf("CreateWithRole worker: %v", err)
	}
	if butler.Role != RoleButler || worker.Role != RoleWorker {
		t.Fatalf("roles not set: butler=%q worker=%q", butler.Role, worker.Role)
	}

	// Unknown role must be rejected.
	if _, err := mA.CreateWithRole("dev-1", "claude-code", "/p", "x", "supervisor"); err == nil {
		t.Error("CreateWithRole with unknown role should error")
	}

	// Reload from disk and confirm roles persisted.
	mB, err := NewManager(path, "/tmp/default", testLogger())
	if err != nil {
		t.Fatalf("NewManager B: %v", err)
	}
	gotB, ok := mB.Get(butler.ID)
	if !ok || gotB.Role != RoleButler {
		t.Errorf("butler role not persisted: ok=%v role=%q", ok, gotB.Role)
	}
	gotW, ok := mB.Get(worker.ID)
	if !ok || gotW.Role != RoleWorker {
		t.Errorf("worker role not persisted: ok=%v role=%q", ok, gotW.Role)
	}
}

// TestListDeviceFacingExcludesWorker verifies device-facing listings drop
// worker sessions while List (butler/dispatch layer) still sees them.
func TestListDeviceFacingExcludesWorker(t *testing.T) {
	m, _ := newTestManager(t)

	plain, err := m.Create("dev-1", "claude-code", "/p", "plain")
	if err != nil {
		t.Fatalf("Create plain: %v", err)
	}
	butler, err := m.CreateWithRole("dev-1", "claude-code", "/p", "butler", RoleButler)
	if err != nil {
		t.Fatalf("Create butler: %v", err)
	}
	worker, err := m.CreateWithRole("dev-1", "claude-code", "/p", "worker", RoleWorker)
	if err != nil {
		t.Fatalf("Create worker: %v", err)
	}

	// Full List sees all three (butler/dispatch still relies on this).
	if got := m.List("dev-1", "", 0); len(got) != 3 {
		t.Errorf("List should return all 3 sessions, got %d", len(got))
	}

	// Device-facing list excludes the worker.
	df := m.ListDeviceFacing("dev-1", "", 0)
	if len(df) != 2 {
		t.Fatalf("ListDeviceFacing should return 2 (plain+butler), got %d", len(df))
	}
	for _, s := range df {
		if s.ID == worker.ID {
			t.Errorf("ListDeviceFacing leaked worker session %s", worker.ID)
		}
		if s.Role == RoleWorker {
			t.Errorf("ListDeviceFacing returned a RoleWorker session: %+v", s)
		}
	}
	_ = plain
	_ = butler
}

// TestListDeviceFacingFilterBeforeLimit verifies workers are filtered before
// the limit is applied, so they cannot crowd visible sessions out of a capped
// result.
func TestListDeviceFacingFilterBeforeLimit(t *testing.T) {
	m, _ := newTestManager(t)

	// Mint workers first (older), then a visible session (newest by LastUsedAt).
	for i := 0; i < 3; i++ {
		if _, err := m.CreateWithRole("dev-1", "claude-code", "/p", "w", RoleWorker); err != nil {
			t.Fatalf("Create worker %d: %v", i, err)
		}
	}
	visible, err := m.Create("dev-1", "claude-code", "/p", "visible")
	if err != nil {
		t.Fatalf("Create visible: %v", err)
	}

	df := m.ListDeviceFacing("dev-1", "", 1)
	if len(df) != 1 {
		t.Fatalf("expected 1 result under limit, got %d", len(df))
	}
	if df[0].ID != visible.ID {
		t.Errorf("limit dropped the visible session in favor of a worker: got %s", df[0].ID)
	}
}

// TestLegacyRecordLoadsAsRoleNone verifies a sessions.json written before the
// Role field existed (no "role" key) loads with RoleNone and stays
// device-visible.
func TestLegacyRecordLoadsAsRoleNone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	legacy := `{
  "version": 1,
  "sessions": {
    "ls-deadbeefdeadbeef": {
      "id": "ls-deadbeefdeadbeef",
      "deviceId": "dev-1",
      "driver": "claude-code",
      "cwd": "/p",
      "title": "legacy",
      "createdAt": "2026-01-01T00:00:00Z",
      "lastUsedAt": "2026-01-01T00:00:00Z"
    }
  }
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}
	m, err := NewManager(path, "/tmp/default", testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	got, ok := m.Get("ls-deadbeefdeadbeef")
	if !ok {
		t.Fatal("legacy session not loaded")
	}
	if got.Role != RoleNone {
		t.Errorf("legacy record should load as RoleNone, got %q", got.Role)
	}
	if df := m.ListDeviceFacing("dev-1", "", 0); len(df) != 1 {
		t.Errorf("legacy session should be device-visible, ListDeviceFacing returned %d", len(df))
	}
}
