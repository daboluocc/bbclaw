package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/driverstate"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/config"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// mockModelDriver is mockBasicDriver (agent_sessions_test.go) + ModelLister.
type mockModelDriver struct {
	mockBasicDriver
	models []agent.ModelInfo
}

func (m *mockModelDriver) ListModels(context.Context) ([]agent.ModelInfo, error) {
	return m.models, nil
}

// ─────────────────────────── pure builders ───────────────────────────

func TestBuildDriversMenu(t *testing.T) {
	in := []agent.DriverInfo{{Name: "opencode"}, {Name: "claude-code"}, {Name: "ollama"}}
	m := buildDriversMenu(in, "claude-code")

	if m.ID != "drivers" || m.MenuVersion != menuVersion || m.Title == "" {
		t.Fatalf("menu header wrong: %+v", m)
	}
	// Sorted by name: claude-code, ollama, opencode.
	want := []string{"claude-code", "ollama", "opencode"}
	if len(m.Rows) != 3 {
		t.Fatalf("rows=%d want 3", len(m.Rows))
	}
	for i, r := range m.Rows {
		if r.ID != want[i] || r.Label != want[i] {
			t.Errorf("row %d = %q want %q", i, r.ID, want[i])
		}
		if r.Action.Type != "set_driver" || r.Action.Driver != want[i] {
			t.Errorf("row %d action = %+v", i, r.Action)
		}
	}
	// active marker + cursor on claude-code (index 0).
	if m.Rows[0].Marker != "active" || m.SelectedIndex != 0 {
		t.Errorf("active marker/cursor wrong: marker=%q sel=%d", m.Rows[0].Marker, m.SelectedIndex)
	}
	if m.Rows[1].Marker != "" {
		t.Errorf("non-active row should have empty marker, got %q", m.Rows[1].Marker)
	}
	if got := buildDriversMenu(nil, ""); len(got.Rows) != 0 || got.EmptyText == "" {
		t.Errorf("empty drivers menu wrong: %+v", got)
	}
}

func TestBuildModelsMenu(t *testing.T) {
	in := []agent.ModelInfo{{ID: "m1", Label: "Model One"}, {ID: "m2"}} // m2 has no label
	m := buildModelsMenu("claude-code", in, "m2")

	if m.ID != "models" || m.MenuVersion != menuVersion {
		t.Fatalf("menu header wrong: %+v", m)
	}
	if m.Rows[0].Label != "Model One" {
		t.Errorf("row0 label = %q want Model One", m.Rows[0].Label)
	}
	if m.Rows[1].Label != "m2" { // fallback to ID when label empty
		t.Errorf("row1 label = %q want m2 (id fallback)", m.Rows[1].Label)
	}
	if m.Rows[1].Marker != "active" || m.SelectedIndex != 1 {
		t.Errorf("active marker/cursor wrong: marker=%q sel=%d", m.Rows[1].Marker, m.SelectedIndex)
	}
	a := m.Rows[0].Action
	if a.Type != "set_model" || a.Driver != "claude-code" || a.Model != "m1" {
		t.Errorf("row0 action = %+v", a)
	}
	if got := buildModelsMenu("claude-code", nil, ""); len(got.Rows) != 0 || got.EmptyText == "" {
		t.Errorf("empty models menu wrong: %+v", got)
	}
}

// ─────────────────────────── handlers ───────────────────────────

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

func decodeMenu(t *testing.T, w *httptest.ResponseRecorder) Menu {
	t.Helper()
	var resp struct {
		OK   bool `json:"ok"`
		Data Menu `json:"data"`
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
	url := "/v1/agent/menu/" + id + query
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	srv.handleAgentMenu(w, req)
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

func postMenuAction(t *testing.T, srv *Server, action map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"action": action})
	req := httptest.NewRequest(http.MethodPost, "/v1/agent/menu/action", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleAgentMenuAction(w, req)
	return w
}

func TestHandleAgentMenuAction_SetDriver(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger()) // default
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

	// A genuinely unknown action type.
	w := postMenuAction(t, srv, map[string]any{"type": "frobnicate"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 UNSUPPORTED_ACTION", w.Code)
	}
}

// ─────────────────────────── sessions / cwd ───────────────────────────

func TestFormatRelativeTime(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "刚刚"},
		{5 * time.Minute, "5 分钟前"},
		{3 * time.Hour, "3 小时前"},
		{2 * 24 * time.Hour, "2 天前"},
	}
	for _, c := range cases {
		if got := formatRelativeTime(now.Add(-c.d), now); got != c.want {
			t.Errorf("d=%s got %q want %q", c.d, got, c.want)
		}
	}
	if got := formatRelativeTime(time.Time{}, now); got != "" {
		t.Errorf("zero time → %q want empty", got)
	}
	if got := formatRelativeTime(now.Add(-30*24*time.Hour), now); got == "" || strings.Contains(got, "前") {
		t.Errorf(">7d should format as a date, got %q", got)
	}
}

func TestBuildSessionsMenu(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	items := []sessionMenuItem{
		{ID: "ls-1", Title: "Auth", Driver: "claude-code", CwdName: "proj", LastUsedAt: now.Add(-5 * time.Minute)},
		{ID: "ls-2", Title: "", Driver: "ollama", LastUsedAt: now.Add(-2 * time.Hour)},
	}
	m := buildSessionsMenu(items, "ls-2", now)
	if m.ID != "sessions" || len(m.Rows) != 3 { // 2 sessions + "+ 新建"
		t.Fatalf("menu=%+v", m)
	}
	if m.Rows[0].Secondary != "claude-code · 5 分钟前 · proj" {
		t.Errorf("row0 secondary=%q", m.Rows[0].Secondary)
	}
	if m.Rows[1].Label != "(无标题)" {
		t.Errorf("empty-title fallback wrong: %q", m.Rows[1].Label)
	}
	if m.Rows[1].Marker != "active" || m.SelectedIndex != 1 {
		t.Errorf("ls-2 should be active+cursor: marker=%q sel=%d", m.Rows[1].Marker, m.SelectedIndex)
	}
	if m.Rows[0].Action.Type != "select_session" || m.Rows[0].Action.SessionID != "ls-1" {
		t.Errorf("row0 action=%+v", m.Rows[0].Action)
	}
	last := m.Rows[2]
	if last.ID != "__new__" || last.Action.Type != "open_menu" || last.Action.MenuID != "cwd" {
		t.Errorf("new row=%+v", last)
	}
	if em := buildSessionsMenu(nil, "", now); len(em.Rows) != 1 || em.Rows[0].ID != "__new__" {
		t.Errorf("empty sessions menu should be just the new row: %+v", em.Rows)
	}
}

func TestBuildCwdMenu(t *testing.T) {
	m := buildCwdMenu([]string{"a", "b"})
	if len(m.Rows) != 2 {
		t.Fatalf("rows=%d", len(m.Rows))
	}
	if m.Rows[0].Action.Type != "create_session" || m.Rows[0].Action.Cwd != "a" {
		t.Errorf("row0 action=%+v", m.Rows[0].Action)
	}
	em := buildCwdMenu(nil)
	if len(em.Rows) != 1 || em.Rows[0].ID != "__default__" || em.Rows[0].Action.Cwd != "" {
		t.Errorf("empty pool should offer default workspace: %+v", em.Rows)
	}
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

func decodeActionResult(t *testing.T, w *httptest.ResponseRecorder) menuActionResult {
	t.Helper()
	var resp struct {
		OK   bool             `json:"ok"`
		Data menuActionResult `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Fatalf("ok=false: %s", w.Body.String())
	}
	return resp.Data
}

func postMenuActionFull(t *testing.T, srv *Server, deviceID string, action map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"deviceId": deviceID, "action": action})
	req := httptest.NewRequest(http.MethodPost, "/v1/agent/menu/action", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleAgentMenuAction(w, req)
	return w
}

func TestHandleAgentMenu_Sessions(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger())
	srv, mgr := newMenuServerWithSessions(t, router, []config.CwdEntry{{Name: "proj", Path: "/p/proj"}})

	s1, _ := mgr.Create("dev1", "claude-code", "/p/proj", "First")
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
	_ = s1
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

	// Unknown id → 400.
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
	// The new session should exist with the resolved path.
	got, ok := mgr.Get(logicalsession.ID(res.SessionID))
	if !ok || got.Cwd != "/p/proj" || got.Driver != "claude-code" {
		t.Errorf("created session wrong: ok=%v %+v", ok, got)
	}

	// Unknown cwd name → 400.
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
