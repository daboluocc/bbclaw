package homeadapter

// ADR-014 Phase B: logical-session resolution in the cloud-relay agent
// proxy (kind="agent.message" envelopes). Mirrors the LAN-direct tests
// in internal/httpapi/agent_logical_test.go.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// recordingProxyDriver mirrors the LAN-direct recordingDriver — a minimal
// agent.Driver that captures the ResumeID it was started with.
type recordingProxyDriver struct {
	mu        sync.Mutex
	mintedSid agent.SessionID
	resumeIDs []string
	startN    int
	events    chan agent.Event
}

func newRecordingProxyDriver(mintedSid agent.SessionID) *recordingProxyDriver {
	return &recordingProxyDriver{
		mintedSid: mintedSid,
		events:    make(chan agent.Event, 16),
	}
}

func (d *recordingProxyDriver) Name() string { return "claude-code" }
func (d *recordingProxyDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{Streaming: true}
}

func (d *recordingProxyDriver) Start(_ context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	d.mu.Lock()
	d.startN++
	d.resumeIDs = append(d.resumeIDs, opts.ResumeID)
	d.mu.Unlock()
	return d.mintedSid, nil
}

func (d *recordingProxyDriver) Send(_ agent.SessionID, _ string) error {
	d.events <- agent.Event{Type: agent.EvText, Text: "ack"}
	d.events <- agent.Event{Type: agent.EvTurnEnd}
	return nil
}

func (d *recordingProxyDriver) Events(_ agent.SessionID) <-chan agent.Event { return d.events }
func (d *recordingProxyDriver) Approve(_ agent.SessionID, _ agent.ToolID, _ agent.Decision) error {
	return agent.ErrUnsupported
}
func (d *recordingProxyDriver) Stop(_ agent.SessionID) error { return nil }

func (d *recordingProxyDriver) resumeIDsCopy() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.resumeIDs))
	copy(out, d.resumeIDs)
	return out
}
func (d *recordingProxyDriver) startCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.startN
}

func newProxyTestAdapterWithMgr(t *testing.T, drv agent.Driver) (*Adapter, *logicalsession.Manager) {
	t.Helper()
	a := newProxyTestAdapter(t, drv)
	mgrPath := filepath.Join(t.TempDir(), "sessions.json")
	mgr, err := logicalsession.NewManager(mgrPath, "/tmp/default", obs.NewLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	a.SetSessionManager(mgr)
	return a, mgr
}

func TestAgentProxyLogicalSessionResolveAndUpdate(t *testing.T) {
	drv := newRecordingProxyDriver("cc-proxy-fresh")
	a, mgr := newProxyTestAdapterWithMgr(t, drv)

	ls, err := mgr.Create("device-x", "claude-code", "/tmp/wd", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.UpdateCLISessionID(ls.ID, "cc-stored"); err != nil {
		t.Fatalf("UpdateCLISessionID: %v", err)
	}

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
	err = a.handleRequest(context.Background(), write, CloudEnvelope{
		Type: "request", MessageID: "m-ls", DeviceID: "device-x", Kind: "agent.message",
		Payload: map[string]any{"text": "hello", "sessionId": string(ls.ID)},
	})
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatalf("no envelopes written")
	}
	// First event must be the session frame, with the LOGICAL id (not the
	// minted cli id) so the device persists a stable id.
	if got[0].Kind != "agent.event" {
		t.Fatalf("frame 0 kind=%q want agent.event", got[0].Kind)
	}
	if typ, _ := got[0].Payload["type"].(string); typ != "session" {
		t.Fatalf("frame 0 payload.type=%q want session", typ)
	}
	if sid, _ := got[0].Payload["sessionId"].(string); sid != string(ls.ID) {
		t.Errorf("frame 0 sessionId=%q want %q", sid, ls.ID)
	}

	// Driver was started with the stored cli id as ResumeID.
	resumeIDs := drv.resumeIDsCopy()
	if len(resumeIDs) != 1 || resumeIDs[0] != "cc-stored" {
		t.Errorf("driver ResumeIDs=%v want [cc-stored]", resumeIDs)
	}

	// Manager wrote back the new minted cli id.
	updated, ok := mgr.Get(ls.ID)
	if !ok || updated.CLISessionID != "cc-proxy-fresh" {
		t.Errorf("manager CLISessionID=%q want cc-proxy-fresh", updated.CLISessionID)
	}
}

func TestAgentProxyLogicalSessionAutoMintOnEmpty(t *testing.T) {
	drv := newRecordingProxyDriver("cc-proxy-mint-1")
	a, mgr := newProxyTestAdapterWithMgr(t, drv)

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
		Type: "request", MessageID: "m-mint", DeviceID: "device-y", Kind: "agent.message",
		Payload: map[string]any{"text": "hi"},
	})
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || got[0].Kind != "agent.event" {
		t.Fatalf("no session event: %+v", got)
	}
	sid, _ := got[0].Payload["sessionId"].(string)
	if len(sid) < 4 || sid[:3] != "ls-" {
		t.Errorf("expected ls- prefix on auto-minted session, got %q", sid)
	}

	// Manager has exactly one logical session and DeviceID is bound.
	list := mgr.List("", "", 0)
	if len(list) != 1 {
		t.Fatalf("manager List len=%d want 1", len(list))
	}
	if string(list[0].ID) != sid {
		t.Errorf("manager id=%q != session frame id=%q", list[0].ID, sid)
	}
	if list[0].DeviceID != "device-y" {
		t.Errorf("DeviceID=%q want device-y", list[0].DeviceID)
	}

	// Driver started with empty ResumeID.
	resumeIDs := drv.resumeIDsCopy()
	if len(resumeIDs) != 1 || resumeIDs[0] != "" {
		t.Errorf("ResumeIDs=%v want [\"\"]", resumeIDs)
	}
}

func TestAgentProxyUnknownLogicalSession(t *testing.T) {
	drv := newRecordingProxyDriver("never-used")
	a, mgr := newProxyTestAdapterWithMgr(t, drv)

	var got []CloudEnvelope
	write := func(env CloudEnvelope) error {
		got = append(got, env)
		return nil
	}
	err := a.handleRequest(context.Background(), write, CloudEnvelope{
		Type: "request", MessageID: "m-unk", DeviceID: "device-z", Kind: "agent.message",
		Payload: map[string]any{"text": "hi", "sessionId": "ls-doesnotexist"},
	})
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}

	// Should produce: an error event + a final reply with ok=false. Driver
	// must NOT have been started.
	foundErrEvent := false
	foundReply := false
	for _, env := range got {
		if env.Type == "event" && env.Payload["error"] == "UNKNOWN_LOGICAL_SESSION" {
			foundErrEvent = true
		}
		if env.Type == "reply" && env.Payload["error"] == "UNKNOWN_LOGICAL_SESSION" {
			foundReply = true
		}
	}
	if !foundErrEvent {
		t.Errorf("missing UNKNOWN_LOGICAL_SESSION error event in %+v", got)
	}
	if !foundReply {
		t.Errorf("missing UNKNOWN_LOGICAL_SESSION reply in %+v", got)
	}
	if drv.startCount() != 0 {
		t.Errorf("driver Start count=%d want 0", drv.startCount())
	}
	if list := mgr.List("", "", 0); len(list) != 0 {
		t.Errorf("manager List len=%d want 0", len(list))
	}
}
