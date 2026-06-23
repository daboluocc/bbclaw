package extract

import "strings"

// toolstep.go scrapes claude's tool-invocation bullets ("⏺ Name(args)") from the
// visible grid so the device can show DISPLAY-ONLY progress (ADR-030): a dimmed
// "Bash: …" chip while a turn runs. Tool steps are NEVER spoken — TTS is driven
// solely by the prose reply. The detection rule is deliberately strict so a normal
// prose reply ("⏺ The answer is …", "⏺ 你好") is never mistaken for a tool step
// (which would also wrongly drop it from the spoken reply — see extract.go's
// extractMarkerBlock guard).

// ToolStep is one claude tool invocation: Name is the tool ("Bash"/"Edit"/…),
// Hint the short argument preview (the parenthesised content, truncated).
type ToolStep struct {
	Name string
	Hint string
}

// toolHintMax bounds the parenthesised hint (rune-safe) so a long command can't
// bloat the device chip or a cloud frame.
const toolHintMax = 80

// isToolStep reports whether a raw grid line is a tool-step bullet.
func isToolStep(rawLine string) bool {
	_, ok := parseToolStep(rawLine)
	return ok
}

// parseToolStep extracts {Name, Hint} from a "⏺ Name(args)" line, or ok=false.
// The discriminator is a TitleCase ASCII identifier IMMEDIATELY followed by "("
// (no space): "⏺ Bash(curl …)" → {Bash, "curl …"}; "⏺ The capital is Paris."
// fails (space after "The", and no "("); "⏺ 你好" fails (CJK, no "("); "⏺ [Opus
// …]" fails ("[" not A-Z). Hint runs from the first "(" to the last ")", or to
// end-of-line when the closing ")" scrolled off the 80-col grid (we do NOT require
// it). A blank hint is rejected.
func parseToolStep(rawLine string) (ToolStep, bool) {
	t := strings.TrimLeft(rawLine, " ")
	if !strings.HasPrefix(t, replyMarker) {
		return ToolStep{}, false
	}
	body := stripReplyMarker(strings.TrimRight(t, " \t"))
	open := strings.IndexByte(body, '(')
	if open <= 0 { // no "(", or "(" at the very start (e.g. "(earlier) …")
		return ToolStep{}, false
	}
	name := body[:open]
	if !isToolName(name) {
		return ToolStep{}, false
	}
	rest := body[open+1:]
	hint := rest
	if c := strings.LastIndexByte(rest, ')'); c >= 0 {
		hint = rest[:c]
	}
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return ToolStep{}, false
	}
	return ToolStep{Name: name, Hint: truncateRunes(hint, toolHintMax)}, true
}

// isToolName reports whether s is a TitleCase ASCII tool identifier: A-Z then
// zero+ ASCII letters/digits, nothing else. Strict on purpose so prose openers
// ("The", "I'll") and chrome never match. MCP tools (mcp__foo__bar) intentionally
// don't match yet — they'd simply not show, never mis-speak.
func isToolName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if r < 'A' || r > 'Z' {
				return false
			}
			continue
		}
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// ScanToolSteps returns every tool step on the visible grid, in screen order.
func ScanToolSteps(visible string) []ToolStep {
	if visible == "" {
		return nil
	}
	var out []ToolStep
	for _, line := range strings.Split(visible, "\n") {
		if st, ok := parseToolStep(line); ok {
			out = append(out, st)
		}
	}
	return out
}

// truncateRunes caps s to n runes (rune-safe — never splits a multibyte rune),
// appending an ellipsis when it truncates.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
