package httpapi

// ADR-014 Phase B: logical-session resolution in the LAN-direct
// /v1/agent/message handler. These tests verify the new control flow
// activated when SetSessionManager is wired up:
//
//   1. "ls-XXX" sessionId → looked up; logical's CLISessionID becomes the
//      driver's ResumeID; the device-visible session frame still carries
//      the ls- id (stable across cli rotation); after Start the cli id is
//      written back into the logical entry.
//   2. empty sessionId → auto-mint a logical, no resume.
//   3. "ls-doesnotexist" → emit UNKNOWN_LOGICAL_SESSION error frame; never
//      call the driver.
//   4. legacy raw cli id (no ls- prefix) → fall through unchanged: ResumeID
//      = the raw id, manager state untouched.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// recordingDriver is a minimal agent.Driver that records the ResumeID it
// was started with and emits a fixed reply on Send. Each test gets its
// own instance so assertions don't interfere across tests.
type recordingDriver struct {
	mu        sync.Mutex
	mintedSid agent.SessionID
	resumeIDs []string
	startN    int
	sentTexts []string
	events    chan agent.Event
}

func newRecordingDriver(mintedSid agent.SessionID) *recordingDriver {
	return &recordingDriver{
		mintedSid: mintedSid,
		events:    make(chan agent.Event, 16),
	}
}

func (d *recordingDriver) Name() string                   { return "claude-code" }
func (d *recordingDriver) Capabilities() agent.Capabilities { return agent.Capabilities{Streaming: true} }

func (d *recordingDriver) Start(_ context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	d.mu.Lock()
	d.startN++
	d.resumeIDs = append(d.resumeIDs, opts.ResumeID)
	d.mu.Unlock()
	return d.mintedSid, nil
}

func (d *recordingDriver) Send(_ agent.SessionID, text string) error {
	d.mu.Lock()
	d.sentTexts = append(d.sentTexts, text)
	d.mu.Unlock()
	d.events <- agent.Event{Type: agent.EvText, Text: "ack"}
	d.events <- agent.Event{Type: agent.EvTurnEnd}
	return nil
}

func (d *recordingDriver) Events(_ agent.SessionID) <-chan agent.Event { return d.events }
func (d *recordingDriver) Approve(_ agent.SessionID, _ agent.ToolID, _ agent.Decision) error {
	return agent.ErrUnsupported
}
func (d *recordingDriver) Stop(_ agent.SessionID) error { return nil }

// resumeIDsCopy returns a snapshot of all ResumeID values seen by Start.
func (d *recordingDriver) resumeIDsCopy() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.resumeIDs))
	copy(out, d.resumeIDs)
	return out
}

func (d *recordingDriver) startCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.startN
}

// newServerWithManager builds a Server with a recording driver and the
// logical session manager wired up. Returns the server, ts, driver, and mgr.
func newServerWithManager(t *testing.T, drv *recordingDriver) (*Server, *httptest.Server, *logicalsession.Manager) {
	t.Helper()
	srv := NewServer(AppConfig{}, nil, nil, nil, nil, obs.NewLogger(), obs.NewMetrics())
	srv.SetAgentDriver(drv)

	mgrPath := filepath.Join(t.TempDir(), "sessions.json")
	mgr, err := logicalsession.NewManager(mgrPath, "/tmp/default", obs.NewLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	srv.SetSessionManager(mgr)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
	})
	return srv, ts, mgr
}

func TestHandleAgentMessage_LogicalSessionResolveAndUpdate(t *testing.T) {
	drv := newRecordingDriver("cc-fresh-789")
	_, ts, mgr := newServerWithManager(t, drv)

	// Pre-create a logical session with a known CLISessionID.
	ls, err := mgr.Create("dev-1", "claude-code", "/tmp/wd", "test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.UpdateCLISessionID(ls.ID, "cc-abc"); err != nil {
		t.Fatalf("UpdateCLISessionID: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"text": "hi", "sessionId": string(ls.ID)})
	resp, err := http.Post(ts.URL+"/v1/agent/message", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	frames := readNDJSONFrames(t, resp.Body)
	if len(frames) == 0 || frames[0]["type"] != "session" {
		t.Fatalf("first frame must be session, got %+v", frames)
	}
	// Device sees the logical id, NOT the cli id.
	if got, _ := frames[0]["sessionId"].(string); got != string(ls.ID) {
		t.Errorf("session frame sessionId=%q want %q", got, ls.ID)
	}

	// Driver was started with the stored cli id as ResumeID.
	resumeIDs := drv.resumeIDsCopy()
	if len(resumeIDs) != 1 || resumeIDs[0] != "cc-abc" {
		t.Errorf("driver ResumeIDs=%v want [cc-abc]", resumeIDs)
	}

	// Manager wrote back the new minted cli id.
	got, ok := mgr.Get(ls.ID)
	if !ok {
		t.Fatalf("logical session disappeared")
	}
	if got.CLISessionID != "cc-fresh-789" {
		t.Errorf("CLISessionID=%q want cc-fresh-789", got.CLISessionID)
	}
}

func TestHandleAgentMessage_LogicalSessionAutoMintOnEmpty(t *testing.T) {
	drv := newRecordingDriver("cc-fresh-1")
	_, ts, mgr := newServerWithManager(t, drv)

	body, _ := json.Marshal(map[string]any{"text": "hi"})
	resp, err := http.Post(ts.URL+"/v1/agent/message?deviceId=dev-X", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	frames := readNDJSONFrames(t, resp.Body)
	if len(frames) == 0 || frames[0]["type"] != "session" {
		t.Fatalf("first frame must be session, got %+v", frames)
	}
	sessionID, _ := frames[0]["sessionId"].(string)
	if sessionID == "" {
		t.Fatalf("session frame missing sessionId: %+v", frames[0])
	}
	if got := sessionID; len(got) < 4 || got[:3] != "ls-" {
		t.Errorf("expected ls- prefix on auto-minted session, got %q", got)
	}

	// Manager has exactly one logical session (the auto-minted one) and it
	// matches the id the device received.
	list := mgr.List("", "", 0)
	if len(list) != 1 {
		t.Fatalf("manager List len=%d want 1", len(list))
	}
	if string(list[0].ID) != sessionID {
		t.Errorf("auto-minted id=%q, manager has %q", sessionID, list[0].ID)
	}
	if list[0].DeviceID != "dev-X" {
		t.Errorf("DeviceID=%q want dev-X", list[0].DeviceID)
	}

	// Driver started with empty ResumeID (fresh conversation).
	resumeIDs := drv.resumeIDsCopy()
	if len(resumeIDs) != 1 || resumeIDs[0] != "" {
		t.Errorf("driver ResumeIDs=%v want [\"\"]", resumeIDs)
	}
}

func TestHandleAgentMessage_UnknownLogicalSession(t *testing.T) {
	drv := newRecordingDriver("cc-never-used")
	_, ts, mgr := newServerWithManager(t, drv)

	body, _ := json.Marshal(map[string]any{"text": "hi", "sessionId": "ls-doesnotexist"})
	resp, err := http.Post(ts.URL+"/v1/agent/message", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	// Either 200 with NDJSON error frame OR a JSON error response is
	// acceptable; the contract is "device sees an UNKNOWN_LOGICAL_SESSION
	// signal and the driver was never invoked". Our implementation streams
	// it as an NDJSON error frame.
	frames := readNDJSONFrames(t, resp.Body)
	foundErr := false
	for _, f := range frames {
		if f["type"] == "error" && f["error"] == "UNKNOWN_LOGICAL_SESSION" {
			foundErr = true
		}
	}
	if !foundErr {
		t.Fatalf("expected UNKNOWN_LOGICAL_SESSION error frame, got frames=%+v", frames)
	}

	// Driver must NOT have been started.
	if drv.startCount() != 0 {
		t.Errorf("driver Start count=%d want 0", drv.startCount())
	}

	// Manager state untouched.
	if got := mgr.List("", "", 0); len(got) != 0 {
		t.Errorf("manager List len=%d want 0", len(got))
	}
}

func TestHandleAgentMessage_LegacyCliIdUntouched(t *testing.T) {
	drv := newRecordingDriver("cc-fresh-legacy")
	_, ts, _ := newServerWithManager(t, drv)

	// ADR-014 Phase C: bare CLI session ids (no ls- prefix) are now rejected
	// with 400 INVALID_SESSION_ID. The backward-compat window closed when
	// v0.5 firmware shipped logical ids universally.
	body, _ := json.Marshal(map[string]any{"text": "hi", "sessionId": "cc-legacy"})
	resp, err := http.Post(ts.URL+"/v1/agent/message", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 (bare CLI ids rejected since Phase C)", resp.StatusCode)
	}
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.OK || r.Error != "INVALID_SESSION_ID" {
		t.Fatalf("resp=%+v want {ok:false, error:INVALID_SESSION_ID}", r)
	}
}
