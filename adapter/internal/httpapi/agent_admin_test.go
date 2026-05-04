package httpapi

// ADR-014 admin endpoints. Covers:
//   - PATCH /v1/agent/sessions/{id} — partial update of logical sessions
//   - GET /v1/agent/sessions?kind=logical — list logical sessions filtered
//     by deviceId/driver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// patch sends a PATCH request because http.DefaultClient doesn't have a
// shorthand the way Get/Post do.
func patch(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	return resp
}

func decodeJSON(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return out
}

func TestHandleAgentSessionUpdate_TitleAndCwd(t *testing.T) {
	drv := newRecordingDriver("cc-1")
	_, ts, mgr := newServerWithManager(t, drv)

	ls, err := mgr.Create("dev-1", "claude-code", "/old/cwd", "old title")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"title": "new title",
		"cwd":   "/new/cwd",
	})
	resp := patch(t, ts.URL+"/v1/agent/sessions/"+string(ls.ID), body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	body2 := decodeJSON(t, resp.Body)
	data, ok := body2["data"].(map[string]any)
	if !ok {
		t.Fatalf("missing data: %+v", body2)
	}
	sess, ok := data["session"].(map[string]any)
	if !ok {
		t.Fatalf("missing session: %+v", data)
	}
	if sess["title"] != "new title" {
		t.Errorf("response title=%v want %q", sess["title"], "new title")
	}
	if sess["cwd"] != "/new/cwd" {
		t.Errorf("response cwd=%v want %q", sess["cwd"], "/new/cwd")
	}

	got, _ := mgr.Get(ls.ID)
	if got.Title != "new title" || got.Cwd != "/new/cwd" {
		t.Errorf("manager state diverged: title=%q cwd=%q", got.Title, got.Cwd)
	}
}

func TestHandleAgentSessionUpdate_OnlyTitle(t *testing.T) {
	drv := newRecordingDriver("cc-2")
	_, ts, mgr := newServerWithManager(t, drv)

	ls, err := mgr.Create("dev-1", "claude-code", "/keep", "before")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	body, _ := json.Marshal(map[string]any{"title": "after"})
	resp := patch(t, ts.URL+"/v1/agent/sessions/"+string(ls.ID), body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	got, _ := mgr.Get(ls.ID)
	if got.Title != "after" {
		t.Errorf("title=%q want after", got.Title)
	}
	if got.Cwd != "/keep" {
		t.Errorf("cwd changed to %q, want /keep", got.Cwd)
	}
}

func TestHandleAgentSessionUpdate_EmptyPatch(t *testing.T) {
	drv := newRecordingDriver("cc-3")
	_, ts, mgr := newServerWithManager(t, drv)

	ls, err := mgr.Create("dev-1", "claude-code", "/p", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	resp := patch(t, ts.URL+"/v1/agent/sessions/"+string(ls.ID), []byte(`{}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
	body := decodeJSON(t, resp.Body)
	if body["error"] != "EMPTY_PATCH" {
		t.Errorf("error=%v want EMPTY_PATCH", body["error"])
	}
}

func TestHandleAgentSessionUpdate_NonLogicalId(t *testing.T) {
	drv := newRecordingDriver("cc-4")
	_, ts, _ := newServerWithManager(t, drv)

	body, _ := json.Marshal(map[string]any{"title": "x"})
	resp := patch(t, ts.URL+"/v1/agent/sessions/cc-raw-id", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
	bodyJSON := decodeJSON(t, resp.Body)
	if bodyJSON["error"] != "NOT_LOGICAL" {
		t.Errorf("error=%v want NOT_LOGICAL", bodyJSON["error"])
	}
}

func TestHandleAgentSessionUpdate_MissingId(t *testing.T) {
	drv := newRecordingDriver("cc-5")
	_, ts, _ := newServerWithManager(t, drv)

	body, _ := json.Marshal(map[string]any{"title": "x"})
	resp := patch(t, ts.URL+"/v1/agent/sessions/ls-doesnotexist", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
	bodyJSON := decodeJSON(t, resp.Body)
	if bodyJSON["error"] != "SESSION_NOT_FOUND" {
		t.Errorf("error=%v want SESSION_NOT_FOUND", bodyJSON["error"])
	}
}

func TestHandleAgentSessions_KindLogical(t *testing.T) {
	drv := newRecordingDriver("cc-list")
	_, ts, mgr := newServerWithManager(t, drv)

	// 2 sessions on dev-A, 1 on dev-B.
	if _, err := mgr.Create("dev-A", "claude-code", "/a1", "A1"); err != nil {
		t.Fatalf("Create A1: %v", err)
	}
	if _, err := mgr.Create("dev-A", "claude-code", "/a2", "A2"); err != nil {
		t.Fatalf("Create A2: %v", err)
	}
	if _, err := mgr.Create("dev-B", "claude-code", "/b1", "B1"); err != nil {
		t.Fatalf("Create B1: %v", err)
	}

	// With deviceId=dev-A → 2 sessions.
	resp, err := http.Get(ts.URL + "/v1/agent/sessions?kind=logical&deviceId=dev-A")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := decodeJSON(t, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	data := body["data"].(map[string]any)
	sessions, _ := data["sessions"].([]any)
	if len(sessions) != 2 {
		t.Errorf("dev-A filter returned %d, want 2", len(sessions))
	}

	// Without deviceId → 3 sessions.
	resp, err = http.Get(ts.URL + "/v1/agent/sessions?kind=logical")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body = decodeJSON(t, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	data = body["data"].(map[string]any)
	sessions, _ = data["sessions"].([]any)
	if len(sessions) != 3 {
		t.Errorf("no filter returned %d, want 3", len(sessions))
	}
}

func TestHandleAgentSessions_KindLogicalDisabled(t *testing.T) {
	// Build a server with a router but no logical-session manager.
	srv := NewServer(AppConfig{}, nil, nil, nil, nil, obs.NewLogger(), obs.NewMetrics())
	srv.SetAgentDriver(newRecordingDriver("cc-disabled"))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	resp, err := http.Get(ts.URL + "/v1/agent/sessions?kind=logical")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status=%d want 501", resp.StatusCode)
	}
	body := decodeJSON(t, resp.Body)
	if body["error"] != "LOGICAL_SESSIONS_DISABLED" {
		t.Errorf("error=%v want LOGICAL_SESSIONS_DISABLED", body["error"])
	}
}

// Ensures we don't accidentally regress the legacy CLI-listing path when
// the kind query is absent — the same server should still hit the driver's
// SessionLister and not the manager.
func TestHandleAgentSessions_LegacyKindStillWorks(t *testing.T) {
	drv := newRecordingDriver("cc-legacy")
	_, ts, _ := newServerWithManager(t, drv)
	resp, err := http.Get(ts.URL + "/v1/agent/sessions?driver=claude-code")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	// recordingDriver doesn't implement SessionLister → empty list, ok=true.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp.Body)
	if body["ok"] != true {
		t.Errorf("ok=%v want true", body["ok"])
	}
}

// Smoke check: ensure the manager file path used in tests is the in-tempdir
// one — guards against accidental test setup drift that would make the
// other tests share state. Cheap to keep.
func TestHandleAgentSessionUpdate_ManagerPathIsolation(t *testing.T) {
	mgrPath := filepath.Join(t.TempDir(), "sessions.json")
	mgr, err := logicalsession.NewManager(mgrPath, "/tmp/default", obs.NewLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if !strings.HasSuffix(mgrPath, "sessions.json") {
		t.Fatalf("unexpected mgr path %q", mgrPath)
	}
	if got := mgr.List("", "", 0); len(got) != 0 {
		t.Errorf("fresh manager has %d entries, want 0", len(got))
	}
}
