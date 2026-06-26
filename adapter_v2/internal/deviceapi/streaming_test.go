package deviceapi

import (
	"context"
	"sync"
	"testing"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/vtscreen"
)

// TestEmitToolStepsDedupAndDisplayOnly: tool-step bullets on screen produce one
// display-only ToolStep per distinct {name,hint} per turn (deduped across repaints),
// and never enter the reply/audio path.
func TestEmitToolStepsDedupAndDisplayOnly(t *testing.T) {
	ev := &recordingEvents{}
	b := &Bridge{screen: vtscreen.New(80, 24), events: ev}
	ts := &turnStream{}

	b.screen.Feed([]byte("\x1b[3;1H⏺ Bash(df -h)\r\n"))
	b.emitToolSteps(ts)
	b.emitToolSteps(ts) // same screen repaint → deduped, no second emit
	if len(ev.tools) != 1 || ev.tools[0] != "Bash|df -h" {
		t.Fatalf("want one ToolStep Bash|df -h, got %v", ev.tools)
	}

	b.screen.Feed([]byte("\x1b[4;1H⏺ Read(main.go)\r\n"))
	b.emitToolSteps(ts)
	if len(ev.tools) != 2 || ev.tools[1] != "Read|main.go" {
		t.Fatalf("want second ToolStep Read|main.go, got %v", ev.tools)
	}
	if len(ev.deltas) != 0 || len(ev.complete) != 0 {
		t.Errorf("tool steps must not touch the reply/audio path: deltas=%v complete=%v", ev.deltas, ev.complete)
	}
}

// TestSeedVisibleToolStepsNoReplay: a prior turn's tool bullet still painted on the
// grid when a new turn baselines must NOT replay into the new turn (regression for
// "an earlier device set-volume showed up during a later weather question"). Only
// bullets painted DURING the new turn should emit.
func TestSeedVisibleToolStepsNoReplay(t *testing.T) {
	ev := &recordingEvents{}
	b := &Bridge{screen: vtscreen.New(80, 24), events: ev}

	// Prior turn left a tool bullet on screen.
	b.screen.Feed([]byte("\x1b[3;1H⏺ Bash(\"$BIN\" device set-volume 5)\r\n"))

	// New turn baselines against the current screen: seed what's already visible.
	ts := &turnStream{}
	b.seedVisibleToolSteps(ts)

	// First paint(s) of the new turn: the lingering prior bullet must not replay.
	b.emitToolSteps(ts)
	if len(ev.tools) != 0 {
		t.Fatalf("prior-turn tool bullet replayed into new turn: %v", ev.tools)
	}

	// A genuinely new bullet painted this turn still emits.
	b.screen.Feed([]byte("\x1b[4;1H⏺ Read(weather.go)\r\n"))
	b.emitToolSteps(ts)
	if len(ev.tools) != 1 || ev.tools[0] != "Read|weather.go" {
		t.Fatalf("want new-turn ToolStep Read|weather.go, got %v", ev.tools)
	}
}

func TestNextSentence(t *testing.T) {
	cases := []struct {
		in       string
		sentence string
		consumed string
	}{
		{"", "", ""},
		{"no terminator yet", "", ""}, // still streaming → wait
		{"Hello world.", "Hello world.", "Hello world."},
		{"Hello. Next", "Hello.", "Hello. "}, // swallows the trailing space
		{"你好。还有", "你好。", "你好。"},            // CJK full stop (3-byte rune)
		{"Done!\nmore", "Done!", "Done!\n"},  // newline terminator + swallow
		{"a? b? c", "a?", "a? "},             // first sentence only
	}
	for _, c := range cases {
		gotS, gotC := nextSentence(c.in)
		if gotS != c.sentence || gotC != c.consumed {
			t.Errorf("nextSentence(%q) = (%q,%q), want (%q,%q)", c.in, gotS, gotC, c.sentence, c.consumed)
		}
	}
}

// recordingEvents captures the Events callbacks for assertions.
type recordingEvents struct {
	mu       sync.Mutex
	deltas   []string
	complete []string
	tools    []string // "name|hint" per ToolStep
	idles    int
}

func (e *recordingEvents) ReplyDelta(text string)    { e.mu.Lock(); e.deltas = append(e.deltas, text); e.mu.Unlock() }
func (e *recordingEvents) ReplyComplete(text string) { e.mu.Lock(); e.complete = append(e.complete, text); e.mu.Unlock() }
func (e *recordingEvents) ToolStep(name, hint string) {
	e.mu.Lock()
	e.tools = append(e.tools, name+"|"+hint)
	e.mu.Unlock()
}
func (e *recordingEvents) TurnIdle() { e.mu.Lock(); e.idles++; e.mu.Unlock() }

// TestOnProgressStreamsDeltas: with StreamReplyDelta on, each new reply snapshot
// fires one ReplyDelta; unchanged text does not; blank text is suppressed.
func TestOnProgressStreamsDeltas(t *testing.T) {
	ev := &recordingEvents{}
	b := &Bridge{cfg: Config{StreamReplyDelta: true}, events: ev}
	ts := &turnStream{}

	_ = b.onProgress(context.Background(), "", ts)         // blank → no delta
	_ = b.onProgress(context.Background(), "Hello", ts)    // new → delta
	_ = b.onProgress(context.Background(), "Hello", ts)    // unchanged → no delta
	_ = b.onProgress(context.Background(), "Hello world", ts) // grew → delta

	if got := len(ev.deltas); got != 2 {
		t.Fatalf("deltas = %v (len %d), want 2 [Hello, Hello world]", ev.deltas, got)
	}
	if ev.deltas[0] != "Hello" || ev.deltas[1] != "Hello world" {
		t.Errorf("deltas = %v, want [Hello, Hello world]", ev.deltas)
	}
}

// TestOnProgressDeltaOffIsSilent: default config emits no deltas.
func TestOnProgressDeltaOff(t *testing.T) {
	ev := &recordingEvents{}
	b := &Bridge{cfg: Config{}, events: ev}
	_ = b.onProgress(context.Background(), "Hello", &turnStream{})
	if len(ev.deltas) != 0 {
		t.Errorf("deltas = %v, want none when StreamReplyDelta is off", ev.deltas)
	}
}

// TestOnProgressSuppressesSeed is the regression guard for the cross-turn leak:
// claude's extractor surfaces the LAST "⏺" reply block, so at the start of a new
// turn the PRIOR turn's reply is still extracted until this turn paints. onProgress
// must NOT stream/speak that seed text (it would leak the previous answer, or on a
// barge-in the just-interrupted one). Once distinct text appears, streaming flows.
func TestOnProgressSuppressesSeed(t *testing.T) {
	ev := &recordingEvents{}
	tts := &countingTTS{format: "pcm16", out: []byte("AUD")}
	sink := &recordingSink{}
	b := &Bridge{cfg: Config{StreamReplyDelta: true, SegmentTTS: true}, events: ev, tts: tts, sink: sink}
	// New turn opened with the prior turn's reply still on screen as the seed.
	ts := &turnStream{seed: "ANSWER: alpha."}

	// While the screen still shows the prior reply: no delta, no audio.
	_ = b.onProgress(context.Background(), "ANSWER: alpha.", ts)
	if len(ev.deltas) != 0 {
		t.Errorf("leaked prior reply as a delta: %v", ev.deltas)
	}
	if got := tts.calls(); len(got) != 0 {
		t.Errorf("re-spoke prior reply: %v", got)
	}

	// This turn paints its own distinct reply → streaming resumes.
	_ = b.onProgress(context.Background(), "ANSWER: bravo.", ts)
	if len(ev.deltas) != 1 || ev.deltas[0] != "ANSWER: bravo." {
		t.Errorf("deltas = %v, want [ANSWER: bravo.] once seed cleared", ev.deltas)
	}
	if got := tts.calls(); len(got) != 1 || got[0] != "ANSWER: bravo." {
		t.Errorf("tts calls = %v, want [ANSWER: bravo.]", got)
	}
}

// TestOnProgressSegmentTTSSpeaksPerSentence: with SegmentTTS on, a completed
// sentence is synthesised as soon as it appears; the trailing incomplete one waits.
func TestOnProgressSegmentTTSSpeaksPerSentence(t *testing.T) {
	tts := &countingTTS{format: "pcm16", out: []byte("AUD")}
	sink := &recordingSink{}
	b := &Bridge{cfg: Config{SegmentTTS: true}, tts: tts, sink: sink}
	ts := &turnStream{}

	// Reply streams in: first sentence complete, second still partial, then complete.
	_ = b.onProgress(context.Background(), "Hello there.", ts)         // speak "Hello there."
	_ = b.onProgress(context.Background(), "Hello there. How are", ts) // partial 2nd → wait
	_ = b.onProgress(context.Background(), "Hello there. How are you?", ts) // speak "How are you?"

	calls := tts.calls()
	if len(calls) != 2 {
		t.Fatalf("tts calls = %v (len %d), want 2 sentences", calls, len(calls))
	}
	if calls[0] != "Hello there." {
		t.Errorf("first spoken = %q, want %q", calls[0], "Hello there.")
	}
	if calls[1] != "How are you?" {
		t.Errorf("second spoken = %q, want %q", calls[1], "How are you?")
	}
	// The spoken prefix advanced past both sentences.
	if ts.spoken != "Hello there. How are you?" {
		t.Errorf("spoken prefix = %q, want full text", ts.spoken)
	}
}
