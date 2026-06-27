package deviceapi

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

// recordingSink captures every audio buffer Play receives, so the integration
// test can assert the inject → reply → boundary → TTS path actually reached the
// device. It is goroutine-safe: Run calls Play from its own goroutine.
type recordingSink struct {
	mu      sync.Mutex
	plays   []play
	playErr error
}

type play struct {
	audio  []byte
	format string
}

func (s *recordingSink) Play(_ context.Context, audio []byte, format string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.playErr != nil {
		return s.playErr
	}
	s.plays = append(s.plays, play{audio: append([]byte(nil), audio...), format: format})
	return nil
}

func (s *recordingSink) snapshot() []play {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]play(nil), s.plays...)
}

// countingTTS records the texts it was asked to synthesize and returns a tiny
// non-empty buffer, so a test can assert both that TTS was called and with what.
type countingTTS struct {
	mu     sync.Mutex
	texts  []string
	out    []byte
	format string
	err    error
}

func (t *countingTTS) Synthesize(_ context.Context, text string) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.texts = append(t.texts, text)
	if t.err != nil {
		return nil, t.err
	}
	out := t.out
	if out == nil {
		out = []byte("AUDIO")
	}
	return out, nil
}

func (t *countingTTS) OutputFormat() string {
	if t.format == "" {
		return "wav"
	}
	return t.format
}

func (t *countingTTS) calls() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.texts...)
}

// mockCLI is a bash one-liner that imitates an agent TUI over a PTY well enough
// for the boundary detector:
//
//   - it prints an idle prompt line that collapses to a bare ">" so
//     extract.Detector sees the CLI ready for input;
//   - on each line read from stdin it prints a "working" spinner line carrying
//     the "esc to interrupt" affordance (so the detector sees the turn in
//     flight), then the reply text "ANSWER: <input>", then ERASES the spinner
//     line and reprints the idle ">" prompt and goes quiet.
//
// Once it goes quiet with the spinner gone and the idle prompt present, the
// detector's three heuristics all hold after the settle window and Run speaks.
//
// Faithfulness to a real TUI: SubmitVoiceTurn prepends an ESC (the interrupt
// key) before the transcript. A real raw-mode CLI consumes ESC as a distinct
// keypress, so it never becomes part of the typed text. A bash `read` in the
// PTY's canonical mode would instead capture the leading ESC into $line, so we
// strip a single leading ESC here to model the real CLI's behaviour. (Without
// this, the captured ESC would re-enter the VT mirror as the start of an escape
// sequence and swallow the next character.)
//
// Positioning uses absolute cursor moves so reply / spinner / prompt land on
// distinct, stable rows that the VT mirror and noise classifier handle cleanly.
const mockCLI = `
printf '\033[5;1H> '
while IFS= read -r line; do
  line="${line#$'\033'}"
  printf '\033[10;1H\033[2K\033[2m. Working... (1s esc to interrupt)\033[0m'
  printf '\033[7;1H\033[2KANSWER: %s' "$line"
  printf '\033[10;1H\033[2K'
  printf '\033[5;1H\033[2K> '
done
`

// newMockSession spawns the mockCLI under a real PTY session so SubmitVoiceTurn
// writes to genuine PTY stdin and the reply flows back through Run, exactly as
// in production. The session is killed via reap() on cleanup.
func newMockSession(t *testing.T) (*session.Manager, *session.Session) {
	t.Helper()
	m := session.NewManager()
	s, err := m.Create("dev", ptyhost.Config{
		Argv:        []string{"bash", "-c", mockCLI},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("create mock session: %v", err)
	}
	return m, s
}

// waitForPlays blocks until the sink has recorded at least n plays or the
// deadline elapses, returning what it observed.
func waitForPlays(sink *recordingSink, n int, d time.Duration) []play {
	deadline := time.After(d)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if p := sink.snapshot(); len(p) >= n {
			return p
		}
		select {
		case <-deadline:
			return sink.snapshot()
		case <-tick.C:
		}
	}
}

// TestInjectReplyBoundaryTTS is the spec's core acceptance test: a mock ASR text
// is injected, the mock CLI replies, the boundary fires, and TTS is called and
// its audio reaches the device sink. Table-driven over transcript variants.
func TestInjectReplyBoundaryTTS(t *testing.T) {
	cases := []struct {
		name       string
		transcript string
		wantInOut  string // substring the extracted/spoken reply must contain
	}{
		{name: "simple", transcript: "hello there", wantInOut: "ANSWER: hello there"},
		{name: "another", transcript: "what time is it", wantInOut: "ANSWER: what time is it"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, s := newMockSession(t)
			tts := &countingTTS{format: "wav"}
			sink := &recordingSink{}
			b := New(s, StaticRecognizer{Text: tc.transcript}, tts, sink,
				Config{Cols: 80, Rows: 24, PollInterval: 40 * time.Millisecond})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			runErr := make(chan error, 1)
			go func() { runErr <- b.Run(ctx) }()

			// Give Run a beat to attach and baseline before injecting the turn.
			time.Sleep(100 * time.Millisecond)
			if err := b.SubmitVoiceTurn(tc.transcript); err != nil {
				t.Fatalf("SubmitVoiceTurn: %v", err)
			}

			plays := waitForPlays(sink, 1, 4*time.Second)
			if len(plays) == 0 {
				t.Fatalf("TTS never reached the device sink (calls=%v)", tts.calls())
			}

			// TTS was called with the extracted reply text.
			calls := tts.calls()
			if len(calls) == 0 {
				t.Fatalf("Synthesize was not called")
			}
			if !strings.Contains(calls[0], tc.wantInOut) {
				t.Errorf("spoken text = %q, want substring %q", calls[0], tc.wantInOut)
			}
			// The user's own transcript must not be voiced back (echo policy: off).
			// (The reply happens to embed it after "ANSWER:", which is the CLI's
			// echo, not ours — so we only assert the reply marker is present.)

			// The audio + format propagated to the device.
			if string(plays[0].audio) != "AUDIO" {
				t.Errorf("sink audio = %q, want %q", plays[0].audio, "AUDIO")
			}
			if plays[0].format != "wav" {
				t.Errorf("sink format = %q, want %q", plays[0].format, "wav")
			}

			cancel()
			<-runErr
			_ = m // session child is reaped on process exit / manager GC
		})
	}
}

// bargeInCLI models a slow agent TUI so a barge-in (interrupt mid-turn) is
// observable. Each line it reads is one turn N:
//
//   - it raises the spinner (so the detector sees the turn in flight);
//   - it prints that turn's reply on its OWN row (row 6+N), so successive turns
//     land on DISTINCT, persistent rows — an un-excluded earlier reply would stay
//     visible on the grid and leak into a later turn's extraction;
//   - it then SLEEPS, holding the turn "in flight" long enough for a barge-in
//     turn to be injected before this one settles, then clears the spinner and
//     reprints the idle ">" prompt and goes quiet.
//
// A real raw-mode CLI consumes the leading ESC as interrupt and renders typed
// input in its own prompt box, not via terminal echo. We model that faithfully
// by disabling terminal echo (`stty -echo`), so an injected "ESC + transcript"
// never paints raw onto the grid at the cursor (which in canonical mode would
// corrupt whatever row the cursor sits on, e.g. an in-flight reply row). The
// leading ESC is still captured into $line by bash's canonical `read`, so we
// strip one leading ESC, the same trick as mockCLI.
const bargeInCLI = `
stty -echo
n=0
printf '\033[5;1H> '
while IFS= read -r line; do
  line="${line#$'\033'}"
  n=$((n+1))
  row=$((6+n))
  printf '\033[10;1H\033[2K\033[2m. Working... (1s esc to interrupt)\033[0m'
  printf '\033[%d;1H\033[2KANSWER: %s' "$row" "$line"
  sleep 0.5
  printf '\033[10;1H\033[2K'
  printf '\033[5;1H\033[2K> '
done
`

// TestBargeInSpeaksOnlyLatestTurn is the multi-turn + barge-in regression for the
// re-arm-at-injection fix. Through a SINGLE Run it injects turn A, then — before
// A's boundary settles (the CLI is still mid-turn, spinner up, A's partial reply
// on screen) — injects turn B. It asserts every spoken reply is turn B's ANSWER
// and that turn A's partial NEVER reaches TTS, neither alone nor concatenated
// with B. Without re-baselining the Extractor at injection time, A's reply line
// stays outside the baseline and leaks into B's extraction (the device speaks the
// stale interrupted reply) — exactly the failure DESIGN.md §8 forbids.
func TestBargeInSpeaksOnlyLatestTurn(t *testing.T) {
	m := session.NewManager()
	s, err := m.Create("bargein", ptyhost.Config{
		Argv:        []string{"bash", "-c", bargeInCLI},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("create barge-in session: %v", err)
	}

	tts := &countingTTS{format: "wav"}
	sink := &recordingSink{}
	br := New(s, nil, tts, sink,
		Config{Cols: 80, Rows: 24, PollInterval: 40 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- br.Run(ctx) }()

	// Let Run attach and baseline the idle prompt before the first turn.
	time.Sleep(120 * time.Millisecond)

	// Turn A goes in flight: the CLI raises the spinner, prints A's reply, and
	// sleeps (still mid-turn).
	if err := br.SubmitVoiceTurn("turn alpha"); err != nil {
		t.Fatalf("SubmitVoiceTurn(A): %v", err)
	}
	// Barge in with turn B while A is still settling (well under the CLI's 0.5s
	// hold), so no completed-turn boundary fired for A before B is injected.
	time.Sleep(120 * time.Millisecond)
	if err := br.SubmitVoiceTurn("turn bravo"); err != nil {
		t.Fatalf("SubmitVoiceTurn(B): %v", err)
	}

	// Wait for a spoken reply to arrive after the barge-in.
	plays := waitForPlays(sink, 1, 5*time.Second)
	if len(plays) == 0 {
		t.Fatalf("no reply ever reached TTS after barge-in (calls=%v)", tts.calls())
	}

	// Every synthesized reply must be turn B's answer; turn A's partial must never
	// be spoken, alone or concatenated with B.
	for _, c := range tts.calls() {
		if strings.Contains(c, "turn alpha") {
			t.Errorf("interrupted turn A leaked into TTS: %q", c)
		}
		if !strings.Contains(c, "ANSWER: turn bravo") {
			t.Errorf("spoken text = %q, want it to contain %q", c, "ANSWER: turn bravo")
		}
	}

	cancel()
	<-runErr
	_ = m // session child is reaped on process exit / manager GC
}

// TestSubmitVoiceTurnInjectsTranscript asserts the inject path writes the
// transcript followed by a carriage return to PTY stdin (the mock CLI echoes it
// back through its reply), and that the interrupt key precedes it.
func TestSubmitVoiceTurnInjectsTranscript(t *testing.T) {
	_, s := newMockSession(t)

	// Attach a raw client to observe what the CLI echoes after injection.
	client, _, _ := s.Attach()
	defer s.Detach(client)

	if err := b(s).SubmitVoiceTurn("ping123"); err != nil {
		t.Fatalf("SubmitVoiceTurn: %v", err)
	}

	if !drainContains(client.Out, "ANSWER: ping123", 3*time.Second) {
		t.Fatalf("CLI did not echo the injected transcript; inject likely failed")
	}
}

// TestSubmitVoiceTurnBlankIsNoop asserts a blank/whitespace transcript neither
// injects nor errors (so an ASR misfire never interrupts a live turn).
func TestSubmitVoiceTurnBlankIsNoop(t *testing.T) {
	_, s := newMockSession(t)
	for _, blank := range []string{"", "   ", "\t\n"} {
		if err := b(s).SubmitVoiceTurn(blank); err != nil {
			t.Errorf("SubmitVoiceTurn(%q) returned error: %v", blank, err)
		}
	}
}

// TestSubmitVoicePTTRunsRecognizer asserts the full PTT path: audio → ASR →
// inject. The static recognizer's text reaches the CLI.
func TestSubmitVoicePTTRunsRecognizer(t *testing.T) {
	_, s := newMockSession(t)
	client, _, _ := s.Attach()
	defer s.Detach(client)

	br := New(s, StaticRecognizer{Text: "voiced query"}, nil, nil, Config{Cols: 80, Rows: 24})
	if err := br.SubmitVoicePTT(context.Background(), []byte("fake-audio")); err != nil {
		t.Fatalf("SubmitVoicePTT: %v", err)
	}
	if !drainContains(client.Out, "ANSWER: voiced query", 3*time.Second) {
		t.Fatalf("recognizer text did not reach the CLI")
	}
}

// TestSubmitVoicePTTBlankRecognitionNoop asserts a blank ASR result does not
// inject anything.
func TestSubmitVoicePTTBlankRecognitionNoop(t *testing.T) {
	_, s := newMockSession(t)
	br := New(s, StaticRecognizer{Text: "   "}, nil, nil, Config{})
	if err := br.SubmitVoicePTT(context.Background(), []byte("audio")); err != nil {
		t.Errorf("blank recognition should be a no-op, got %v", err)
	}
}

// TestSubmitVoiceTurnClosedSession asserts a write to an exited session surfaces
// the package's ErrClosed sentinel.
func TestSubmitVoiceTurnClosedSession(t *testing.T) {
	m := session.NewManager()
	s, err := m.Create("short", ptyhost.Config{
		Argv:        []string{"bash", "-c", "true"}, // exits immediately
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Wait for the child to exit and the session to mark itself closed.
	deadline := time.After(3 * time.Second)
	for s.Status() == session.Connected || m.Get("short") != nil {
		select {
		case <-deadline:
			t.Fatalf("session never closed")
		case <-time.After(20 * time.Millisecond):
		}
		if m.Get("short") == nil {
			break
		}
	}
	err = b(s).SubmitVoiceTurn("late")
	if err != nil && !errors.Is(err, ErrClosed) {
		t.Errorf("want ErrClosed for a closed session, got %v", err)
	}
}

// TestSpeakNilDepsNoop asserts a Bridge with no TTS/sink drains a completed turn
// without panicking and without claiming to have spoken.
func TestSpeakNilDepsNoop(t *testing.T) {
	br := New(nil, nil, nil, nil, Config{})
	if err := br.speak(context.Background(), "anything"); err != nil {
		t.Errorf("speak with nil deps should be a no-op, got %v", err)
	}
}

// interruptProbeCLI reports whether the line it read began with the ESC
// interrupt key, proving SubmitVoiceTurn injects ESC ahead of the transcript
// (the documented in-flight policy: interrupt, not queue). It echoes
// "SAW_ESC:<rest>" when the leading byte was ESC, else "NO_ESC:<line>".
// interruptProbeCLI echoes each submitted line back tagged with whether a barge-in
// ESC preceded it. It runs `stty kill undef` so Ctrl-U (0x15) is a LITERAL byte —
// matching real claude (raw mode, Ctrl-U handled by its composer), not the default
// canonical mode where Ctrl-U is VKILL and would erase the line. ADR-041's barge-in
// sequence is ESC + Ctrl-U(clear composer) + transcript + Enter, so the probe strips
// a leading ESC (recording it) and then a leading Ctrl-U before reporting the text.
const interruptProbeCLI = `
stty kill undef 2>/dev/null
while IFS= read -r line; do
  esc=NO_ESC; cu=-
  case "$line" in $'\033'*) esc=SAW_ESC; line="${line#$'\033'}" ;; esac
  case "$line" in $'\025'*) cu=CU; line="${line#$'\025'}" ;; esac
  printf '%s:%s:%s\n' "$esc" "$cu" "$line"
done
`

// TestSubmitVoiceTurnInterruptsInFlight asserts the in-flight policy: ESC is
// injected ONLY to abort a turn that is actually running (a barge-in), never at
// an idle prompt — where a gratuitous ESC would be read as an escape sequence and
// swallow the transcript's first character (fatal for a leading CJK rune). We
// observe ESC reaching stdin via a probe CLI.
func TestSubmitVoiceTurnInterruptsInFlight(t *testing.T) {
	m := session.NewManager()
	s, err := m.Create("probe", ptyhost.Config{
		Argv:        []string{"bash", "-c", interruptProbeCLI},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	client, _, _ := s.Attach()
	defer s.Detach(client)
	br := b(s)

	// At an idle prompt there is nothing to interrupt: no ESC precedes the text.
	if err := br.SubmitVoiceTurn("first turn"); err != nil {
		t.Fatalf("SubmitVoiceTurn (idle): %v", err)
	}
	if !drainContains(client.Out, "NO_ESC:-:first turn", 3*time.Second) {
		t.Fatalf("idle SubmitVoiceTurn must NOT inject an ESC ahead of the transcript")
	}

	// With a turn in flight, a barge-in interrupts: ESC precedes the new text.
	br.inFlight.Store(true)
	if err := br.SubmitVoiceTurn("second turn"); err != nil {
		t.Fatalf("SubmitVoiceTurn (in-flight): %v", err)
	}
	if !drainContains(client.Out, "SAW_ESC:CU:second turn", 3*time.Second) {
		t.Fatalf("in-flight SubmitVoiceTurn must inject ESC ahead of the transcript")
	}
}

// TestInterruptEscOnlyWhenInFlight covers Bridge.Interrupt (the turn.cancel /
// barge-in abort path): a no-op at an idle prompt (no stray ESC to corrupt the
// next keystroke), and an ESC + in-flight clear when a turn IS running — so the
// CLI drops the current turn and the next submitted line carries no second ESC.
func TestInterruptEscOnlyWhenInFlight(t *testing.T) {
	m := session.NewManager()
	s, err := m.Create("probe-int", ptyhost.Config{
		Argv:        []string{"bash", "-c", interruptProbeCLI},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	client, _, _ := s.Attach()
	defer s.Detach(client)
	br := b(s)

	// (a) Idle: Interrupt is a no-op — nothing in flight, no ESC emitted.
	if err := br.Interrupt(); err != nil {
		t.Fatalf("Interrupt(idle): %v", err)
	}
	if br.inFlight.Load() {
		t.Fatal("Interrupt at idle must not set inFlight")
	}
	// (b) Idle submit confirms no ESC is pending ahead of it.
	if err := br.SubmitVoiceTurn("first"); err != nil {
		t.Fatalf("SubmitVoiceTurn: %v", err)
	}
	if !drainContains(client.Out, "NO_ESC:-:first", 3*time.Second) {
		t.Fatal("no ESC should precede the first (idle) turn")
	}

	// (c) A turn is now in flight (SubmitVoiceTurn set it). Interrupt emits ESC and
	// clears the flag.
	if !br.inFlight.Load() {
		t.Fatal("SubmitVoiceTurn should have set inFlight")
	}
	if err := br.Interrupt(); err != nil {
		t.Fatalf("Interrupt(in-flight): %v", err)
	}
	if br.inFlight.Load() {
		t.Fatal("Interrupt must clear inFlight")
	}
	// (d) The next submit adds no ESC of its own (inFlight now false), so the ESC the
	// probe sees ahead of this line came from Interrupt.
	if err := br.SubmitVoiceTurn("second"); err != nil {
		t.Fatalf("SubmitVoiceTurn: %v", err)
	}
	if !drainContains(client.Out, "SAW_ESC:-:second", 3*time.Second) {
		t.Fatal("Interrupt's ESC must land ahead of the next submitted line")
	}
}

// TestSayTTSSynthesizesWav exercises the real macOS `say` Synthesizer end to end:
// it must produce a non-empty RIFF/WAVE buffer. Skipped where `say` is absent
// (non-macOS CI), since SayTTS is the documented local-only stopgap.
func TestSayTTSSynthesizesWav(t *testing.T) {
	tts := NewSayTTS()
	if err := tts.Ping(); err != nil {
		t.Skipf("say unavailable: %v", err)
	}
	if got := tts.OutputFormat(); got != "wav" {
		t.Errorf("OutputFormat = %q, want wav", got)
	}
	audio, err := tts.Synthesize(context.Background(), "hello from bbclaw")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(audio) < 44 { // a WAVE header alone is 44 bytes
		t.Fatalf("audio too short to be a WAV: %d bytes", len(audio))
	}
	if string(audio[:4]) != "RIFF" || string(audio[8:12]) != "WAVE" {
		t.Errorf("not a RIFF/WAVE buffer: % x", audio[:12])
	}

	// Empty text is rejected (nothing to speak).
	if _, err := tts.Synthesize(context.Background(), "   "); err == nil {
		t.Errorf("expected error for empty text")
	}
}

// TestSayTTSDrivesDeviceSink wires SayTTS into the full reply→TTS→sink speak path
// (bypassing the PTY) to prove the local TTS slots in behind the Synthesizer
// interface and its bytes reach the device. Skipped without `say`.
func TestSayTTSDrivesDeviceSink(t *testing.T) {
	tts := NewSayTTS()
	if err := tts.Ping(); err != nil {
		t.Skipf("say unavailable: %v", err)
	}
	sink := &recordingSink{}
	br := New(nil, nil, tts, sink, Config{})
	if err := br.speak(context.Background(), "speak this reply"); err != nil {
		t.Fatalf("speak: %v", err)
	}
	plays := sink.snapshot()
	if len(plays) != 1 {
		t.Fatalf("want 1 play, got %d", len(plays))
	}
	if plays[0].format != "wav" || len(plays[0].audio) < 44 {
		t.Errorf("sink got format=%q len=%d, want wav + real audio", plays[0].format, len(plays[0].audio))
	}
}

// --- helpers ---

// b builds a minimal Bridge for inject-only tests (no TTS/sink/ASR needed).
func b(s *session.Session) *Bridge {
	return New(s, nil, nil, nil, Config{Cols: 80, Rows: 24})
}

// drainContains reads from a session client channel until want is seen or the
// deadline passes.
func drainContains(ch <-chan []byte, want string, d time.Duration) bool {
	var sb strings.Builder
	deadline := time.After(d)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return strings.Contains(sb.String(), want)
			}
			sb.Write(chunk)
			if strings.Contains(sb.String(), want) {
				return true
			}
		case <-deadline:
			return strings.Contains(sb.String(), want)
		}
	}
}
