// Package memory implements the butler's long-term memory pipeline (ADR-021
// §4): at the end of a butler turn the engine hands the turn's user/reply text
// to a Writer, a single background worker distills it into a few durable notes
// (user preferences / current projects / key decisions), and the Store appends
// them — deduped, size-clamped, poison-filtered — into the managed marker block
// of the workspace CLAUDE.md so a `claude -p --resume` (cwd=workspace) butler
// natively reloads them across restarts and devices.
//
// This package owns the *persistence + safety* logic (Store/filter); the LLM
// distillation that produces notes lives behind the Distiller interface and is
// gated off by default (BBCLAW_BUTLER_MEMORY_DISTILL). Cloud multi-tenant runs
// do not wire the Writer at all in v1 (per-user scoping lands later).
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/workspace"
)

// DefaultMaxBytes caps the managed block's inner text (ADR-021 §4: 总长上限).
// When appending would exceed it, the oldest notes are evicted FIFO so the
// block never grows unbounded and bloats every butler turn's loaded context.
const DefaultMaxBytes = 4096

// Item is a single distilled memory note. Category is one of the three buckets
// from ADR-021 §4 (preference / project / decision); Text is the speakable
// one-liner persisted into the managed block.
type Item struct {
	Category string
	Text     string
}

// formatLine renders an item as one managed-block bullet line. The full line
// (category + text) is the dedup key, so the same note never appends twice.
func formatLine(it Item) string {
	cat := strings.TrimSpace(it.Category)
	text := strings.TrimSpace(it.Text)
	if cat == "" {
		return "- " + text
	}
	return "- [" + cat + "] " + text
}

// Store appends distilled notes into the managed block of a CLAUDE.md file.
// It is safe for the single memory worker; it is not designed for concurrent
// writers (the pipeline runs concurrency=1, ADR-021 §3).
type Store struct {
	path     string
	maxBytes int
}

// NewStore targets the given CLAUDE.md path (typically workspace.ClaudeMDPath()).
func NewStore(claudeMDPath string) *Store {
	return &Store{path: claudeMDPath, maxBytes: DefaultMaxBytes}
}

// Append merges items into the managed block: notes already present are skipped
// (hash/content dedup → idempotent), fresh notes are appended, and the block is
// clamped to maxBytes by evicting the oldest notes (FIFO). Content outside the
// BEGIN/END BBClaw-managed markers is never touched. The write is atomic (temp
// + rename) with 0600 perms. A no-op merge (nothing new) skips the write so
// re-running the same turn leaves the file byte-identical.
func (s *Store) Append(items []Item) error {
	items = sanitize(items)
	if len(items) == 0 {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("memory: read %s: %w", s.path, err)
	}
	content := string(raw)
	existing, _ := workspace.ManagedBlock(content)

	merged := mergeManaged(existing, items, s.maxBytes)
	if merged == existing {
		// Idempotent: every note already present (or all clamped out). No write.
		return nil
	}

	updated := workspace.ReplaceManagedBlock(content, merged)
	return atomicWrite(s.path, []byte(updated), 0o600)
}

// sanitize drops blank and poisoned items before they ever reach disk. Poison
// filtering here is the last line of defence (ADR-020 §2 / ADR-021 §4): a
// distiller bug or prompt-injected note must not get persisted and then
// reloaded as butler context on the next turn.
func sanitize(items []Item) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		if strings.TrimSpace(it.Text) == "" {
			continue
		}
		if IsPoisoned(it.Text) || IsPoisoned(it.Category) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// mergeManaged is the pure core: given the existing managed inner text and new
// items, it returns the new inner text. Dedup is by exact bullet line (the
// content hash), order is append (oldest first), and the result is clamped to
// maxBytes via FIFO eviction of the oldest lines. Returned text carries no
// leading/trailing blank lines so it round-trips through ManagedBlock cleanly.
func mergeManaged(existing string, items []Item, maxBytes int) string {
	var lines []string
	seen := make(map[string]struct{})

	add := func(line string) {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			return
		}
		if _, dup := seen[line]; dup {
			return
		}
		seen[line] = struct{}{}
		lines = append(lines, line)
	}

	for _, ln := range strings.Split(strings.Trim(existing, "\n"), "\n") {
		add(ln)
	}
	for _, it := range items {
		add(formatLine(it))
	}

	// FIFO clamp: drop oldest lines until the joined block fits maxBytes.
	for len(lines) > 1 && len(strings.Join(lines, "\n")) > maxBytes {
		lines = lines[1:]
	}

	return strings.Join(lines, "\n")
}

// atomicWrite writes data to path via a temp file + rename in the same dir.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".claude-md-*.tmp")
	if err != nil {
		return fmt.Errorf("memory: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("memory: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("memory: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("memory: close temp: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("memory: chmod temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("memory: rename temp to %s: %w", path, err)
	}
	return nil
}
