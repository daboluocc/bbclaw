package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// mockButlerDriver is a butler-capable driver for testing PUT
// /v1/agent/butler_driver (ADR-023).
type mockButlerDriver struct{ mockBasicDriver }

func (m *mockButlerDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{Butler: true}
}

func decodeData(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		OK    bool           `json:"ok"`
		Error string         `json:"error"`
		Data  map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	return env.Data
}

func TestHandleAgentButlerDriverPut(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockButlerDriver{mockBasicDriver{name: "claude-code"}}, obs.NewLogger())
	router.Register(&mockBasicDriver{name: "ollama"}, obs.NewLogger()) // not butler-capable
	srv := newMenuTestServer(t, router, true)

	// Valid: claude-code is butler-capable.
	req := httptest.NewRequest(http.MethodPut, "/v1/agent/butler_driver", strings.NewReader(`{"name":"claude-code"}`))
	w := httptest.NewRecorder()
	srv.handleAgentButlerDriverPut(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("claude-code: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got := decodeData(t, w)["butler_driver"]; got != "claude-code" {
		t.Errorf("want butler_driver=claude-code, got %v", got)
	}
	if got := srv.driverState.ButlerDriver(); got != "claude-code" {
		t.Errorf("persisted butler_driver: want claude-code, got %q", got)
	}

	// Rejected: ollama is registered but not butler-capable.
	req = httptest.NewRequest(http.MethodPut, "/v1/agent/butler_driver", strings.NewReader(`{"name":"ollama"}`))
	w = httptest.NewRecorder()
	srv.handleAgentButlerDriverPut(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("ollama: want 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "NOT_BUTLER_CAPABLE") {
		t.Errorf("want NOT_BUTLER_CAPABLE, got %s", w.Body.String())
	}

	// Unknown driver.
	req = httptest.NewRequest(http.MethodPut, "/v1/agent/butler_driver", strings.NewReader(`{"name":"nope"}`))
	w = httptest.NewRecorder()
	srv.handleAgentButlerDriverPut(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "UNKNOWN_DRIVER") {
		t.Errorf("unknown driver: want 400 UNKNOWN_DRIVER, got %d %s", w.Code, w.Body.String())
	}

	// active_driver must be untouched by a butler_driver change.
	if srv.driverState.ActiveDriver() != "" {
		t.Errorf("butler_driver PUT must not set active_driver, got %q", srv.driverState.ActiveDriver())
	}
}

func TestHandleAgentDrivers_ButlerFields(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockButlerDriver{mockBasicDriver{name: "claude-code"}}, obs.NewLogger())
	router.Register(&mockBasicDriver{name: "ollama"}, obs.NewLogger())
	srv := newMenuTestServer(t, router, true)

	req := httptest.NewRequest(http.MethodGet, "/v1/agent/drivers", nil)
	w := httptest.NewRecorder()
	srv.handleAgentDrivers(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	data := decodeData(t, w)
	// butler_driver defaults to claude-code even when nothing is persisted.
	if got := data["butler_driver"]; got != "claude-code" {
		t.Errorf("want butler_driver=claude-code (fallback), got %v", got)
	}
	rows, _ := data["drivers"].([]any)
	if len(rows) != 2 {
		t.Fatalf("want 2 drivers, got %d", len(rows))
	}
	for _, r := range rows {
		row := r.(map[string]any)
		switch row["name"] {
		case "claude-code":
			if row["butler_capable"] != true {
				t.Errorf("claude-code should be butler_capable, got %v", row["butler_capable"])
			}
		case "ollama":
			if row["butler_capable"] != false {
				t.Errorf("ollama should not be butler_capable, got %v", row["butler_capable"])
			}
		}
	}
}

func TestHandleAgentEnvironment(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, obs.NewLogger())
	srv := newMenuTestServer(t, router, true)

	req := httptest.NewRequest(http.MethodGet, "/v1/agent/environment", nil)
	w := httptest.NewRecorder()
	srv.handleAgentEnvironment(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	drivers, ok := decodeData(t, w)["drivers"].(map[string]any)
	if !ok {
		t.Fatalf("want drivers map, got %s", w.Body.String())
	}
	// Each known driver must be present with an "installed" bool, regardless of
	// whether the CLI happens to be on this host.
	for _, name := range []string{"claude-code", "opencode", "aider", "ollama", "openclaw", "codex"} {
		row, ok := drivers[name].(map[string]any)
		if !ok {
			t.Errorf("missing env row for %q", name)
			continue
		}
		if _, ok := row["installed"].(bool); !ok {
			t.Errorf("%q: installed should be bool, got %v", name, row["installed"])
		}
	}
}

// ensure context import is used (mockButlerDriver embeds mockBasicDriver whose
// methods reference context).
var _ = context.Background
