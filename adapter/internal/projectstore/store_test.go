package projectstore

import (
	"os"
	"path/filepath"
	"testing"
)

func storePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "projects.json")
}

func TestOpenMissingFileIsEmpty(t *testing.T) {
	s, err := Open(storePath(t))
	if err != nil {
		t.Fatalf("Open missing: %v", err)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("expected empty pool, got %v", got)
	}
}

func TestSeedIfMissingThenEditable(t *testing.T) {
	path := storePath(t)
	if _, err := Bootstrap(path, []Project{{Name: "main", Path: "/tmp/main"}}); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	list := s.List()
	if len(list) != 1 || list[0].Name != "main" || list[0].Source != SourceEnv {
		t.Fatalf("seed not surfaced: %+v", list)
	}
	// Seeded (env-origin) projects ARE removable now — web-first model.
	if ok, err := s.Remove("main"); err != nil || !ok {
		t.Fatalf("Remove seeded project = (%v,%v), want (true,nil)", ok, err)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("pool not empty after removing seeded project: %v", got)
	}
}

func TestSeedIfMissingIsNoOpWhenFileExists(t *testing.T) {
	path := storePath(t)
	if _, err := Bootstrap(path, []Project{{Name: "first", Path: "/tmp/first"}}); err != nil {
		t.Fatal(err)
	}
	// A second seed with different content must be ignored (file wins).
	if _, err := Bootstrap(path, []Project{{Name: "second", Path: "/tmp/second"}}); err != nil {
		t.Fatal(err)
	}
	s, _ := Open(path)
	list := s.List()
	if len(list) != 1 || list[0].Name != "first" {
		t.Fatalf("file should win over re-seed, got %+v", list)
	}
}

func TestBootstrapMigratesLegacyAndMergesSeed(t *testing.T) {
	path := storePath(t)
	// Simulate a pre-release file in the old delta format with one admin entry.
	legacy := `{"version":1,"added":[{"name":"agent_room","path":"/Users/me/github/agent_room","source":"admin"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := Bootstrap(path, []Project{
		{Name: "agent_room", Path: "/Users/me/github/agent_room"}, // dup of existing → skipped
		{Name: "bbclaw", Path: "/Users/me/github/bbclaw"},         // new env entry → merged
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != BootstrapMigrated {
		t.Fatalf("status = %q, want migrated", status)
	}
	s, _ := Open(path)
	names := map[string]bool{}
	for _, p := range s.List() {
		names[p.Name] = true
	}
	// Existing admin entry preserved, env entry merged, no duplicate.
	if !names["agent_room"] || !names["bbclaw"] || len(names) != 2 {
		t.Fatalf("after migration names = %v, want {agent_room, bbclaw}", names)
	}
	// File is now current-format: a re-bootstrap is a no-op.
	if status, _ := Bootstrap(path, []Project{{Name: "other", Path: "/x"}}); status != BootstrapNoop {
		t.Fatalf("second bootstrap status = %q, want noop", status)
	}
}

func TestAddValidatesAndPersists(t *testing.T) {
	path := storePath(t)
	dir := t.TempDir()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Add("proj", dir); err != nil {
		t.Fatalf("Add valid dir: %v", err)
	}

	// Survives reopen (persisted delta).
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	list := s2.List()
	if len(list) != 1 || list[0].Name != "proj" || list[0].Path != filepath.Clean(dir) || list[0].Source != SourceAdmin {
		t.Fatalf("admin add not persisted/reloaded: %+v", list)
	}
}

func TestAddRejectsBadInput(t *testing.T) {
	s, _ := Open(storePath(t))
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, path, why string
	}{
		{"", dir, "empty name"},
		{"a,b", dir, "comma in name"},
		{"a:b", dir, "colon in name"},
		{"ok", "relative/path", "relative path"},
		{"ok", filepath.Join(dir, "missing"), "missing path"},
		{"ok", file, "path is a file"},
	}
	for _, c := range cases {
		if _, err := s.Add(c.name, c.path); err == nil {
			t.Errorf("Add(%q,%q) should fail (%s)", c.name, c.path, c.why)
		}
	}
}

func TestAddRejectsDuplicates(t *testing.T) {
	path := storePath(t)
	if _, err := Bootstrap(path, []Project{{Name: "env1", Path: "/tmp/envpath"}}); err != nil {
		t.Fatal(err)
	}
	s, _ := Open(path)
	dir := t.TempDir()
	if _, err := s.Add("a", dir); err != nil {
		t.Fatal(err)
	}
	// duplicate name
	if _, err := s.Add("a", t.TempDir()); err == nil {
		t.Error("duplicate name should be rejected")
	}
	// duplicate path
	if _, err := s.Add("b", dir); err == nil {
		t.Error("duplicate path should be rejected")
	}
	// name colliding with a seeded entry
	if _, err := s.Add("env1", t.TempDir()); err == nil {
		t.Error("name colliding with seeded entry should be rejected")
	}
}

func TestRemoveAdmin(t *testing.T) {
	s, _ := Open(storePath(t))
	dir := t.TempDir()
	if _, err := s.Add("gone", dir); err != nil {
		t.Fatal(err)
	}
	ok, err := s.Remove("gone")
	if err != nil || !ok {
		t.Fatalf("Remove admin = (%v,%v), want (true,nil)", ok, err)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("pool not empty after remove: %v", got)
	}
	// Removing a non-existent project is a no-op, not an error.
	ok, err = s.Remove("nope")
	if err != nil || ok {
		t.Fatalf("Remove missing = (%v,%v), want (false,nil)", ok, err)
	}
}

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	path := storePath(t)
	if _, err := Bootstrap(path, []Project{{Name: "main", Path: "/tmp/main"}}); err != nil {
		t.Fatal(err)
	}
	s, _ := Open(path)
	if _, err := s.Add("side", dir); err != nil {
		t.Fatal(err)
	}
	if cwd, ok := s.Resolve("main", ""); !ok || cwd != "/tmp/main" {
		t.Errorf("Resolve by seeded name = (%q,%v)", cwd, ok)
	}
	if cwd, ok := s.Resolve("", dir); !ok || cwd != dir {
		t.Errorf("Resolve by admin path = (%q,%v)", cwd, ok)
	}
	if _, ok := s.Resolve("nope", "/not/listed"); ok {
		t.Error("Resolve of unlisted selection should fail")
	}
}

func TestAddPathDerivesUniqueName(t *testing.T) {
	s, _ := Open(storePath(t))
	parentA := t.TempDir()
	parentB := t.TempDir()
	// Two different dirs that share the base name "proj".
	dirA := filepath.Join(parentA, "proj")
	dirB := filepath.Join(parentB, "proj")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := s.AddPath(dirA)
	if err != nil || p1.Name != "proj" {
		t.Fatalf("AddPath dirA = (%+v,%v), want name=proj", p1, err)
	}
	p2, err := s.AddPath(dirB)
	if err != nil || p2.Name != "proj-2" {
		t.Fatalf("AddPath dirB = (%+v,%v), want name=proj-2", p2, err)
	}
}

func TestListReloadsOnFileChange(t *testing.T) {
	path := storePath(t)
	dir := t.TempDir()
	writer, _ := Open(path)
	reader, _ := Open(path)

	if _, err := writer.Add("late", dir); err != nil {
		t.Fatal(err)
	}
	// A second, independent Store (simulating the mcp-server subprocess) must
	// see the add on its next List() via mtime-based reload.
	found := false
	for _, p := range reader.List() {
		if p.Name == "late" {
			found = true
		}
	}
	if !found {
		t.Fatal("reader Store did not reload the admin add from disk")
	}
}
