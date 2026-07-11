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
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/command"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/extract"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/vtscreen"
)

// debugExtract, when ADAPTER_V2_DEBUG_EXTRACT is set, logs what the boundary
// extracted plus the tail of the VT mirror at that moment — to diagnose why a
// turn can extract a stale (previous-turn) reply.
var debugExtract = os.Getenv("ADAPTER_V2_DEBUG_EXTRACT") != ""

// debugTaskList, when ADAPTER_V2_DEBUG_TASKLIST is set, makes the task-list probe
// (ADR-034) ALSO dump the raw screen tail on a hit, for full fixture reconstruction.
// The compact one-line probe log itself is ON BY DEFAULT (not gated): a real-device
// TodoWrite render may only appear probabilistically, so we must capture it even
// when no debug env was pre-set in the field. See Bridge.captureTaskListProbe.
var debugTaskList = os.Getenv("ADAPTER_V2_DEBUG_TASKLIST") != ""

// tailLines returns the last n non-empty lines of s, for compact debug logging.
func tailLines(s string, n int) string {
	var kept []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, "\n")
}

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

// killLineKey is Ctrl-U — "kill the input line". Sent before injecting a barge-in
// transcript to CLEAR any text left in claude's composer (ADR-041): after an ESC
// interrupt, claude needs a few hundred ms to settle, and a transcript injected
// too soon does NOT submit — it lingers in the composer. A rapid follow-up
// barge-in then APPENDS its transcript to that lingering text, so the CLI runs a
// single garbled turn that merges both utterances (the 串轮 bug, reproduced and
// fixed in cmd/cancelprobe). Ctrl-U guarantees we type into an empty composer, so
// the latest barge-in always replaces — never merges with — an un-submitted one.
// No-op on an already-empty idle composer.
const killLineKey = "\x15"

// interruptSettle is the gap after an interrupt ESC before we touch the composer.
// Two reasons: (1) a terminal distinguishes a lone ESC from an escape SEQUENCE by
// timing — an ESC immediately followed by a byte is read as Alt+<byte> and
// swallows the next character (fatal for a leading CJK rune); (2) empirically
// (cmd/cancelprobe) claude takes a few HUNDRED ms to finish processing the
// interrupt — injecting at the old 60ms landed in a transitional composer that
// dropped the submit (串轮). 250ms covers both.
const interruptSettle = 250 * time.Millisecond

// injectPause separates the discrete input phases (Ctrl-U → transcript → Enter)
// so each lands as its own keystroke in order, defeating claude's burst/paste
// detection that otherwise treats "transcript\r" as a paste and turns the Enter
// into a newline instead of a submit (cmd/cancelprobe confirmed a separated Enter
// submits where a glued one does not).
const injectPause = 120 * time.Millisecond

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
	// ToolStep reports a NEW tool invocation claude started mid-turn (Bash, Edit,
	// …) for DISPLAY-ONLY device progress (ADR-030): a dimmed "Bash: …" chip. It is
	// NEVER spoken — TTS is driven solely by the prose reply. Emitted at most once
	// per distinct {name,hint} per turn (deduped in Run). nil Events: no-op.
	ToolStep(name, hint string)
	// TurnIdle reports the turn is fully done (spoken) and the device may PTT
	// again. The transport emits its turn{idle} frame here.
	TurnIdle()
}

// PromptObserver is the OPTIONAL extension a transport implements to receive
// blocking-prompt events (ADR-033). The Bridge type-asserts it off the Events
// value set via SetEvents, so existing Events implementors (cloud, LAN) compile
// unchanged and only opt in when they grow these methods (P1/P2). A transport
// that implements it can render the menu and call Bridge.SelectPromptOption to
// answer. nil observer ⇒ the prompt still auto-resolves on timeout (no-device
// safety, §11), it just isn't shown anywhere.
type PromptObserver interface {
	// PromptOpen reports a blocking permission/confirm menu is on screen and
	// awaiting a choice. The transport shows {question, options} and lets the user
	// pick; the chosen Option.Key goes back via Bridge.SelectPromptOption.
	PromptOpen(p PromptSpec)
	// PromptClosed reports the menu is gone: reason is "answered" (a selection was
	// injected), "timeout" (auto-resolved), "superseded" (claude rewrote it — a new
	// PromptOpen follows), "cleared" (it left the screen by other means), or
	// "respawn" (the PTY was replaced). The transport dismisses its prompt UI.
	PromptClosed(promptID, reason string)
}

// PromptOption is one selectable row forwarded to the device. Key is the LITERAL
// digit the device echoes back and the adapter injects to submit it (digit-submit;
// ADR-033 spike). Default marks the highlighted row.
type PromptOption struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Default bool   `json:"default"`
}

// PromptSpec is a blocking menu forwarded to the device for confirmation.
type PromptSpec struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Question  string         `json:"question"`
	Options   []PromptOption `json:"options"`
	Mechanism string         `json:"mechanism"` // "digit"
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

	// ConfirmPrompts enables blocking-prompt forwarding (ADR-033): Run detects
	// claude's permission/tool-confirm menus, emits PromptOpen/PromptClosed to a
	// PromptObserver, and answers via SelectPromptOption. OFF (the default) is the
	// exact pre-ADR-033 behaviour — no detection, no auto-deny — so voice-only and
	// existing paths are untouched. The forward-to-device deployment sets it (and
	// the butler must spawn claude with --permission-mode default, else no menu ever
	// appears — ADR-033 spike).
	ConfirmPrompts bool

	// PromptTimeout bounds how long a forwarded permission menu may sit unanswered
	// before the Bridge auto-resolves it the SAFE way (deny: pick "No"/ESC), so a
	// disconnected or silent device can't wedge the PTY and never auto-approves a
	// destructive tool (§11 invariant). Zero ⇒ defaultPromptTimeout. Only consulted
	// when ConfirmPrompts is on.
	PromptTimeout time.Duration
}

// defaultPromptTimeout is the fallback auto-deny window for a forwarded menu.
const defaultPromptTimeout = 90 * time.Second

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

	// promptObs is the optional blocking-prompt sink, type-asserted from events in
	// SetEvents (nil when the transport doesn't implement PromptObserver). ADR-033.
	promptObs PromptObserver

	// promptMu guards promptPending, which is read/written by Run's goroutine
	// (checkPrompt) AND the transport goroutine (SelectPromptOption). nil when no
	// menu is currently forwarded.
	promptMu      sync.Mutex
	promptPending *pendingPrompt
	// closedSig is the signature of the menu we JUST answered/denied. The digit we
	// injected only clears the menu off the mirror after claude repaints (a PTY
	// round-trip), so until then the same menu lingers on screen with promptPending
	// already nil — without this guard the next checkPrompt would re-open it as a
	// "new" menu (a fresh promptId + a spurious PromptOpen). Cleared once the menu
	// leaves the screen. ADR-033 §6.
	closedSig string

	// promptSeq mints monotonic promptIds ("p1", "p2", …) so a stale select against
	// a superseded/answered menu is recognised and dropped (§4/§6).
	promptSeq atomic.Uint64

	// lastTaskProbeSig dedups the ADR-034 task-list probe: the same TodoWrite block
	// lingers across many repaints, so we log it once and re-log only when the row
	// set actually changes. Read/written only on Run's goroutine.
	lastTaskProbeSig string

	// cmdHooks is the optional command-router wiring (ADR-042): when set,
	// SubmitVoiceTurn first runs the transcript through command.Parse and, on a
	// match, executes it instead of injecting a billable CLI turn. nil ⇒ no
	// interception (every transcript goes to the PTY, the pre-ADR-042 behaviour).
	cmdHooks *CommandHooks
}

// CommandHooks supplies the side-effects the command router needs without
// deviceapi importing the scheduler/status providers (ADR-042 §2.1). The Bridge
// owns turn.cancel (Interrupt) and session.new (inject the CLI's /clear) itself;
// the wiring fills in the rest. Any nil func means "not wired" — for reminder.*
// that makes the phrase fall through to a normal CLI turn (graceful degrade
// until the scheduler lands), so a reminder said before M2 is at worst typed at
// the CLI rather than dropped.
type CommandHooks struct {
	// Status returns the spoken status line for status.show (driver / session /
	// cloud). nil ⇒ a minimal built-in fallback is spoken.
	Status func() string
	// ReminderCreate persists a reminder and returns the spoken confirmation
	// ("已设置 30 分钟后提醒"). nil ⇒ reminder.create falls through to a CLI turn.
	ReminderCreate func(args map[string]string) (confirm string, err error)
	// ReminderList returns the spoken summary of pending reminders. nil ⇒ falls
	// through to a CLI turn.
	ReminderList func() string
}

// pendingPrompt is the Bridge's record of the menu currently forwarded to the
// device, kept so SelectPromptOption can validate the id/key, the timeout path
// can auto-deny, and supersede can fire when the option set changes.
type pendingPrompt struct {
	id       string
	sig      string          // extract.Prompt.Signature; a change ⇒ supersede
	denyKey  string          // option Key to inject for a safe auto-deny ("" ⇒ ESC)
	keys     map[string]bool // valid option keys, for SelectPromptOption validation
	openedAt time.Time
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
// clears it. Used by the device-WS transport to emit reply.end / turn frames. If
// e also implements PromptObserver, it receives blocking-prompt events too.
func (b *Bridge) SetEvents(e Events) {
	b.events = e
	b.promptObs, _ = e.(PromptObserver)
}

// SetCommandHooks attaches the command router (ADR-042). Call before Run. nil
// disables interception (every transcript goes to the CLI). Safe to call once at
// wiring time; not goroutine-safe vs. an in-flight SubmitVoiceTurn.
func (b *Bridge) SetCommandHooks(h *CommandHooks) { b.cmdHooks = h }

// handleCommand runs transcript through the command router and executes a match,
// returning handled=true so SubmitVoiceTurn short-circuits the CLI turn. A nil
// hooks set, a non-command phrase, or an unwired reminder.* returns false so the
// transcript falls through to the normal PTY injection.
func (b *Bridge) handleCommand(transcript string) bool {
	if b.cmdHooks == nil {
		return false
	}
	in := command.Parse(transcript, "voice")
	if in == nil {
		return false
	}
	switch in.Kind {
	case command.KindCancel:
		// "停止 / 取消": abort the in-flight turn (no-op at an idle prompt). Never
		// reaches the LLM.
		_ = b.Interrupt()
		return true
	case command.KindNewSession:
		// "新对话 / 清空": interrupt any live turn, then send the CLI's own /clear
		// slash command (a CLI command, not a billable LLM turn).
		b.newSession()
		return true
	case command.KindStatus:
		b.speakStatus()
		return true
	case command.KindReminderCreate:
		if b.cmdHooks.ReminderCreate == nil {
			return false // scheduler not wired (pre-M2) → fall through to a CLI turn
		}
		confirm, err := b.cmdHooks.ReminderCreate(in.Args)
		if err != nil {
			log.Printf("deviceapi: reminder.create failed: %v", err)
			b.speakBackground("没设置成功，请再说一次时间。")
			return true
		}
		if confirm == "" {
			confirm = "已设置提醒。"
		}
		b.speakBackground(confirm)
		return true
	case command.KindReminderList:
		if b.cmdHooks.ReminderList == nil {
			return false
		}
		b.speakBackground(b.cmdHooks.ReminderList())
		return true
	}
	return false
}

// newSession starts a fresh CLI conversation: interrupt a live turn first (so the
// /clear isn't swallowed mid-reply), then inject the CLI's /clear slash command.
// /clear is a CLI command (not an LLM turn), so this costs nothing and clears the
// context the next PTT starts from.
func (b *Bridge) newSession() {
	if b.inFlight.Load() {
		_ = b.Interrupt()
		time.Sleep(injectPause)
	}
	if err := b.sess.Write([]byte("/clear" + enterKey)); err != nil {
		log.Printf("deviceapi: newSession write: %v", err)
		return
	}
	b.signalRearm()
	b.speakBackground("好的，开始新对话。")
}

// speakStatus voices the current status line (status.show command). Uses the
// wired Status hook, or a minimal fallback when none is set.
func (b *Bridge) speakStatus() {
	text := "设备在线，会话正常。"
	if b.cmdHooks != nil && b.cmdHooks.Status != nil {
		if s := b.cmdHooks.Status(); s != "" {
			text = s
		}
	}
	b.speakBackground(text)
}

// speakBackground voices a short adapter-originated line (command ack, status)
// outside the turn loop. It mirrors maybeSpeak's reply path so the device shows
// and speaks it: ReplyComplete updates the screen subtitle, speak plays TTS, and
// TurnIdle hands control back. Best-effort — a synth/sink error is logged, not
// surfaced, since a failed ack must not wedge the device.
func (b *Bridge) speakBackground(text string) {
	if isBlank(text) {
		return
	}
	if b.events != nil {
		b.events.ReplyComplete(text)
	}
	if err := b.speak(context.Background(), text); err != nil {
		log.Printf("deviceapi: speakBackground: %v", err)
	}
	if b.events != nil {
		b.events.TurnIdle()
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
	// Command router (ADR-042): intercept short commands ("停止"/"状态"/"新对话"/
	// reminders) BEFORE injecting, so they don't become a billable CLI turn. A
	// matched command is executed here and the turn short-circuits; everything else
	// falls through to PTY injection unchanged. turn.cancel intentionally runs
	// before the ready gate (you can cancel during warmup); the others are cheap.
	if b.handleCommand(transcript) {
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
		// Barge-in: interrupt the running turn (ESC), let claude SETTLE, CLEAR the
		// composer of any lingering un-submitted transcript (Ctrl-U), then type the
		// new turn and submit it with the Enter as a SEPARATE keystroke. This is the
		// 串轮 fix (ADR-041; reproduced + validated in cmd/cancelprobe): at the old
		// "ESC + 60ms + transcript\r" the transcript injected before claude finished
		// the interrupt, did not submit, and a rapid second barge-in APPENDED to it →
		// one garbled merged turn. Clearing + pacing makes the latest barge-in always
		// REPLACE an un-submitted one, never merge — i.e. "撤销 = 全新重发".
		if err := b.sess.Write([]byte(interruptKey)); err != nil {
			return mapErr(err)
		}
		time.Sleep(interruptSettle)
		if err := b.sess.Write([]byte(killLineKey)); err != nil {
			return mapErr(err)
		}
		time.Sleep(injectPause)
		if err := b.sess.Write([]byte(transcript)); err != nil {
			return mapErr(err)
		}
		time.Sleep(injectPause)
		if err := b.sess.Write([]byte(enterKey)); err != nil {
			return mapErr(err)
		}
	} else {
		// Clean first/sequential turn at an idle prompt: nothing to interrupt or
		// clear. Even here the Enter MUST be a separate keystroke after a settle —
		// gluing "transcript\r" into one write lets claude's TUI paste-burst
		// heuristic (a big chunk arriving at once looks like a bracketed paste)
		// swallow the trailing \r as a literal newline in the composer instead of
		// SUBMITTING. The transcript then sits un-sent until a later lone Enter (the
		// next turn) flushes both at once — the user-visible "松手没发出去,下一句
		// 连着上一句一起发" 串轮. A long CJK transcript is the common trigger (more
		// bytes → more paste-like). Same lesson as the barge-in path above (ADR-041);
		// the old single-burst "no 串轮 risk without a prior turn" assumption was wrong.
		if err := b.sess.Write([]byte(transcript)); err != nil {
			return mapErr(err)
		}
		time.Sleep(injectPause)
		if err := b.sess.Write([]byte(enterKey)); err != nil {
			return mapErr(err)
		}
	}
	b.inFlight.Store(true)
	// A new user turn is now in flight. Signal Run to re-baseline the Extractor on
	// the (possibly interrupted) current screen and reset the Detector's settle
	// clock, so this turn's reply is isolated from the prior one and a barge-in
	// never speaks the interrupted partial. See the rearm field and Run's drain.
	b.signalRearm()
	return nil
}

// Interrupt aborts the in-flight turn without starting a new one — the explicit
// barge-in/cancel path (the device's turn.cancel), as opposed to SubmitVoiceTurn's
// interrupt-then-inject. It sends claude's ESC ("esc to interrupt") so the CLI
// drops the current turn and returns to its idle prompt, clears the in-flight flag
// (so a following SubmitVoiceTurn injects cleanly with no gratuitous second ESC),
// and re-baselines so the interrupted partial never leaks into the next turn.
// No-op when nothing is in flight (a cancel at an idle prompt must NOT emit a
// stray ESC — it would corrupt the next keystroke; see SubmitVoiceTurn).
func (b *Bridge) Interrupt() error {
	if !b.inFlight.Load() {
		return nil
	}
	if err := b.sess.Write([]byte(interruptKey)); err != nil {
		return mapErr(err)
	}
	time.Sleep(interruptSettle)
	// No Ctrl-U here: a PURE cancel injects nothing, so the composer is already
	// empty after ESC, and the NEXT turn takes SubmitVoiceTurn's clean (inFlight=
	// false) path. The composer-clear is only needed on the barge-in INJECT path
	// (SubmitVoiceTurn), where a lingering un-submitted transcript could be appended
	// to (the 串轮 fix, ADR-041).
	b.inFlight.Store(false)
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

// forwardablePromptKind reports whether a parsed menu needs a human decision and
// so is forwarded to the device. upsell/trust are config-suppressed / handled by
// warmup+auto-enter and must NOT be forwarded (ADR-033 §2); survey never reaches
// here (ParsePrompt never returns it).
func forwardablePromptKind(k extract.PromptKind) bool {
	switch k {
	case extract.PromptPermission, extract.PromptEditConfirm, extract.PromptUnknown:
		return true
	default:
		return false
	}
}

// checkPrompt drives the blocking-prompt forward lifecycle from Run's goroutine:
// open a promptId when a permission/confirm menu appears (PromptOpen), supersede
// when its option set changes, auto-DENY on timeout (no-device / silent-device
// safety — never auto-approve), and close it when it leaves the screen. It
// returns pending=true while a menu is up so Run suppresses speaking/streaming
// over it. No-op (false) unless Config.ConfirmPrompts is set, so the default and
// voice-only paths are untouched.
func (b *Bridge) checkPrompt(now time.Time) (pending bool) {
	if !b.cfg.ConfirmPrompts {
		return false
	}
	p, ok := extract.ParsePrompt(b.screen.VisibleText())
	forward := ok && forwardablePromptKind(p.Kind)

	b.promptMu.Lock()
	cur := b.promptPending

	if !forward {
		b.closedSig = "" // the menu (if any) has left the screen
		if cur != nil {  // a pending menu left the screen by other means
			b.promptPending = nil
			b.promptMu.Unlock()
			b.notifyPromptClosed(cur.id, "cleared")
			return false
		}
		b.promptMu.Unlock()
		return false
	}

	// The menu we just answered/denied lingers on the mirror until claude repaints;
	// keep suppressing speak but do NOT re-open it.
	if cur == nil && p.Signature == b.closedSig {
		b.promptMu.Unlock()
		return true
	}

	if cur != nil && cur.sig == p.Signature { // same menu still waiting
		if now.Sub(cur.openedAt) >= b.promptTimeout() {
			deny, id := cur.denyKey, cur.id
			b.promptPending = nil
			b.closedSig = cur.sig // suppress re-open until the deny repaints it away
			b.promptMu.Unlock()
			b.injectDeny(deny) // safe option / ESC — never the default Yes
			b.notifyPromptClosed(id, "timeout")
			return true
		}
		b.promptMu.Unlock()
		return true
	}

	// New menu, or the option set changed under the same screen → supersede the old.
	b.closedSig = "" // a genuinely different menu; drop the closing guard
	superseded := ""
	if cur != nil {
		superseded = cur.id
	}
	id := b.nextPromptID()
	keys := make(map[string]bool, len(p.Options))
	for _, o := range p.Options {
		keys[o.Key] = true
	}
	b.promptPending = &pendingPrompt{
		id: id, sig: p.Signature, denyKey: denyKeyFor(p), keys: keys, openedAt: now,
	}
	spec := toPromptSpec(id, p)
	b.promptMu.Unlock()

	if superseded != "" {
		b.notifyPromptClosed(superseded, "superseded")
	}
	if b.promptObs != nil {
		b.promptObs.PromptOpen(spec)
	}
	return true
}

// SelectPromptOption answers a forwarded blocking menu: it validates promptID is
// still the live pending menu and key is one of its options, then injects the
// digit into the PTY (digit-submit — NO leading ESC, which would cancel it). A
// stale/unknown promptID (superseded, already answered, or from a respawned PTY)
// or an unknown key is a safe ack-and-drop no-op (§6), never a mis-inject. Called
// from the transport goroutine.
func (b *Bridge) SelectPromptOption(promptID, key string) error {
	b.promptMu.Lock()
	cur := b.promptPending
	if cur == nil || cur.id != promptID || !cur.keys[key] {
		b.promptMu.Unlock()
		return nil
	}
	b.promptPending = nil
	b.closedSig = cur.sig // the menu lingers until claude repaints; don't re-open it
	b.promptMu.Unlock()

	// Single small PTY write from this (transport) goroutine; Bridge.Run and the web
	// terminal may also write the shared PTY, but each session.Write is one atomic
	// os.File write of a 1-byte key, so keystrokes can't tear into each other. The
	// promptMu CAS above already made double-answer (select vs timeout-deny) safe.
	if err := b.sess.Write([]byte(key)); err != nil {
		return mapErr(err)
	}
	b.notifyPromptClosed(promptID, "answered")
	return nil
}

// closePendingPrompt clears any forwarded menu and tells the observer why — used
// on Run exit (PTY respawn / ctx cancel) so the device never holds a dead menu.
func (b *Bridge) closePendingPrompt(reason string) {
	b.promptMu.Lock()
	cur := b.promptPending
	b.promptPending = nil
	b.promptMu.Unlock()
	if cur != nil {
		b.notifyPromptClosed(cur.id, reason)
	}
}

func (b *Bridge) notifyPromptClosed(id, reason string) {
	if b.promptObs != nil {
		b.promptObs.PromptClosed(id, reason)
	}
}

func (b *Bridge) nextPromptID() string { return "p" + strconv.FormatUint(b.promptSeq.Add(1), 10) }

func (b *Bridge) promptTimeout() time.Duration {
	if b.cfg.PromptTimeout > 0 {
		return b.cfg.PromptTimeout
	}
	return defaultPromptTimeout
}

// injectDeny answers a menu the SAFE way: the explicit "No" option if claude
// offered one, else ESC (which the menu's "Esc to cancel" footer honours). Never
// the highlighted default (which is "Yes").
func (b *Bridge) injectDeny(key string) {
	if key != "" {
		_ = b.sess.Write([]byte(key))
		return
	}
	_ = b.sess.Write([]byte(interruptKey))
}

// denyKeyFor returns the Key of the option whose label denies (starts with "No"),
// or "" when there is none (caller falls back to ESC).
func denyKeyFor(p extract.Prompt) string {
	for _, o := range p.Options {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(o.Label)), "no") {
			return o.Key
		}
	}
	return ""
}

// toPromptSpec converts the extractor's Prompt into the transport-facing spec.
func toPromptSpec(id string, p extract.Prompt) PromptSpec {
	opts := make([]PromptOption, len(p.Options))
	for i, o := range p.Options {
		opts[i] = PromptOption{Key: o.Key, Label: o.Label, Default: o.Default}
	}
	return PromptSpec{
		ID: id, Kind: string(p.Kind), Question: p.Question, Options: opts, Mechanism: p.Mechanism,
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
	// On exit (PTY respawn / ctx cancel) drop any forwarded menu so the device
	// never holds a dead promptId pointing at a gone process (ADR-033 §6).
	defer b.closePendingPrompt("respawn")

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

	// dismissSurvey clears claude's input-blocking session-feedback overlay the
	// moment it appears, pressing "0: Dismiss". Single-shot per appearance (armed
	// again only after the overlay leaves the screen) so we never spam the key into
	// the prompt if it's already gone.
	surveyArmed := true
	dismissSurvey := func() {
		if strings.Contains(b.screen.VisibleText(), surveyMarker) {
			if surveyArmed {
				surveyArmed = false
				if debugExtract {
					log.Printf("deviceapi: dismissing claude session-feedback overlay")
				}
				_ = b.sess.Write([]byte(surveyDismissKey))
			}
			return
		}
		surveyArmed = true
	}

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
		// Tool-step analogue of `seed`: the PRIOR turn's "⏺ Bash(…)" bullets are
		// still on the visible grid until this turn paints over them. Without seeding,
		// the just-reset ts.steps makes those stale bullets look NEW and replay a
		// previous turn's tool call (e.g. an earlier "device set-volume") into this turn.
		b.seedVisibleToolSteps(ts)
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
			dismissSurvey()
			reply, _ := ext.OnOutput()
			det.Observe(time.Now(), b.screen, len(chunk) > 0)
			b.emitToolSteps(ts)                            // display-only tool progress (ADR-030); only on a new paint
			b.captureTaskListProbe(b.screen.VisibleText()) // ADR-034 data-acquisition probe; logs only, no behaviour change
			if b.checkPrompt(time.Now()) {
				continue // a blocking permission/confirm menu is up — don't speak/stream over it
			}
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
			dismissSurvey()
			det.Observe(time.Now(), b.screen, false)
			if b.checkPrompt(time.Now()) {
				continue // blocking menu up — suppress turn-end speak (also a §0 belt-and-suspenders)
			}
			reply, _ := ext.OnOutput()
			if spoke, err := b.maybeSpeak(ctx, det, reply.Text, ts); err != nil {
				return err
			} else if spoke {
				rebaseline()
			}
		}
	}
}

// surveyMarker is claude's periodic, input-BLOCKING session-feedback overlay
// ("● How is Claude doing this session? (optional) / 1: Bad 2: Fine 3: Good
// 0: Dismiss"). Left up it eats the next injected transcript, so the turn never
// runs and the device gets no reply / no TTS (observed: it wedged the line for
// minutes until every turn timed out). surveyDismissKey is its "0: Dismiss"
// option; we press it whenever the overlay appears so the prompt stays clear.
const (
	surveyMarker     = "How is Claude doing this session"
	surveyDismissKey = "0"
)

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
	seed      string              // the prior turn's reply, still on screen until this turn paints; suppressed by onProgress
	lastDelta string              // last full reply text emitted via Events.ReplyDelta
	spoken    string              // reply prefix already synthesised by per-segment TTS
	steps     map[string]struct{} // tool steps already emitted this turn (dedup); reset per turn via rebaseline
}

// seedVisibleToolSteps pre-fills ts.steps with the tool-step bullets currently on
// the visible grid, marking them already-emitted. Called at turn rebaseline so a
// prior turn's bullet (still painted on screen until this turn overwrites it) is
// NOT replayed into the new turn — the tool-step analogue of turnStream.seed for
// reply text. Uses the same key format as emitToolSteps.
func (b *Bridge) seedVisibleToolSteps(ts *turnStream) {
	steps := map[string]struct{}{}
	for _, st := range extract.ScanToolSteps(b.screen.VisibleText()) {
		steps[st.Name+"\x00"+st.Hint] = struct{}{}
	}
	ts.steps = steps
}

// emitToolSteps scans the screen for claude's tool-step bullets and emits a
// DISPLAY-ONLY ToolStep for each NEW {name,hint} this turn (deduped via ts.steps).
// No audio — progress only. Called per PTY paint; the dedup makes repeated repaints
// of the same step emit once.
func (b *Bridge) emitToolSteps(ts *turnStream) {
	if b.events == nil {
		return
	}
	for _, st := range extract.ScanToolSteps(b.screen.VisibleText()) {
		key := st.Name + "\x00" + st.Hint
		if ts.steps == nil {
			ts.steps = map[string]struct{}{}
		}
		if _, seen := ts.steps[key]; seen {
			continue
		}
		ts.steps[key] = struct{}{}
		b.events.ToolStep(st.Name, st.Hint)
	}
}

// taskProbeMinRows is the smallest contiguous checklist-ish run the probe treats as
// a candidate TodoWrite block. A lone checkbox-looking line is more likely prose
// (or a stray glyph) than a real todo list, so we require at least two adjacent
// same-indent rows before logging.
const taskProbeMinRows = 2

// captureTaskListProbe is ADR-034's data-acquisition probe — pure observation, no
// extraction or speak-behaviour change. We don't yet know the exact glyph→status
// mapping claude's TodoWrite uses (ADR-034 §3 is a P0 gate needing a real-device
// capture), and that render may only appear PROBABILISTICALLY in the field. So
// rather than hide this behind a debug env that would be off in the wild, it runs
// by DEFAULT and logs a compact, deduped one-liner whenever a candidate checklist
// block appears — recording each row's leading marker as a U+XXXX code point (☐ vs
// □ vs ◻ are different code points that look alike on screen) plus its text. That
// one log line is enough to pin the mapping and build the fixture later. With
// ADAPTER_V2_DEBUG_TASKLIST set it also dumps the raw screen tail. Not gated on
// b.events: useful even when no device sink is attached (e.g. a web-terminal turn).
func (b *Bridge) captureTaskListProbe(visible string) {
	run := extract.LongestRun(extract.ScanTaskListCandidates(visible))
	if len(run) < taskProbeMinRows {
		return
	}
	var sb strings.Builder
	for i, c := range run {
		if i > 0 {
			sb.WriteByte('|')
		}
		fmt.Fprintf(&sb, "%s %q", c.Lead, c.Text)
	}
	sig := sb.String()
	if sig == b.lastTaskProbeSig {
		return // same block still on screen across repaints — already logged
	}
	b.lastTaskProbeSig = sig
	log.Printf("tasklist-probe (ADR-034): %d candidate rows: %s", len(run), sig)
	if debugTaskList {
		log.Printf("tasklist-probe raw screen tail:\n--- begin ---\n%s\n--- end ---", tailLines(visible, 20))
	}
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
	// Stale-guard (the "device replays the previous answer" bug): a multi-step /
	// tool-using turn has QUIET GAPS between tool calls where the spinner clears, so
	// the boundary can look "ended" BEFORE this turn's reply has rendered. At that
	// moment the newest "⏺" block on screen is still the PREVIOUS turn's reply — the
	// seed captured at rebaseline. Firing now would (re)speak that stale reply. So
	// while the extraction is still exactly the seed, the turn is not really done:
	// wait for the new reply to diverge from it. (A blank/tool-only turn falls
	// through below to hand control back; an identical consecutive reply is the rare
	// case that waits for the cloud's reply timeout.)
	if !isBlank(text) && text == ts.seed {
		if debugExtract {
			log.Printf("deviceapi: boundary suppressed — extract still == seed (awaiting this turn's reply)")
		}
		return false, nil
	}
	// The turn the user injected is done; a later barge-in now has nothing to
	// interrupt, so the next SubmitVoiceTurn injects cleanly without an ESC.
	b.inFlight.Store(false)

	if debugExtract {
		log.Printf("deviceapi: boundary extract=%q\n--- mirror tail ---\n%s\n--- end ---", text, tailLines(b.screen.VisibleText(), 12))
	}

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
