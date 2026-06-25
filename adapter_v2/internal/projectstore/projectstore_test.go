package projectstore

import (
	"os"
	"path/filepath"
	"testing"
)

func storeAt(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, dir
}

func TestOpenMissingFileIsEmpty(t *testing.T) {
	s, _ := storeAt(t)
	if got := s.List(); len(got) != 0 {
		t.Fatalf("missing file should be empty, got %+v", got)
	}
}

func TestOpenCorruptFileIsUsableWithError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err == nil {
		t.Error("corrupt file should surface an error")
	}
	if s == nil {
		t.Fatal("corrupt file should still return a usable store")
	}
	if got := s.List(); len(got) != 0 {
		t.Errorf("corrupt file store should be empty, got %+v", got)
	}
}

func TestAddValidatesPath(t *testing.T) {
	s, dir := storeAt(t)
	cases := map[string]Project{
		"empty path":    {Path: ""},
		"relative path": {Path: "relative/dir"},
		"missing path":  {Path: filepath.Join(dir, "does-not-exist")},
	}
	for name, in := range cases {
		if _, err := s.Add(in); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
	// A file (not a directory) is rejected.
	f := filepath.Join(dir, "afile")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Project{Path: f}); err == nil {
		t.Error("a file path should be rejected (not a directory)")
	}
}

func TestAddDerivesNameAndPersistsFields(t *testing.T) {
	s, dir := storeAt(t)
	proj := filepath.Join(dir, "buildhub")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := s.Add(Project{Path: proj, Summary: "内部构建平台", CLIBin: "bbclaw-buildhub"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got.Name != "buildhub" {
		t.Errorf("name = %q, want derived 'buildhub'", got.Name)
	}
	if got.Summary != "内部构建平台" || got.CLIBin != "bbclaw-buildhub" {
		t.Errorf("summary/cliBin not preserved: %+v", got)
	}
	if got.Source != SourceAdmin || got.AddedAt.IsZero() {
		t.Errorf("source/addedAt not stamped: %+v", got)
	}

	// Reopen from disk: fields round-trip.
	s2, err := Open(s.Path())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	list := s2.List()
	if len(list) != 1 || list[0].Summary != "内部构建平台" || list[0].CLIBin != "bbclaw-buildhub" {
		t.Errorf("round-trip lost fields: %+v", list)
	}
}

func TestAddRejectsDelimiterNameAndDuplicates(t *testing.T) {
	s, dir := storeAt(t)
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Add(Project{Name: "bad,name", Path: a}); err == nil {
		t.Error("name with ',' should be rejected")
	}
	if _, err := s.Add(Project{Name: "proj", Path: a}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if _, err := s.Add(Project{Name: "proj", Path: b}); err == nil {
		t.Error("duplicate name should be rejected")
	}
	if _, err := s.Add(Project{Name: "other", Path: a}); err == nil {
		t.Error("duplicate path should be rejected")
	}
}

func TestAddUniqueNameDerivation(t *testing.T) {
	s, dir := storeAt(t)
	// Two different dirs with the same base name → second derives "<base>-2".
	d1 := filepath.Join(dir, "x", "proj")
	d2 := filepath.Join(dir, "y", "proj")
	for _, d := range []string{d1, d2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := s.Add(Project{Path: d1})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.Add(Project{Path: d2})
	if err != nil {
		t.Fatal(err)
	}
	if p1.Name != "proj" || p2.Name != "proj-2" {
		t.Errorf("name derivation = %q, %q; want proj, proj-2", p1.Name, p2.Name)
	}
}

func TestRemove(t *testing.T) {
	s, dir := storeAt(t)
	proj := filepath.Join(dir, "p")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Project{Name: "p", Path: proj}); err != nil {
		t.Fatal(err)
	}
	ok, err := s.Remove("p")
	if err != nil || !ok {
		t.Fatalf("Remove returned ok=%v err=%v", ok, err)
	}
	if len(s.List()) != 0 {
		t.Error("project not removed")
	}
	if ok, _ := s.Remove("nope"); ok {
		t.Error("removing a missing project should return false")
	}
}

func TestResolve(t *testing.T) {
	s, dir := storeAt(t)
	proj := filepath.Join(dir, "p")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cleaned := filepath.Clean(proj)
	if _, err := s.Add(Project{Name: "p", Path: proj}); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Resolve("p", ""); !ok || got != cleaned {
		t.Errorf("Resolve by name = %q,%v want %q,true", got, ok, cleaned)
	}
	if got, ok := s.Resolve("", cleaned); !ok || got != cleaned {
		t.Errorf("Resolve by path = %q,%v want %q,true", got, ok, cleaned)
	}
	if _, ok := s.Resolve("ghost", ""); ok {
		t.Error("Resolve of unknown name should be false")
	}
}
