package httpapi

// ADR-014 Phase A: tests for DELETE /v1/agent/sessions/{id} with logical
// session ids, and GET /v1/agent/sessions/{id}/messages resolving logical
// ids to the underlying CLI session id.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// ── DELETE /v1/agent/sessions/{id} ──────────────────────────────────────

func TestHandleAgentDeleteSession_LogicalSession(t *testing.T) {
	drv := newRecordingDriver("cc-del-1")
	_, ts, mgr := newServerWithManager(t, drv)

	ls, err := mgr.Create("dev-1", "claude-code", "/p", "to-delete")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Simulate a CLI session having been started.
	if err := mgr.UpdateCLISessionID(ls.ID, "cc-cli-abc"); err != nil {
		t.Fatalf("UpdateCLISessionID: %v", err)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/agent/sessions/"+string(ls.ID), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp.Body)
	if body["ok"] != true {
		t.Errorf("ok=%v want true", body["ok"])
	}

	// Session must be gone from the manager.
	if _, ok := mgr.Get(ls.ID); ok {
		t.Errorf("logical session still exists after DELETE")
	}
}

func TestHandleAgentDeleteSession_LogicalNotFound(t *testing.T) {
	drv := newRecordingDriver("cc-del-2")
	_, ts, _ := newServerWithManager(t, drv)

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/agent/sessions/ls-doesnotexist", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
	body := decodeJSON(t, resp.Body)
	if body["error"] != "SESSION_NOT_FOUND" {
		t.Errorf("error=%v want SESSION_NOT_FOUND", body["error"])
	}
}

func TestHandleAgentDeleteSession_LegacyCliId(t *testing.T) {
	// Legacy path: a raw CLI id (no ls- prefix) should be removed from the
	// in-memory session registry, not the logical manager.
	drv := newRecordingDriver("cc-del-3")
	srv, ts, _ := newServerWithManager(t, drv)

	// Manually inject a legacy session entry into the registry.
	srv.agentSessions.put("cc-legacy-del", &sessionEntry{
		sid:        "cc-legacy-del",
		driverName: "claude-code",
	})

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/agent/sessions/cc-legacy-del", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	// Verify it's gone from the registry.
	if _, ok := srv.agentSessions.get("cc-legacy-del"); ok {
		t.Errorf("legacy session still in registry after DELETE")
	}
}

func TestHandleAgentDeleteSession_EmptyId(t *testing.T) {
	drv := newRecordingDriver("cc-del-4")
	_, ts, _ := newServerWithManager(t, drv)

	// The mux pattern DELETE /v1/agent/sessions/{id} won't match an empty id
	// segment, so we test the handler directly.
	srv := NewServer(AppConfig{}, nil, nil, nil, nil, obs.NewLogger(), obs.NewMetrics())
	srv.SetAgentDriver(drv)
	mgrPath := filepath.Join(t.TempDir(), "sessions.json")
	mgr, _ := logicalsession.NewManager(mgrPath, "/tmp", obs.NewLogger())
	srv.SetSessionManager(mgr)

	req := httptest.NewRequest(http.MethodDelete, "/v1/agent/sessions/", nil)
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()
	srv.handleAgentDeleteSession(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", w.Code)
	}
	_ = ts // keep ts alive for cleanup
}

// ── GET /v1/agent/sessions/{id}/messages — logical id resolution ────────

func TestHandleAgentSessionMessages_LogicalIdResolvesToCli(t *testing.T) {
	log := obs.NewLogger()
	metrics := obs.NewMetrics()

	router := agent.NewRouter()
	drv := &mockMessageLoaderDriver{name: "claude-code", all: makeSeqMessages(6)}
	router.Register(drv, log)

	srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
	srv.SetAgentRouter(router)

	mgrPath := filepath.Join(t.TempDir(), "sessions.json")
	mgr, err := logicalsession.NewManager(mgrPath, "/tmp", log)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	srv.SetSessionManager(mgr)

	ls, err := mgr.Create("dev-1", "claude-code", "/p", "test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.UpdateCLISessionID(ls.ID, "cli-real-id"); err != nil {
		t.Fatalf("UpdateCLISessionID: %v", err)
	}

	// Request with the logical id — the handler should resolve it to
	// "cli-real-id" before calling LoadMessages.
	req := httptest.NewRequest("GET", "/v1/agent/sessions/"+string(ls.ID)+"/messages?driver=claude-code&limit=3", nil)
	req.SetPathValue("id", string(ls.ID))
	w := httptest.NewRecorder()
	srv.handleAgentSessionMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (body=%s)", w.Code, w.Body.String())
	}
	_, msgs, total, hasMore := decodePage(t, w.Body.Bytes())
	if total != 6 {
		t.Errorf("total=%d want 6", total)
	}
	if !hasMore {
		t.Errorf("hasMore should be true (3/6)")
	}
	if len(msgs) != 3 {
		t.Fatalf("len(msgs)=%d want 3", len(msgs))
	}
}

func TestHandleAgentSessionMessages_LogicalIdNotFound(t *testing.T) {
	log := obs.NewLogger()
	metrics := obs.NewMetrics()

	router := agent.NewRouter()
	drv := &mockMessageLoaderDriver{name: "claude-code", all: makeSeqMessages(4)}
	router.Register(drv, log)

	srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
	srv.SetAgentRouter(router)

	mgrPath := filepath.Join(t.TempDir(), "sessions.json")
	mgr, _ := logicalsession.NewManager(mgrPath, "/tmp", log)
	srv.SetSessionManager(mgr)

	req := httptest.NewRequest("GET", "/v1/agent/sessions/ls-missing/messages?driver=claude-code", nil)
	req.SetPathValue("id", "ls-missing")
	w := httptest.NewRecorder()
	srv.handleAgentSessionMessages(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
	var resp response
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != "SESSION_NOT_FOUND" {
		t.Errorf("error=%q want SESSION_NOT_FOUND", resp.Error)
	}
}

func TestHandleAgentSessionMessages_LogicalIdNoCli(t *testing.T) {
	log := obs.NewLogger()
	metrics := obs.NewMetrics()

	router := agent.NewRouter()
	drv := &mockMessageLoaderDriver{name: "claude-code", all: makeSeqMessages(4)}
	router.Register(drv, log)

	srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
	srv.SetAgentRouter(router)

	mgrPath := filepath.Join(t.TempDir(), "sessions.json")
	mgr, _ := logicalsession.NewManager(mgrPath, "/tmp", log)
	srv.SetSessionManager(mgr)

	// Create a logical session but don't set a CLI session id (first turn
	// hasn't happened yet).
	ls, _ := mgr.Create("dev-1", "claude-code", "/p", "fresh")

	req := httptest.NewRequest("GET", "/v1/agent/sessions/"+string(ls.ID)+"/messages?driver=claude-code", nil)
	req.SetPathValue("id", string(ls.ID))
	w := httptest.NewRecorder()
	srv.handleAgentSessionMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	_, msgs, total, hasMore := decodePage(t, w.Body.Bytes())
	if total != 0 || hasMore || len(msgs) != 0 {
		t.Errorf("expected empty page, got total=%d hasMore=%v msgs=%d", total, hasMore, len(msgs))
	}
}

func TestHandleAgentSessionMessages_RawCliIdPassthrough(t *testing.T) {
	// When the session manager is wired but the id has no ls- prefix, the
	// handler should pass it through unchanged (backward compat).
	log := obs.NewLogger()
	metrics := obs.NewMetrics()

	router := agent.NewRouter()
	drv := &mockMessageLoaderDriver{name: "claude-code", all: makeSeqMessages(4)}
	router.Register(drv, log)

	srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
	srv.SetAgentRouter(router)

	mgrPath := filepath.Join(t.TempDir(), "sessions.json")
	mgr, _ := logicalsession.NewManager(mgrPath, "/tmp", log)
	srv.SetSessionManager(mgr)

	req := httptest.NewRequest("GET", "/v1/agent/sessions/cc-raw-id/messages?driver=claude-code&limit=2", nil)
	req.SetPathValue("id", "cc-raw-id")
	w := httptest.NewRecorder()
	srv.handleAgentSessionMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	_, msgs, total, _ := decodePage(t, w.Body.Bytes())
	if total != 4 || len(msgs) != 2 {
		t.Errorf("expected total=4 msgs=2, got total=%d msgs=%d", total, len(msgs))
	}
}

// ── POST /v1/agent/sessions — create endpoint ──────────────────────────

func TestHandleAgentSessionCreate_Success(t *testing.T) {
	drv := newRecordingDriver("cc-create-1")
	_, ts, mgr := newServerWithManager(t, drv)

	body, _ := json.Marshal(map[string]any{
		"driver": "claude-code",
		"title":  "My Session",
	})
	resp, err := http.Post(ts.URL+"/v1/agent/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	result := decodeJSON(t, resp.Body)
	data, _ := result["data"].(map[string]any)
	sess, _ := data["session"].(map[string]any)
	id, _ := sess["id"].(string)
	if len(id) < 4 || id[:3] != "ls-" {
		t.Errorf("expected ls- prefix, got %q", id)
	}
	if sess["title"] != "My Session" {
		t.Errorf("title=%v want My Session", sess["title"])
	}

	// Verify it's in the manager.
	list := mgr.List("", "", 0)
	if len(list) != 1 || string(list[0].ID) != id {
		t.Errorf("manager state: %+v", list)
	}
}

func TestHandleAgentSessionCreate_Disabled(t *testing.T) {
	// No session manager → 501.
	srv := NewServer(AppConfig{}, nil, nil, nil, nil, obs.NewLogger(), obs.NewMetrics())
	srv.SetAgentDriver(newRecordingDriver("cc-disabled"))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	resp, err := http.Post(ts.URL+"/v1/agent/sessions", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status=%d want 501", resp.StatusCode)
	}
}
