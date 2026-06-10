package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/settingsstore"
)

func newSettingsTestServer(t *testing.T) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	base := settingsstore.Settings{
		Version:  1,
		Topology: settingsstore.TopologySettings{CloudRelayEnabled: true, LocalVoiceEnabled: false},
		OpenClaw: settingsstore.OpenClawSettings{WSURL: "ws://127.0.0.1:18789", NodeID: "bbclaw-adapter"},
		Cloud:    settingsstore.CloudSettings{WSURL: "wss://bbclaw.daboluo.cc/ws"},
	}
	store, err := settingsstore.Open(path, base)
	if err != nil {
		t.Fatalf("open settings store: %v", err)
	}
	srv := &Server{log: obs.NewLogger()}
	srv.SetSettingsStore(store)
	return srv
}

func TestHandleAdminSettingsGetDefault(t *testing.T) {
	srv := newSettingsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/settings", nil)
	w := httptest.NewRecorder()
	srv.handleAdminSettingsGet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeData(t, w)
	if data["restart_required"] != false {
		t.Errorf("fresh server: restart_required want false, got %v", data["restart_required"])
	}
	settings, ok := data["settings"].(map[string]any)
	if !ok {
		t.Fatalf("want settings object, got %s", w.Body.String())
	}
	topo, _ := settings["topology"].(map[string]any)
	if topo["cloud_relay_enabled"] != true {
		t.Errorf("want cloud_relay_enabled=true, got %v", topo["cloud_relay_enabled"])
	}
}

func TestHandleAdminSettingsPutValidSetsRestartFlag(t *testing.T) {
	t.Setenv("ADAPTER_MODE", "auto")
	srv := newSettingsTestServer(t)

	// Cloud-default settings (local voice off) need no ASR/TTS — must validate.
	body := settingsstore.Settings{
		Version:  1,
		Topology: settingsstore.TopologySettings{CloudRelayEnabled: false, LocalVoiceEnabled: false},
		AI:       settingsstore.AISettings{AnthropicBaseURL: "https://proxy.example", AnthropicAuthToken: "sk-test"},
		OpenClaw: settingsstore.OpenClawSettings{WSURL: "ws://127.0.0.1:18789", NodeID: "bbclaw-adapter"},
		Cloud:    settingsstore.CloudSettings{WSURL: "wss://bbclaw.daboluo.cc/ws"},
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/settings", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	srv.handleAdminSettingsPut(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	// The write must persist and flip restart_required on the subsequent GET.
	gw := httptest.NewRecorder()
	srv.handleAdminSettingsGet(gw, httptest.NewRequest(http.MethodGet, "/v1/admin/settings", nil))
	data := decodeData(t, gw)
	if data["restart_required"] != true {
		t.Errorf("after PUT: restart_required want true, got %v", data["restart_required"])
	}
	settings, _ := data["settings"].(map[string]any)
	ai, _ := settings["ai"].(map[string]any)
	if ai["anthropic_base_url"] != "https://proxy.example" {
		t.Errorf("persisted anthropic_base_url = %v", ai["anthropic_base_url"])
	}
}

// Incomplete voice config is NOT a hard error: local mode saves (200) and the
// response flags voice_incomplete so the page can nudge the user to fill ASR/TTS.
func TestHandleAdminSettingsPutLocalVoiceIncompleteSavesWithFlag(t *testing.T) {
	t.Setenv("ADAPTER_MODE", "auto")
	t.Setenv("ASR_LOCAL_BIN", "")
	srv := newSettingsTestServer(t)

	body := settingsstore.Settings{
		Version:  1,
		Topology: settingsstore.TopologySettings{LocalVoiceEnabled: true},
		Voice: settingsstore.VoiceSettings{
			ASR: settingsstore.ASRSettings{Provider: "local", LocalBin: ""},
			TTS: settingsstore.TTSSettings{Provider: "mock"},
		},
		OpenClaw: settingsstore.OpenClawSettings{WSURL: "ws://127.0.0.1:18789", NodeID: "bbclaw-adapter"},
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/settings", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	srv.handleAdminSettingsPut(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (incomplete voice degrades, not rejected), got %d: %s", w.Code, w.Body.String())
	}
	data := decodeData(t, w)
	if data["voice_incomplete"] != true {
		t.Errorf("want voice_incomplete=true, got %v", data["voice_incomplete"])
	}
}

// A genuinely structural error (malformed OpenClaw URL) is still rejected 400.
func TestHandleAdminSettingsPutStructuralRejected(t *testing.T) {
	t.Setenv("ADAPTER_MODE", "auto")
	srv := newSettingsTestServer(t)

	body := settingsstore.Settings{
		Version:  1,
		Topology: settingsstore.TopologySettings{CloudRelayEnabled: false, LocalVoiceEnabled: false},
		OpenClaw: settingsstore.OpenClawSettings{WSURL: "not a url", NodeID: "bbclaw-adapter"},
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/settings", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	srv.handleAdminSettingsPut(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for malformed openclaw url, got %d: %s", w.Code, w.Body.String())
	}
	if srv.settingsRestartReq.Load() {
		t.Error("rejected PUT must not set restart_required")
	}
}

func TestHandleAdminSettingsDisabled(t *testing.T) {
	srv := &Server{log: obs.NewLogger()} // no settings store wired
	w := httptest.NewRecorder()
	srv.handleAdminSettingsGet(w, httptest.NewRequest(http.MethodGet, "/v1/admin/settings", nil))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("want 501 when settings disabled, got %d", w.Code)
	}
}
