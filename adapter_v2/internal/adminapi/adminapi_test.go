package adminapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/settingsstore"
)

func newStore(t *testing.T) *settingsstore.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	st, err := settingsstore.Open(path, settingsstore.Settings{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

// TestLocalOnlyRejectsRemote verifies a non-loopback peer is 403'd (fail-closed).
func TestLocalOnlyRejectsRemote(t *testing.T) {
	h := LocalOnly(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for _, addr := range []string{"203.0.113.7:44321", "not-an-ip", ""} {
		req := httptest.NewRequest(http.MethodGet, "/v1/settings", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("RemoteAddr %q: status = %d, want 403", addr, rec.Code)
		}
	}
}

// TestLocalOnlyAllowsLoopback verifies loopback peers (v4 + v6) pass through.
func TestLocalOnlyAllowsLoopback(t *testing.T) {
	h := LocalOnly(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	for _, addr := range []string{"127.0.0.1:5050", "[::1]:5050", "127.5.6.7:9"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/settings", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusTeapot {
			t.Errorf("RemoteAddr %q: status = %d, want pass-through 418", addr, rec.Code)
		}
	}
}

// TestSettingsGetPut verifies GET returns the snapshot + derived, and PUT
// persists and sets the restart flag.
func TestSettingsGetPut(t *testing.T) {
	store := newStore(t)
	restart := &RestartFlag{}
	derive := func() Derived {
		return Derived{Version: "test", HomeSiteID: "uuid-123", Workspace: "/ws", SettingsFile: store.Path()}
	}
	h := Settings(store, restart, derive)

	// GET initial.
	{
		req := httptest.NewRequest(http.MethodGet, "/v1/settings", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET status = %d", rec.Code)
		}
		var resp struct {
			OK   bool `json:"ok"`
			Data struct {
				RestartRequired bool    `json:"restartRequired"`
				Derived         Derived `json:"derived"`
			} `json:"data"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode GET: %v", err)
		}
		if !resp.OK || resp.Data.RestartRequired {
			t.Fatalf("unexpected GET body: ok=%v restart=%v", resp.OK, resp.Data.RestartRequired)
		}
		if resp.Data.Derived.HomeSiteID != "uuid-123" {
			t.Fatalf("derived homeSiteId = %q", resp.Data.Derived.HomeSiteID)
		}
	}

	// PUT a new doc.
	body := `{"voice":{"asr":{"provider":"doubao_native","apiKey":"k"}},"device":{"streamDelta":false}}`
	{
		req := httptest.NewRequest(http.MethodPut, "/v1/settings", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			b, _ := io.ReadAll(rec.Body)
			t.Fatalf("PUT status = %d body=%s", rec.Code, b)
		}
	}
	if !restart.Load() {
		t.Fatalf("restart flag not set after PUT")
	}
	got := store.Snapshot()
	if got.Voice.ASR.Provider != "doubao_native" || got.Voice.ASR.APIKey != "k" {
		t.Fatalf("PUT did not persist: %+v", got.Voice.ASR)
	}
	if got.Device.StreamDelta {
		t.Fatalf("PUT did not persist streamDelta=false")
	}
}

// TestSettingsPutBadBody verifies a malformed body 400s.
func TestSettingsPutBadBody(t *testing.T) {
	store := newStore(t)
	h := SettingsPut(store, &RestartFlag{})
	req := httptest.NewRequest(http.MethodPut, "/v1/settings", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d, want 400", rec.Code)
	}
}
