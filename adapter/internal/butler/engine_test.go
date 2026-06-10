package butler

import (
	"context"
	"errors"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// ─────────────────────────── fakes ───────────────────────────

type fakeFrame struct {
	kind string // "session" | "event" | "error"
	ev   agent.Event
	code string
	text string
	// session fields
	visibleID string
	isNew     bool
	driver    string
}

type fakeSink struct {
	frames    []fakeFrame
	sessionOK bool // when false, EmitSession returns false (client gone)
}

func newFakeSink() *fakeSink { return &fakeSink{sessionOK: true} }

func (s *fakeSink) EmitSession(visibleID string, isNew bool, driver string) bool {
	s.frames = append(s.frames, fakeFrame{kind: "session", visibleID: visibleID, isNew: isNew, driver: driver})
	return s.sessionOK
}
func (s *fakeSink) EmitEvent(ev agent.Event) bool {
	s.frames = append(s.frames, fakeFrame{kind: "event", ev: ev})
	return true
}
func (s *fakeSink) EmitError(code, text string, _ bool) bool {
	s.frames = append(s.frames, fakeFrame{kind: "error", code: code, text: text})
	return true
}

func (s *fakeSink) eventTypes() []agent.EventType {
	var out []agent.EventType
	for _, f := range s.frames {
		if f.kind == "event" {
			out = append(out, f.ev.Type)
		}
	}
	return out
}

type fakeRegEntry struct {
	driver string
	sid    agent.SessionID
	state  string
}

type fakeRegistry struct {
	m map[string]*fakeRegEntry
}

func newFakeRegistry() *fakeRegistry { return &fakeRegistry{m: map[string]*fakeRegEntry{}} }

func (r *fakeRegistry) Get(id string) (string, agent.SessionID, bool) {
	e, ok := r.m[id]
	if !ok {
		return "", "", false
	}
	return e.driver, e.sid, true
}
func (r *fakeRegistry) Put(id, driver string, sid agent.SessionID) {
	r.m[id] = &fakeRegEntry{driver: driver, sid: sid, state: "running"}
}
func (r *fakeRegistry) Touch(string)   {}
func (r *fakeRegistry) Drop(id string) { delete(r.m, id) }
func (r *fakeRegistry) SetState(id, state string) {
	if e, ok := r.m[id]; ok {
		e.state = state
	}
}

// scriptedDriver replays a fixed sequence of events per Send and returns a new
// sid per Start. It can optionally emit SESSION_NOT_FOUND on the first attempt.
type scriptedDriver struct {
	name          string
	events        chan agent.Event
	startN        int
	resumeIDs     []string
	systemPrompts []string
	mcpConfigs    []string
	cwds          []string
	sendTexts     []string
	scripts       [][]agent.Event // per-Send event scripts
	sendCalls     int
}

func newScriptedDriver(name string, scripts ...[]agent.Event) *scriptedDriver {
	return &scriptedDriver{name: name, events: make(chan agent.Event, 32), scripts: scripts}
}

func (d *scriptedDriver) Name() string { return d.name }
func (d *scriptedDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{Streaming: true}
}
func (d *scriptedDriver) Start(_ context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	d.startN++
	d.resumeIDs = append(d.resumeIDs, opts.ResumeID)
	d.systemPrompts = append(d.systemPrompts, opts.SystemPrompt)
	// Record the first injected MCP server name ("" when none) — the routing
	// tests assert butler sessions get the dispatch server and others don't.
	mcpName := ""
	if len(opts.MCPServers) > 0 {
		mcpName = opts.MCPServers[0].Name
	}
	d.mcpConfigs = append(d.mcpConfigs, mcpName)
	d.cwds = append(d.cwds, opts.Cwd)
	return agent.SessionID(d.name + "-sid"), nil
}
func (d *scriptedDriver) Send(_ agent.SessionID, text string) error {
	d.sendTexts = append(d.sendTexts, text)
	idx := d.sendCalls
	d.sendCalls++
	if idx < len(d.scripts) {
		for _, ev := range d.scripts[idx] {
			d.events <- ev
		}
	}
	return nil
}
func (d *scriptedDriver) Events(agent.SessionID) <-chan agent.Event { return d.events }
func (d *scriptedDriver) Approve(agent.SessionID, agent.ToolID, agent.Decision) error {
	return agent.ErrUnsupported
}
func (d *scriptedDriver) Stop(agent.SessionID) error { return nil }

// failStartDriver fails Start.
type failStartDriver struct{ *scriptedDriver }

func (d *failStartDriver) Start(context.Context, agent.StartOpts) (agent.SessionID, error) {
	return "", errors.New("boom")
}

// noopMetrics implements butler.MetricsSink with no-ops. Tests don't assert on
// metric names (those are verified per-caller in httpapi/homeadapter); they only
// need a non-panicking sink.
type noopMetrics struct{}

func (noopMetrics) TurnStart()              {}
func (noopMetrics) ResumeSkippedMissing()   {}
func (noopMetrics) SessionNotFoundRetry()   {}
func (noopMetrics) TurnDone(bool, int, int) {}

func baseDeps(router *agent.Router, sink EventSink, reg SessionRegistry, p Policy) Deps {
	return Deps{
		Router:             router,
		Registry:           reg,
		Sink:               sink,
		Policy:             p,
		Metrics:            noopMetrics{},
		Log:                obs.NewLogger(),
		ResolveActiveModel: func(string) string { return "" },
		StartCtx:           context.Background(),
	}
}

func routerWith(t *testing.T, d agent.Driver) *agent.Router {
	t.Helper()
	r := agent.NewRouter()
	r.Register(d, obs.NewLogger())
	return r
}

// ─────────────────────────── tests ───────────────────────────

func TestRunTurn_LocalEmitsTurnEndFrame(t *testing.T) {
	drv := newScriptedDriver("mock", []agent.Event{
		{Type: agent.EvText, Text: "hi"},
		{Type: agent.EvTurnEnd},
	})
	sink := newFakeSink()
	p := Policy{EmitTurnEndFrame: true}
	eng := NewEngine(baseDeps(routerWith(t, drv), sink, newFakeRegistry(), p))

	res, err := eng.RunTurn(context.Background(), Request{Text: "hello"})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !res.TurnEnded {
		t.Fatalf("turnEnded=false")
	}
	// LOCAL: session + text + turn_end event.
	types := sink.eventTypes()
	if len(types) != 2 || types[0] != agent.EvText || types[1] != agent.EvTurnEnd {
		t.Fatalf("event types=%v want [text turn_end]", types)
	}
	if sink.frames[0].kind != "session" {
		t.Fatalf("frame 0 not session: %+v", sink.frames[0])
	}
}

func TestRunTurn_CloudSuppressesTurnEndFrame(t *testing.T) {
	drv := newScriptedDriver("mock", []agent.Event{
		{Type: agent.EvText, Text: "hi"},
		{Type: agent.EvTurnEnd},
	})
	sink := newFakeSink()
	p := Policy{EmitTurnEndFrame: false}
	eng := NewEngine(baseDeps(routerWith(t, drv), sink, newFakeRegistry(), p))

	res, err := eng.RunTurn(context.Background(), Request{Text: "hello"})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !res.TurnEnded {
		t.Fatalf("turnEnded=false")
	}
	// CLOUD: session + text only; turn_end NOT emitted as an event.
	types := sink.eventTypes()
	if len(types) != 1 || types[0] != agent.EvText {
		t.Fatalf("event types=%v want [text] (no turn_end)", types)
	}
}

func TestRunTurn_SessionNotFoundRetrySuppressesFrame(t *testing.T) {
	// Attempt 0 → SESSION_NOT_FOUND error then turn_end; attempt 1 → text + turn_end.
	drv := newScriptedDriver("mock",
		[]agent.Event{{Type: agent.EvError, Text: "SESSION_NOT_FOUND: gone"}, {Type: agent.EvTurnEnd}},
		[]agent.Event{{Type: agent.EvText, Text: "recovered"}, {Type: agent.EvTurnEnd}},
	)
	sink := newFakeSink()
	reg := newFakeRegistry()
	p := Policy{EmitTurnEndFrame: true}
	eng := NewEngine(baseDeps(routerWith(t, drv), sink, reg, p))

	res, err := eng.RunTurn(context.Background(), Request{Text: "hello"})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if drv.startN != 2 {
		t.Fatalf("startN=%d want 2 (retry expected)", drv.startN)
	}
	// The SESSION_NOT_FOUND error frame on attempt 0 must be suppressed; only
	// the final attempt's text + turn_end reach the sink.
	for _, f := range sink.frames {
		if f.kind == "event" && f.ev.Type == agent.EvError {
			t.Fatalf("SESSION_NOT_FOUND error frame leaked: %+v", f)
		}
	}
	if res.TextCount != 1 || res.LastText != "recovered" {
		t.Fatalf("final tallies wrong: %+v", res)
	}
}

func TestRunTurn_UnknownDriverPreStream(t *testing.T) {
	drv := newScriptedDriver("mock")
	sink := newFakeSink()
	eng := NewEngine(baseDeps(routerWith(t, drv), sink, newFakeRegistry(), Policy{}))

	_, err := eng.RunTurn(context.Background(), Request{Text: "hi", RequestedDriver: "nope"})
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != "UNKNOWN_DRIVER" || !ce.PreStream {
		t.Fatalf("err=%v want PreStream UNKNOWN_DRIVER", err)
	}
	// No frame must have reached the sink for a PreStream error.
	if len(sink.frames) != 0 {
		t.Fatalf("sink got %d frames, want 0 for PreStream error: %+v", len(sink.frames), sink.frames)
	}
}

func TestRunTurn_StartFailedEmitFrameToggle(t *testing.T) {
	// LOCAL: EmitStartFailedFrame=true → an error frame is emitted.
	mkDriver := func() agent.Driver { return &failStartDriver{newScriptedDriver("mock")} }

	sinkLocal := newFakeSink()
	engLocal := NewEngine(baseDeps(routerWith(t, mkDriver()), sinkLocal, newFakeRegistry(),
		Policy{EmitStartFailedFrame: true}))
	_, errLocal := engLocal.RunTurn(context.Background(), Request{Text: "hi"})
	var ceL *CodedError
	if !errors.As(errLocal, &ceL) || ceL.Code != "AGENT_START_FAILED" {
		t.Fatalf("local err=%v want AGENT_START_FAILED", errLocal)
	}
	if len(sinkLocal.frames) != 1 || sinkLocal.frames[0].kind != "error" {
		t.Fatalf("local expected 1 error frame, got %+v", sinkLocal.frames)
	}

	// CLOUD: EmitStartFailedFrame=false → no frame, only the CodedError.
	sinkCloud := newFakeSink()
	engCloud := NewEngine(baseDeps(routerWith(t, mkDriver()), sinkCloud, newFakeRegistry(),
		Policy{EmitStartFailedFrame: false}))
	_, errCloud := engCloud.RunTurn(context.Background(), Request{Text: "hi"})
	var ceC *CodedError
	if !errors.As(errCloud, &ceC) || ceC.Code != "AGENT_START_FAILED" {
		t.Fatalf("cloud err=%v want AGENT_START_FAILED", errCloud)
	}
	if len(sinkCloud.frames) != 0 {
		t.Fatalf("cloud expected 0 frames, got %+v", sinkCloud.frames)
	}
}

func TestRunTurn_BareCLIIDRejectedWhenDisallowed(t *testing.T) {
	drv := newScriptedDriver("mock")
	sink := newFakeSink()
	eng := NewEngine(baseDeps(routerWith(t, drv), sink, newFakeRegistry(),
		Policy{AllowBareCLIID: false}))

	_, err := eng.RunTurn(context.Background(), Request{Text: "hi", RequestedSession: "cc-bare"})
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != "INVALID_SESSION_ID" || !ce.PreStream {
		t.Fatalf("err=%v want PreStream INVALID_SESSION_ID", err)
	}
	if drv.startN != 0 {
		t.Fatalf("driver started %d times, want 0", drv.startN)
	}
}

func TestRunTurn_InjectsSystemPrompt(t *testing.T) {
	drv := newScriptedDriver("mock", []agent.Event{
		{Type: agent.EvText, Text: "hi"},
		{Type: agent.EvTurnEnd},
	})
	sink := newFakeSink()
	deps := baseDeps(routerWith(t, drv), sink, newFakeRegistry(), Policy{EmitTurnEndFrame: true})
	deps.SystemPrompt = func(cwd, deviceID string) string { return "PERSONA:" + cwd }
	eng := NewEngine(deps)

	if _, err := eng.RunTurn(context.Background(), Request{Text: "hello"}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(drv.systemPrompts) != 1 || drv.systemPrompts[0] != "PERSONA:" {
		t.Fatalf("systemPrompts=%v want [PERSONA:] (empty cwd → no suffix)", drv.systemPrompts)
	}
}

// nil SystemPrompt dep must inject nothing (no panic, empty StartOpts.SystemPrompt).
func TestRunTurn_NoSystemPromptWhenNil(t *testing.T) {
	drv := newScriptedDriver("mock", []agent.Event{{Type: agent.EvTurnEnd}})
	deps := baseDeps(routerWith(t, drv), newFakeSink(), newFakeRegistry(), Policy{})
	deps.SystemPrompt = nil
	if _, err := NewEngine(deps).RunTurn(context.Background(), Request{Text: "hi"}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(drv.systemPrompts) != 1 || drv.systemPrompts[0] != "" {
		t.Fatalf("systemPrompts=%v want [\"\"]", drv.systemPrompts)
	}
}

func TestRunTurn_BareCLIIDResumeWhenAllowed(t *testing.T) {
	// CLOUD: bare id not in registry → used as ResumeID on attempt 0.
	drv := newScriptedDriver("mock", []agent.Event{{Type: agent.EvText, Text: "ok"}, {Type: agent.EvTurnEnd}})
	sink := newFakeSink()
	eng := NewEngine(baseDeps(routerWith(t, drv), sink, newFakeRegistry(),
		Policy{AllowBareCLIID: true}))

	_, err := eng.RunTurn(context.Background(), Request{Text: "hi", RequestedSession: "cc-resume"})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(drv.resumeIDs) != 1 || drv.resumeIDs[0] != "cc-resume" {
		t.Fatalf("resumeIDs=%v want [cc-resume]", drv.resumeIDs)
	}
}
