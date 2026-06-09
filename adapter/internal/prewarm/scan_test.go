package prewarm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordSeedsProjectsMD(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Title\n\nA tiny tool that does one thing well.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "projects.md")
	if err := Record("proj", repo, out); err != nil {
		t.Fatalf("Record: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, want := range []string{
		"# 最近在做的项目", // header preserved
		marker("proj"),
		"## proj",
		"Go",                    // stack detected
		"A tiny tool that does", // README lifted
		"- 路径：" + repo,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("projects.md missing %q\n---\n%s", want, got)
		}
	}
}

func TestRecordIsIdempotentUpsert(t *testing.T) {
	repo := t.TempDir()
	out := filepath.Join(t.TempDir(), "projects.md")

	if err := Record("proj", repo, out); err != nil {
		t.Fatal(err)
	}
	if err := Record("proj", repo, out); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(out)
	if n := strings.Count(string(body), marker("proj")); n != 1 {
		t.Fatalf("expected exactly 1 section for proj after re-add, got %d\n%s", n, body)
	}
}

func TestRecordPreservesOtherSectionsAndUserHeader(t *testing.T) {
	repo := t.TempDir()
	out := filepath.Join(t.TempDir(), "projects.md")

	if err := Record("alpha", repo, out); err != nil {
		t.Fatal(err)
	}
	if err := Record("beta", repo, out); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(out)
	got := string(body)
	if !strings.Contains(got, marker("alpha")) || !strings.Contains(got, marker("beta")) {
		t.Fatalf("both sections must survive:\n%s", got)
	}
	// alpha sorts before beta (deterministic order).
	if strings.Index(got, marker("alpha")) > strings.Index(got, marker("beta")) {
		t.Errorf("sections not in deterministic sorted order:\n%s", got)
	}
}
