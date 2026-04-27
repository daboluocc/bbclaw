package openclawdriver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/openclaw"
)

// fakeClient is a mock implementation of openclawClient. Each call to
// SendVoiceTranscriptStream replays the canned events list and then returns
// the canned err.
type fakeClient struct {
	mu sync.Mutex

	// events is the list of stream events to feed onEvent on each call.
	events []openclaw.VoiceTranscriptStreamEvent
	// err is returned from SendVoiceTranscriptStream after replaying events.
	err error
	// reply is returned in the VoiceTranscriptDelivery.
	reply string

	// recorded:
	calls []openclaw.VoiceTranscriptEvent
}

func (f *fakeClient) SendVoiceTranscriptStream(
	ctx context.Context,
	event openclaw.VoiceTranscriptEvent,
	onEvent func(openclaw.VoiceTranscriptStreamEvent),
) (openclaw.VoiceTranscriptDelivery, error) {
	f.mu.Lock()
	f.calls = append(f.calls, event)
	evs := append([]openclaw.VoiceTranscriptStreamEvent(nil), f.events...)
	err := f.err
	reply := f.reply
	f.mu.Unlock()

	if onEvent != nil {
		for _, e := range evs {
			onEvent(e)
		}
	}
	return openclaw.VoiceTranscriptDelivery{ReplyText: reply}, err
}

func (f *fakeClient) snapshot() []openclaw.VoiceTranscriptEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]openclaw.VoiceTranscriptEvent, len(f.calls))
	copy(out, f.calls)
	return out
}

// drainAll reads from ch until EvTurnEnd is observed or timeout fires.
func drainAll(t *testing.T, ch <-chan agent.Event, timeout time.Duration) []agent.Event {
	t.Helper()
	var out []agent.Event
	tm := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
			if ev.Type == agent.EvTurnEnd {
				return out
			}
		case <-tm:
			t.Fatalf("timed out waiting for TurnEnd; collected=%+v", out)
		}
	}
}

// TestCapabilitiesAndName pins the public capability surface. If anyone
// later flips ToolApproval to true or drops Resume, this fails loudly so
// they remember to update the device-side UX expectations as well.
func TestCapabilitiesAndName(t *testing.T) {
	d := newWithClient(&fakeClient{}, "node-x", obs.NewLogger())
	if d.Name() != "openclaw" {
		t.Errorf("Name=%s want openclaw", d.Name())
	}
	caps := d.Capabilities()
	if caps.ToolApproval {
		t.Error("ToolApproval must be false: openclaw has no approval round-trip")
	}
	if !caps.Resume {
		t.Error("Resume must be true: session_key reuse provides continuity")
	}
	if !caps.Streaming {
		t.Error("Streaming must be true: SendVoiceTranscriptStream emits deltas")
	}
	if caps.MaxInputBytes != 64*1024 {
		t.Errorf("MaxInputBytes=%d want 65536", caps.MaxInputBytes)
	}
}

// TestSendTranslatesStreamEvents feeds reply.delta + thinking + tool_call
// events through the driver and verifies the unified agent.Event mapping.
func TestSendTranslatesStreamEvents(t *testing.T) {
	fake := &fakeClient{
		events: []openclaw.VoiceTranscriptStreamEvent{
			{Type: "reply.delta", Text: "Hello"},
			{Type: "thinking", Text: "let me think..."}, // must be dropped
			{Type: "reply.delta", Text: " world"},
			{Type: "tool_call", Text: "Bash"},
			{Type: "tool.running", Text: "Bash"}, // unknown — dropped
		},
		reply: "Hello world",
	}

	d := newWithClient(fake, "test-node", obs.NewLogger())
	sid, err := d.Start(context.Background(), agent.StartOpts{ResumeID: "session-123"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := d.Events(sid)

	done := make(chan error, 1)
	go func() { done <- d.Send(sid, "hi") }()

	evs := drainAll(t, events, 2*time.Second)
	if err := <-done; err != nil {
		t.Fatalf("Send err: %v", err)
	}

	// Expected: 2 EvText + 1 EvToolCall + 1 EvTurnEnd. No EvError. No
	// "thinking" event leakage.
	var texts []string
	var tools []string
	var turnEnds, errs int
	for _, e := range evs {
		switch e.Type {
		case agent.EvText:
			texts = append(texts, e.Text)
		case agent.EvToolCall:
			if e.Tool == nil {
				t.Errorf("EvToolCall with nil Tool: %+v", e)
				continue
			}
			tools = append(tools, e.Tool.Tool)
		case agent.EvTurnEnd:
			turnEnds++
		case agent.EvError:
			errs++
		}
	}
	if got, want := strings.Join(texts, ""), "Hello world"; got != want {
		t.Errorf("assembled text=%q want %q", got, want)
	}
	if len(tools) != 1 || tools[0] != "Bash" {
		t.Errorf("tools=%v want [Bash]", tools)
	}
	if turnEnds != 1 {
		t.Errorf("turnEnds=%d want 1", turnEnds)
	}
	if errs != 0 {
		t.Errorf("errs=%d want 0", errs)
	}

	// Outbound event must carry the resumed session_key and the configured
	// node id. StreamID is per-turn, prefixed with the session id.
	calls := fake.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls=%d want 1", len(calls))
	}
	if calls[0].SessionKey != "session-123" {
		t.Errorf("SessionKey=%q want session-123 (resume id was provided)", calls[0].SessionKey)
	}
	if calls[0].NodeID != "test-node" {
		t.Errorf("NodeID=%q want test-node", calls[0].NodeID)
	}
	if calls[0].Source != "bbclaw.adapter.agent" {
		t.Errorf("Source=%q want bbclaw.adapter.agent", calls[0].Source)
	}
	if !strings.HasPrefix(calls[0].StreamID, "oc-") || !strings.HasSuffix(calls[0].StreamID, "-1") {
		t.Errorf("StreamID=%q want shape oc-<sid>-1", calls[0].StreamID)
	}
	if calls[0].Text != "hi" {
		t.Errorf("Text=%q want hi", calls[0].Text)
	}
}

// TestSendErrorEmitsEvError verifies that a transport error from openclaw
// surfaces as EvError + EvTurnEnd and that Send itself returns nil
// (consistent with the other drivers' "errors are events" contract).
func TestSendErrorEmitsEvError(t *testing.T) {
	fake := &fakeClient{
		err: errors.New("dial openclaw ws: connection refused"),
	}
	d := newWithClient(fake, "test-node", obs.NewLogger())
	sid, err := d.Start(context.Background(), agent.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := d.Events(sid)

	done := make(chan error, 1)
	go func() { done <- d.Send(sid, "hello") }()

	evs := drainAll(t, events, 2*time.Second)
	if sendErr := <-done; sendErr != nil {
		t.Fatalf("Send returned err=%v; want nil (errors flow as events)", sendErr)
	}

	var sawError, sawEnd bool
	var errText string
	for _, e := range evs {
		switch e.Type {
		case agent.EvError:
			sawError = true
			errText = e.Text
		case agent.EvTurnEnd:
			sawEnd = true
		}
	}
	if !sawError {
		t.Error("missing EvError event")
	}
	if !strings.Contains(errText, "connection refused") {
		t.Errorf("EvError text=%q does not mention transport error", errText)
	}
	if !sawEnd {
		t.Error("missing EvTurnEnd event after error")
	}
}

// TestStartGeneratesSessionKeyWhenNoResume guarantees the gateway always
// gets a non-empty session_key. Without one, openclaw can't subscribe to
// chat events and we'd silently drop replies.
func TestStartGeneratesSessionKeyWhenNoResume(t *testing.T) {
	fake := &fakeClient{}
	d := newWithClient(fake, "n", obs.NewLogger())
	sid, err := d.Start(context.Background(), agent.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- d.Send(sid, "x") }()
	drainAll(t, d.Events(sid), 2*time.Second)
	if err := <-done; err != nil {
		t.Fatalf("Send: %v", err)
	}

	calls := fake.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls=%d want 1", len(calls))
	}
	if strings.TrimSpace(calls[0].SessionKey) == "" {
		t.Error("auto-generated session_key must not be empty")
	}
	if !strings.HasPrefix(calls[0].SessionKey, "oc-") {
		t.Errorf("SessionKey=%q want oc- prefix", calls[0].SessionKey)
	}
}

// TestStopIsIdempotent matches the agent_bus.md §3 lifecycle contract.
func TestStopIsIdempotent(t *testing.T) {
	d := newWithClient(&fakeClient{}, "n", obs.NewLogger())
	sid, err := d.Start(context.Background(), agent.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := d.Stop(sid); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := d.Stop(sid); !errors.Is(err, agent.ErrUnknownSession) {
		t.Errorf("second Stop err=%v want ErrUnknownSession", err)
	}
}

// TestApproveUnsupported is a quick sanity check on the negative capability.
func TestApproveUnsupported(t *testing.T) {
	d := newWithClient(&fakeClient{}, "n", obs.NewLogger())
	sid, _ := d.Start(context.Background(), agent.StartOpts{})
	if err := d.Approve(sid, "tool-1", agent.DecisionOnce); !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("Approve err=%v want ErrUnsupported", err)
	}
}
