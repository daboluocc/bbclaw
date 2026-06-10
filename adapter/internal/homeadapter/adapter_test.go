package homeadapter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/openclaw"
	"github.com/gorilla/websocket"
)

type fakeSink struct {
	delivery     openclaw.VoiceTranscriptDelivery
	streamEvents []openclaw.VoiceTranscriptStreamEvent
	err          error
	last         openclaw.VoiceTranscriptEvent
}

func (f *fakeSink) SendVoiceTranscript(_ context.Context, event openclaw.VoiceTranscriptEvent) (openclaw.VoiceTranscriptDelivery, error) {
	f.last = event
	return f.delivery, f.err
}

func (f *fakeSink) SendVoiceTranscriptStream(
	_ context.Context,
	event openclaw.VoiceTranscriptEvent,
	onEvent func(openclaw.VoiceTranscriptStreamEvent),
) (openclaw.VoiceTranscriptDelivery, error) {
	f.last = event
	for _, evt := range f.streamEvents {
		onEvent(evt)
	}
	return f.delivery, f.err
}

func TestHandleRequestRequiresText(t *testing.T) {
	a := &Adapter{
		cfg:     Config{HomeSiteID: "home-1"},
		log:     obs.NewLogger(),
		metrics: obs.NewMetrics(),
	}
	err := a.handleRequest(context.Background(), func(CloudEnvelope) error { return nil }, CloudEnvelope{
		Type:     "request",
		DeviceID: "device-1",
		Kind:     "voice.transcript",
		Payload:  map[string]any{},
	})
	if err == nil || err.Error() != "payload.text is required" {
		t.Fatalf("handleRequest() err = %v", err)
	}
}

func TestHandleRequestIgnoresUnsupportedKind(t *testing.T) {
	a := &Adapter{
		cfg:     Config{HomeSiteID: "home-1"},
		log:     obs.NewLogger(),
		metrics: obs.NewMetrics(),
	}
	if err := a.handleRequest(context.Background(), nil, CloudEnvelope{
		Type:     "request",
		DeviceID: "device-1",
		Kind:     "noop",
	}); err != nil {
		t.Fatalf("handleRequest() err = %v", err)
	}
}

func TestHandleRequestReturnsSinkError(t *testing.T) {
	sink := &fakeSink{err: errors.New("boom")}
	a := &Adapter{
		cfg:     Config{HomeSiteID: "home-1"},
		log:     obs.NewLogger(),
		metrics: obs.NewMetrics(),
		sink:    sink,
	}
	err := a.handleRequest(context.Background(), func(CloudEnvelope) error { return nil }, CloudEnvelope{
		Type:     "request",
		DeviceID: "device-1",
		Kind:     "voice.transcript",
		Payload: map[string]any{
			"text":       "hello",
			"sessionKey": "agent:main:bbclaw",
			"streamId":   "stream-1",
		},
	})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("handleRequest() err = %v", err)
	}
	if sink.last.Text != "hello" {
		t.Fatalf("sink.last.Text = %q", sink.last.Text)
	}
}

func TestHandleTranscriptRequestStreamsIntermediateEvents(t *testing.T) {
	upgrader := websocket.Upgrader{}
	serverConnCh := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		serverConnCh <- conn
	}))
	defer server.Close()

	wsURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("url.Parse() err = %v", err)
	}
	wsURL.Scheme = "ws"
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
	if err != nil {
		t.Fatalf("Dial() err = %v", err)
	}
	defer clientConn.Close()
	serverConn := <-serverConnCh
	defer serverConn.Close()

	sink := &fakeSink{
		streamEvents: []openclaw.VoiceTranscriptStreamEvent{
			{Type: "reply.delta", Text: "hello"},
			{Type: "reply.delta", Text: "hello world"},
		},
		delivery: openclaw.VoiceTranscriptDelivery{ReplyText: "hello world"},
	}
	a := &Adapter{
		cfg:     Config{HomeSiteID: "home-1"},
		log:     obs.NewLogger(),
		metrics: obs.NewMetrics(),
		sink:    sink,
	}

	done := make(chan error, 1)
	go func() {
		done <- a.handleTranscriptRequest(context.Background(), func(env CloudEnvelope) error {
			return serverConn.WriteJSON(env)
		}, CloudEnvelope{
			Type:      "request",
			MessageID: "msg-1",
			DeviceID:  "device-1",
			Kind:      "voice.transcript",
			Payload: map[string]any{
				"text":       "hello",
				"sessionKey": "agent:main:bbclaw",
				"streamId":   "stream-1",
			},
		})
	}()

	var events []CloudEnvelope
	for i := 0; i < 4; i++ {
		var env CloudEnvelope
		if err := clientConn.ReadJSON(&env); err != nil {
			t.Fatalf("ReadJSON() err = %v", err)
		}
		events = append(events, env)
	}
	if err := <-done; err != nil {
		t.Fatalf("handleTranscriptRequest() err = %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].Type != "event" || events[0].Kind != "voice.reply.status" {
		t.Fatalf("status event = %#v", events[0])
	}
	if events[1].Kind != "voice.reply.delta" || events[1].Payload["text"] != "hello" {
		t.Fatalf("delta1 = %#v", events[1])
	}
	if events[2].Kind != "voice.reply.delta" || events[2].Payload["text"] != "hello world" {
		t.Fatalf("delta2 = %#v", events[2])
	}
	if events[3].Type != "reply" || events[3].Kind != "voice.reply" || events[3].Payload["text"] != "hello world" {
		t.Fatalf("reply = %#v", events[3])
	}
}

// TestHeartbeatDuringLongAgentTurn verifies that the legacy voice path (no butler)
// emits voice.reply.heartbeat envelopes when the agent driver is silent for longer
// than the configured HeartbeatInterval.
func TestHeartbeatDuringLongAgentTurn(t *testing.T) {
	// Use a very short interval so the test runs fast.
	const interval = 20 * time.Millisecond

	// A driver that waits before emitting any events.
	drv := &slowFakeAgentDriver{
		name:   "mock-slow",
		delay:  4 * interval, // silent for 4 intervals → expect ≥3 heartbeats
		events: make(chan agent.Event, 4),
	}
	r := agent.NewRouter()
	r.Register(drv, obs.NewLogger())

	a := &Adapter{
		cfg:     Config{HomeSiteID: "home-1", HeartbeatInterval: interval},
		log:     obs.NewLogger(),
		metrics: obs.NewMetrics(),
	}
	a.SetRouter(r)

	var mu sync.Mutex
	var got []CloudEnvelope
	write := func(env CloudEnvelope) error {
		mu.Lock()
		got = append(got, env)
		mu.Unlock()
		return nil
	}
	env := CloudEnvelope{Type: "request", MessageID: "m-hb", DeviceID: "dev-hb", Kind: "voice.transcript"}
	if err := a.handleChatTextViaAgent(context.Background(), write, env, "hello", "sk-1", "st-1", "mock-slow", time.Now()); err != nil {
		t.Fatalf("handleChatTextViaAgent: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var heartbeats int
	for _, e := range got {
		if e.Kind == "voice.reply.heartbeat" {
			heartbeats++
			if e.Payload["phase"] != "thinking" {
				t.Errorf("heartbeat phase=%v want thinking", e.Payload["phase"])
			}
		}
	}
	if heartbeats < 2 {
		t.Fatalf("got %d heartbeats, want ≥2 (frames: %+v)", heartbeats, got)
	}
}

// slowFakeAgentDriver is a driver that sleeps for `delay` before emitting a
// text event, simulating a long tool-call or thinking silent stretch.
type slowFakeAgentDriver struct {
	name   string
	delay  time.Duration
	events chan agent.Event
}

func (d *slowFakeAgentDriver) Name() string                     { return d.name }
func (d *slowFakeAgentDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{Streaming: true, MaxInputBytes: 4096}
}
func (d *slowFakeAgentDriver) Start(context.Context, agent.StartOpts) (agent.SessionID, error) {
	return agent.SessionID(d.name + "-sid"), nil
}
func (d *slowFakeAgentDriver) Send(_ agent.SessionID, _ string) error {
	go func() {
		time.Sleep(d.delay)
		d.events <- agent.Event{Type: agent.EvText, Seq: 1, Text: "done"}
		d.events <- agent.Event{Type: agent.EvTurnEnd, Seq: 2}
	}()
	return nil
}
func (d *slowFakeAgentDriver) Events(agent.SessionID) <-chan agent.Event { return d.events }
func (d *slowFakeAgentDriver) Approve(agent.SessionID, agent.ToolID, agent.Decision) error {
	return agent.ErrUnsupported
}
func (d *slowFakeAgentDriver) Stop(agent.SessionID) error { return nil }
