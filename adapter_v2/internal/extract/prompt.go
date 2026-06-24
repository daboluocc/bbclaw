package extract

import "strings"

// prompt.go recognises claude's BLOCKING interactive menus — the permission /
// tool-confirmation popups that park the TUI waiting for a human choice:
//
//	 Bash command
//	   cat /etc/hostname
//	 Do you want to proceed?
//	 ❯ 1. Yes
//	   2. Yes, allow reading from etc/ from this project
//	   3. No
//	 Esc to cancel · Tab to amend · ctrl+e to explain
//
// (verbatim from real claude 2.1.186, ADR-033 spike). When one is on screen the
// turn is NOT over — boundary.go must not mistake it for turn-end — and the
// adapter forwards {question, options} to the device and injects the chosen digit
// back (digit-submit confirmed: writing "1" selects+submits, no arrows/Enter).
//
// Detection is deliberately strict: it requires >=2 contiguous numbered option
// rows PLUS a corroborator that a prose numbered list can never have — claude's
// highlight pointer ("❯"/"›") on a row OR an "Esc to cancel" footer. Matching is
// by option LABEL, never by box geometry or fixed digit position: claude rewrites
// the wording/scope and the option COUNT varies (option 2 above is context-
// specific), so the parser degrades gracefully across UI tweaks. On any doubt it
// returns ok=false and the caller fails safe (boundary keeps waiting; the
// forward path auto-denies rather than hanging). See design
// adapter_v2_blocking_prompt_confirm.md §3 and the fixtures in prompt_test.go.

// PromptKind classifies a blocking prompt. survey is intentionally ABSENT: the
// session-rating survey has its own dismisser (deviceapi.dismissSurvey) and must
// never enter the forward-to-device path (ADR-033 §2) — ParsePrompt never returns
// it.
type PromptKind string

const (
	PromptPermission  PromptKind = "permission"  // tool / shell-command approval
	PromptEditConfirm PromptKind = "editConfirm" // file edit / create approval
	PromptUpsell      PromptKind = "upsell"      // fullscreen-renderer etc. upsell
	PromptTrust       PromptKind = "trust"       // trust-this-folder
	PromptUnknown     PromptKind = "unknown"
)

// PromptOption is one selectable menu row. Key is the LITERAL digit the device
// echoes back and the adapter injects to submit it (digit-submit, ADR-033 spike).
// Default marks the highlighted row (claude points "❯" at it; option 1 by
// default).
type PromptOption struct {
	Key     string
	Label   string
	Default bool
}

// Prompt is a parsed blocking select-menu.
type Prompt struct {
	Kind      PromptKind
	Question  string
	Options   []PromptOption
	Mechanism string // "digit" — the only mechanism today (ADR-033 spike)
	Signature string // option-label fingerprint; supersede the promptId when it changes
}

const (
	promptOptionMin = 2   // a real menu offers at least Yes/No
	promptLabelMax  = 120 // rune-cap a context-scoped label so it can't bloat a frame
)

// ParsePrompt scans the visible grid for a blocking select-menu and returns it.
// ok=false when no menu is confidently present — the safe default.
func ParsePrompt(visible string) (Prompt, bool) {
	if visible == "" {
		return Prompt{}, false
	}
	lines := strings.Split(visible, "\n")

	// A live working spinner means claude is busy, not parked on a menu.
	for _, ln := range lines {
		if isSpinnerLine(strings.TrimSpace(ln)) {
			return Prompt{}, false
		}
	}

	// Find the LAST contiguous run of >=2 numbered option rows. The menu sits at
	// the bottom above the footer/input, so the last block wins over any earlier
	// numbered prose in the reply history.
	start, end := -1, -1
	for i := 0; i < len(lines); {
		if isOptionLine(lines[i]) {
			j := i + 1
			for j < len(lines) && isOptionLine(lines[j]) {
				j++
			}
			if j-i >= promptOptionMin {
				start, end = i, j
			}
			i = j
		} else {
			i++
		}
	}
	if start < 0 {
		return Prompt{}, false
	}

	var opts []PromptOption
	for _, ln := range lines[start:end] {
		ptr, key, label, ok := parseOptionLine(ln)
		if !ok {
			continue
		}
		opts = append(opts, PromptOption{Key: key, Label: truncateRunes(label, promptLabelMax), Default: ptr})
	}
	if len(opts) < promptOptionMin {
		return Prompt{}, false
	}
	pointerOnFirst := opts[0].Default // claude points "❯" at the highlighted row, always option 1

	// Corroborate this is a real claude select-menu, not prose that happens to hold a
	// numbered list (and maybe an "Esc to cancel" sentence). The STRONGEST signal is
	// claude's universal Yes…/No… confirm shape (label-anchored — design §2 ranks
	// option labels most stable); failing that, the highlight pointer ON OPTION 1
	// backed by the real "Esc to cancel" footer or a confirm question. A prose how-to
	// list has none of these (its first item is not "Yes", its glyphs aren't ❯ on
	// row 1), and a footer/pointer ALONE is too weak (a reply can quote either).
	question := findQuestion(lines, start)
	yesNo := optStartsWith(opts[0], "yes") && anyOptStartsWith(opts[1:], "no")
	if !(yesNo || (pointerOnFirst && (hasCancelFooter(lines, end) || isConfirmQuestion(question)))) {
		return Prompt{}, false
	}

	// Mark exactly one default for display: claude's pointer (option 1), or option 1
	// as the fallback when the ❯ glyph didn't survive the emulator.
	if !pointerOnFirst {
		for i := range opts {
			opts[i].Default = false
		}
		opts[0].Default = true
	}

	return Prompt{
		Kind:      classifyPrompt(question, lines, start),
		Question:  question,
		Options:   opts,
		Mechanism: "digit",
		Signature: promptSignature(opts),
	}, true
}

// hasCancelFooter reports whether claude's blocking-menu footer sits within a few
// lines below the options. Anchored on the full affordance ("esc to cancel" /
// "to amend" / "to explain"), not a bare "to cancel" (which is common reply
// prose). Only ever used in combination with a pointer-on-option-1, so even this
// can't fire a menu on its own.
func hasCancelFooter(lines []string, optEnd int) bool {
	for k := optEnd; k < len(lines) && k < optEnd+4; k++ {
		t := strings.ToLower(lines[k])
		if strings.Contains(t, "esc to cancel") || strings.Contains(t, "to amend") || strings.Contains(t, "to explain") {
			return true
		}
	}
	return false
}

// optStartsWith reports whether the option's label begins with prefix (case- and
// space-insensitive) — the label-anchored confirm-menu test.
func optStartsWith(o PromptOption, prefix string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(o.Label)), prefix)
}

func anyOptStartsWith(opts []PromptOption, prefix string) bool {
	for _, o := range opts {
		if optStartsWith(o, prefix) {
			return true
		}
	}
	return false
}

// isConfirmQuestion reports whether q reads like claude's confirm header.
func isConfirmQuestion(q string) bool {
	q = strings.ToLower(q)
	for _, s := range []string{"do you want", "proceed", "trust", "make this edit", "allow"} {
		if strings.Contains(q, s) {
			return true
		}
	}
	return false
}

// IsTurnBlockingKind reports whether a prompt kind is a mid-TURN pause — a
// tool/permission confirm that must NOT be read as turn-end (boundary.go keys off
// this). upsell/trust are startup/periodic chrome, not part of a reply turn, and
// are dismissed elsewhere, so they don't suppress the boundary. Mirrors
// deviceapi.forwardablePromptKind.
func IsTurnBlockingKind(k PromptKind) bool {
	switch k {
	case PromptPermission, PromptEditConfirm, PromptUnknown:
		return true
	default:
		return false
	}
}

// findQuestion returns the nearest content line above the options — claude's
// "Do you want to proceed?" / "Do you want to make this edit?" header. Skips
// blank rows, rules/borders, and the option rows themselves. "" if none nearby.
func findQuestion(lines []string, optStart int) string {
	for k := optStart - 1; k >= 0 && k >= optStart-6; k-- {
		t := strings.TrimSpace(lines[k])
		if t == "" || isBoxDrawingOnly(t) || isOptionLine(t) {
			continue
		}
		return t
	}
	return ""
}

// classifyPrompt buckets the menu by the question + a little preamble context.
// Never returns survey. Defaults to permission for tool/command approvals and
// unknown only when nothing matches (still forwarded; the device just shows it).
func classifyPrompt(question string, lines []string, optStart int) PromptKind {
	parts := []string{strings.ToLower(question)}
	for k := optStart - 1; k >= 0 && k >= optStart-8; k-- {
		parts = append(parts, strings.ToLower(strings.TrimSpace(lines[k])))
	}
	ctx := strings.Join(parts, " ")
	switch {
	case strings.Contains(ctx, "fullscreen") || strings.Contains(ctx, "renderer"):
		return PromptUpsell
	case strings.Contains(ctx, "trust"):
		return PromptTrust
	case strings.Contains(ctx, "make this edit") || strings.Contains(ctx, "edit to") ||
		strings.Contains(ctx, "create") || strings.Contains(ctx, "write"):
		return PromptEditConfirm
	case strings.Contains(ctx, "proceed") || strings.Contains(ctx, "command") ||
		strings.Contains(ctx, "run") || strings.Contains(ctx, "bash"):
		return PromptPermission
	default:
		return PromptUnknown
	}
}

// promptSignature fingerprints the option set (keys + labels). A change — claude
// appends a context-scoped option or reorders — flips the signature so the caller
// supersedes the old promptId (design §4).
func promptSignature(opts []PromptOption) string {
	var b strings.Builder
	for i, o := range opts {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(o.Key)
		b.WriteByte(':')
		b.WriteString(o.Label)
	}
	return b.String()
}
