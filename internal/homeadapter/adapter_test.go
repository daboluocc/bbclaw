package homeadapter

import (
	"context"
	"errors"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/openclaw"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

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
	err := a.handleRequest(context.Background(), nil, CloudEnvelope{
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
	err := a.handleRequest(context.Background(), nil, CloudEnvelope{
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
		done <- a.handleTranscriptRequest(context.Background(), serverConn, CloudEnvelope{
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
