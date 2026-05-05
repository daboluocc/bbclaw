package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/config"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

func TestHandleAgentCwdPool(t *testing.T) {
	log := obs.NewLogger()
	metrics := obs.NewMetrics()

	t.Run("empty pool returns empty array", func(t *testing.T) {
		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		req := httptest.NewRequest("GET", "/v1/agent/cwd-pool", nil)
		w := httptest.NewRecorder()
		srv.handleAgentCwdPool(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var resp response
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !resp.OK {
			t.Fatal("expected ok=true")
		}
		data := resp.Data.(map[string]any)
		pool := data["pool"].([]any)
		if len(pool) != 0 {
			t.Fatalf("expected empty pool, got %d entries", len(pool))
		}
	})

	t.Run("pool entries returned by name only (no path)", func(t *testing.T) {
		srv := NewServer(AppConfig{
			CwdPool: []config.CwdEntry{
				{Name: "myproject", Path: "/secret/path/myproject"},
				{Name: "side", Path: "/secret/path/side"},
			},
		}, nil, nil, nil, nil, log, metrics)
		req := httptest.NewRequest("GET", "/v1/agent/cwd-pool", nil)
		w := httptest.NewRecorder()
		srv.handleAgentCwdPool(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var resp response
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		data := resp.Data.(map[string]any)
		poolRaw, _ := json.Marshal(data["pool"])
		var pool []map[string]any
		json.Unmarshal(poolRaw, &pool)

		if len(pool) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(pool))
		}
		if pool[0]["name"] != "myproject" {
			t.Errorf("pool[0].name = %v", pool[0]["name"])
		}
		if pool[1]["name"] != "side" {
			t.Errorf("pool[1].name = %v", pool[1]["name"])
		}
		// path must NOT be present in the response
		if _, hasPath := pool[0]["path"]; hasPath {
			t.Error("pool[0] must not expose 'path' field to device")
		}
	})
}

func TestHandleAgentSessionCreateWithCwdName(t *testing.T) {
	log := obs.NewLogger()
	metrics := obs.NewMetrics()

	// Build a minimal session manager backed by a temp file.
	dir := t.TempDir()
	mgr, err := logicalsession.NewManager(filepath.Join(dir, "sessions.json"), "", log)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	router := agent.NewRouter()
	router.Register(&mockBasicDriver{name: "claude-code"}, log)

	t.Run("cwdName resolves to path", func(t *testing.T) {
		srv := NewServer(AppConfig{
			CwdPool: []config.CwdEntry{
				{Name: "myproject", Path: "/Users/mikas/code/myproject"},
			},
		}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)
		srv.SetSessionManager(mgr)

		body, _ := json.Marshal(map[string]string{
			"driver":  "claude-code",
			"cwdName": "myproject",
		})
		req := httptest.NewRequest("POST", "/v1/agent/sessions", bytes.NewReader(body))
		w := httptest.NewRecorder()
		srv.handleAgentSessionCreate(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp response
		json.NewDecoder(w.Body).Decode(&resp)
		if !resp.OK {
			t.Fatalf("expected ok=true, got error=%s", resp.Error)
		}
		data := resp.Data.(map[string]any)
		sessRaw, _ := json.Marshal(data["session"])
		var sess logicalsession.LogicalSession
		json.Unmarshal(sessRaw, &sess)
		if sess.Cwd != "/Users/mikas/code/myproject" {
			t.Errorf("session.cwd = %q, want /Users/mikas/code/myproject", sess.Cwd)
		}
	})

	t.Run("unknown cwdName returns 400", func(t *testing.T) {
		srv := NewServer(AppConfig{
			CwdPool: []config.CwdEntry{
				{Name: "myproject", Path: "/Users/mikas/code/myproject"},
			},
		}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)
		srv.SetSessionManager(mgr)

		body, _ := json.Marshal(map[string]string{
			"driver":  "claude-code",
			"cwdName": "nonexistent",
		})
		req := httptest.NewRequest("POST", "/v1/agent/sessions", bytes.NewReader(body))
		w := httptest.NewRecorder()
		srv.handleAgentSessionCreate(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
		var resp response
		json.NewDecoder(w.Body).Decode(&resp)
		if resp.Error != "UNKNOWN_CWD_NAME" {
			t.Errorf("expected UNKNOWN_CWD_NAME, got %s", resp.Error)
		}
	})

	t.Run("no cwdName falls back to default cwd", func(t *testing.T) {
		mgr2, _ := logicalsession.NewManager(
			filepath.Join(t.TempDir(), "sessions.json"), "/default/cwd", log)
		srv := NewServer(AppConfig{
			CwdPool: []config.CwdEntry{
				{Name: "myproject", Path: "/Users/mikas/code/myproject"},
			},
		}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)
		srv.SetSessionManager(mgr2)

		body, _ := json.Marshal(map[string]string{"driver": "claude-code"})
		req := httptest.NewRequest("POST", "/v1/agent/sessions", bytes.NewReader(body))
		w := httptest.NewRecorder()
		srv.handleAgentSessionCreate(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp response
		json.NewDecoder(w.Body).Decode(&resp)
		data := resp.Data.(map[string]any)
		sessRaw, _ := json.Marshal(data["session"])
		var sess logicalsession.LogicalSession
		json.Unmarshal(sessRaw, &sess)
		// No cwdName → manager uses its defaultCwd
		if sess.Cwd != "/default/cwd" {
			t.Errorf("session.cwd = %q, want /default/cwd", sess.Cwd)
		}
	})

}
