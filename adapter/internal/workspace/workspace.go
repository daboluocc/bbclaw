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
	"log"
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

	// memoryDirName is the workspace sub-directory holding the butler's
	// dimensioned long-term memory files (ADR-022 / #90). It complements the
	// CLAUDE.md managed block: CLAUDE.md is loaded natively every turn, while
	// the MEMORY/ files are read on demand via the index in the persona.
	memoryDirName = "MEMORY"

	// ManagedBegin / ManagedEnd delimit the adapter-managed region inside
	// CLAUDE.md. Anything between them is owned by the adapter (long-term
	// memory); anything outside is the user's and is never touched.
	ManagedBegin = "<!-- BEGIN BBClaw-managed -->"
	ManagedEnd   = "<!-- END BBClaw-managed -->"
)

// MemoryDimension describes one dimensioned long-term memory file under
// workspace/MEMORY/. The set is anchored on ADR-022 (#90); it lives here as
// data so the dimensions stay cheap to revise and so the consolidation engine
// (sub-task B) can reuse the same definitions when writing.
type MemoryDimension struct {
	// File is the file name under MEMORY/ (e.g. "preferences.md").
	File string
	// Skeleton is the empty placeholder written when the file is missing.
	Skeleton string
}

// MemoryDimensions is the canonical set of long-term memory dimension files
// the butler maintains: user preferences, recent projects, and key decisions.
var MemoryDimensions = []MemoryDimension{
	{
		File: "preferences.md",
		Skeleton: "# 用户长期偏好\n\n" +
			"<!-- 由 BBClaw 自动维护：用户稳定的口味、习惯与默认选择。 -->\n",
	},
	{
		File: "projects.md",
		Skeleton: "# 最近在做的项目\n\n" +
			"<!-- 由 BBClaw 自动维护：用户近期关注的项目与进展线索。 -->\n",
	},
	{
		File: "decisions.md",
		Skeleton: "# 关键决策记录\n\n" +
			"<!-- 由 BBClaw 自动维护：影响后续工作的重要决策及其原因。 -->\n",
	},
}

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

// MemoryDir returns the workspace MEMORY/ directory (Dir()/MEMORY), where the
// dimensioned long-term memory files live.
func MemoryDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, memoryDirName), nil
}

// MemoryFilePath returns the path to a memory dimension file under MEMORY/
// (e.g. MemoryFilePath("preferences.md")). The consolidation engine (sub-task
// B) uses this to locate the file it writes into.
func MemoryFilePath(name string) (string, error) {
	dir, err := MemoryDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
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
		ensureMemoryScaffold(dir)
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
	ensureMemoryScaffold(dir)
	return dir, nil
}

// ensureMemoryScaffold creates the MEMORY/ directory and the dimension files
// under it when missing, writing an empty skeleton for each (0600 — memory
// content is more sensitive than the persona). It is idempotent: existing
// files are never clobbered. Per the butler scaffold contract, any failure
// here is logged and swallowed — it must never abort EnsureScaffold or block
// adapter startup, since the persona remains usable without the MEMORY files.
func ensureMemoryScaffold(workspaceDir string) {
	memDir := filepath.Join(workspaceDir, memoryDirName)
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		log.Printf("workspace: create MEMORY dir %s failed (non-fatal): %v", memDir, err)
		return
	}
	for _, dim := range MemoryDimensions {
		file := filepath.Join(memDir, dim.File)
		if _, err := os.Stat(file); err == nil {
			continue // existing file: never clobber
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Printf("workspace: stat MEMORY file %s failed (non-fatal): %v", file, err)
			continue
		}
		if err := os.WriteFile(file, []byte(dim.Skeleton), 0o600); err != nil {
			log.Printf("workspace: write MEMORY file %s failed (non-fatal): %v", file, err)
		}
	}
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
