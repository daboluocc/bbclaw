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

// dimensions are the canonical memory buckets (ADR-021 §4 / ADR-022 §2). Each
// becomes one MEMORY/<dim>.md profile file. Order is stable so consolidation is
// deterministic.
var dimensions = []string{"preference", "project", "decision"}

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
		bullets := c.cleanAndClamp(merged[dim])
		body := renderProfile(dim, bullets)
		if err := atomicWrite(c.profilePath(dim), []byte(body), 0o600); err != nil {
			return fmt.Errorf("memory: consolidate write %s: %w", dim, err)
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

// readProfiles loads the existing MEMORY/<dim>.md bodies into a dimension-keyed
// map. Missing files are simply absent (first run). Read errors other than
// not-exist are returned so a flaky disk doesn't silently lose history.
func (c *Consolidator) readProfiles() (map[string]string, error) {
	out := make(map[string]string, len(dimensions))
	for _, dim := range dimensions {
		raw, err := os.ReadFile(c.profilePath(dim))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("memory: read profile %s: %w", dim, err)
		}
		out[dim] = string(raw)
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

func (c *Consolidator) profilePath(dim string) string {
	return filepath.Join(c.memoryDir, dim+".md")
}

// renderProfile renders a dimension's bullets as a stable MEMORY/<dim>.md body.
// Bullets are sorted so an unchanged set produces a byte-identical file
// (idempotent re-consolidation).
func renderProfile(dim string, bullets []string) string {
	sorted := append([]string(nil), bullets...)
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(dim)
	b.WriteString("\n\n")
	for _, line := range sorted {
		b.WriteString("- ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
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
