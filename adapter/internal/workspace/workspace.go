// Package workspace owns the BBClaw butler's home directory —
// ~/.bbclaw-adapter/workspace/ — and the CLAUDE.md inside it (ADR-021 §4).
//
// That CLAUDE.md is the butler's persona (an orchestrator that dispatches
// coding tasks to worker agents, constrained by the walkie-talkie form
// factor) plus its long-term memory. When the butler session runs with
// cwd=workspace, Claude loads this CLAUDE.md natively (the ADR-021 spike
// confirmed cwd-implicit loading, not --add-dir).
//
// The package centralises the workspace path constants so the butler router
// (#80) and the memory pipeline can reuse them instead of re-deriving the
// data directory. It also owns the managed-marker block helpers: a
// BEGIN/END region the adapter may rewrite for long-term memory while
// leaving everything the user hand-wrote untouched.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// dataDirEnv overrides the default data directory. Mirrors the resolution
	// used by driverstate / logicalsession so all adapter state lives together.
	dataDirEnv     = "BBCLAW_DATA_DIR"
	defaultDataDir = ".bbclaw-adapter"
	workspaceName  = "workspace"
	claudeMDName   = "CLAUDE.md"

	// ManagedBegin / ManagedEnd delimit the adapter-managed region inside
	// CLAUDE.md. Anything between them is owned by the adapter (long-term
	// memory); anything outside is the user's and is never touched.
	ManagedBegin = "<!-- BEGIN BBClaw-managed -->"
	ManagedEnd   = "<!-- END BBClaw-managed -->"
)

// DataDir resolves the adapter data directory: $BBCLAW_DATA_DIR when set,
// otherwise ~/.bbclaw-adapter. Shared with driverstate and logicalsession.
func DataDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv(dataDirEnv)); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, defaultDataDir), nil
}

// Dir returns the butler workspace directory (DataDir()/workspace).
func Dir() (string, error) {
	dataDir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, workspaceName), nil
}

// ClaudeMDPath returns the path to the workspace CLAUDE.md.
func ClaudeMDPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, claudeMDName), nil
}

// EnsureScaffold guarantees the workspace directory exists and that its
// CLAUDE.md is usable. It is idempotent and conservative:
//
//   - missing directory      → created (0755)
//   - missing CLAUDE.md       → factory default written (0644)
//   - existing CLAUDE.md      → never clobbered; only a managed marker block
//     is appended when none is present, so the memory pipeline has an anchor.
//
// It returns the workspace directory path on success. Callers should treat a
// non-nil error as non-fatal (log + degrade), matching the driverstate /
// logicalsession "run without persistence" style.
func EnsureScaffold() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create workspace dir %s: %w", dir, err)
	}

	path := filepath.Join(dir, claudeMDName)
	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.WriteFile(path, []byte(DefaultClaudeMD), 0o644); err != nil {
			return "", fmt.Errorf("write default CLAUDE.md %s: %w", path, err)
		}
		return dir, nil
	case err != nil:
		return "", fmt.Errorf("read CLAUDE.md %s: %w", path, err)
	}

	// CLAUDE.md exists: respect the user's content entirely. Only ensure a
	// managed block exists so memory appends have a home.
	if !HasManagedBlock(string(existing)) {
		updated := ensureTrailingNewline(string(existing)) + "\n" + emptyManagedBlock() + "\n"
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			return "", fmt.Errorf("append managed block to %s: %w", path, err)
		}
	}
	return dir, nil
}

// HasManagedBlock reports whether content contains a well-formed managed block.
func HasManagedBlock(content string) bool {
	_, _, ok := findManagedBlock(content)
	return ok
}

// ManagedBlock returns the inner text between the managed markers (trimmed of
// the surrounding newlines), and whether a block was found.
func ManagedBlock(content string) (inner string, ok bool) {
	start, end, found := findManagedBlock(content)
	if !found {
		return "", false
	}
	inner = content[start+len(ManagedBegin) : end-len(ManagedEnd)]
	return strings.Trim(inner, "\n"), true
}

// ReplaceManagedBlock returns content with the managed block's inner text set
// to inner. If no managed block exists yet, a fresh one is appended at the end.
// This is the entry point the memory pipeline uses to persist long-term notes.
func ReplaceManagedBlock(content, inner string) string {
	block := ManagedBegin + "\n" + inner + "\n" + ManagedEnd
	start, end, ok := findManagedBlock(content)
	if !ok {
		return ensureTrailingNewline(content) + "\n" + block + "\n"
	}
	return content[:start] + block + content[end:]
}

// findManagedBlock locates the managed block, returning the index of
// ManagedBegin and the index just past ManagedEnd. ok is false when either
// marker is missing or they appear out of order.
func findManagedBlock(content string) (start, end int, ok bool) {
	b := strings.Index(content, ManagedBegin)
	if b < 0 {
		return 0, 0, false
	}
	rel := strings.Index(content[b+len(ManagedBegin):], ManagedEnd)
	if rel < 0 {
		return 0, 0, false
	}
	return b, b + len(ManagedBegin) + rel + len(ManagedEnd), true
}

func emptyManagedBlock() string {
	return ManagedBegin + "\n" + ManagedEnd
}

func ensureTrailingNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
