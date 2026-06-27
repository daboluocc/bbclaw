package extract

import (
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/vtscreen"
)

// fakeScreen is a minimal screenView for table-driven detector tests: it returns
// a fixed plain-text grid so a test can assert TurnEnded against an exact screen
// state without driving a real vtscreen byte stream.
type fakeScreen struct{ text string }

func (f fakeScreen) VisibleText() string { return f.text }

// busyScreen / idleScreen are the two canonical screen states the boundary keys
// off, written the way claude renders them (spinner status line carrying the
// "esc to interrupt" affordance; idle "> " prompt box).
const (
	busyScreen = "" +
		"⏺ The capital of France is Paris.\n" +
		"\n" +
		"✶ Cogitating… (3s · ↑ 240 tokens · esc to interrupt)\n" +
		"╭──────────────────────────────╮\n" +
		"│ >                            │\n" +
		"╰──────────────────────────────╯"

	idleScreen = "" +
		"⏺ The capital of France is Paris.\n" +
		"  and is its largest city.\n" +
		"\n" +
		"╭──────────────────────────────╮\n" +
		"│ >                            │\n" +
		"╰──────────────────────────────╯"

	// idlePromptWithUserText is the box still echoing the in-flight question — it
	// must NOT count as the idle prompt (the turn is not over yet).
	promptWithUserText = "" +
		"⏺ Working on it.\n" +
		"╭──────────────────────────────╮\n" +
		"│ > what is the capital?       │\n" +
		"╰──────────────────────────────╯"

	// interruptedScreen is claude's post-ESC redirect (ADR-041): the working
	// spinner is GONE (claude waits on the user to redirect), an "Interrupted ·
	// What should Claude do instead?" banner is shown, and a bare "❯" composer box
	// is present — which on its own satisfies isIdlePromptLine. The turn is NOT
	// over and it is NOT a clean idle prompt.
	interruptedScreen = "" +
		"⏺ Once there was a little fox who thought\n" +
		"\n" +
		"Interrupted · What should Claude do instead?\n" +
		"╭──────────────────────────────╮\n" +
		"│ ❯                            │\n" +
		"╰──────────────────────────────╯"
)

// TestTurnEnded table-drives the three-heuristic combination. Each case sets up
// a detector state (last byte time relative to a fixed `now`, plus the screen it
// last observed) and asserts TurnEnded.
func TestTurnEnded(t *testing.T) {
	now := time.Unix(1_000_000, 0)

	tests := []struct {
		name      string
		sinceByte time.Duration // now - lastByteAt; <0 means "never observed"
		screen    string
		want      bool
	}{
		{
			name:      "never observed",
			sinceByte: -1,
			screen:    idleScreen,
			want:      false,
		},
		{
			name:      "settled + idle prompt + no spinner -> ended",
			sinceByte: Quiet + 50*time.Millisecond,
			screen:    idleScreen,
			want:      true,
		},
		{
			name:      "still within quiet window -> not ended",
			sinceByte: Quiet - 50*time.Millisecond,
			screen:    idleScreen,
			want:      false,
		},
		{
			name:      "settled but spinner still on screen (mid-reply pause) -> not ended",
			sinceByte: 5 * time.Second, // long pause, but spinner persists
			screen:    busyScreen,
			want:      false,
		},
		{
			name:      "settled, no spinner, but prompt still shows question -> not ended",
			sinceByte: Quiet + time.Second,
			screen:    promptWithUserText,
			want:      false,
		},
		{
			name:      "exactly at the Quiet boundary -> ended",
			sinceByte: Quiet,
			screen:    idleScreen,
			want:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var d Detector
			if tc.sinceByte >= 0 {
				d.Observe(now.Add(-tc.sinceByte), fakeScreen{tc.screen}, true)
			}
			if got := d.TurnEnded(now); got != tc.want {
				t.Errorf("TurnEnded = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestInterruptedRedirectIsNotTurnEnd (ADR-041): after a spinner has been seen
// this turn, claude's post-ESC "Interrupted · What should Claude do instead?"
// redirect — spinner cleared + bare "❯" present — must NOT be read as turn-end.
// Before the fix the sawSpinner short-circuit fired here, so a barge-in injected
// INTO the redirect frame (串轮). After the fix interruptedOnScreen holds it false.
func TestInterruptedRedirectIsNotTurnEnd(t *testing.T) {
	now := time.Unix(3_000_000, 0)

	var d Detector
	// Turn was working (spinner up): sets the sticky sawSpinner.
	d.Observe(now.Add(-2*Quiet), fakeScreen{busyScreen}, true)
	if !d.sawSpinner {
		t.Fatal("precondition: spinner should have been seen")
	}
	// ESC interrupted -> redirect on screen, spinner gone, output settled past Quiet.
	d.Observe(now.Add(-Quiet-10*time.Millisecond), fakeScreen{interruptedScreen}, true)
	if d.TurnEnded(now) {
		t.Error("TurnEnded=true on the INTERRUPTED redirect; want false (ADR-041)")
	}

	// Regression guard: a TRUE clean idle (no banner) after the same setup DOES end.
	d.Observe(now.Add(-Quiet-10*time.Millisecond), fakeScreen{idleScreen}, true)
	if !d.TurnEnded(now) {
		t.Error("TurnEnded=false on clean idle after dismiss; want true")
	}

	// IsCleanIdle helper distinguishes the two (used by the cancel state machine).
	if IsCleanIdle(interruptedScreen) {
		t.Error("IsCleanIdle(interrupted)=true; want false")
	}
	if !IsCleanIdle(idleScreen) {
		t.Error("IsCleanIdle(idle)=false; want true")
	}
	if !HasInterruptedBanner(interruptedScreen) || HasInterruptedBanner(idleScreen) {
		t.Error("HasInterruptedBanner mis-detected")
	}
}

// TestFixtureFullTurnEnds is the core acceptance test: replay the #209 fixture
// (a full claude turn that finishes with the spinner cleared and the idle prompt
// restored). After the whole stream is fed and the Quiet window elapses,
// TurnEnded must be true — and crucially it must be FALSE at the mid-reply point
// where the spinner is still alive, even if output had paused there.
func TestFixtureFullTurnEnds(t *testing.T) {
	pre, post := loadFixture(t)

	s := vtscreen.New(fixtureCols, fixtureRows)
	s.Feed(pre)

	var d Detector
	t0 := time.Unix(2_000_000, 0)
	d.Observe(t0, s, true)

	// Feed the post-baseline stream (reply + interleaved spinner, then the final
	// clear-spinner + idle-prompt redraw). This is one PTY burst.
	s.Feed(post)
	tEnd := t0.Add(10 * time.Millisecond)
	d.Observe(tEnd, s, true)

	// Immediately after the burst we are still inside the Quiet window: not ended.
	if d.TurnEnded(tEnd) {
		t.Fatalf("turn reported ended before the Quiet window elapsed")
	}

	// Once output has been settled past Quiet, and the screen shows no spinner and
	// the idle prompt is back, the turn is genuinely over.
	if !d.TurnEnded(tEnd.Add(Quiet + time.Millisecond)) {
		vis := s.VisibleText()
		t.Fatalf("full turn did not report ended after Quiet\nspinner present: %v\nfinal screen:\n%s",
			strings.Contains(vis, "esc to interrupt"), vis)
	}
}

// TestMidReplyPauseDoesNotEnd is the adversarial false-positive guard the issue
// calls out: a reply pauses mid-stream (network stall) — bytes stop for well
// over Quiet — but the spinner is still on screen and the idle prompt has not
// returned. TurnEnded must stay false the entire time, then flip true only once
// the turn actually completes.
func TestMidReplyPauseDoesNotEnd(t *testing.T) {
	s := vtscreen.New(fixtureCols, fixtureRows)
	s.Feed([]byte("\x1b[2J\x1b[H"))

	var d Detector
	t0 := time.Unix(3_000_000, 0)

	// Half a reply has streamed in, and the spinner is alive (claude keeps it up
	// while waiting on the model). Draw reply text + the working status line.
	s.Feed([]byte("\x1b[3;1H⏺ The capital of France is"))
	s.Feed([]byte("\x1b[21;1H\r\x1b[2K✶ Cogitating… (3s · ↑ 240 tokens · esc to interrupt)"))
	// And the prompt box is present but shows the (busy) box, not idle-ready.
	s.Feed([]byte("\x1b[23;1H│ >                            │"))
	d.Observe(t0, s, true)

	// Long network pause: no bytes for many seconds. Output is settled well past
	// Quiet, but the spinner persists -> the turn is NOT over.
	for _, gap := range []time.Duration{Quiet, 2 * time.Second, 10 * time.Second} {
		if d.TurnEnded(t0.Add(gap)) {
			t.Fatalf("mid-reply pause falsely reported turn ended after %v", gap)
		}
	}

	// The reply resumes and completes: more text, then claude clears the spinner
	// and the idle prompt returns.
	t1 := t0.Add(10 * time.Second)
	s.Feed([]byte("\x1b[4;1H  Paris, since the 12th century."))
	s.Feed([]byte("\x1b[21;1H\x1b[2K"))                                 // clear the spinner row
	s.Feed([]byte("\x1b[23;1H\x1b[2K│ >                            │")) // idle prompt restored
	d.Observe(t1, s, true)

	// Still inside the new Quiet window: not yet ended.
	if d.TurnEnded(t1) {
		t.Fatalf("turn reported ended before Quiet elapsed after resume")
	}
	// After Quiet, with spinner gone and idle prompt present, the turn ends.
	if !d.TurnEnded(t1.Add(Quiet + time.Millisecond)) {
		t.Fatalf("turn did not report ended after the reply genuinely completed")
	}
}

// TestApiRetryWaitDoesNotEnd: claude's network/API retry state ("Waiting for API
// response · will retry in Ns · check your network") drops the "esc to interrupt"
// affordance, but the turn is NOT over — it will retry. Treating it as idle ended
// the turn early and cleared the in-flight flag, so a barge-in typed into the box
// (claude queued it) instead of ESC-aborting. The detector must keep it in flight.
func TestApiRetryWaitDoesNotEnd(t *testing.T) {
	s := vtscreen.New(fixtureCols, fixtureRows)
	s.Feed([]byte("\x1b[2J\x1b[H"))

	var d Detector
	t0 := time.Unix(5_500_000, 0)

	// A spinner appeared — the turn started working.
	s.Feed([]byte("\x1b[3;1H⏺ working on it"))
	s.Feed([]byte("\x1b[21;1H✶ Seasoning… (3s · ↑ 12 tokens · esc to interrupt)"))
	d.Observe(t0, s, true)

	// The API call fails; claude drops the "esc to interrupt" spinner and shows the
	// retry wait instead.
	t1 := t0.Add(time.Second)
	s.Feed([]byte("\x1b[21;1H\x1b[2K")) // esc-to-interrupt spinner gone
	s.Feed([]byte("\x1b[22;1HWaiting for API response · will retry in 2m 26s · check your network"))
	d.Observe(t1, s, true)

	// Output is settled well past Quiet, but the turn is NOT over (it will retry),
	// so the detector must keep reporting in-flight — otherwise inFlight clears and
	// the next barge-in can't ESC.
	for _, gap := range []time.Duration{Quiet, 5 * time.Second, 30 * time.Second} {
		if d.TurnEnded(t1.Add(gap)) {
			t.Fatalf("API-retry wait falsely reported turn ended after %v", gap)
		}
	}
}

// TestNeverSpeakingGuard covers the false-negative end: a spinner-less / very
// fast CLI whose reply finishes without ever painting a spinner must still end
// on prompt-return + quiet, so the device is never stuck silent.
func TestNeverSpeakingGuard(t *testing.T) {
	s := vtscreen.New(40, 10)
	s.Feed([]byte("\x1b[2J\x1b[H"))

	var d Detector
	t0 := time.Unix(4_000_000, 0)

	// Reply lands in one shot, no spinner ever, then the idle prompt.
	s.Feed([]byte("\x1b[1;1H⏺ 4."))
	s.Feed([]byte("\x1b[3;1H> "))
	d.Observe(t0, s, true)

	if d.sawSpinner {
		t.Fatalf("test setup drew a spinner unexpectedly")
	}
	if !d.TurnEnded(t0.Add(Quiet + time.Millisecond)) {
		t.Fatalf("spinner-less reply never reported ended; device would stay silent")
	}
}

// TestObserveNoBytesKeepsClock verifies the hadBytes=false path: a screen
// refresh that is NOT a PTY read must update prompt/spinner visibility without
// resetting the settled clock, so a tick that observes the idle screen after a
// quiet period still lets the turn end.
func TestObserveNoBytesKeepsClock(t *testing.T) {
	var d Detector
	t0 := time.Unix(5_000_000, 0)

	// A real PTY read of the busy screen starts the clock.
	d.Observe(t0, fakeScreen{busyScreen}, true)
	if d.TurnEnded(t0.Add(Quiet + time.Millisecond)) {
		t.Fatalf("busy screen should not be ended")
	}

	// A later refresh (no new bytes) observes the now-idle screen. The clock must
	// NOT advance, so the turn is judged ended relative to the original byte time.
	d.Observe(t0.Add(5*time.Second), fakeScreen{idleScreen}, false)
	if !d.TurnEnded(t0.Add(Quiet + time.Millisecond)) {
		t.Fatalf("no-bytes refresh should not have reset the settled clock")
	}
}

// TestResetClearsState confirms a Detector can be reused across turns.
func TestResetClearsState(t *testing.T) {
	var d Detector
	t0 := time.Unix(6_000_000, 0)
	d.Observe(t0, fakeScreen{idleScreen}, true)
	if !d.TurnEnded(t0.Add(Quiet + time.Millisecond)) {
		t.Fatalf("precondition: turn should be ended")
	}
	d.Reset()
	if d.TurnEnded(t0.Add(time.Hour)) {
		t.Fatalf("after Reset, TurnEnded must report false until new output")
	}
}

// TestScanStatus table-drives the single-pass screen scanner over the spinner /
// idle-prompt shapes (and the negatives that must not trip either flag).
func TestScanStatus(t *testing.T) {
	tests := []struct {
		name        string
		visible     string
		wantSpinner bool
		wantPrompt  bool
	}{
		{"empty grid", "", false, false},
		{"idle only", "Done.\n> ", false, true},
		{"spinner only", "Thinking…\n✶ Working… (2s · esc to interrupt)", true, false},
		// The busy screen carries both a spinner AND an empty (idle-shaped) prompt
		// box; the spinner is what makes TurnEnded report "not ended" — scanStatus
		// just reports the raw presence of each.
		{"both present (busy box)", busyScreen, true, true},
		{"idle box, no spinner", idleScreen, false, true},
		{"prompt echoing user text is not idle", promptWithUserText, false, false},
		{"plain reply, no chrome", "The capital of France is Paris.", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spinner, prompt := scanStatus(tc.visible)
			if spinner != tc.wantSpinner {
				t.Errorf("spinner = %v, want %v", spinner, tc.wantSpinner)
			}
			if prompt != tc.wantPrompt {
				t.Errorf("idlePrompt = %v, want %v", prompt, tc.wantPrompt)
			}
		})
	}
}

// TestIsIdlePromptLine distinguishes the empty idle box from a box still holding
// the user's in-flight question.
func TestIsIdlePromptLine(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{">", true},
		{" > ", true},
		{"│ > ", true},
		{"│ >                  │", true}, // idle box with both borders kept on the row
		{"> what is the capital?", false},
		{"│ > draft a poem", false},
		{"The capital of France is Paris.", false},
		{"x > y means x exceeds y", false},
	}
	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			if got := isIdlePromptLine(tc.line); got != tc.want {
				t.Errorf("isIdlePromptLine(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}
