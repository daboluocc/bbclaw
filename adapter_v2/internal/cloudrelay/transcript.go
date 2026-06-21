package cloudrelay

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/deviceapi"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
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

	// One turn at a time per device: serialise, then arm the events observer with
	// this request's write/env, inject, and wait for the turn to complete.
	cb.turnMu.Lock()
	defer cb.turnMu.Unlock()
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
	// turnMu serialises turns for this device. The cloud already waits for each
	// voice.reply before relaying the next transcript, so same-device turns don't
	// overlap in practice — this guards against a future pipelining cloud and
	// keeps the single-turn cloudEvents observer correct.
	turnMu sync.Mutex
}

// bridgeManager lazily creates one cloudBridge per device id and keeps its
// Bridge.Run goroutine alive for the process lifetime.
type bridgeManager struct {
	mgr  *session.Manager
	argv []string
	cwd  string

	mu      sync.Mutex
	bridges map[string]*cloudBridge
}

func newBridgeManager(argv []string, cwd string) *bridgeManager {
	return &bridgeManager{mgr: session.NewManager(), argv: argv, cwd: cwd, bridges: map[string]*cloudBridge{}}
}

func (m *bridgeManager) get(deviceID string) (*cloudBridge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cb, ok := m.bridges[deviceID]; ok {
		return cb, nil
	}
	sess, err := m.mgr.GetOrCreate(deviceID, ptyhost.Config{Argv: m.argv, Cwd: m.cwd})
	if err != nil {
		return nil, err
	}
	ev := &cloudEvents{}
	// asr/tts/sink are nil: this path is text-only (cloud does ASR/TTS). The
	// Bridge injects the transcript and extracts the reply; the events observer
	// streams that reply text to the cloud. StreamReplyDelta on → live deltas.
	bridge := deviceapi.New(sess, nil, nil, nil, deviceapi.Config{StreamReplyDelta: true})
	bridge.SetEvents(ev)
	go bridge.Run(context.Background())
	cb := &cloudBridge{sess: sess, bridge: bridge, ev: ev}
	m.bridges[deviceID] = cb
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
