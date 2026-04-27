// Package openclawdriver implements agent.Driver on top of the existing
// openclaw WebSocket client. It wraps internal/openclaw's Client and
// translates VoiceTranscriptStream events into the unified agent.Event
// stream.
//
// Why a separate package next to internal/openclaw:
//   - internal/openclaw owns the WS protocol (auth, voice.transcript,
//     chat.subscribe, slash command). It is also used by the legacy PTT /
//     buildSink path; treating it as a stable dependency lets us add the
//     AgentDriver shape without touching that path.
//   - the package suffix "driver" avoids the import-name collision with
//     internal/openclaw — both can be imported in the same file.
//
// Capabilities:
//   - ToolApproval: false. openclaw emits session.tool / toolCall events but
//     has no approval round-trip — the agent just runs the tool itself.
//   - Resume: true. Multi-turn continuity is achieved by reusing the same
//     session_key across Send calls (see openclaw.Client chat.subscribe /
//     waitChatFinalText, which maintains server-side history under that
//     key).
//   - Streaming: true. SendVoiceTranscriptStream emits reply.delta chunks.
//   - MaxInputBytes: 64 KiB.
package openclawdriver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/openclaw"
)

const (
	driverName   = "openclaw"
	eventBufSize = 64
)

// openclawClient is the minimal slice of *openclaw.Client we depend on. The
// indirection exists purely so tests can swap in a mock without spinning up
// a real WebSocket gateway.
type openclawClient interface {
	SendVoiceTranscriptStream(
		ctx context.Context,
		event openclaw.VoiceTranscriptEvent,
		onEvent func(openclaw.VoiceTranscriptStreamEvent),
	) (openclaw.VoiceTranscriptDelivery, error)
}

// Options configures the driver.
type Options struct {
	// URL is the openclaw WS endpoint (e.g. ws://127.0.0.1:18789). Required.
	URL string
	// AuthToken is forwarded to openclaw.NewClient.
	AuthToken string
	// NodeID identifies this adapter on the openclaw bus and is threaded
	// into every VoiceTranscriptEvent.NodeID.
	NodeID string
	// DeviceIdentityPath is the on-disk path for the persistent device
	// keypair openclaw expects. Empty means use openclaw's default.
	DeviceIdentityPath string
	// ReplyWaitTimeout is the idle window the openclaw client waits for the
	// final chat reply after the initial RPC ack.
	ReplyWaitTimeout time.Duration
	// HTTPTimeout is the per-request timeout used by the underlying
	// http/websocket client.
	HTTPTimeout time.Duration
}

// Driver is the openclaw AgentDriver implementation. The zero value is not
// usable; construct with New.
type Driver struct {
	client openclawClient
	nodeID string
	log    *obs.Logger

	mu       sync.Mutex
	sessions map[agent.SessionID]*session
}

// New constructs a Driver that talks to the openclaw WS gateway at
// opts.URL. The logger is required.
func New(opts Options, log *obs.Logger) *Driver {
	timeout := opts.HTTPTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	c := openclaw.NewClient(opts.URL, timeout, openclaw.Options{
		NodeID:             opts.NodeID,
		AuthToken:          opts.AuthToken,
		DeviceIdentityPath: opts.DeviceIdentityPath,
		ReplyWaitTimeout:   opts.ReplyWaitTimeout,
	})
	return newWithClient(c, opts.NodeID, log)
}

// newWithClient is the test seam: it lets tests inject a mock openclawClient
// without going through openclaw.NewClient. The exported New always uses a
// real client.
func newWithClient(c openclawClient, nodeID string, log *obs.Logger) *Driver {
	return &Driver{
		client:   c,
		nodeID:   strings.TrimSpace(nodeID),
		log:      log,
		sessions: make(map[agent.SessionID]*session),
	}
}

// Name implements agent.Driver.
func (d *Driver) Name() string { return driverName }

// Capabilities implements agent.Driver.
func (d *Driver) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		ToolApproval:  false,
		Resume:        true,
		Streaming:     true,
		MaxInputBytes: 64 * 1024,
	}
}

// Start allocates a new session. opts.ResumeID, if non-empty, is reused as
// the openclaw session_key — this is how we get multi-turn continuity, the
// gateway maintains chat history keyed off the session_key. Empty
// ResumeID means we mint a fresh session_key.
//
// No RPC is performed here; the WS handshake happens inside Send.
func (d *Driver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	sid := agent.SessionID("oc-" + uuid.NewString())
	sessionKey := strings.TrimSpace(opts.ResumeID)
	if sessionKey == "" {
		sessionKey = "oc-" + uuid.NewString()
	}
	s := &session{
		id:         sid,
		events:     make(chan agent.Event, eventBufSize),
		rootCtx:    ctx,
		sessionKey: sessionKey,
	}
	d.mu.Lock()
	d.sessions[sid] = s
	d.mu.Unlock()
	return sid, nil
}

// Events implements agent.Driver. Returns a closed channel when sid is
// unknown (mirrors the other drivers' behaviour).
func (d *Driver) Events(sid agent.SessionID) <-chan agent.Event {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		ch := make(chan agent.Event)
		close(ch)
		return ch
	}
	return s.events
}

// Send forwards text to openclaw via SendVoiceTranscriptStream. Stream
// events are translated 1:1 into agent.Event:
//
//	reply.delta -> EvText
//	thinking    -> dropped (would confuse end users; logs only)
//	tool_call   -> EvToolCall (display-only; ToolApproval=false)
//	others      -> dropped
//
// On stream completion (or error) an EvTurnEnd is always emitted.
func (d *Driver) Send(sid agent.SessionID, text string) error {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}

	turn := atomic.AddUint64(&s.turn, 1)
	streamID := fmt.Sprintf("oc-%s-%d", sid, turn)

	ctx, cancel := context.WithCancel(s.rootCtx)
	s.setCancel(cancel)
	defer cancel()

	event := openclaw.VoiceTranscriptEvent{
		Text:       text,
		SessionKey: s.sessionKey,
		StreamID:   streamID,
		Source:     "bbclaw.adapter.agent",
		NodeID:     d.nodeID,
	}

	d.log.Infof("openclaw: send sid=%s session_key=%s stream_id=%s bytes=%d",
		sid, s.sessionKey, streamID, len(text))

	tStart := time.Now()
	_, err := d.client.SendVoiceTranscriptStream(ctx, event, func(evt openclaw.VoiceTranscriptStreamEvent) {
		switch evt.Type {
		case "reply.delta":
			if evt.Text != "" {
				s.emit(agent.Event{Type: agent.EvText, Text: evt.Text})
			}
		case "thinking":
			// Intentionally dropped: the agent_bus event schema doesn't have a
			// "thinking" channel and surfacing partial reasoning to a small
			// device screen is more confusing than helpful. We log it for
			// debugging only.
			if evt.Text != "" {
				d.log.Infof("openclaw: thinking sid=%s preview=%q", sid, truncate(evt.Text, 80))
			}
		case "tool_call":
			if evt.Text != "" {
				s.emit(agent.Event{
					Type: agent.EvToolCall,
					Tool: &agent.ToolCall{
						// openclaw doesn't expose a stable per-call ID today;
						// best-effort empty IDs are fine while ToolApproval
						// stays false (no round-trip needs to address this
						// call).
						Tool: evt.Text,
						Hint: "",
					},
				})
			}
		default:
			// tool.running / tool.done etc. — not part of the unified schema
			// today; logged but not emitted.
			d.log.Infof("openclaw: stream evt sid=%s type=%s preview=%q",
				sid, evt.Type, truncate(evt.Text, 80))
		}
	})

	if err != nil {
		s.emit(agent.Event{Type: agent.EvError, Text: fmt.Sprintf("openclaw: %v", err)})
	}
	s.emit(agent.Event{Type: agent.EvTurnEnd})

	d.log.Infof("openclaw: send done sid=%s session_key=%s elapsed_ms=%d err=%v",
		sid, s.sessionKey, time.Since(tStart).Milliseconds(), err)
	return nil
}

// Approve is unsupported — openclaw has no tool-approval round-trip. See
// Capabilities().ToolApproval.
func (d *Driver) Approve(sid agent.SessionID, tid agent.ToolID, decision agent.Decision) error {
	return agent.ErrUnsupported
}

// Stop cancels any in-flight Send and closes the session's event channel.
// Safe to call multiple times for the same sid (subsequent calls return
// ErrUnknownSession).
func (d *Driver) Stop(sid agent.SessionID) error {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	delete(d.sessions, sid)
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	closed := s.closed
	s.closed = true
	s.mu.Unlock()
	if !closed {
		close(s.events)
	}
	return nil
}

// ─── session ────────────────────────────────────────────────────────────

type session struct {
	id         agent.SessionID
	events     chan agent.Event
	rootCtx    context.Context
	sessionKey string

	turn uint64 // monotonically increasing per Send
	seq  uint64 // event sequence counter

	mu     sync.Mutex
	cancel context.CancelFunc
	closed bool
}

func (s *session) setCancel(c context.CancelFunc) {
	s.mu.Lock()
	s.cancel = c
	s.mu.Unlock()
}

func (s *session) emit(e agent.Event) {
	e.Seq = atomic.AddUint64(&s.seq, 1)
	select {
	case s.events <- e:
	case <-s.rootCtx.Done():
	}
}

// ─── helpers ────────────────────────────────────────────────────────────

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
