package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/driverstate"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/menu"
	"github.com/daboluocc/bbclaw/adapter/internal/config"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// Menu shaping is unit-tested in internal/agent/menu; these tests cover the
// httpapi handlers: routing, data resolution, and action persistence (ADR-019).

// mockModelDriver is mockBasicDriver (agent_sessions_test.go) + ModelLister.
type mockModelDriver struct {
	mockBasicDriver
	models []agent.ModelInfo
}

func (m *mockModelDriver) ListModels(context.Context) ([]agent.ModelInfo, error) {
	return m.models, nil
}

func newMenuTestServer(t *testing.T, router *agent.Router, withState bool) *Server {
	t.Helper()
	srv := NewServer(AppConfig{}, nil, nil, nil, nil, obs.NewLogger(), obs.NewMetrics())
	srv.SetAgentRouter(router)
	if withState {
		st, err := driverstate.NewStore(filepath.Join(t.TempDir(), "driver_state.json"), obs.NewLogger())
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		srv.SetDriverState(st)
	}
	return srv
}

func newMenuServerWithSessions(t *testing.T, router *agent.Router, pool []config.CwdEntry) (*Server, *logicalsession.Manager) {
	t.Helper()
	srv := NewServer(AppConfig{CwdPool: pool}, nil, nil, nil, nil, obs.NewLogger(), obs.NewMetrics())
	srv.SetAgentRouter(router)
	mgr, err := logicalsession.NewManager(filepath.Join(t.TempDir(), "sessions.json"), "/tmp/default", obs.NewLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	srv.SetSessionManager(mgr)
	return srv, mgr
}

func decodeMenu(t *testing.T, w *httptest.ResponseRecorder) menu.Menu {
	t.Helper()
	var resp struct {
		OK   bool      `json:"ok"`
		Data menu.Menu `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Fatalf("ok=false: %s", w.Body.String())
	}
	return resp.Data
}

func decodeActionResult(t *testing.T, w *httptest.ResponseRecorder) menu.Result {
	t.Helper()
	var resp struct {
		OK   bool        `json:"ok"`
		Data menu.Result `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Fatalf("ok=false: %s", w.Body.String())
	}
	return resp.Data
}

func getMenu(t *testing.T, srv *Server, id, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/agent/menu/"+id+query, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	srv.handleAgentMenu(w, req)
	return w
}

func postMenuAction(t *testing.T, srv *Server, action map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return postMenuActionFull(t, srv, "", action)
}

func postMenuActionFull(t *testing.T, srv *Server, deviceID string, action map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"deviceId": deviceID, "action": action})
	req := httptest.NewRequest(http.MethodPost, "/v1/agent/menu/action", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleAgentMenuAction(w, req)
	return w
}

func TestHandleAgentMenu_Drivers(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger()) // first → default/active
	router.Register(&mockBasicDriver{name: "ollama"}, obs.NewLogger())
	srv := newMenuTestServer(t, router, false)

	w := getMenu(t, srv, "drivers", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	m := decodeMenu(t, w)
	if m.ID != "drivers" || len(m.Rows) != 2 {
		t.Fatalf("menu=%+v", m)
	}
	for _, r := range m.Rows {
		if r.ID == "claude-code" && r.Marker != "active" {
			t.Errorf("router default claude-code should be active, marker=%q", r.Marker)
		}
		if r.ID == "ollama" && r.Marker != "" {
			t.Errorf("ollama should not be active")
		}
	}
}

func TestHandleAgentMenu_Models(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockModelDriver{
		mockBasicDriver: mockBasicDriver{name: "claude-code"},
		models:          []agent.ModelInfo{{ID: "a"}, {ID: "b"}},
	}, obs.NewLogger())
	srv := newMenuTestServer(t, router, true)
	if err := srv.driverState.SetActiveModel("claude-code", "b"); err != nil {
		t.Fatalf("SetActiveModel: %v", err)
	}

	w := getMenu(t, srv, "models", "?driver=claude-code")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	m := decodeMenu(t, w)
	if m.ID != "models" || len(m.Rows) != 2 {
		t.Fatalf("menu=%+v", m)
	}
	if m.Rows[1].ID != "b" || m.Rows[1].Marker != "active" {
		t.Errorf("model b should be active: %+v", m.Rows)
	}
}

func TestHandleAgentMenu_UnknownMenu(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger())
	srv := newMenuTestServer(t, router, false)

	w := getMenu(t, srv, "bogus", "") // not a known menu id
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", w.Code)
	}
}

func TestHandleAgentMenu_ModelsUnknownDriver(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger())
	srv := newMenuTestServer(t, router, false)

	w := getMenu(t, srv, "models", "?driver=nope")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", w.Code)
	}
}

func TestHandleAgentMenuAction_SetDriver(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger())
	router.Register(&mockBasicDriver{name: "ollama"}, obs.NewLogger())
	srv := newMenuTestServer(t, router, true)

	w := postMenuAction(t, srv, map[string]any{"type": "set_driver", "driver": "ollama"})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := srv.driverState.ActiveDriver(); got != "ollama" {
		t.Errorf("ActiveDriver=%q want ollama", got)
	}
	if got := router.DefaultName(); got != "ollama" {
		t.Errorf("router default=%q want ollama (mirrored)", got)
	}
}

func TestHandleAgentMenuAction_SetModel(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger())
	srv := newMenuTestServer(t, router, true)

	w := postMenuAction(t, srv, map[string]any{"type": "set_model", "driver": "claude-code", "model": "claude-opus-4-8"})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := srv.driverState.ActiveModel("claude-code"); got != "claude-opus-4-8" {
		t.Errorf("ActiveModel=%q want claude-opus-4-8", got)
	}
}

func TestHandleAgentMenuAction_Unsupported(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger())
	srv := newMenuTestServer(t, router, true)

	w := postMenuAction(t, srv, map[string]any{"type": "frobnicate"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 UNSUPPORTED_ACTION", w.Code)
	}
}

func TestHandleAgentMenuAction_EmptyAction(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger())
	srv := newMenuTestServer(t, router, true)

	// Missing/empty action → EMPTY_ACTION (must match the cloud proxy's code,
	// not the old UNSUPPORTED_ACTION divergence).
	w := postMenuAction(t, srv, map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", w.Code)
	}
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != "EMPTY_ACTION" {
		t.Errorf("error=%q want EMPTY_ACTION", resp.Error)
	}
}

func TestHandleAgentMenu_Sessions(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger())
	srv, mgr := newMenuServerWithSessions(t, router, []config.CwdEntry{{Name: "proj", Path: "/p/proj"}})

	_, _ = mgr.Create("dev1", "claude-code", "/p/proj", "First")
	s2, _ := mgr.Create("dev1", "claude-code", "", "Second")

	w := getMenu(t, srv, "sessions", "?deviceId=dev1&driver=claude-code&current="+string(s2.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	m := decodeMenu(t, w)
	if m.ID != "sessions" || len(m.Rows) != 3 { // 2 sessions + "+ 新建"
		t.Fatalf("menu rows=%d: %+v", len(m.Rows), m.Rows)
	}
	var active string
	for _, r := range m.Rows {
		if r.Marker == "active" {
			active = r.ID
		}
	}
	if active != string(s2.ID) {
		t.Errorf("active=%q want %q", active, s2.ID)
	}
}

func TestHandleAgentMenu_Cwd(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger())
	srv, _ := newMenuServerWithSessions(t, router, []config.CwdEntry{{Name: "a", Path: "/pa"}, {Name: "b", Path: "/pb"}})

	w := getMenu(t, srv, "cwd", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	m := decodeMenu(t, w)
	if m.ID != "cwd" || len(m.Rows) != 2 || m.Rows[0].Label != "a" {
		t.Fatalf("menu=%+v", m)
	}
}

func TestHandleAgentMenuAction_SelectSession(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger())
	srv, mgr := newMenuServerWithSessions(t, router, nil)
	sess, _ := mgr.Create("dev1", "claude-code", "", "x")

	w := postMenuAction(t, srv, map[string]any{"type": "select_session", "sessionId": string(sess.ID)})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	res := decodeActionResult(t, w)
	if res.Result != "closed" || res.SessionID != string(sess.ID) || !res.LoadHistory {
		t.Errorf("result=%+v", res)
	}

	if w2 := postMenuAction(t, srv, map[string]any{"type": "select_session", "sessionId": "ls-nope"}); w2.Code != http.StatusBadRequest {
		t.Errorf("unknown session status=%d want 400", w2.Code)
	}
}

func TestHandleAgentMenuAction_CreateSession(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger())
	srv, mgr := newMenuServerWithSessions(t, router, []config.CwdEntry{{Name: "proj", Path: "/p/proj"}})

	w := postMenuActionFull(t, srv, "dev1", map[string]any{"type": "create_session", "cwd": "proj"})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	res := decodeActionResult(t, w)
	if res.Result != "closed" || res.SessionID == "" {
		t.Fatalf("result=%+v", res)
	}
	got, ok := mgr.Get(logicalsession.ID(res.SessionID))
	if !ok || got.Cwd != "/p/proj" || got.Driver != "claude-code" {
		t.Errorf("created session wrong: ok=%v %+v", ok, got)
	}

	if w2 := postMenuActionFull(t, srv, "dev1", map[string]any{"type": "create_session", "cwd": "nope"}); w2.Code != http.StatusBadRequest {
		t.Errorf("unknown cwd status=%d want 400", w2.Code)
	}
}

func TestHandleAgentMenuAction_OpenMenuCwd(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger())
	srv, _ := newMenuServerWithSessions(t, router, []config.CwdEntry{{Name: "a", Path: "/pa"}})

	w := postMenuAction(t, srv, map[string]any{"type": "open_menu", "menuId": "cwd"})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	res := decodeActionResult(t, w)
	if res.Result != "navigate" || res.NextMenu == nil || res.NextMenu.ID != "cwd" {
		t.Fatalf("result=%+v", res)
	}
}
