package extract

// Turn boundary detection — the crux of the device line.
//
// v1's stream-json delivered an explicit `turn_end` event. Over a PTY there is
// no such signal, so we infer "the assistant finished this turn" from screen
// behaviour. This decides WHEN the device starts TTS playback, so false
// positives (speaking a half-finished reply) and false negatives (never
// speaking) are both bad.
//
// We combine three heuristics, ALL of which must hold for TurnEnded to be true:
//
//  1. Output settled: no new PTY bytes for ~Quiet duration. A reply still
//     streaming in (or a CLI actively redrawing its spinner) keeps producing
//     bytes, so we never call a turn finished while content is moving.
//  2. Prompt returned: the cursor region is back at the CLI's idle input prompt
//     (claude's empty "> " box), not inside a streaming response. When the model
//     is working the prompt box is absent or shows the in-flight question; an
//     idle prompt is the CLI saying "your move".
//  3. Spinner gone: the "thinking…" / "Cogitating…" status line — the one
//     carrying claude's "esc to interrupt" affordance — is no longer on screen.
//     A live spinner is the single most reliable "still busy" signal: claude
//     keeps it up for the entire turn and only clears it when the turn ends.
//
// Why all three, and not just "output settled"? A mid-reply network pause is the
// adversarial case: the model stalls, bytes stop for well over Quiet, yet the
// turn is NOT over. In that state the spinner line is still on screen (claude
// keeps "esc to interrupt" visible while waiting) and the idle prompt has not
// returned. Requiring (2) and (3) on top of (1) rejects that false positive,
// while (1) on its own guards against speaking over a reply that is still
// actively painting.
//
// The spinner / prompt shapes are claude-specific and shared with noise.go
// (promptInterruptHint, isPromptLine). Keeping the detector keyed off the same
// line classifiers means a claude UI change is fixed in one place.

import (
	"strings"
	"time"
)

// Quiet is the debounce window for "output settled": the minimum gap since the
// last PTY byte before the turn may be considered finished. 600ms is long
// enough to ride over the brief stalls between streamed chunks of a single
// reply, but short enough that the device starts speaking promptly once the CLI
// truly goes idle.
const Quiet = 600 * time.Millisecond

// screenView is the read-only slice of vtscreen.Screen the Detector needs. It
// is satisfied by *vtscreen.Screen and lets tests drive the detector with a
// trivial fake (and keeps this package from importing more of vtscreen than it
// uses).
type screenView interface {
	// VisibleText returns the plain-text content of the visible grid (no ANSI).
	VisibleText() string
}

// Detector is the per-session turn-boundary state machine. It is fed the same
// timeline as the Extractor — Observe on every PTY chunk, with the screen
// already updated — and answers TurnEnded on demand. It is not goroutine-safe;
// the owning Session serialises access, mirroring Extractor and vtscreen.Screen.
type Detector struct {
	// lastByteAt is when the most recent non-empty PTY chunk arrived; the
	// "output settled" clock counts from here. Zero until the first Observe.
	lastByteAt time.Time

	// spinnerOnScreen records whether, as of the last Observe, the CLI's working
	// spinner / status line was visible. While true the turn is in flight no
	// matter how long output has been quiet (covers the mid-reply network pause).
	spinnerOnScreen bool

	// idlePromptOnScreen records whether the CLI's idle input prompt was visible
	// at the last Observe — the CLI signalling it has handed control back.
	idlePromptOnScreen bool

	// sawSpinner records whether a spinner has appeared at any point this turn.
	// It guards the false-negative end of the spectrum: a reply with no spinner
	// at all (a very fast or spinner-less CLI) still ends on prompt-return +
	// quiet, so we never get stuck waiting for a spinner that will never show.
	sawSpinner bool

	// awaitingPromptOnScreen records whether a BLOCKING interactive menu (claude's
	// "Do you want to proceed? ❯ 1. Yes …" permission/confirm popup) is on screen.
	// In that state claude has CLEARED the spinner (it is waiting on the human, not
	// the model) yet the turn is NOT over — it is parked on the menu. Without this,
	// TurnEnded's sawSpinner short-circuit would mis-fire turn-end and the device
	// would speak blank / stale text over the prompt (ADR-033 §0; the bug §9 wrongly
	// claimed was "naturally" handled).
	awaitingPromptOnScreen bool
}

// Observe records a PTY chunk: t is when the bytes arrived, screen is the
// vtscreen.Screen AFTER those bytes were Fed (so the grid reflects them). Pass a
// zero-length chunk's screen with hadBytes=false to refresh the screen-derived
// state (prompt/spinner visibility) without resetting the settled clock — useful
// for a periodic tick that re-evaluates the screen between PTY reads.
//
// Callers that simply forward every PTY read can ignore hadBytes and pass true.
func (d *Detector) Observe(t time.Time, screen screenView, hadBytes bool) {
	if hadBytes {
		d.lastByteAt = t
	}
	d.refresh(screen)
}

// refresh re-samples the screen-derived heuristics (spinner present, idle prompt
// present) from the current grid. Split out so a future periodic tick can update
// the view without touching the byte clock.
func (d *Detector) refresh(screen screenView) {
	if screen == nil {
		return
	}
	visible := screen.VisibleText()
	spinner, prompt := scanStatus(visible)
	d.spinnerOnScreen = spinner
	d.idlePromptOnScreen = prompt
	if spinner {
		d.sawSpinner = true
	}
	// Only a mid-TURN blocking menu (permission/tool confirm) suppresses turn-end.
	// A startup/periodic upsell or trust dialog is not part of a reply turn and is
	// dismissed elsewhere — keying off it here could wedge the turn forever (no
	// dismisser in this loop). See IsTurnBlockingKind.
	if p, ok := ParsePrompt(visible); ok && IsTurnBlockingKind(p.Kind) {
		d.awaitingPromptOnScreen = true
	} else {
		d.awaitingPromptOnScreen = false
	}
}

// TurnEnded reports whether, as of now, the current turn looks complete. All
// three heuristics must agree:
//
//   - output has been settled for at least Quiet (and we have seen at least one
//     chunk, so a never-started session is not "ended");
//   - the working spinner is no longer on screen;
//   - the CLI's idle input prompt has returned.
//
// Returning false while any of these fails is the safe direction: at worst the
// device waits a beat longer before speaking, never speaks half a reply.
func (d *Detector) TurnEnded(now time.Time) bool {
	if d.lastByteAt.IsZero() {
		return false // nothing has happened yet
	}
	if now.Sub(d.lastByteAt) < Quiet {
		return false // still within the settle window: output may be streaming
	}
	if d.spinnerOnScreen {
		return false // CLI is still working (e.g. a mid-reply network pause)
	}
	if d.awaitingPromptOnScreen {
		// A blocking permission/confirm menu is up: claude cleared the spinner to
		// wait on the human, but the turn is parked, NOT finished. Must precede the
		// sawSpinner short-circuit below, which would otherwise mis-fire turn-end and
		// speak blank/stale text over the menu (ADR-033 §0).
		return false
	}
	// Output settled and the working spinner is gone. The primary completion
	// signal is "the CLI was working this turn (a spinner appeared) and it is now
	// gone" — robust across CLIs whose idle prompt we cannot pin down: real claude
	// renders a "❯" box with a rotating "Try …" placeholder, never a bare ">", so
	// keying on idle-prompt text alone never fired against the real TUI.
	if d.sawSpinner {
		return true
	}
	// Fallback for a CLI/turn that never showed a spinner (a very fast reply):
	// require the idle prompt to have returned, so we do not fire in the gap
	// before the CLI even started working.
	return d.idlePromptOnScreen
}

// Reset clears per-turn state so the Detector can be reused for the next user
// turn (mirroring how a fresh Extractor is built per turn). The settled clock
// and screen-derived flags are cleared; TurnEnded then reports false until new
// output arrives.
func (d *Detector) Reset() {
	d.lastByteAt = time.Time{}
	d.spinnerOnScreen = false
	d.idlePromptOnScreen = false
	d.sawSpinner = false
	d.awaitingPromptOnScreen = false
}

// scanStatus inspects the plain-text visible grid for the two screen-derived
// boundary signals in a single pass:
//
//   - spinner: any line is the CLI's working status line (carries the
//     "esc to interrupt" affordance — see isSpinnerLine).
//   - idlePrompt: any line is the CLI's *empty* idle input prompt ("> " with no
//     user text). A prompt still showing the in-flight question does NOT count;
//     the turn is not over until the box is clear and ready for the next input.
func scanStatus(visible string) (spinner, idlePrompt bool) {
	if visible == "" {
		return false, false
	}
	for _, line := range strings.Split(visible, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if isSpinnerLine(t) {
			spinner = true
		}
		if isIdlePromptLine(t) {
			idlePrompt = true
		}
	}
	return spinner, idlePrompt
}

// isIdlePromptLine reports whether t is the CLI's empty, ready-for-input prompt
// — the "> " box with nothing typed, after any box borders are stripped. Unlike
// isPromptLine (noise.go), which also matches a prompt holding the user's typed
// question, this matches ONLY the idle box: a prompt still echoing the in-flight
// question means the CLI has not finished the turn.
//
// Real vtscreen renders of claude's idle box collapse to a bare " >", but some
// emulator/geometry combinations keep the box's right border on the same row, so
// we strip a trailing "│" too before requiring exactly ">".
func isIdlePromptLine(t string) bool {
	t = strings.TrimLeft(t, "│|")
	t = strings.TrimRight(t, "│|")
	t = strings.TrimSpace(t)
	return t == ">" || t == "❯" // bare prompt glyph with no trailing user text
}
