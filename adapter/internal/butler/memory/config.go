package memory

import (
	"os"
	"strconv"
	"strings"
)

const (
	// envEnable gates the whole long-term-memory write pipeline. Default ON
	// so users get long-term memory out of the box. Set to 0/false/no/off to
	// disable. LOCAL-only — cloud multi-tenant v1 never wires the Writer
	// (per-user scoping lands later, ADR-021 §4).
	envEnable = "BBCLAW_BUTLER_MEMORY_DISTILL"
	// envModel overrides the distillation model; default is the cheapest Haiku.
	envModel = "BBCLAW_BUTLER_MEMORY_MODEL"
	// envClaudeBin overrides the claude binary path used for distillation.
	envClaudeBin = "BBCLAW_BUTLER_MEMORY_CLAUDE_BIN"

	defaultModel = "claude-3-5-haiku-latest"
)

// Enabled reports whether the memory write pipeline is switched on via env.
// Accepts 1/true/yes/on (case-insensitive); everything else (incl. unset) = on.
// Default is ON so users get the long-term memory feature out of the box.
func Enabled() bool {
	v := strings.TrimSpace(os.Getenv(envEnable))
	if v == "" {
		return true  // default ON
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
func NewFromEnv(claudeMDPath, claudeBin string, log Logger) (*Writer, bool) {
	if !Enabled() {
		return nil, false
	}
	if bin := strings.TrimSpace(os.Getenv(envClaudeBin)); bin != "" {
		claudeBin = bin
	}
	model := strings.TrimSpace(os.Getenv(envModel))
	if model == "" {
		model = defaultModel
	}
	store := NewStore(claudeMDPath)
	distiller := newClaudeDistiller(claudeBin, model)
	return NewWriter(store, distiller, log), true
}
