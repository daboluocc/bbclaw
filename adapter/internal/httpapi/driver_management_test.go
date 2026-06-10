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

// TestResolveButlerDriverFollowsActiveDriver verifies the ADR-024 §1 collapse:
// the single active_driver drives the butler when butler-capable, else falls
// back to claude-code. There is no separate butler_driver setting.
func TestResolveButlerDriverFollowsActiveDriver(t *testing.T) {
	router := agent.NewRouter()
	router.Register(&mockButlerDriver{mockBasicDriver{name: "claude-code"}}, obs.NewLogger())
	router.Register(&mockButlerDriver{mockBasicDriver{name: "opencode"}}, obs.NewLogger())
	router.Register(&mockBasicDriver{name: "ollama"}, obs.NewLogger()) // not butler-capable
	srv := newMenuTestServer(t, router, true)

	// Default: first-registered claude-code, butler-capable → butler = claude-code.
	if got := srv.resolveButlerDriver(); got != "claude-code" {
		t.Errorf("default: butler want claude-code, got %q", got)
	}

	// Switch active_driver to opencode (butler-capable) → butler follows it.
	if err := srv.driverState.SetActiveDriver("opencode"); err != nil {
		t.Fatalf("SetActiveDriver: %v", err)
	}
	srv.router.SetDefault("opencode")
	if got := srv.resolveButlerDriver(); got != "opencode" {
		t.Errorf("active=opencode: butler want opencode, got %q", got)
	}

	// Switch active_driver to ollama (NOT butler-capable) → butler falls back.
	if err := srv.driverState.SetActiveDriver("ollama"); err != nil {
		t.Fatalf("SetActiveDriver: %v", err)
	}
	srv.router.SetDefault("ollama")
	if got := srv.resolveButlerDriver(); got != "claude-code" {
		t.Errorf("active=ollama (not butler-capable): butler want claude-code fallback, got %q", got)
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
