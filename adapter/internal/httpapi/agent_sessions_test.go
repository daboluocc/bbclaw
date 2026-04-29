package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// mockSessionListerDriver is a test driver that implements SessionLister
type mockSessionListerDriver struct {
	name     string
	sessions []agent.SessionInfo
	err      error
}

func (m *mockSessionListerDriver) Name() string { return m.name }
func (m *mockSessionListerDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{}
}
func (m *mockSessionListerDriver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	return "", nil
}
func (m *mockSessionListerDriver) Send(sid agent.SessionID, text string) error { return nil }
func (m *mockSessionListerDriver) Events(sid agent.SessionID) <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (m *mockSessionListerDriver) Approve(sid agent.SessionID, tid agent.ToolID, decision agent.Decision) error {
	return nil
}
func (m *mockSessionListerDriver) Stop(sid agent.SessionID) error { return nil }
func (m *mockSessionListerDriver) ListSessions(ctx context.Context, limit int) ([]agent.SessionInfo, error) {
	if m.err != nil {
		return nil, m.err
	}
	if limit > 0 && len(m.sessions) > limit {
		return m.sessions[:limit], nil
	}
	return m.sessions, nil
}

// mockBasicDriver is a test driver that does NOT implement SessionLister
type mockBasicDriver struct {
	name string
}

func (m *mockBasicDriver) Name() string { return m.name }
func (m *mockBasicDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{}
}
func (m *mockBasicDriver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	return "", nil
}
func (m *mockBasicDriver) Send(sid agent.SessionID, text string) error { return nil }
func (m *mockBasicDriver) Events(sid agent.SessionID) <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (m *mockBasicDriver) Approve(sid agent.SessionID, tid agent.ToolID, decision agent.Decision) error {
	return nil
}
func (m *mockBasicDriver) Stop(sid agent.SessionID) error { return nil }

func TestHandleAgentSessions(t *testing.T) {
	log := obs.NewLogger()
	metrics := obs.NewMetrics()

	t.Run("success with sessions", func(t *testing.T) {
		router := agent.NewRouter()
		lister := &mockSessionListerDriver{
			name: "test-driver",
			sessions: []agent.SessionInfo{
				{ID: "session-1", Preview: "Hello world", LastUsed: 1714000000, MessageCount: 5},
				{ID: "session-2", Preview: "Test message", LastUsed: 1714000100, MessageCount: 3},
			},
		}
		router.Register(lister, log)

		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)

		req := httptest.NewRequest("GET", "/v1/agent/sessions?driver=test-driver&limit=10", nil)
		w := httptest.NewRecorder()

		srv.handleAgentSessions(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var resp response
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if !resp.OK {
			t.Errorf("expected ok=true, got false")
		}

		data, ok := resp.Data.(map[string]any)
		if !ok {
			t.Fatalf("expected data to be map, got %T", resp.Data)
		}

		sessionsRaw, ok := data["sessions"]
		if !ok {
			t.Fatalf("expected sessions in data")
		}

		// Re-marshal to parse as []agent.SessionInfo
		sessionsJSON, _ := json.Marshal(sessionsRaw)
		var sessions []agent.SessionInfo
		if err := json.Unmarshal(sessionsJSON, &sessions); err != nil {
			t.Fatalf("failed to parse sessions: %v", err)
		}

		if len(sessions) != 2 {
			t.Errorf("expected 2 sessions, got %d", len(sessions))
		}

		if sessions[0].ID != "session-1" {
			t.Errorf("expected first session ID to be session-1, got %s", sessions[0].ID)
		}
	})

	t.Run("driver not implementing SessionLister returns empty list", func(t *testing.T) {
		router := agent.NewRouter()
		drv := &mockBasicDriver{name: "basic-driver"}
		router.Register(drv, log)

		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)

		req := httptest.NewRequest("GET", "/v1/agent/sessions?driver=basic-driver", nil)
		w := httptest.NewRecorder()

		srv.handleAgentSessions(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var resp response
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if !resp.OK {
			t.Errorf("expected ok=true, got false")
		}

		data, ok := resp.Data.(map[string]any)
		if !ok {
			t.Fatalf("expected data to be map, got %T", resp.Data)
		}

		sessionsRaw, ok := data["sessions"]
		if !ok {
			t.Fatalf("expected sessions in data")
		}

		sessionsJSON, _ := json.Marshal(sessionsRaw)
		var sessions []agent.SessionInfo
		if err := json.Unmarshal(sessionsJSON, &sessions); err != nil {
			t.Fatalf("failed to parse sessions: %v", err)
		}

		if len(sessions) != 0 {
			t.Errorf("expected 0 sessions for non-implementing driver, got %d", len(sessions))
		}
	})

	t.Run("unknown driver returns error", func(t *testing.T) {
		router := agent.NewRouter()
		// Register a driver so the router is considered configured
		router.Register(&mockBasicDriver{name: "existing-driver"}, log)
		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)

		req := httptest.NewRequest("GET", "/v1/agent/sessions?driver=nonexistent", nil)
		w := httptest.NewRecorder()

		srv.handleAgentSessions(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", w.Code)
		}

		var resp response
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if resp.OK {
			t.Errorf("expected ok=false, got true")
		}

		if resp.Error != "UNKNOWN_DRIVER" {
			t.Errorf("expected error UNKNOWN_DRIVER, got %s", resp.Error)
		}
	})

	t.Run("missing driver parameter returns error", func(t *testing.T) {
		router := agent.NewRouter()
		// Register a driver so the router is considered configured
		router.Register(&mockBasicDriver{name: "existing-driver"}, log)
		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)

		req := httptest.NewRequest("GET", "/v1/agent/sessions", nil)
		w := httptest.NewRecorder()

		srv.handleAgentSessions(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", w.Code)
		}

		var resp response
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if resp.Error != "DRIVER_REQUIRED" {
			t.Errorf("expected error DRIVER_REQUIRED, got %s", resp.Error)
		}
	})

	t.Run("limit parameter is respected", func(t *testing.T) {
		router := agent.NewRouter()
		lister := &mockSessionListerDriver{
			name: "test-driver",
			sessions: []agent.SessionInfo{
				{ID: "session-1", Preview: "msg1", LastUsed: 1714000000, MessageCount: 1},
				{ID: "session-2", Preview: "msg2", LastUsed: 1714000100, MessageCount: 2},
				{ID: "session-3", Preview: "msg3", LastUsed: 1714000200, MessageCount: 3},
			},
		}
		router.Register(lister, log)

		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)

		req := httptest.NewRequest("GET", "/v1/agent/sessions?driver=test-driver&limit=2", nil)
		w := httptest.NewRecorder()

		srv.handleAgentSessions(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var resp response
		json.NewDecoder(w.Body).Decode(&resp)
		data := resp.Data.(map[string]any)
		sessionsJSON, _ := json.Marshal(data["sessions"])
		var sessions []agent.SessionInfo
		json.Unmarshal(sessionsJSON, &sessions)

		if len(sessions) != 2 {
			t.Errorf("expected 2 sessions with limit=2, got %d", len(sessions))
		}
	})

	t.Run("router not configured returns error", func(t *testing.T) {
		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		// Don't set router

		req := httptest.NewRequest("GET", "/v1/agent/sessions?driver=test-driver", nil)
		w := httptest.NewRecorder()

		srv.handleAgentSessions(w, req)

		if w.Code != http.StatusNotImplemented {
			t.Errorf("expected status 501, got %d", w.Code)
		}

		var resp response
		json.NewDecoder(w.Body).Decode(&resp)

		if resp.Error != "AGENT_NOT_CONFIGURED" {
			t.Errorf("expected error AGENT_NOT_CONFIGURED, got %s", resp.Error)
		}
	})
}
