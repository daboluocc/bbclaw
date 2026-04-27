package homeadapter

// Phase 4.8 cloud agent proxy unit coverage.
//
// These tests exercise the home-adapter side of the cloud agent bus
// reverse proxy: the cloud sends a CloudEnvelope with kind="agent.message"
// or "agent.drivers", and the adapter dispatches it through the locally-
// attached agent.Router and writes back a sequence of envelopes.

import (
	"context"
	"sync"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// fakeAgentDriver is the minimum agent.Driver shape needed to exercise the
// proxy. Tests inject events directly on the channel returned by Events().
// We can't import the package-private stubDriver from internal/agent so we
// roll our own.
type fakeAgentDriver struct {
	name   string
	caps   agent.Capabilities
	events chan agent.Event
	startN int
	sentTo []string
	mu     sync.Mutex
}

func newFakeAgentDriver(name string) *fakeAgentDriver {
	return &fakeAgentDriver{
		name:   name,
		caps:   agent.Capabilities{Streaming: true, MaxInputBytes: 4096},
		events: make(chan agent.Event, 16),
	}
}

func (f *fakeAgentDriver) Name() string                   { return f.name }
func (f *fakeAgentDriver) Capabilities() agent.Capabilities { return f.caps }
func (f *fakeAgentDriver) Start(context.Context, agent.StartOpts) (agent.SessionID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startN++
	return agent.SessionID(f.name + "-sid-1"), nil
}
func (f *fakeAgentDriver) Send(_ agent.SessionID, text string) error {
	f.mu.Lock()
	f.sentTo = append(f.sentTo, text)
	f.mu.Unlock()
	// Emit a canned reply: text frame, tokens frame, turn_end. The test
	// asserts the proxy forwards all three plus the leading session frame.
	f.events <- agent.Event{Type: agent.EvText, Seq: 1, Text: "hi"}
	f.events <- agent.Event{Type: agent.EvTokens, Seq: 2, Tokens: &agent.Tokens{In: 3, Out: 4}}
	f.events <- agent.Event{Type: agent.EvTurnEnd, Seq: 3}
	return nil
}
func (f *fakeAgentDriver) Events(agent.SessionID) <-chan agent.Event { return f.events }
func (f *fakeAgentDriver) Approve(agent.SessionID, agent.ToolID, agent.Decision) error {
	return agent.ErrUnsupported
}
func (f *fakeAgentDriver) Stop(agent.SessionID) error { return nil }

func newProxyTestAdapter(t *testing.T, driver agent.Driver) *Adapter {
	t.Helper()
	r := agent.NewRouter()
	if driver != nil {
		r.Register(driver, obs.NewLogger())
	}
	a := &Adapter{
		cfg:     Config{HomeSiteID: "home-1"},
		log:     obs.NewLogger(),
		metrics: obs.NewMetrics(),
	}
	a.SetRouter(r)
	return a
}

func TestAgentProxyDriversListsRegisteredDriver(t *testing.T) {
	a := newProxyTestAdapter(t, newFakeAgentDriver("claude-code"))

	var got []CloudEnvelope
	write := func(env CloudEnvelope) error {
		got = append(got, env)
		return nil
	}
	if err := a.handleRequest(context.Background(), write, CloudEnvelope{
		Type: "request", MessageID: "m-1", Kind: "agent.drivers",
	}); err != nil {
		t.Fatalf("agent.drivers: err=%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("agent.drivers: want 1 envelope, got %d", len(got))
	}
	env := got[0]
	if env.Type != "reply" || env.Kind != "agent.drivers.reply" {
		t.Fatalf("agent.drivers reply shape wrong: %+v", env)
	}
	drivers, ok := env.Payload["drivers"].([]map[string]any)
	if !ok || len(drivers) != 1 {
		t.Fatalf("agent.drivers payload wrong: %+v", env.Payload)
	}
	if drivers[0]["name"] != "claude-code" {
		t.Fatalf("agent.drivers name wrong: %v", drivers[0])
	}
}

func TestAgentProxyMessageStreamsEventsThenReply(t *testing.T) {
	drv := newFakeAgentDriver("claude-code")
	a := newProxyTestAdapter(t, drv)

	var (
		mu  sync.Mutex
		got []CloudEnvelope
	)
	write := func(env CloudEnvelope) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, env)
		return nil
	}
	err := a.handleRequest(context.Background(), write, CloudEnvelope{
		Type: "request", MessageID: "m-2", DeviceID: "device-x", Kind: "agent.message",
		Payload: map[string]any{"text": "hello"},
	})
	if err != nil {
		t.Fatalf("agent.message: err=%v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Expected: session, text, tokens, turn_end events + final reply.
	if len(got) != 5 {
		t.Fatalf("agent.message: want 5 envelopes (4 event + 1 reply), got %d: %+v", len(got), got)
	}

	// Frame 0 — session event (the proxy emits this before the driver runs).
	if got[0].Kind != "agent.event" {
		t.Fatalf("frame 0 kind=%q want agent.event", got[0].Kind)
	}
	if t0, _ := got[0].Payload["type"].(string); t0 != "session" {
		t.Fatalf("frame 0 payload.type=%q want session", t0)
	}
	if sid, _ := got[0].Payload["sessionId"].(string); sid != "claude-code-sid-1" {
		t.Fatalf("frame 0 sessionId=%q", sid)
	}

	// Frame 1 — text "hi".
	if t1, _ := got[1].Payload["type"].(string); t1 != "text" {
		t.Fatalf("frame 1 type=%q want text", t1)
	}
	if txt, _ := got[1].Payload["text"].(string); txt != "hi" {
		t.Fatalf("frame 1 text=%q", txt)
	}

	// Frame 2 — tokens.
	if t2, _ := got[2].Payload["type"].(string); t2 != "tokens" {
		t.Fatalf("frame 2 type=%q want tokens", t2)
	}

	// Frame 3 — turn_end.
	if t3, _ := got[3].Payload["type"].(string); t3 != "turn_end" {
		t.Fatalf("frame 3 type=%q want turn_end", t3)
	}

	// Frame 4 — final reply with ok=true and turnEnd=true.
	if got[4].Type != "reply" || got[4].Kind != "agent.reply" {
		t.Fatalf("frame 4 shape wrong: type=%q kind=%q", got[4].Type, got[4].Kind)
	}
	if okField, _ := got[4].Payload["ok"].(bool); !okField {
		t.Fatalf("frame 4 ok=false: %+v", got[4].Payload)
	}
	if turnEnd, _ := got[4].Payload["turnEnd"].(bool); !turnEnd {
		t.Fatalf("frame 4 turnEnd=false: %+v", got[4].Payload)
	}

	// Driver got the user's text exactly once.
	drv.mu.Lock()
	defer drv.mu.Unlock()
	if len(drv.sentTo) != 1 || drv.sentTo[0] != "hello" {
		t.Fatalf("driver Send invocations wrong: %+v", drv.sentTo)
	}
	if drv.startN != 1 {
		t.Fatalf("driver Start invocations=%d want 1", drv.startN)
	}
}

func TestAgentProxyMessageRejectsEmptyText(t *testing.T) {
	a := newProxyTestAdapter(t, newFakeAgentDriver("claude-code"))
	err := a.handleRequest(context.Background(), func(CloudEnvelope) error { return nil }, CloudEnvelope{
		Type: "request", MessageID: "m-3", Kind: "agent.message",
		Payload: map[string]any{"text": "   "},
	})
	if err == nil || err.Error() != "EMPTY_TEXT" {
		t.Fatalf("agent.message empty text err=%v want EMPTY_TEXT", err)
	}
}

func TestAgentProxyMessageRejectsUnknownDriver(t *testing.T) {
	a := newProxyTestAdapter(t, newFakeAgentDriver("claude-code"))
	err := a.handleRequest(context.Background(), func(CloudEnvelope) error { return nil }, CloudEnvelope{
		Type: "request", MessageID: "m-4", Kind: "agent.message",
		Payload: map[string]any{"text": "hi", "driver": "nope"},
	})
	if err == nil {
		t.Fatalf("agent.message unknown driver: want error, got nil")
	}
	if got := err.Error(); got != "UNKNOWN_DRIVER:nope" {
		t.Fatalf("agent.message unknown driver err=%q", got)
	}
}
