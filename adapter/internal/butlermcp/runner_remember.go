package butlermcp

import (
	"fmt"
	"os"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/butler/memory"
	"github.com/daboluocc/bbclaw/adapter/internal/workspace"
)

// validCategories is the closed set of memory categories the `remember` tool
// accepts. Each entry maps to a MEMORY/<file>.md on disk (plural, matching
// the workspace scaffold and persona). The butler addresses them by file name
// so AI-authored notes and admin UI see the same files.
var validCategories = map[string]string{
	"profile":     "profile.md",
	"preferences": "preferences.md",
	"projects":    "projects.md",
	"decisions":   "decisions.md",
}

// MemoryWriter is the narrow interface the remember tool uses to locate and
// update MEMORY/*.md files. It is satisfied by a workspace-backed implementation
// (memoryFileWriter) and by test stubs.
type MemoryWriter interface {
	// WriteMemory writes text into the managed sub-block of the given
	// MEMORY/<file>.md, preserving any content outside the block. category is
	// one of profile / preferences / projects / decisions; text is the
	// pre-sanitized content to store.
	WriteMemory(category, text string) error
}

// workspaceMemoryWriter is the production MemoryWriter. It resolves paths via
// workspace.MemoryFilePath, applies IsPoisoned, and writes via
// workspace.ReplaceManagedBlock + atomic write — identical strategy to the
// consolidate path.
type workspaceMemoryWriter struct{}

// NewWorkspaceMemoryWriter returns the production MemoryWriter backed by the
// workspace MEMORY/ directory.
func NewWorkspaceMemoryWriter() MemoryWriter { return workspaceMemoryWriter{} }

func (workspaceMemoryWriter) WriteMemory(category, text string) error {
	file, ok := validCategories[category]
	if !ok {
		return fmt.Errorf("remember: unknown category %q", category)
	}
	// Poison guard — same defence as Store.Append and Consolidator.cleanAndClamp.
	if memory.IsPoisoned(text) || memory.IsPoisoned(category) {
		return fmt.Errorf("remember: poisoned content rejected")
	}

	path, err := workspace.MemoryFilePath(file)
	if err != nil {
		return fmt.Errorf("remember: resolve path: %w", err)
	}
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return fmt.Errorf("remember: mkdir: %w", err)
	}

	// Read existing content; empty string on first write.
	var existing string
	if raw, err := os.ReadFile(path); err == nil {
		existing = string(raw)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("remember: read %s: %w", path, err)
	}

	// Append text as a new bullet inside the managed block. We reuse the same
	// workspace.ReplaceManagedBlock round-trip as the consolidation engine: read
	// the current inner text, append the new bullet, write back. Content outside
	// the managed block (prewarm markers, hand-written notes) is preserved.
	inner, _ := workspace.ManagedBlock(existing)
	inner = strings.TrimSpace(inner)

	// Build new bullet line (normalise to "- text").
	bullet := strings.TrimSpace(text)
	if !strings.HasPrefix(bullet, "- ") {
		bullet = "- " + bullet
	}

	// Dedup: skip if the exact bullet is already present.
	for _, ln := range strings.Split(inner, "\n") {
		if strings.TrimSpace(ln) == strings.TrimSpace(bullet) {
			return nil // idempotent
		}
	}

	var newInner string
	if inner == "" {
		newInner = bullet
	} else {
		newInner = inner + "\n" + bullet
	}

	updated := workspace.ReplaceManagedBlock(existing, newInner)

	// atomicWrite is unexported in the memory package; mirror it inline for the
	// MCP layer (same temp+rename approach, same 0600 perm).
	return atomicWriteFile(path, []byte(updated), 0o600)
}

// atomicWriteFile writes data to path via a temp file + rename in the same
// directory. Mirrors memory.atomicWrite; duplicated here to avoid an exported
// dep on the memory package's internal helper.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := dirOf(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("remember: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".remember-*.tmp")
	if err != nil {
		return fmt.Errorf("remember: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("remember: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("remember: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("remember: close temp: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("remember: chmod temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("remember: rename to %s: %w", path, err)
	}
	return nil
}

// dirOf returns the directory component of path (filepath.Dir equivalent
// without importing path/filepath to keep the import list short — we already
// import strings).
func dirOf(path string) string {
	idx := strings.LastIndexByte(path, '/')
	if idx < 0 {
		idx = strings.LastIndexByte(path, '\\')
	}
	if idx < 0 {
		return "."
	}
	return path[:idx]
}
