package cloudrelay

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/butler"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/deviceapi"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

// handleTranscript runs one relayed voice turn: inject the cloud's transcript into
// the device's PTY, stream the assistant reply back as voice.reply.delta events,
// and finish with the authoritative voice.reply. The cloud already did ASR and
// will do TTS — this path is text in, text out.
func (r *Relay) handleTranscript(ctx context.Context, write func(Envelope) error, env Envelope) error {
	text, _ := env.Payload["text"].(string)
	text = strings.TrimSpace(text)
	deviceID := strings.TrimSpace(env.DeviceID)
	if deviceID == "" {
		deviceID = "cloud-anon"
	}
	// Log the conversation IN (the cloud's ASR transcript). The cloud does ASR/TTS,
	// so this transcript + the reply below are the full I/O visible to adapter_v2 —
	// the primary signal for debugging a relayed voice turn.
	r.log("cloudrelay: ◀ asr device=%s text=%q", deviceID, text)
	started := time.Now()
	if text == "" {
		r.log("cloudrelay: ▶ reply device=%s (empty transcript — skipped, no CLI turn)", deviceID)
		return write(Envelope{
			Type: "reply", MessageID: env.MessageID, DeviceID: env.DeviceID,
			HomeSiteID: r.cfg.HomeSiteID, Kind: "voice.reply",
			Payload: map[string]any{"ok": true, "text": ""},
		})
	}

	cb, err := r.bridges.get(deviceID)
	if err != nil {
		return err
	}

	// Barge-in (ADR-028): a newer transcript preempts the in-flight turn. Cancel
	// the running turn's wait BEFORE taking turnMu so its handler releases the lock;
	// then SubmitVoiceTurn's ESC below aborts the CLI's now-stale generation. This
	// is the natural turn-taking of voice — "stop, do THIS instead" — rather than
	// queueing a now-stale answer ahead of what the user just said.
	cb.preempt()
	cb.turnMu.Lock()
	defer cb.turnMu.Unlock()

	// Register this turn so a later transcript can preempt it; turnCtx fires when
	// that happens.
	turnCtx, cancelTurn := context.WithCancel(ctx)
	defer cancelTurn()
	h := cb.arm(cancelTurn)
	defer cb.disarm(h)

	done := cb.ev.begin(write, env, r.cfg.HomeSiteID)
	defer cb.ev.end()

	if err := cb.bridge.SubmitVoiceTurn(text); err != nil {
		return err
	}

	// Keepalive: a silent turn (long thinking / a long-running tool call) must
	// reset the cloud's per-request reply-idle timer (ReplyIdleWait, default 120s)
	// or the cloud drops the turn. Only a MessageID-bearing event resets it (the
	// 25s connection ping does not), so emit voice.reply.heartbeat every 15s until
	// the turn ends — mirroring v1's startHeartbeat.
	stopHB := make(chan struct{})
	go r.heartbeat(write, env, stopHB)
	defer close(stopHB)

	timedOut := false
	select {
	case <-done:
	case <-turnCtx.Done():
		// Preempted by a newer transcript (barge-in). With final-only TTS nothing
		// was spoken for this turn yet, so return a clean empty reply to complete
		// the cloud's request for THIS messageID; the newer transcript carries its
		// own reply (and its SubmitVoiceTurn ESC-aborted this turn's CLI work).
		r.log("cloudrelay: ⊘ reply device=%s superseded by newer transcript (barge-in) after %s",
			deviceID, time.Since(started).Round(time.Millisecond))
		return write(Envelope{
			Type: "reply", MessageID: env.MessageID, DeviceID: env.DeviceID,
			HomeSiteID: r.cfg.HomeSiteID, Kind: "voice.reply",
			Payload: map[string]any{"ok": true, "text": "", "superseded": true},
		})
	case <-time.After(r.cfg.ReplyWait):
		timedOut = true
	case <-ctx.Done():
		r.log("cloudrelay: ✗ turn device=%s cancelled after %s (connection dropped?)", deviceID, time.Since(started).Round(time.Millisecond))
		return ctx.Err()
	}

	reply := cb.ev.reply()
	// Log the conversation OUT (the reply text the cloud will TTS). A blank reply
	// here is the signal that extraction returned nothing for this turn.
	r.log("cloudrelay: ▶ reply device=%s elapsed=%s timedOut=%v text=%q",
		deviceID, time.Since(started).Round(time.Millisecond), timedOut, reply)
	return write(Envelope{
		Type: "reply", MessageID: env.MessageID, DeviceID: env.DeviceID,
		HomeSiteID: r.cfg.HomeSiteID, Kind: "voice.reply",
		Payload: map[string]any{"ok": true, "text": reply, "replyWaitTimedOut": timedOut},
	})
}

// handleTurnCancel aborts the in-flight turn on the device's barge-in/abort
// (turn.cancel). It preempts the waiting handleTranscript (so it returns and frees
// turnMu, completing its own request) and sends the CLI an ESC to drop the running
// generation. No-op when nothing is in flight. Always acks so the cloud's
// turn.cancel request completes.
func (r *Relay) handleTurnCancel(write func(Envelope) error, env Envelope) {
	deviceID := strings.TrimSpace(env.DeviceID)
	if deviceID == "" {
		deviceID = "cloud-anon"
	}
	if cb := r.bridges.peek(); cb != nil {
		cb.preempt() // the in-flight handleTranscript returns superseded, frees turnMu
		if err := cb.bridge.Interrupt(); err != nil {
			r.log("cloudrelay: turn.cancel device=%s interrupt error: %v", deviceID, err)
		} else {
			r.log("cloudrelay: ⊘ turn.cancel device=%s — interrupted in-flight turn", deviceID)
		}
	} else {
		r.log("cloudrelay: turn.cancel device=%s — nothing in flight", deviceID)
	}
	_ = write(Envelope{
		Type: "reply", MessageID: env.MessageID, DeviceID: env.DeviceID,
		HomeSiteID: r.cfg.HomeSiteID, Kind: "turn.cancel",
		Payload: map[string]any{"ok": true},
	})
}

// heartbeatInterval is well under the cloud's ReplyIdleWait (default 120s) so a
// silent turn never trips HOME_ADAPTER_TIMEOUT.
const heartbeatInterval = 15 * time.Second

// heartbeat emits voice.reply.heartbeat for env.MessageID every heartbeatInterval
// until stop is closed, keeping the cloud's per-request reply-idle timer alive
// during a silent agent turn.
func (r *Relay) heartbeat(write func(Envelope) error, env Envelope, stop <-chan struct{}) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if write(Envelope{
				Type: "event", MessageID: env.MessageID, DeviceID: env.DeviceID,
				HomeSiteID: r.cfg.HomeSiteID, Kind: "voice.reply.heartbeat",
				Payload: map[string]any{"phase": "thinking"},
			}) != nil {
				return
			}
		}
	}
}

// ── per-device bridge ───────────────────────────────────────────────────────

// cloudBridge is one device's PTY session + Bridge + the events observer that
// turns the Bridge's reply stream into cloud frames.
type cloudBridge struct {
	sess   *session.Session
	bridge *deviceapi.Bridge
	ev     *cloudEvents
	// turnMu serialises turns for this device so the single-turn cloudEvents
	// observer stays correct. Barge-in does not skip it: a newer transcript first
	// preempt()s the running turn (which then releases turnMu), then takes it.
	turnMu sync.Mutex

	// actMu guards active, the cancel handle of the turn currently holding turnMu.
	// preempt() cancels it so a barging-in transcript doesn't block behind it.
	actMu  sync.Mutex
	active *turnHandle
}

// turnHandle identifies one in-flight turn for preemption. Pointer identity (not
// the un-comparable CancelFunc) lets disarm clear only its own turn.
type turnHandle struct{ cancel context.CancelFunc }

// arm records h as the in-flight turn and returns it (for disarm).
func (cb *cloudBridge) arm(cancel context.CancelFunc) *turnHandle {
	h := &turnHandle{cancel: cancel}
	cb.actMu.Lock()
	cb.active = h
	cb.actMu.Unlock()
	return h
}

// disarm clears the active turn iff it is still h (a newer turn may have replaced
// it via arm already).
func (cb *cloudBridge) disarm(h *turnHandle) {
	cb.actMu.Lock()
	if cb.active == h {
		cb.active = nil
	}
	cb.actMu.Unlock()
}

// preempt cancels the in-flight turn (if any) so its handler returns and releases
// turnMu. No-op when nothing is in flight (the common sequential case).
func (cb *cloudBridge) preempt() {
	cb.actMu.Lock()
	h := cb.active
	cb.active = nil
	cb.actMu.Unlock()
	if h != nil {
		h.cancel()
	}
}

// bridgeManager lazily creates one cloudBridge per device id and keeps its
// Bridge.Run goroutine alive for the process lifetime.
type bridgeManager struct {
	mgr *session.Manager
	dev *butler.DeviceSession // supplies the default session's spawn config

	mu      sync.Mutex
	bridges map[string]*cloudBridge
}

func newBridgeManager(mgr *session.Manager, dev *butler.DeviceSession) *bridgeManager {
	return &bridgeManager{mgr: mgr, dev: dev, bridges: map[string]*cloudBridge{}}
}

// peek returns the live default bridge without creating one — for turn.cancel,
// which must abort an EXISTING turn, never spawn a CLI. nil when no live bridge
// (nothing to cancel).
func (m *bridgeManager) peek() *cloudBridge {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cb, ok := m.bridges[session.DefaultID]; ok && m.mgr.Get(session.DefaultID) == cb.sess {
		return cb
	}
	return nil
}

// get returns the cloud relay's bridge onto the shared DEFAULT session. The
// deviceID is kept for logging/echo only; in P1 every relayed device drives the
// single default session (the one the web terminal also joins), so the Bridge and
// session are keyed on session.DefaultID, not the device id. (Per-device default
// sessions are a later phase.)
func (m *bridgeManager) get(deviceID string) (*cloudBridge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cb, ok := m.bridges[session.DefaultID]; ok {
		// Reuse the cached bridge only if its session is still the live one. If the
		// PTY exited, the Manager evicted that session (onExit), so Get no longer
		// returns it — the cached bridge is dead (its Run returned, writes give
		// ErrClosed). Drop it and rebuild below so voice recovers without a restart.
		if m.mgr.Get(session.DefaultID) == cb.sess {
			return cb, nil
		}
		delete(m.bridges, session.DefaultID)
	}
	sess, err := m.mgr.GetOrCreate(session.DefaultID, m.dev.Config())
	if err != nil {
		return nil, err
	}
	ev := &cloudEvents{}
	// asr/tts/sink are nil: this path is text-only (cloud does ASR/TTS). The Bridge
	// injects the transcript and extracts the reply; the events observer forwards
	// that reply text to the cloud, which TTSs it.
	//
	// StreamReplyDelta is OFF: the cloud TTSs the voice.reply.delta stream, but our
	// deltas are NormalizeReply'd snapshots of a still-growing reply, and
	// normalization (wrap-rejoin / space-collapse) is non-monotonic as text grows —
	// so streamed snapshots desync the cloud's append-only diff and it speaks the
	// wrong text. Instead we forward ONE delta with the final, clean reply at turn
	// end (cloudEvents.ReplyComplete → forwardDelta), so the cloud TTSs exactly the
	// text the device extracted. (Streaming sentence-level TTS can return later with
	// a normalization-aware, monotonic delta stream.)
	bridge := deviceapi.New(sess, nil, nil, nil, deviceapi.Config{Warmup: true})
	bridge.SetEvents(ev)
	go bridge.Run(context.Background())
	cb := &cloudBridge{sess: sess, bridge: bridge, ev: ev}
	m.bridges[session.DefaultID] = cb
	return cb, nil
}

// cloudEvents implements deviceapi.Events, routing one turn's reply stream to the
// cloud frames of the request currently being served. Exactly one turn is active
// at a time per device (the cloud relays transcripts sequentially per device).
type cloudEvents struct {
	mu        sync.Mutex
	active    bool
	write     func(Envelope) error
	env       Envelope
	homeSite  string
	replyText string
	// sent is the last reply snapshot forwarded to the cloud this turn. deviceapi
	// emits ReplyDelta as a FULL snapshot (robust to TUI redraw, for the device's
	// re-render). The cloud's TTS streamer, however, treats each voice.reply.delta
	// as APPEND-only: it speaks text[len(prevDelta):] when the snapshot extends the
	// previous one, else it speaks the WHOLE text again. claude's TUI redraws make
	// snapshots non-monotonic, so forwarding every snapshot makes the cloud re-speak
	// the growing reply over and over. We therefore forward a snapshot ONLY when it
	// strictly extends `sent` (a prefix-monotonic stream), so the cloud's diff sums
	// to exactly the reply with no repeats. Non-monotonic snapshots are skipped.
	// Tradeoff: if the FINAL reply diverges mid-string from the last forwarded
	// snapshot (a TUI reflow at the final paint — rare for the short replies the
	// voice persona enforces), the final is skipped too, so the cloud speaks the last
	// forwarded prefix (slightly stale) rather than re-speaking; the authoritative
	// full text still goes out in voice.reply. We prefer "never repeat" over "always
	// fully voiced".
	sent string
	done chan struct{}
}

// begin arms the observer for a new turn and returns its completion channel.
func (e *cloudEvents) begin(write func(Envelope) error, env Envelope, homeSite string) <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.active = true
	e.write = write
	e.env = env
	e.homeSite = homeSite
	e.replyText = ""
	e.sent = ""
	e.done = make(chan struct{})
	return e.done
}

// end disarms the observer after the turn (so a late event is dropped). It nils
// done too, so a stale TurnIdle from an abandoned (timed-out) turn — which fires
// from the Bridge goroutine after handleTranscript already returned — finds
// done==nil and no-ops instead of closing the NEXT turn's done channel.
func (e *cloudEvents) end() {
	e.mu.Lock()
	e.active = false
	e.write = nil
	e.done = nil
	e.mu.Unlock()
}

// reply returns the final reply text captured for the turn.
func (e *cloudEvents) reply() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.replyText
}

// forwardDelta forwards a reply snapshot to the cloud ONLY if it strictly extends
// what we've already sent this turn (prefix-monotonic), keeping the cloud's
// append-only TTS diff correct (no repeats). Non-monotonic snapshots — caused by
// claude's TUI redrawing the reply block — are dropped.
func (e *cloudEvents) forwardDelta(text string) {
	e.mu.Lock()
	if !e.active || e.write == nil || len(text) <= len(e.sent) || !strings.HasPrefix(text, e.sent) {
		e.mu.Unlock()
		return
	}
	w, env, home := e.write, e.env, e.homeSite
	e.sent = text
	e.mu.Unlock()
	_ = w(Envelope{
		Type: "event", MessageID: env.MessageID, DeviceID: env.DeviceID,
		HomeSiteID: home, Kind: "voice.reply.delta", Payload: map[string]any{"text": text},
	})
}

// ReplyDelta streams the growing reply as a monotonic voice.reply.delta.
func (e *cloudEvents) ReplyDelta(text string) { e.forwardDelta(text) }

// ToolStep forwards a DISPLAY-ONLY tool_call event to the cloud, which relays it to
// the device as a dimmed progress chip (ADR-030) — never TTS'd. Discrete events:
// no prefix-monotonic gate (unlike forwardDelta) and no dedup (the Bridge already
// deduped per turn). The same active/write guard as forwardDelta drops a late step
// after end().
func (e *cloudEvents) ToolStep(name, hint string) {
	e.mu.Lock()
	if !e.active || e.write == nil {
		e.mu.Unlock()
		return
	}
	w, env, home := e.write, e.env, e.homeSite
	e.mu.Unlock()
	_ = w(Envelope{
		Type: "event", MessageID: env.MessageID, DeviceID: env.DeviceID,
		HomeSiteID: home, Kind: "tool_call",
		Payload: map[string]any{"type": "tool_call", "name": name, "hint": hint},
	})
}

// ReplyComplete records the authoritative final reply text and forwards the final
// tail when it extends the last snapshot (covering the whole reply in the common
// append-only case; see the `sent` field doc for the mid-string-divergence
// tradeoff). It relies on forwardDelta's e.active/e.write guard to drop a late
// final after end() — that guard must not be removed.
func (e *cloudEvents) ReplyComplete(text string) {
	e.forwardDelta(text)
	e.mu.Lock()
	if e.active {
		e.replyText = text
	}
	e.mu.Unlock()
}

// TurnIdle signals the turn is done so handleTranscript can send voice.reply.
func (e *cloudEvents) TurnIdle() {
	e.mu.Lock()
	if e.active && e.done != nil {
		close(e.done)
		e.done = nil
		e.active = false
	}
	e.mu.Unlock()
}
