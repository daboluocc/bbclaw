package memory

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// envEnable gates the whole long-term-memory write pipeline. Default ON
	// so users get long-term memory out of the box. Set to 0/false/no/off to
	// disable. Wired on BOTH the local-ingress and cloud-relay butler engines
	// (ADR-021 §4) — one home adapter == one home, so a single Writer is correct;
	// the multi-tenant per-user scoping concern lives in the cloud backend.
	envEnable = "BBCLAW_BUTLER_MEMORY_DISTILL"
	// envModel overrides the distillation model; default is the cheapest Haiku.
	envModel = "BBCLAW_BUTLER_MEMORY_MODEL"
	// envClaudeBin overrides the claude binary path used for distillation.
	envClaudeBin = "BBCLAW_BUTLER_MEMORY_CLAUDE_BIN"

	// envConsolidate gates the second-layer consolidation engine (ADR-022).
	// Default OFF: kept disabled until two defects are fixed (2026-06-10) —
	// (1) the consolidator writes singular orphan files MEMORY/{preference,
	// project,decision}.md that nothing reads (scaffold/persona/admin/prewarm all
	// use the plural names), and (2) its full-file overwrite of the project
	// dimension would clobber prewarm's <!-- prewarm:NAME --> blocks in
	// projects.md. Until then the distilled inbox in CLAUDE.md's managed block
	// (auto-reloaded by claude-code at cwd=workspace) already serves as the
	// butler's background long-term memory, so enabling consolidation now would
	// only DRAIN that working inbox into dead files. Requires envEnable on too.
	// Wired wherever the distill pipeline is (local + cloud-relay).
	envConsolidate          = "BBCLAW_BUTLER_MEMORY_CONSOLIDATE"
	envConsolidateThreshold = "BBCLAW_BUTLER_MEMORY_CONSOLIDATE_THRESHOLD"
	envConsolidateIdle      = "BBCLAW_BUTLER_MEMORY_CONSOLIDATE_IDLE"
	envConsolidateMaxGap    = "BBCLAW_BUTLER_MEMORY_CONSOLIDATE_MAXGAP"
	envConsolidateCooldown  = "BBCLAW_BUTLER_MEMORY_CONSOLIDATE_COOLDOWN"
	envConsolidateMaxPerDim = "BBCLAW_BUTLER_MEMORY_CONSOLIDATE_MAXPERDIM"

	// defaultModel is the cheap model used by both distill and consolidate. It
	// MUST be a model id the operator's claude auth can actually select: under a
	// Pro/Max subscription login (OAuth, not an API key) the CLI's --model only
	// accepts subscription-available ids, and the old "claude-3-5-haiku-latest"
	// API alias is now rejected (3.5 Haiku retired) — every distill exited 1,
	// leaving the inbox empty. "claude-haiku-4-5" is the current cheap Haiku and
	// resolves under subscription auth. Override via BBCLAW_BUTLER_MEMORY_MODEL.
	defaultModel = "claude-haiku-4-5"

	// Consolidation trigger defaults (ADR-022 §1). Conservative for v1 灰度.
	defaultThresholdRatio = 0.75
	defaultIdleGap        = 5 * time.Minute
	defaultMaxGap         = 6 * time.Hour
	defaultCooldown       = 10 * time.Minute
)

// Enabled reports whether the memory write pipeline is switched on via env.
// Accepts 1/true/yes/on (case-insensitive); everything else (incl. unset) = on.
// Default is ON so users get the long-term memory feature out of the box.
func Enabled() bool {
	return parseBoolEnv(envEnable, true)
}

// ConsolidateEnabled reports whether the second-layer consolidation engine is
// switched on via env. Default OFF (ADR-022 §5): see envConsolidate for the two
// defects that must land before it can safely archive the inbox into MEMORY/*.md.
func ConsolidateEnabled() bool {
	return parseBoolEnv(envConsolidate, false)
}

// parseBoolEnv reads a boolean env var, returning def when unset/blank and
// accepting 1/true/yes/on (case-insensitive) as true, everything else false.
func parseBoolEnv(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	switch strings.ToLower(v) {
	case "yes", "on":
		return true
	default:
		return false
	}
}

// NewFromEnv builds a Writer for the given CLAUDE.md path using a Haiku claude -p
// distiller configured from env. It returns (nil, false) when the pipeline is
// disabled (the default) so callers leave Deps.Memory nil and the engine skips
// the whole step. claudeBin is the operator-resolved binary path; an env
// override (BBCLAW_BUTLER_MEMORY_CLAUDE_BIN) takes precedence when set.
//
// When BBCLAW_BUTLER_MEMORY_CONSOLIDATE is on, a Consolidator is attached to the
// same single worker so the inbox is periodically archived into MEMORY/*.md
// (ADR-022). Consolidation is disabled by default.
func NewFromEnv(claudeMDPath, claudeBin string, log Logger) (*Writer, bool) {
	if bin := strings.TrimSpace(os.Getenv(envClaudeBin)); bin != "" {
		claudeBin = bin
	}
	model := strings.TrimSpace(os.Getenv(envModel))
	if model == "" {
		model = defaultModel
	}
	return NewWithRunner(claudeMDPath, ClaudePromptRunner(claudeBin, model), log)
}

// ClaudePromptRunnerFromEnv builds the default claude `-p` memory runner with
// the env-configured cheap model (preserving the Haiku cost optimisation). Used
// by main.go as the claude-driver branch of the driver-aware runner.
func ClaudePromptRunnerFromEnv(claudeBin string) PromptRunner {
	if bin := strings.TrimSpace(os.Getenv(envClaudeBin)); bin != "" {
		claudeBin = bin
	}
	model := strings.TrimSpace(os.Getenv(envModel))
	if model == "" {
		model = defaultModel
	}
	return ClaudePromptRunner(claudeBin, model)
}

// NewWithRunner builds the memory Writer using an injected PromptRunner
// (ADR-024 §6) so the distill/consolidate step follows the active driver rather
// than always shelling to claude. Returns (nil,false) when the pipeline is
// disabled (the default). NewFromEnv is the claude-`-p` convenience wrapper.
func NewWithRunner(claudeMDPath string, run PromptRunner, log Logger) (*Writer, bool) {
	if !Enabled() {
		return nil, false
	}
	store := NewStore(claudeMDPath)
	distiller := newRunnerDistiller(run)

	w := newWriter(store, distiller, log)

	if ConsolidateEnabled() {
		maxPerDim := intEnv(envConsolidateMaxPerDim, defaultMaxPerDim)
		summarizer := newRunnerSummarizer(run, maxPerDim)
		cons := NewConsolidator(claudeMDPath, summarizer, log)
		cons.maxPerDim = maxPerDim
		w.consolidator = cons
		w.trig = triggerConfig{
			maxBytes:       store.maxBytes,
			thresholdRatio: floatEnv(envConsolidateThreshold, defaultThresholdRatio),
			idleGap:        durationEnv(envConsolidateIdle, defaultIdleGap),
			maxGap:         durationEnv(envConsolidateMaxGap, defaultMaxGap),
			cooldown:       durationEnv(envConsolidateCooldown, defaultCooldown),
		}
		if log != nil {
			log.Infof("butler-memory: consolidation enabled (threshold=%.2f idle=%s maxGap=%s cooldown=%s)",
				w.trig.thresholdRatio, w.trig.idleGap, w.trig.maxGap, w.trig.cooldown)
		}
	}

	w.start()
	return w, true
}

// intEnv reads a non-negative int env var, falling back to def on unset/invalid.
func intEnv(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return def
}

// floatEnv reads a positive float env var, falling back to def on unset/invalid.
func floatEnv(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
		return f
	}
	return def
}

// durationEnv reads a Go duration env var (e.g. "5m", "6h"), falling back to def
// on unset/invalid.
func durationEnv(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return def
}
