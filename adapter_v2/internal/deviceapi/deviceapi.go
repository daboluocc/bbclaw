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
}

const defaultPollInterval = 150 * time.Millisecond

// Bridge couples one device to one session: ASR transcripts in via
// SubmitVoiceTurn, completed replies out via Run → Synthesizer → DeviceSink.
type Bridge struct {
	sess *session.Session
	asr  Recognizer
	tts  Synthesizer
	sink DeviceSink
	cfg  Config

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
}

// New builds a device bridge over an existing session. asr/tts/sink supply the
// device I/O; any may be nil if the corresponding direction is unused (e.g. a
// text-only test passes a nil Recognizer and drives SubmitVoiceTurn directly).
func New(sess *session.Session, asr Recognizer, tts Synthesizer, sink DeviceSink, cfg Config) *Bridge {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	return &Bridge{
		sess:   sess,
		asr:    asr,
		tts:    tts,
		sink:   sink,
		cfg:    cfg,
		screen: vtscreen.New(cfg.Cols, cfg.Rows),
		rearm:  make(chan struct{}, 1),
	}
}

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

	ext := extract.New(b.screen)
	det := &extract.Detector{}

	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-b.rearm:
			// A new user turn was just injected (SubmitVoiceTurn / SubmitVoicePTT).
			// Re-baseline on the current screen — which, after the injected ESC, has
			// the interrupted prior reply cleared back toward the idle prompt — so
			// extract() excludes it, and restart the Detector's settle clock so the
			// boundary fires on THIS turn, not the partial we just interrupted. This
			// is the same re-arm maybeSpeak runs after a completed turn.
			ext = extract.New(b.screen)
			det.Reset()

		case chunk, ok := <-client.Out:
			if !ok {
				return ErrClosed // PTY exited; session closed the channel
			}
			b.screen.Feed(chunk)
			ext.OnOutput() // advance extracted text; we read it at boundary time
			det.Observe(time.Now(), b.screen, len(chunk) > 0)
			if err := b.maybeSpeak(ctx, det, &ext); err != nil {
				return err
			}

		case <-ticker.C:
			// No new bytes: re-check the boundary so a turn that ended on a quiet
			// screen (the settle window elapsing after the last chunk) is still
			// detected without waiting for the next unrelated chunk.
			det.Observe(time.Now(), b.screen, false)
			if err := b.maybeSpeak(ctx, det, &ext); err != nil {
				return err
			}
		}
	}
}

// maybeSpeak checks the turn boundary and, if the turn just completed, speaks the
// extracted reply and re-arms the extractor for the next turn. extP points at the
// caller's Extractor slot so a fresh Extractor (re-baselined on the current
// screen) replaces it once the turn is consumed.
func (b *Bridge) maybeSpeak(ctx context.Context, det *extract.Detector, extP **extract.Extractor) error {
	if !det.TurnEnded(time.Now()) {
		return nil
	}
	// The turn the user injected is done; a later barge-in now has nothing to
	// interrupt, so the next SubmitVoiceTurn injects cleanly without an ESC.
	b.inFlight.Store(false)
	reply, _ := (*extP).OnOutput()
	text := reply.Text
	// Re-arm for the next turn regardless of whether this one had speakable text:
	// a fresh Extractor baselines the just-finished reply so it is excluded next
	// time, and the Detector forgets this turn's settle clock.
	*extP = extract.New(b.screen)
	det.Reset()

	if isBlank(text) {
		return nil // completed turn produced no speakable text (e.g. a tool-only turn)
	}
	return b.speak(ctx, text)
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
