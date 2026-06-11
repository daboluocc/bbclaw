package homeadapter

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// When a butler workspace is configured, the cloud voice path routes the turn
// through the butler engine: it re-emits agent text as voice.reply.delta frames,
// finishes with a voice.reply envelope, and creates the device's butler logical
// session (Role=butler, cwd=workspace). ADR-021 §1.
func TestHandleChatTextViaAgent_ButlerRouting(t *testing.T) {
	drv := newFakeAgentDriver("claude-code") // butler.ButlerDriver
	r := agent.NewRouter()
	r.Register(drv, obs.NewLogger())

	mgr, err := logicalsession.NewManager(filepath.Join(t.TempDir(), "sessions.json"), "/tmp/default", obs.NewLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	a := &Adapter{
		cfg:     Config{HomeSiteID: "home-1"},
		log:     obs.NewLogger(),
		metrics: obs.NewMetrics(),
	}
	a.SetRouter(r)
	a.SetSessionManager(mgr)
	a.SetButlerWorkspace("/ws", []agent.MCPServerSpec{{Name: "bbclaw", Command: "x", Args: []string{"mcp-server"}}})

	var got []CloudEnvelope
	write := func(env CloudEnvelope) error {
		got = append(got, env)
		return nil
	}
	env := CloudEnvelope{Type: "request", MessageID: "m-1", DeviceID: "dev-1", Kind: "voice.transcript"}

	// driverName "mock2" is what the legacy path would have used; butler routing
	// must override it to claude-code regardless.
	if err := a.handleChatTextViaAgent(context.Background(), write, env, "hello", "sk-1", "st-1", "mock2", time.Now()); err != nil {
		t.Fatalf("handleChatTextViaAgent: %v", err)
	}

	// Expect a voice.reply.delta event for "hi" followed by a final voice.reply.
	var sawDelta bool
	var final *CloudEnvelope
	for i := range got {
		e := got[i]
		if e.Type == "event" && e.Kind == "voice.reply.delta" && e.Payload["text"] == "hi" {
			sawDelta = true
		}
		if e.Type == "reply" && e.Kind == "voice.reply" {
			final = &got[i]
		}
	}
	if !sawDelta {
		t.Fatalf("missing voice.reply.delta frame: %+v", got)
	}
	if final == nil {
		t.Fatalf("missing final voice.reply: %+v", got)
	}
	if final.Payload["ok"] != true || final.Payload["text"] != "hi" {
		t.Fatalf("final reply payload=%v want ok=true text=hi", final.Payload)
	}

	// The butler driver must have received the user text.
	if len(drv.sentTo) != 1 || drv.sentTo[0] != "hello" {
		t.Fatalf("butler driver sentTo=%v want [hello]", drv.sentTo)
	}

	// A Role=butler session must exist for the device.
	butlers := 0
	for _, s := range mgr.List("dev-1", "claude-code", 0) {
		if s.Role == logicalsession.RoleButler {
			butlers++
			if s.Cwd != "/ws" {
				t.Errorf("butler cwd=%q want /ws", s.Cwd)
			}
		}
	}
	if butlers != 1 {
		t.Fatalf("butler session count=%d want 1", butlers)
	}
}

// Issue #146: the cloud voice (butler) path must forward the resolved session
// id + driver to the device. Historically session frames were flattened away,
// so a pure-voice device never learned its session id and replayed nothing on
// re-entry. We now emit a dedicated voice.session event mid-turn AND redundantly
// carry {sessionId, driver} on the final voice.reply envelope.
func TestHandleChatTextViaButler_EmitsVoiceSession(t *testing.T) {
	drv := newFakeAgentDriver("claude-code") // butler.ButlerDriver
	r := agent.NewRouter()
	r.Register(drv, obs.NewLogger())

	mgr, err := logicalsession.NewManager(filepath.Join(t.TempDir(), "sessions.json"), "/tmp/default", obs.NewLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	a := &Adapter{
		cfg:     Config{HomeSiteID: "home-1"},
		log:     obs.NewLogger(),
		metrics: obs.NewMetrics(),
	}
	a.SetRouter(r)
	a.SetSessionManager(mgr)
	a.SetButlerWorkspace("/ws", []agent.MCPServerSpec{{Name: "bbclaw", Command: "x", Args: []string{"mcp-server"}}})

	var got []CloudEnvelope
	write := func(env CloudEnvelope) error {
		got = append(got, env)
		return nil
	}
	env := CloudEnvelope{Type: "request", MessageID: "m-1", DeviceID: "dev-1", Kind: "voice.transcript"}
	if err := a.handleChatTextViaAgent(context.Background(), write, env, "hello", "sk-1", "st-1", "claude-code", time.Now()); err != nil {
		t.Fatalf("handleChatTextViaAgent: %v", err)
	}

	// A voice.session event must appear with a non-empty sessionId + driver.
	var sessionEvt *CloudEnvelope
	var final *CloudEnvelope
	for i := range got {
		e := got[i]
		if e.Type == "event" && e.Kind == "voice.session" {
			sessionEvt = &got[i]
		}
		if e.Type == "reply" && e.Kind == "voice.reply" {
			final = &got[i]
		}
	}
	if sessionEvt == nil {
		t.Fatalf("missing voice.session event: %+v", got)
	}
	sid, _ := sessionEvt.Payload["sessionId"].(string)
	if sid == "" {
		t.Fatalf("voice.session missing sessionId: %v", sessionEvt.Payload)
	}
	if sessionEvt.Payload["driver"] != "claude-code" {
		t.Fatalf("voice.session driver=%v want claude-code", sessionEvt.Payload["driver"])
	}

	// The final voice.reply must redundantly carry the same sessionId + driver.
	if final == nil {
		t.Fatalf("missing final voice.reply: %+v", got)
	}
	if final.Payload["sessionId"] != sid {
		t.Fatalf("final voice.reply sessionId=%v want %q", final.Payload["sessionId"], sid)
	}
	if final.Payload["driver"] != "claude-code" {
		t.Fatalf("final voice.reply driver=%v want claude-code", final.Payload["driver"])
	}
}

// Without a configured butler workspace the legacy one-shot voice path is used
// unchanged (the requested driver runs directly, no butler session is created).
func TestHandleChatTextViaAgent_LegacyWhenButlerUnconfigured(t *testing.T) {
	drv := newFakeAgentDriver("mock2")
	r := agent.NewRouter()
	r.Register(drv, obs.NewLogger())

	mgr, err := logicalsession.NewManager(filepath.Join(t.TempDir(), "sessions.json"), "/tmp/default", obs.NewLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	a := &Adapter{
		cfg:     Config{HomeSiteID: "home-1"},
		log:     obs.NewLogger(),
		metrics: obs.NewMetrics(),
	}
	a.SetRouter(r)
	a.SetSessionManager(mgr)
	// No SetButlerWorkspace → legacy path.

	var got []CloudEnvelope
	write := func(env CloudEnvelope) error {
		got = append(got, env)
		return nil
	}
	env := CloudEnvelope{Type: "request", MessageID: "m-1", DeviceID: "dev-1", Kind: "voice.transcript"}
	if err := a.handleChatTextViaAgent(context.Background(), write, env, "hello", "sk-1", "st-1", "mock2", time.Now()); err != nil {
		t.Fatalf("handleChatTextViaAgent: %v", err)
	}
	if len(drv.sentTo) != 1 || drv.sentTo[0] != "hello" {
		t.Fatalf("legacy driver sentTo=%v want [hello]", drv.sentTo)
	}
	// Legacy path must not create any logical session.
	if sessions := mgr.List("dev-1", "", 0); len(sessions) != 0 {
		t.Fatalf("legacy path created %d logical sessions, want 0", len(sessions))
	}
}

// TestHeartbeatDuringLongButlerTurn verifies that the butler voice path emits
// voice.reply.heartbeat envelopes when the agent driver is silent for longer
// than the configured HeartbeatInterval.
func TestHeartbeatDuringLongButlerTurn(t *testing.T) {
	const interval = 20 * time.Millisecond

	// slowFakeAgentDriver (defined in adapter_test.go) delays before emitting.
	// We need a fresh channel so events don't cross test runs.
	drv := &slowFakeAgentDriver{
		name:   "claude-code",
		delay:  4 * interval,
		events: make(chan agent.Event, 4),
	}
	r := agent.NewRouter()
	r.Register(drv, obs.NewLogger())

	mgr, err := logicalsession.NewManager(filepath.Join(t.TempDir(), "sessions.json"), "/tmp/default", obs.NewLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	a := &Adapter{
		cfg:     Config{HomeSiteID: "home-1", HeartbeatInterval: interval},
		log:     obs.NewLogger(),
		metrics: obs.NewMetrics(),
	}
	a.SetRouter(r)
	a.SetSessionManager(mgr)
	a.SetButlerWorkspace("/ws", []agent.MCPServerSpec{{Name: "bbclaw", Command: "x", Args: []string{"mcp-server"}}})

	var mu sync.Mutex
	var got []CloudEnvelope
	write := func(env CloudEnvelope) error {
		mu.Lock()
		got = append(got, env)
		mu.Unlock()
		return nil
	}
	env := CloudEnvelope{Type: "request", MessageID: "m-hb", DeviceID: "dev-hb", Kind: "voice.transcript"}
	if err := a.handleChatTextViaAgent(context.Background(), write, env, "hello", "sk-1", "st-1", "claude-code", time.Now()); err != nil {
		t.Fatalf("handleChatTextViaAgent (butler): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var heartbeats int
	for _, e := range got {
		if e.Kind == "voice.reply.heartbeat" {
			heartbeats++
		}
	}
	if heartbeats < 2 {
		t.Fatalf("butler path: got %d heartbeats, want ≥2 (frames: %+v)", heartbeats, got)
	}
}
