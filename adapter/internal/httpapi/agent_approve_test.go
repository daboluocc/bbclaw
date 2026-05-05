package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// mockApproveDriver is a driver that supports tool approval.
type mockApproveDriver struct {
	name         string
	approveErr   error
	approveCalls []struct {
		sid      agent.SessionID
		tid      agent.ToolID
		decision agent.Decision
	}
}

func (m *mockApproveDriver) Name() string { return m.name }
func (m *mockApproveDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{ToolApproval: true}
}
func (m *mockApproveDriver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	return agent.SessionID("cli-" + m.name), nil
}
func (m *mockApproveDriver) Send(sid agent.SessionID, text string) error { return nil }
func (m *mockApproveDriver) Events(sid agent.SessionID) <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (m *mockApproveDriver) Approve(sid agent.SessionID, tid agent.ToolID, decision agent.Decision) error {
	m.approveCalls = append(m.approveCalls, struct {
		sid      agent.SessionID
		tid      agent.ToolID
		decision agent.Decision
	}{sid, tid, decision})
	return m.approveErr
}
func (m *mockApproveDriver) Stop(sid agent.SessionID) error { return nil }

// mockNoApproveDriver is a driver that does NOT support tool approval.
type mockNoApproveDriver struct{ name string }

func (m *mockNoApproveDriver) Name() string { return m.name }
func (m *mockNoApproveDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{ToolApproval: false}
}
func (m *mockNoApproveDriver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	return "", nil
}
func (m *mockNoApproveDriver) Send(sid agent.SessionID, text string) error { return nil }
func (m *mockNoApproveDriver) Events(sid agent.SessionID) <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (m *mockNoApproveDriver) Approve(sid agent.SessionID, tid agent.ToolID, decision agent.Decision) error {
	return agent.ErrUnsupported
}
func (m *mockNoApproveDriver) Stop(sid agent.SessionID) error { return nil }

// newApproveTestServer builds a server with a session registry entry ready for
// approve tests. Returns the server, the CLI session id, and the logical session id.
func newApproveTestServer(t *testing.T, drv agent.Driver) (*Server, string, string) {
	t.Helper()
	log := obs.NewLogger()
	metrics := obs.NewMetrics()

	router := agent.NewRouter()
	router.Register(drv, log)

	srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
	srv.SetAgentRouter(router)

	// Manually inject a CLI session entry into the registry.
	cliSID := "cli-test-session"
	srv.agentSessions.put(cliSID, &sessionEntry{
		sid:        agent.SessionID(cliSID),
		driverName: drv.Name(),
		lastUsed:   time.Now(),
		state:      "running",
	})

	// Create a logical session pointing at the CLI session.
	dir := t.TempDir()
	mgr, err := logicalsession.NewManager(dir+"/sessions.json", "/tmp", log)
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}
	srv.SetSessionManager(mgr)
	ls, err := mgr.Create("dev-1", drv.Name(), "/tmp", "test session")
	if err != nil {
		t.Fatalf("create logical session: %v", err)
	}
	if err := mgr.UpdateCLISessionID(ls.ID, cliSID); err != nil {
		t.Fatalf("update cli session id: %v", err)
	}

	return srv, cliSID, string(ls.ID)
}

func postApprove(srv *Server, sessionID, toolID, decision string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"toolId": toolID, "decision": decision})
	req := httptest.NewRequest(http.MethodPost,
		"/v1/agent/sessions/"+sessionID+"/approve",
		bytes.NewReader(body))
	req.SetPathValue("id", sessionID)
	w := httptest.NewRecorder()
	srv.handleAgentSessionApprove(w, req)
	return w
}

func TestHandleAgentSessionApprove(t *testing.T) {
	t.Run("success via logical session id", func(t *testing.T) {
		drv := &mockApproveDriver{name: "approve-drv"}
		srv, _, lsID := newApproveTestServer(t, drv)

		w := postApprove(srv, lsID, "t-123", "once")

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		var resp response
		json.NewDecoder(w.Body).Decode(&resp)
		if !resp.OK {
			t.Errorf("expected ok=true")
		}
		if len(drv.approveCalls) != 1 {
			t.Fatalf("expected 1 approve call, got %d", len(drv.approveCalls))
		}
		if drv.approveCalls[0].tid != "t-123" {
			t.Errorf("expected toolId t-123, got %s", drv.approveCalls[0].tid)
		}
		if drv.approveCalls[0].decision != agent.DecisionOnce {
			t.Errorf("expected decision once, got %s", drv.approveCalls[0].decision)
		}
	})

	t.Run("deny decision", func(t *testing.T) {
		drv := &mockApproveDriver{name: "approve-drv2"}
		srv, _, lsID := newApproveTestServer(t, drv)

		w := postApprove(srv, lsID, "t-456", "deny")

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if drv.approveCalls[0].decision != agent.DecisionDeny {
			t.Errorf("expected decision deny, got %s", drv.approveCalls[0].decision)
		}
	})

	t.Run("driver does not support tool approval", func(t *testing.T) {
		drv := &mockNoApproveDriver{name: "no-approve-drv"}
		srv, _, lsID := newApproveTestServer(t, drv)

		w := postApprove(srv, lsID, "t-789", "once")

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
		var resp response
		json.NewDecoder(w.Body).Decode(&resp)
		if resp.Error != "TOOL_APPROVAL_NOT_SUPPORTED" {
			t.Errorf("expected TOOL_APPROVAL_NOT_SUPPORTED, got %s", resp.Error)
		}
	})

	t.Run("unknown logical session returns 404", func(t *testing.T) {
		drv := &mockApproveDriver{name: "approve-drv3"}
		srv, _, _ := newApproveTestServer(t, drv)

		w := postApprove(srv, "ls-doesnotexist", "t-1", "once")

		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", w.Code)
		}
		var resp response
		json.NewDecoder(w.Body).Decode(&resp)
		if resp.Error != "SESSION_NOT_FOUND" {
			t.Errorf("expected SESSION_NOT_FOUND, got %s", resp.Error)
		}
	})

	t.Run("missing toolId returns 400", func(t *testing.T) {
		drv := &mockApproveDriver{name: "approve-drv4"}
		srv, _, lsID := newApproveTestServer(t, drv)

		body := strings.NewReader(`{"decision":"once"}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/agent/sessions/"+lsID+"/approve", body)
		req.SetPathValue("id", lsID)
		w := httptest.NewRecorder()
		srv.handleAgentSessionApprove(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})

	t.Run("invalid decision value returns 400", func(t *testing.T) {
		drv := &mockApproveDriver{name: "approve-drv5"}
		srv, _, lsID := newApproveTestServer(t, drv)

		w := postApprove(srv, lsID, "t-1", "always")

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})

	t.Run("router not configured returns 501", func(t *testing.T) {
		log := obs.NewLogger()
		metrics := obs.NewMetrics()
		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)

		w := postApprove(srv, "ls-abc", "t-1", "once")

		if w.Code != http.StatusNotImplemented {
			t.Fatalf("expected 501, got %d", w.Code)
		}
	})
}

func TestHandleAgentSessionGet(t *testing.T) {
	newGetTestServer := func(t *testing.T) (*Server, string) {
		t.Helper()
		log := obs.NewLogger()
		metrics := obs.NewMetrics()
		router := agent.NewRouter()
		router.Register(&mockBasicDriver{name: "test-drv"}, log)
		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)

		dir := t.TempDir()
		mgr, err := logicalsession.NewManager(dir+"/sessions.json", "/tmp", log)
		if err != nil {
			t.Fatalf("create manager: %v", err)
		}
		srv.SetSessionManager(mgr)
		ls, err := mgr.Create("dev-1", "test-drv", "/home/user/project", "My Session")
		if err != nil {
			t.Fatalf("create logical session: %v", err)
		}
		return srv, string(ls.ID)
	}

	getSession := func(srv *Server, sessionID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/agent/sessions/"+sessionID, nil)
		req.SetPathValue("id", sessionID)
		w := httptest.NewRecorder()
		srv.handleAgentSessionGet(w, req)
		return w
	}

	t.Run("returns session metadata with idle state when not in registry", func(t *testing.T) {
		srv, lsID := newGetTestServer(t)

		w := getSession(srv, lsID)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		var resp response
		json.NewDecoder(w.Body).Decode(&resp)
		if !resp.OK {
			t.Errorf("expected ok=true")
		}
		data := resp.Data.(map[string]any)
		sess := data["session"].(map[string]any)
		if sess["id"] != lsID {
			t.Errorf("expected id=%s, got %v", lsID, sess["id"])
		}
		if sess["driver"] != "test-drv" {
			t.Errorf("expected driver=test-drv, got %v", sess["driver"])
		}
		if sess["state"] != "idle" {
			t.Errorf("expected state=idle (not in registry), got %v", sess["state"])
		}
		if sess["cwd"] != "/home/user/project" {
			t.Errorf("expected cwd=/home/user/project, got %v", sess["cwd"])
		}
		if sess["title"] != "My Session" {
			t.Errorf("expected title=My Session, got %v", sess["title"])
		}
	})

	t.Run("returns running state when session is in registry", func(t *testing.T) {
		srv, lsID := newGetTestServer(t)

		// Inject a CLI session entry and link it to the logical session.
		cliSID := "cli-running-123"
		srv.agentSessions.put(cliSID, &sessionEntry{
			sid:        agent.SessionID(cliSID),
			driverName: "test-drv",
			lastUsed:   time.Now(),
			state:      "running",
		})
		srv.sessions.UpdateCLISessionID(logicalsession.ID(lsID), cliSID)

		w := getSession(srv, lsID)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var resp response
		json.NewDecoder(w.Body).Decode(&resp)
		data := resp.Data.(map[string]any)
		sess := data["session"].(map[string]any)
		if sess["state"] != "running" {
			t.Errorf("expected state=running, got %v", sess["state"])
		}
		if sess["cliSessionId"] != cliSID {
			t.Errorf("expected cliSessionId=%s, got %v", cliSID, sess["cliSessionId"])
		}
	})

	t.Run("unknown session returns 404", func(t *testing.T) {
		srv, _ := newGetTestServer(t)

		w := getSession(srv, "ls-doesnotexist")

		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", w.Code)
		}
		var resp response
		json.NewDecoder(w.Body).Decode(&resp)
		if resp.Error != "SESSION_NOT_FOUND" {
			t.Errorf("expected SESSION_NOT_FOUND, got %s", resp.Error)
		}
	})

	t.Run("non-logical id returns 400", func(t *testing.T) {
		srv, _ := newGetTestServer(t)

		w := getSession(srv, "raw-cli-id")

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
		var resp response
		json.NewDecoder(w.Body).Decode(&resp)
		if resp.Error != "NOT_LOGICAL" {
			t.Errorf("expected NOT_LOGICAL, got %s", resp.Error)
		}
	})

	t.Run("session manager not configured returns 501", func(t *testing.T) {
		log := obs.NewLogger()
		metrics := obs.NewMetrics()
		router := agent.NewRouter()
		router.Register(&mockBasicDriver{name: "test-drv"}, log)
		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)
		// No SetSessionManager call

		w := getSession(srv, "ls-abc")

		if w.Code != http.StatusNotImplemented {
			t.Fatalf("expected 501, got %d", w.Code)
		}
	})
}
