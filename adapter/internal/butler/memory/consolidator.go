package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/workspace"
)

// dimension describes one memory bucket: Key is the logical name used in the
// summarizer prompt (unchanged from v1); File is the plural filename that
// matches the workspace scaffold, persona, admin whitelist, and prewarm blocks
// (all use preferences.md / projects.md / decisions.md — #124).
type dimension struct {
	Key  string // summarizer/prompt key (singular, matches ADR-022 §2 contract)
	File string // MEMORY/<File> on disk (plural, matches workspace.MemoryDimensions)
}

// dimensions are the canonical memory buckets (ADR-021 §4 / ADR-022 §2). Order
// is stable so consolidation is deterministic. Files use plural names to match
// the workspace scaffold (#124 Task 2: fix orphan singular file names).
var dimensions = []dimension{
	{Key: "preference", File: "preferences.md"},
	{Key: "project", File: "projects.md"},
	{Key: "decision", File: "decisions.md"},
}

// memoryDirName is the consolidation profile directory, co-located next to the
// workspace CLAUDE.md (ADR-022 §4: defensively self-built, decoupled from #91).
const memoryDirName = "MEMORY"

// defaultMaxPerDim caps how many bullets a single dimension profile may hold
// after consolidation (ADR-022 §3 每维度上限). The LLM trims first; this is the
// last-line hard clamp.
const defaultMaxPerDim = 30

// Summarizer re-classifies the inbox (per-turn distilled notes) plus the
// existing per-dimension profiles into a merged, deduped, override-applied set
// of bullets per dimension. Implementations may call an LLM (claudeSummarizer)
// or be a pure stub in tests. It runs only on the single background worker.
type Summarizer interface {
	// Summarize takes the raw inbox managed-block text and a map of existing
	// dimension -> profile body, and returns dimension -> merged bullet lines.
	Summarize(ctx context.Context, inbox string, existing map[string]string) (map[string][]string, error)
}

// Consolidator implements the second-layer "沉淀" engine (ADR-022): it archives
// the CLAUDE.md inbox into MEMORY/*.md multi-dimension profiles and then clears
// the inbox — but only after every profile write succeeds. All failures are the
// caller's to swallow; Consolidate never clears on a failed run.
type Consolidator struct {
	claudeMDPath string
	memoryDir    string
	summarizer   Summarizer
	maxPerDim    int
	log          Logger
}

// NewConsolidator targets the workspace CLAUDE.md (inbox) and derives the
// MEMORY/ profile dir next to it. summarizer must be non-nil; log may be nil.
func NewConsolidator(claudeMDPath string, summarizer Summarizer, log Logger) *Consolidator {
	return &Consolidator{
		claudeMDPath: claudeMDPath,
		memoryDir:    filepath.Join(filepath.Dir(claudeMDPath), memoryDirName),
		summarizer:   summarizer,
		maxPerDim:    defaultMaxPerDim,
		log:          log,
	}
}

// inboxBytes returns the current size (in bytes) of the inbox managed block and
// whether it holds any content. Used by the trigger decision (ADR-022 §1). A
// read error reports (0, false) so triggers degrade to "nothing to do".
func (c *Consolidator) inboxBytes() (int, bool) {
	raw, err := os.ReadFile(c.claudeMDPath)
	if err != nil {
		return 0, false
	}
	inner, ok := workspace.ManagedBlock(string(raw))
	if !ok {
		return 0, false
	}
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return 0, false
	}
	return len(inner), true
}

// Consolidate runs one archive pass: read inbox snapshot + existing profiles →
// summarize → poison double-filter → per-dimension clamp → atomic 0600 write
// each MEMORY/<dim>.md → only on full success, clear the consumed inbox lines.
// Returns nil when the inbox is empty (idempotent no-op). Any error leaves the
// inbox untouched (ADR-022 §4 时序).
func (c *Consolidator) Consolidate(ctx context.Context) error {
	raw, err := os.ReadFile(c.claudeMDPath)
	if err != nil {
		return fmt.Errorf("memory: consolidate read %s: %w", c.claudeMDPath, err)
	}
	content := string(raw)
	inbox, ok := workspace.ManagedBlock(content)
	if !ok {
		return nil
	}
	snapshot := strings.Trim(inbox, "\n")
	if strings.TrimSpace(snapshot) == "" {
		return nil // nothing to consolidate
	}

	existing, err := c.readProfiles()
	if err != nil {
		return err
	}

	merged, err := c.summarizer.Summarize(ctx, snapshot, existing)
	if err != nil {
		return fmt.Errorf("memory: consolidate summarize: %w", err)
	}

	if err := os.MkdirAll(c.memoryDir, 0o700); err != nil {
		return fmt.Errorf("memory: consolidate mkdir %s: %w", c.memoryDir, err)
	}

	// Write every dimension profile before clearing the inbox. A failed write
	// aborts the whole pass with the inbox intact (the next pass re-derives from
	// the still-present inbox + whatever profiles did land — convergent).
	for _, dim := range dimensions {
		bullets := c.cleanAndClamp(merged[dim.Key])
		if err := c.writeDimProfile(dim, bullets); err != nil {
			return fmt.Errorf("memory: consolidate write %s: %w", dim.Key, err)
		}
	}

	// Success: clear only the snapshot lines from the inbox, preserving any
	// notes that arrived after the snapshot was read (read-clear vs append
	// race, ADR-022 §4).
	if err := c.clearInbox(snapshot); err != nil {
		return fmt.Errorf("memory: consolidate clear inbox: %w", err)
	}
	if c.log != nil {
		c.log.Infof("butler-memory: consolidated inbox into %d profile(s)", len(dimensions))
	}
	return nil
}

// readProfiles loads the existing MEMORY/<File> bodies into a dimension Key-
// keyed map. Missing files are simply absent (first run). Read errors other
// than not-exist are returned so a flaky disk doesn't silently lose history.
// The returned map is keyed by dim.Key (singular) so the Summarizer prompt
// contract is unchanged; only the on-disk filename is now plural (#124).
func (c *Consolidator) readProfiles() (map[string]string, error) {
	out := make(map[string]string, len(dimensions))
	for _, dim := range dimensions {
		raw, err := os.ReadFile(c.profilePath(dim))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("memory: read profile %s: %w", dim.Key, err)
		}
		out[dim.Key] = string(raw)
	}
	return out, nil
}

// cleanAndClamp applies the poison double-filter (ADR-020 §2), drops blanks /
// dups, and hard-caps the dimension to maxPerDim bullets.
func (c *Consolidator) cleanAndClamp(bullets []string) []string {
	limit := c.maxPerDim
	if limit <= 0 {
		limit = defaultMaxPerDim
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(bullets))
	for _, b := range bullets {
		b = strings.TrimSpace(b)
		b = strings.TrimPrefix(b, "- ")
		b = strings.TrimSpace(b)
		if b == "" || IsPoisoned(b) {
			continue
		}
		if _, dup := seen[b]; dup {
			continue
		}
		seen[b] = struct{}{}
		out = append(out, b)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// clearInbox removes exactly the lines present in the snapshot from the current
// inbox managed block, leaving any later-appended notes in place. A no-op (no
// snapshot line still present) skips the write so the file stays byte-identical.
func (c *Consolidator) clearInbox(snapshot string) error {
	raw, err := os.ReadFile(c.claudeMDPath)
	if err != nil {
		return err
	}
	content := string(raw)
	current, ok := workspace.ManagedBlock(content)
	if !ok {
		return nil
	}

	consumed := make(map[string]struct{})
	for _, ln := range strings.Split(snapshot, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		consumed[ln] = struct{}{}
	}

	var kept []string
	for _, ln := range strings.Split(strings.Trim(current, "\n"), "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if _, was := consumed[ln]; was {
			continue // consolidated → drop
		}
		kept = append(kept, ln) // arrived after snapshot → keep
	}

	newInner := strings.Join(kept, "\n")
	if newInner == strings.Trim(current, "\n") {
		return nil // nothing removed
	}
	updated := workspace.ReplaceManagedBlock(content, newInner)
	return atomicWrite(c.claudeMDPath, []byte(updated), 0o600)
}

func (c *Consolidator) profilePath(dim dimension) string {
	return filepath.Join(c.memoryDir, dim.File)
}

// writeDimProfile merges bullets into the managed sub-block of dim's profile
// file, preserving any content outside the block (prewarm markers, AI-written
// content from the `remember` tool). On first run the file is created with a
// minimal skeleton followed by the managed block.
func (c *Consolidator) writeDimProfile(dim dimension, bullets []string) error {
	path := c.profilePath(dim)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	// Read existing content; start from empty string when the file doesn't exist yet.
	var existing string
	if raw, err := os.ReadFile(path); err == nil {
		existing = string(raw)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	inner := renderProfileInner(bullets)
	updated := workspace.ReplaceManagedBlock(existing, inner)
	return atomicWrite(path, []byte(updated), 0o600)
}

// renderProfileInner renders a dimension's bullets as the inner text of the
// managed sub-block (no surrounding markers). Bullets are sorted so an
// unchanged set produces a byte-identical block (idempotent re-consolidation).
func renderProfileInner(bullets []string) string {
	sorted := append([]string(nil), bullets...)
	sort.Strings(sorted)
	var b strings.Builder
	for _, line := range sorted {
		b.WriteString("- ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// parseDimensions extracts the first top-level JSON object from raw model output
// and decodes it into dimension -> bullets. Tolerates surrounding prose / code
// fences by slicing from the first '{' to the last '}' (same defensive strategy
// as parseItems; ADR-022 spike conclusion). Unknown keys are ignored.
func parseDimensions(raw string) (map[string][]string, error) {
	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start < 0 || end < 0 || end < start {
		return map[string][]string{}, nil // no object → nothing merged
	}
	var decoded map[string][]string
	if err := json.Unmarshal([]byte(raw[start:end+1]), &decoded); err != nil {
		return nil, fmt.Errorf("parse consolidate json: %w", err)
	}
	out := make(map[string][]string, len(decoded))
	for k, v := range decoded {
		out[strings.TrimSpace(k)] = v
	}
	return out, nil
}
