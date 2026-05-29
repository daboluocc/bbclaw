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

	w := getMenu(t, srv, "sessions", "") // not implemented in this slice
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

	// select_session belongs to the sessions menu, not wired in this slice.
	w := postMenuAction(t, srv, map[string]any{"type": "select_session", "sessionId": "ls-1"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 UNSUPPORTED_ACTION", w.Code)
	}
}
