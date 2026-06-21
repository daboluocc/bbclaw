// Package deviceapi bridges the extracted plain-text reply stream to the BBClaw
// device protocol: an ASR transcript comes IN and is injected as a PTY user
// turn; the extracted, turn-complete Reply goes OUT through TTS to the device's
// speaker.
//
// It is the only v2 package that knows about the device-facing contract. Per the
// accepted tradeoffs in DESIGN.md §2, the device side only ever needs plain
// reply text + speech — no thinking blocks, tool-approval events, dispatch
// progress, or token counts. That keeps this layer small: read text, speak it.
//
// # How the pieces fit
//
//	PTT audio ─Recognizer.Transcribe─▶ SubmitVoiceTurn ─▶ session.Write (PTY stdin)
//	                                                            │
//	            PTY stdout bytes ─▶ session Client ─▶ Bridge.Run drives:
//	                                                   vtscreen.Screen.Feed
//	                                                   extract.Extractor.OnOutput
//	                                                   extract.Detector.TurnEnded
//	                                                            │ (turn complete)
//	                                          Synthesizer.Synthesize ─▶ DeviceSink.Play
//
// The Bridge attaches to the Session as an ordinary raw-byte Client (the same
// stream the phone/web terminal sees) and runs its OWN vtscreen mirror, so the
// extraction state is private to the device line and never contends with the
// Session's broadcast screen.
//
// # ASR / TTS reuse (deliberate follow-up)
//
// v1 ships a full asr/tts/audio stack (Doubao native, OpenAI/Whisper, local
// command, codecs). Porting that whole stack is out of scope for this issue: it
// drags in HTTP providers, env config, and audio codecs that the Phase 2
// device-line skeleton does not need yet. Instead this package defines small
// Recognizer / Synthesizer / DeviceSink interfaces and ships a self-contained
// macOS `say` TTS (tts_local.go) plus a mock ASR (mock.go) so the inject → reply
// → boundary → TTS path is end-to-end testable today. Wiring v1's real providers
// behind these interfaces is the documented next step (see DESIGN.md §7).
package deviceapi

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/extract"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/vtscreen"
)

// ErrClosed is returned by SubmitVoiceTurn / Run when the underlying session's
// PTY has already exited.
var ErrClosed = errors.New("deviceapi: session closed")

// interruptKey is the byte we inject before a new turn to abort an in-flight one.
// claude (and the other agent TUIs) bind ESC to "interrupt" — the boundary
// detector keys off the very "esc to interrupt" spinner affordance — so a single
// ESC drops the running turn and returns the CLI to its idle prompt, ready to
// accept the next user turn cleanly. See SubmitVoiceTurn for why we interrupt
// rather than queue.
const interruptKey = "\x1b"

// enterKey terminates an injected transcript so the CLI treats it as a submitted
// user turn (carriage return is what a real Enter sends over a PTY).
const enterKey = "\r"

// interruptSettle is the gap between an interrupt ESC and the following
// transcript. A terminal distinguishes a lone ESC key from an escape SEQUENCE by
// timing: an ESC immediately followed by a byte is read as Alt+<byte> / a
// sequence, which would swallow the transcript's first character — harmless-ish
// for ASCII ("reply"→"eply") but fatal for a multibyte CJK rune (ESC eats the
// lead byte, leaving invalid UTF-8 that the CLI drops). So when we do send an
// interrupt, we let it land as its own keystroke before typing.
const interruptSettle = 60 * time.Millisecond

// Recognizer turns a PTT audio buffer into transcript text. It is the device
// line's ASR entry point. Kept deliberately narrower than v1's asr.Provider
// (which threads rich Metadata and segment timing): the device line only needs
// the final text. A v1 provider is adapted behind this interface later.
type Recognizer interface {
	// Transcribe returns the recognised text for one PTT utterance.
	Transcribe(ctx context.Context, audio []byte) (string, error)
}

// Synthesizer turns reply text into an audio buffer for the device speaker.
// Mirrors v1's tts.Provider shape (Synthesize + OutputFormat) so a v1 provider
// drops in behind it unchanged.
type Synthesizer interface {
	// Synthesize renders text to an audio buffer in OutputFormat()'s encoding.
	Synthesize(ctx context.Context, text string) ([]byte, error)
	// OutputFormat names the encoding of Synthesize's bytes (e.g. "wav",
	// "pcm16"), so the device transport knows whether to transcode.
	OutputFormat() string
}

// DeviceSink is the device-facing transport: it accepts a synthesised audio
// buffer and plays / streams it to the BBClaw device. Phase 2 stubs this; the
// real implementation pushes over the device's audio channel.
type DeviceSink interface {
	// Play delivers one synthesised reply's audio (in the given format) to the
	// device. It should block until the buffer is accepted (not necessarily until
	// playback finishes).
	Play(ctx context.Context, audio []byte, format string) error
}

// Events lets a transport (e.g. the bbwire device WebSocket) observe the turn
// lifecycle so it can emit its own protocol frames around the Bridge's autonomous
// reply→TTS loop. All methods are invoked from Bridge.Run's goroutine, in order:
// ReplyComplete (the boundary fired, final reply text) → the Synthesizer/Sink run
// → TurnIdle (the device may speak again). A nil Events (the default) is a no-op,
// so the voice-only Bridge is unaffected. Optional; set via SetEvents.
type Events interface {
	// ReplyDelta reports the assistant reply text AS IT GROWS during a turn (Phase
	// B streaming), so the device screen can update live before the turn ends. The
	// text is the FULL current reply (a snapshot, not an append-delta): the device
	// replaces its subtitle with it, which is robust to the TUI rewriting/reflowing
	// the line. Only emitted when Config.StreamReplyDelta is set; ReplyComplete is
	// always the authoritative final text. The transport emits its reply.delta frame.
	ReplyDelta(text string)
	// ReplyComplete reports the final reply text of a turn, before it is spoken.
	// The transport emits its reply.end frame here.
	ReplyComplete(text string)
	// TurnIdle reports the turn is fully done (spoken) and the device may PTT
	// again. The transport emits its turn{idle} frame here.
	TurnIdle()
}

// Config tunes the Bridge. The zero value is usable: cols/rows fall back to the
// vtscreen defaults and pollInterval to a sane tick.
type Config struct {
	// Cols, Rows size the Bridge's private VT mirror. Match the session's grid so
	// the device-line extraction sees the same wrapping the terminal client does.
	Cols, Rows int
	// PollInterval is how often Run re-evaluates the turn boundary between PTY
	// chunks, so a turn that ends on a quiet screen (no final byte to wake us) is
	// still detected promptly. Defaults to a fraction of extract.Quiet.
	PollInterval time.Duration

	// StreamReplyDelta (Phase B) emits Events.ReplyDelta as the reply grows, for a
	// live device subtitle. Low risk — ReplyComplete remains authoritative — so
	// it is safe to default on. Off → the device only sees the reply at turn end.
	StreamReplyDelta bool

	// SegmentTTS (Phase B) speaks the reply sentence-by-sentence as it streams, so
	// the speaker starts on sentence 1 while sentence 2 is still being scraped.
	// Higher risk (sentence segmentation of in-progress text; a TUI rewrite can
	// cause a re-speak), so it is opt-in. Off → one-shot TTS of the full reply at
	// turn end (the safe Phase A behaviour).
	SegmentTTS bool

	// Warmup makes Run drive the freshly-spawned claude past its startup — confirm
	// the "trust this folder?" dialog and wait for the idle "❯" prompt — BEFORE the
	// first turn is injected, so the first SubmitVoiceTurn isn't swallowed by the
	// trust menu (which otherwise eats turn 1 → a 90s timeout, empty reply). The
	// device paths (cloud relay, LAN) set it; tests that drive SubmitVoiceTurn
	// without Run leave it off so they never block on the ready gate.
	Warmup bool
}

const defaultPollInterval = 150 * time.Millisecond

// Bridge couples one device to one session: ASR transcripts in via
// SubmitVoiceTurn, completed replies out via Run → Synthesizer → DeviceSink.
type Bridge struct {
	sess   *session.Session
	asr    Recognizer
	tts    Synthesizer
	sink   DeviceSink
	cfg    Config
	events Events // optional turn-lifecycle observer (set via SetEvents)

	// screen is the Bridge's OWN VT mirror of the PTY output, fed by Run from the
	// raw byte stream it receives as a session Client. It is separate from the
	// Session's broadcast screen so the device-line extraction state is isolated.
	screen *vtscreen.Screen

	// inFlight is true while a turn we injected is still being worked by the CLI
	// (set when SubmitVoiceTurn injects, cleared when maybeSpeak sees the turn
	// complete). It gates the interrupt: we only send ESC to abort a turn that is
	// actually running. At an idle prompt there is nothing to interrupt, and a
	// gratuitous ESC there corrupts the next keystroke (see interruptSettle), so
	// the common sequential case (and the first turn) injects cleanly with no ESC.
	inFlight atomic.Bool

	// rearm is the cross-goroutine signal that a new user turn was just injected,
	// so Run must re-baseline the Extractor and reset the Detector for that turn.
	// SubmitVoiceTurn (the injecting goroutine) does not own the ext/det locals —
	// they live in Run — so it cannot re-arm them directly; it pokes this channel
	// instead and Run drains it in its select. Buffered cap 1 with a non-blocking
	// send: injection never blocks on Run, and a burst of rapid injections that
	// out-races Run collapses to a single re-arm (the latest screen baseline is
	// what matters; see Run's drain). This is the crux of the in-flight INTERRUPT
	// policy (DESIGN.md §8): without it a barge-in's partial prior reply would
	// stay in the baseline and leak into the next spoken turn.
	rearm chan struct{}

	// ready is closed once the session is ready for turns. With Config.Warmup it is
	// closed by Run after warmup (trust cleared, idle prompt up); SubmitVoiceTurn
	// blocks on it so the first turn isn't injected into claude's startup. Without
	// Warmup it is closed at New, so SubmitVoiceTurn never waits.
	ready chan struct{}
}

// New builds a device bridge over an existing session. asr/tts/sink supply the
// device I/O; any may be nil if the corresponding direction is unused (e.g. a
// text-only test passes a nil Recognizer and drives SubmitVoiceTurn directly).
func New(sess *session.Session, asr Recognizer, tts Synthesizer, sink DeviceSink, cfg Config) *Bridge {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	b := &Bridge{
		sess:   sess,
		asr:    asr,
		tts:    tts,
		sink:   sink,
		cfg:    cfg,
		screen: vtscreen.New(cfg.Cols, cfg.Rows),
		rearm:  make(chan struct{}, 1),
		ready:  make(chan struct{}),
	}
	if !cfg.Warmup {
		close(b.ready) // no warmup → SubmitVoiceTurn never gates
	}
	return b
}

// SetEvents attaches a turn-lifecycle observer. Call before Run. Passing nil
// clears it. Used by the device-WS transport to emit reply.end / turn frames.
func (b *Bridge) SetEvents(e Events) { b.events = e }

// SubmitVoicePTT is the full PTT entry point: recognise the audio, then inject
// the transcript as a user turn. Empty/whitespace transcripts are dropped (a
// silent or unrecognised utterance must not poke the CLI).
func (b *Bridge) SubmitVoicePTT(ctx context.Context, audio []byte) error {
	if b.asr == nil {
		return errors.New("deviceapi: no recognizer configured")
	}
	text, err := b.asr.Transcribe(ctx, audio)
	if err != nil {
		return err
	}
	if isBlank(text) {
		return nil // nothing recognised; do not inject an empty turn
	}
	return b.SubmitVoiceTurn(text)
}

// SubmitVoiceTurn injects transcript + Enter into the PTY stdin as one user
// turn.
//
// In-flight policy: INTERRUPT (chosen over queue, documented). If the CLI is
// still working a previous turn when the user speaks again, we send ESC first to
// abort that turn, then submit the new transcript. Rationale for a voice device:
// a person who barges in mid-reply means "stop, listen to this instead" — the
// natural turn-taking of speech. Queueing would make the device speak a now-stale
// answer before getting to what the user actually just asked, which feels broken
// over voice. ESC is the affordance claude itself advertises ("esc to interrupt"),
// so this rides the CLI's own cancel path rather than fighting it.
//
// Echo: we do NOT echo the transcript back to the device speaker. The injected
// text appears in the terminal view (phone/web) like any keystroke; the device
// line only voices the assistant's reply, not the user's own words.
//
// A blank transcript is a no-op so a misfire never interrupts a live turn.
func (b *Bridge) SubmitVoiceTurn(transcript string) error {
	if isBlank(transcript) {
		return nil
	}
	// Wait until the session is ready (warmup done: trust cleared, idle prompt up),
	// so the first turn isn't swallowed by claude's startup. Closed at New when
	// warmup is off, so this is a no-op for the non-device paths.
	<-b.ready
	// Interrupt ONLY a turn that is actually in flight (a barge-in). At an idle
	// prompt there is nothing to abort, and a gratuitous ESC there is read as the
	// start of an escape sequence that swallows the transcript's first character
	// (fatal for a leading CJK rune). When we do interrupt, let the ESC land as
	// its own keystroke before typing (interruptSettle) so it is not glued to the
	// first byte of the transcript.
	if b.inFlight.Load() {
		if err := b.sess.Write([]byte(interruptKey)); err != nil {
			return mapErr(err)
		}
		time.Sleep(interruptSettle)
	}
	if err := b.sess.Write([]byte(transcript + enterKey)); err != nil {
		return mapErr(err)
	}
	b.inFlight.Store(true)
	// A new user turn is now in flight. Signal Run to re-baseline the Extractor on
	// the (possibly interrupted) current screen and reset the Detector's settle
	// clock, so this turn's reply is isolated from the prior one and a barge-in
	// never speaks the interrupted partial. See the rearm field and Run's drain.
	b.signalRearm()
	return nil
}

// signalRearm performs a non-blocking poke of the rearm channel: if Run has not
// yet consumed a prior signal, the buffer is already full and we drop this one
// (coalescing), since one re-baseline against the latest screen covers any burst
// of injections. Never blocks the injecting goroutine on Run.
func (b *Bridge) signalRearm() {
	select {
	case b.rearm <- struct{}{}:
	default:
	}
}

// Run drives the reply → TTS → device loop until the session's PTY exits or ctx
// is cancelled. It attaches to the session as a raw-byte Client, feeds every
// chunk into its private VT mirror, and on each completed turn (extract.Detector
// says the boundary passed) synthesises the extracted reply and pushes it to the
// device.
//
// One Extractor + Detector pair is built per turn: a fresh Extractor baselines
// the current screen so it surfaces only the NEW reply, and the Detector's
// per-turn state is reset. We re-baseline at two moments: (1) when a new user
// turn is injected (the rearm signal from SubmitVoiceTurn), and (2) after a turn
// is spoken. Re-arming at injection is what honours the "construct a fresh
// Extractor the moment a new user turn is injected" contract in package extract
// and the in-flight INTERRUPT policy in DESIGN.md §8: on a barge-in the prior
// turn may still be streaming (no completed-turn re-baseline fired), so without
// this the interrupted partial reply would stay outside the baseline and leak
// into the next spoken turn, and a stray quiet window could even let the
// Detector speak the interrupted partial. The rearm re-baseline against the
// post-ESC screen excludes that partial and restarts the settle clock.
func (b *Bridge) Run(ctx context.Context) error {
	client, scrollback, snapshot := b.sess.Attach()
	defer b.sess.Detach(client)

	// Replay the attach snapshot into our mirror so the baseline reflects whatever
	// was already on screen when we joined (an in-progress session), not a blank
	// grid. Scrollback is history above the visible grid; feeding it is harmless
	// and keeps the mirror faithful.
	for _, chunk := range scrollback {
		b.screen.Feed(chunk)
	}
	b.screen.Feed(snapshot)

	// Warmup: drive claude past startup (confirm the trust dialog, wait for the idle
	// "❯" prompt) before accepting turns, then open the ready gate. Without it the
	// first injected turn is swallowed by the trust menu. No-op (gate already open)
	// when Config.Warmup is off.
	if b.cfg.Warmup {
		b.warmup(ctx, client)
		close(b.ready)
	}

	ext := extract.New(b.screen)
	det := &extract.Detector{}
	ts := &turnStream{}

	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()

	// rebaseline starts a fresh turn: a new Extractor baselined on the current
	// screen, a reset Detector, and cleared streaming state. It also captures the
	// reply currently on screen as the turn's `seed`: claude's extractor surfaces
	// the LAST "⏺" reply block regardless of baseline, so the PRIOR turn's reply is
	// still extracted until this turn paints its own. onProgress suppresses its
	// streaming side-effects while the extracted text equals that seed, so a new
	// turn (or a barge-in) never streams/speaks the previous turn's answer — the
	// Phase B analogue of the rearm guard in DESIGN.md §8. The boundary path
	// (maybeSpeak) is unaffected, so an identical repeated reply is still spoken.
	rebaseline := func() {
		ext = extract.New(b.screen)
		det.Reset()
		seed, _ := ext.OnOutput()
		*ts = turnStream{seed: seed.Text}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-b.rearm:
			// A new user turn was just injected (SubmitVoiceTurn / SubmitVoicePTT).
			// Re-baseline on the current screen — which, after the injected ESC, has
			// the interrupted prior reply cleared back toward the idle prompt — so
			// extract() excludes it, and restart the Detector's settle clock so the
			// boundary fires on THIS turn, not the partial we just interrupted.
			rebaseline()

		case chunk, ok := <-client.Out:
			if !ok {
				return ErrClosed // PTY exited; session closed the channel
			}
			b.reconcileMirrorSize()
			b.screen.Feed(chunk)
			reply, _ := ext.OnOutput()
			det.Observe(time.Now(), b.screen, len(chunk) > 0)
			if err := b.onProgress(ctx, reply.Text, ts); err != nil {
				return err
			}
			if spoke, err := b.maybeSpeak(ctx, det, reply.Text, ts); err != nil {
				return err
			} else if spoke {
				rebaseline()
			}

		case <-ticker.C:
			// No new bytes: re-check the boundary so a turn that ended on a quiet
			// screen (the settle window elapsing after the last chunk) is still
			// detected without waiting for the next unrelated chunk.
			det.Observe(time.Now(), b.screen, false)
			reply, _ := ext.OnOutput()
			if spoke, err := b.maybeSpeak(ctx, det, reply.Text, ts); err != nil {
				return err
			} else if spoke {
				rebaseline()
			}
		}
	}
}

const (
	// warmupTimeout bounds warmup so a CLI that never reaches an idle prompt can't
	// wedge the device line; on timeout we open the gate and let the first turn try.
	warmupTimeout = 20 * time.Second
	// warmupMinWait is the minimum time before a "settled" screen counts as ready,
	// so claude's trust dialog (which renders ~1-2s after spawn) has time to appear
	// and be handled rather than being missed by an early settle.
	warmupMinWait = 2500 * time.Millisecond
	// warmupSettle is the quiet window (no new PTY bytes) that, for a CLI without
	// claude's "❯" glyph, signals it has finished booting and is ready for input.
	warmupSettle = 800 * time.Millisecond
)

// warmup consumes PTY output until the freshly-spawned CLI is ready for a turn.
// claude shows a "trust this folder?" dialog the first time it runs in a new cwd;
// left alone, the first injected transcript lands IN that menu (its Enter just
// confirms trust) and the turn is lost to a 90s timeout. So warmup confirms the
// dialog (Enter = the default "1. Yes, I trust") and waits until the CLI is at its
// idle input prompt — detected by claude's "❯" glyph, or, for a generic CLI, by
// output going quiet after a minimum wait. Bounded by warmupTimeout.
func (b *Bridge) warmup(ctx context.Context, client *session.Client) {
	start := time.Now()
	deadline := time.After(warmupTimeout)
	tick := time.NewTicker(b.cfg.PollInterval)
	defer tick.Stop()
	var lastByte time.Time
	trustCleared := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return // proceed anyway — an early turn beats a permanent wedge
		case chunk, ok := <-client.Out:
			if !ok {
				return // PTY exited
			}
			b.screen.Feed(chunk)
			lastByte = time.Now()
		case <-tick.C:
		}
		visible := b.screen.VisibleText()
		if isTrustPrompt(visible) {
			if !trustCleared {
				_ = b.sess.Write([]byte(enterKey)) // confirm the default "1. Yes, I trust"
				trustCleared = true
			}
			continue // wait for the dialog to clear before declaring ready
		}
		if strings.Contains(visible, "❯") {
			return // claude's idle input prompt is up → ready
		}
		if !lastByte.IsZero() && time.Since(start) > warmupMinWait && time.Since(lastByte) > warmupSettle {
			return // generic CLI: booted and quiet → ready
		}
	}
}

// reconcileMirrorSize keeps the Bridge's private VT mirror the same size as the
// shared session grid. The default session is shared: a web terminal client can
// resize the PTY (termchan forwards resize regardless of write ownership), which
// reflows claude's output. If the mirror stayed at its construction size the
// reflowed bytes would wrap differently than the live PTY and corrupt extraction
// (the "mirror != PTY" hazard). Called each loop before feeding new bytes.
func (b *Bridge) reconcileMirrorSize() {
	sz := b.sess.Size()
	if sz.Cols == 0 || sz.Rows == 0 {
		return
	}
	if cols, rows := b.screen.Size(); cols != int(sz.Cols) || rows != int(sz.Rows) {
		b.screen.Resize(int(sz.Cols), int(sz.Rows))
	}
}

// isTrustPrompt reports whether the screen is claude's first-run "trust this
// folder?" safety dialog (matched on its stable option/body wording).
func isTrustPrompt(visible string) bool {
	return strings.Contains(visible, "trust this folder") ||
		strings.Contains(visible, "trust the files")
}

// turnStream is Run's per-turn streaming state for Phase B: what reply text has
// been pushed as a delta, and what text has already been spoken sentence-by-
// sentence. Reset at the start of every turn.
type turnStream struct {
	seed      string // the prior turn's reply, still on screen until this turn paints; suppressed by onProgress
	lastDelta string // last full reply text emitted via Events.ReplyDelta
	spoken    string // reply prefix already synthesised by per-segment TTS
}

// onProgress runs the Phase B streaming side-effects on each extraction advance:
// emit a live reply.delta (snapshot of the full current reply) and, if per-
// segment TTS is on, speak any newly-completed sentence. Both are no-ops under
// the safe defaults (StreamReplyDelta off → no delta; SegmentTTS off → all audio
// happens once at turn end in maybeSpeak).
func (b *Bridge) onProgress(ctx context.Context, text string, ts *turnStream) error {
	// Suppress while the screen still shows the PRIOR turn's reply (the seed): this
	// turn hasn't produced its own output yet, so streaming it would leak the
	// previous answer (or, on a barge-in, the just-interrupted one). Once this turn
	// paints distinct text the guard clears for the rest of the turn.
	if text == ts.seed {
		return nil
	}
	if b.cfg.StreamReplyDelta && text != ts.lastDelta {
		ts.lastDelta = text
		if b.events != nil && !isBlank(text) {
			b.events.ReplyDelta(text)
		}
	}
	if b.cfg.SegmentTTS {
		// Append-only segmentation: only speak a sentence when the new text extends
		// what we've already spoken (a TUI rewrite that breaks the prefix is left
		// for the turn-end speak in maybeSpeak, avoiding mid-turn double audio).
		if rest, ok := strings.CutPrefix(text, ts.spoken); ok {
			seg, consumed := nextSentence(rest)
			if seg != "" && !isBlank(seg) {
				ts.spoken += consumed
				if err := b.speak(ctx, seg); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// maybeSpeak checks the turn boundary and, if the turn just completed, speaks the
// reply (or its unspoken tail under per-segment TTS) and reports spoke=true so the
// caller re-baselines for the next turn. text is the latest extracted reply.
func (b *Bridge) maybeSpeak(ctx context.Context, det *extract.Detector, text string, ts *turnStream) (bool, error) {
	if !det.TurnEnded(time.Now()) {
		return false, nil
	}
	// The turn the user injected is done; a later barge-in now has nothing to
	// interrupt, so the next SubmitVoiceTurn injects cleanly without an ESC.
	b.inFlight.Store(false)

	if isBlank(text) {
		// A completed turn with no speakable text (e.g. a tool-only turn) still
		// hands control back: tell the transport the device may speak again.
		if b.events != nil {
			b.events.TurnIdle()
		}
		return true, nil
	}
	// Emit the authoritative final text (transport sends reply.end) before any
	// remaining audio, so the device screen shows the full answer.
	if b.events != nil {
		b.events.ReplyComplete(text)
	}
	// Speak the part not already spoken. Under per-segment TTS this is just the
	// trailing sentence; otherwise (the default) it is the whole reply (one-shot).
	tail := text
	if b.cfg.SegmentTTS {
		if rest, ok := strings.CutPrefix(text, ts.spoken); ok {
			tail = rest // speak only the unspoken remainder
		}
		// If the prefix broke (a rewrite), tail stays the full text — re-speak the
		// reply rather than leave the device with a half-spoken answer.
	}
	if !isBlank(tail) {
		if err := b.speak(ctx, tail); err != nil {
			return true, err
		}
	}
	if b.events != nil {
		b.events.TurnIdle()
	}
	return true, nil
}

// sentenceEnders terminate a spoken segment for per-segment TTS — Latin and CJK
// full stops, exclamation, question marks, plus the newline that separates a
// finished line of reply.
const sentenceEnders = ".!?。！？\n"

// nextSentence returns the first complete sentence at the start of s (up to and
// including its terminator) and the exact text consumed (so the caller can
// advance its spoken prefix). It returns ("","") when s holds no terminator yet
// — i.e. the sentence is still streaming in, so we wait rather than speak a
// fragment. Trailing whitespace after the terminator is included in `consumed`
// so the next sentence starts clean.
func nextSentence(s string) (sentence, consumed string) {
	idx := strings.IndexAny(s, sentenceEnders)
	if idx < 0 {
		return "", ""
	}
	end := idx + 1 // include the terminator rune's first byte position
	// Advance past a multi-byte terminator rune (。！？ are 3 bytes in UTF-8).
	for end < len(s) && s[end]&0xC0 == 0x80 {
		end++
	}
	consumed = s[:end]
	// Swallow following spaces/newlines into consumed so the next segment is clean.
	for end < len(s) && (s[end] == ' ' || s[end] == '\n' || s[end] == '\t') {
		end++
	}
	return strings.TrimSpace(consumed), s[:end]
}

// speak synthesises one reply and hands the audio to the device sink. A nil
// Synthesizer or sink makes this a no-op (a half-wired Bridge still drains the
// stream without panicking).
func (b *Bridge) speak(ctx context.Context, text string) error {
	if b.tts == nil || b.sink == nil {
		return nil
	}
	audio, err := b.tts.Synthesize(ctx, text)
	if err != nil {
		return err
	}
	return b.sink.Play(ctx, audio, b.tts.OutputFormat())
}

// mapErr translates a session-closed error into the package's ErrClosed so
// callers see a stable sentinel regardless of the lower layer's wording.
func mapErr(err error) error {
	if errors.Is(err, session.ErrClosed) {
		return ErrClosed
	}
	return err
}

// isBlank reports whether s is empty or only whitespace, so blank transcripts /
// replies are dropped rather than injected or spoken.
func isBlank(s string) bool {
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
		default:
			return false
		}
	}
	return true
}
