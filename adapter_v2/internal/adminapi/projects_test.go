package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/projectstore"
)

func newProjStore(t *testing.T) *projectstore.Store {
	t.Helper()
	s, err := projectstore.Open(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func decode(t *testing.T, rr *httptest.ResponseRecorder) response {
	t.Helper()
	var r response
	if err := json.Unmarshal(rr.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}
	return r
}

func TestProjectsAddListDelete(t *testing.T) {
	store := newProjStore(t)
	h := Projects(store)
	dir := t.TempDir()

	// POST add — persists, does NOT report a restart (revised UX).
	body := `{"path":"` + dir + `","name":"proj","summary":"测试项目"}`
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPost, "/v1/projects", strings.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("add status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "restartRequired") {
		t.Error("add must not report restartRequired (no-restart UX)")
	}

	// GET list shows it.
	rr = httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/projects", nil))
	got := decode(t, rr)
	data, _ := json.Marshal(got.Data)
	if !strings.Contains(string(data), "proj") || !strings.Contains(string(data), "测试项目") {
		t.Errorf("list missing the added project: %s", data)
	}

	// DELETE removes it.
	del := ProjectByName(store)
	rr = httptest.NewRecorder()
	del(rr, httptest.NewRequest(http.MethodDelete, "/v1/projects/proj", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if len(store.List()) != 0 {
		t.Error("project not removed")
	}
}

func TestProjectsAddRejectsBadPath(t *testing.T) {
	h := Projects(newProjStore(t))
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPost, "/v1/projects", strings.NewReader(`{"path":"relative/dir"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad path should be 400, got %d", rr.Code)
	}
	if got := decode(t, rr); got.OK {
		t.Error("bad path response should be ok=false")
	}
}

func TestProjectsCLIReadyReported(t *testing.T) {
	store := newProjStore(t)
	dir := t.TempDir()
	// An absolute, executable CLI → cliReady true.
	bin := filepath.Join(dir, "tool")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(projectstore.Project{Name: "p", Path: dir, CLIBin: bin}); err != nil {
		t.Fatal(err)
	}
	h := Projects(store)
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/projects", nil))
	if !strings.Contains(rr.Body.String(), `"cliReady":true`) {
		t.Errorf("expected cliReady true for an executable bin: %s", rr.Body.String())
	}
}

func TestPickDir(t *testing.T) {
	orig := pickDir
	defer func() { pickDir = orig }()

	// Picked → returns the path.
	pickDir = func(context.Context) (string, bool, error) { return "/Users/me/proj", true, nil }
	rr := httptest.NewRecorder()
	PickDir()(rr, httptest.NewRequest(http.MethodPost, "/v1/admin/pick-dir", nil))
	if !strings.Contains(rr.Body.String(), `"path":"/Users/me/proj"`) {
		t.Errorf("picked path not returned: %s", rr.Body.String())
	}

	// Cancelled → ok with cancelled:true, no path.
	pickDir = func(context.Context) (string, bool, error) { return "", false, nil }
	rr = httptest.NewRecorder()
	PickDir()(rr, httptest.NewRequest(http.MethodPost, "/v1/admin/pick-dir", nil))
	if !strings.Contains(rr.Body.String(), `"cancelled":true`) {
		t.Errorf("cancel not reported: %s", rr.Body.String())
	}

	// No native picker → ok=false PICKER_UNAVAILABLE so the page can fall back.
	pickDir = func(context.Context) (string, bool, error) { return "", false, errNoPicker }
	rr = httptest.NewRecorder()
	PickDir()(rr, httptest.NewRequest(http.MethodPost, "/v1/admin/pick-dir", nil))
	if !strings.Contains(rr.Body.String(), "PICKER_UNAVAILABLE") {
		t.Errorf("unavailable picker not reported: %s", rr.Body.String())
	}

	// GET is not allowed (it's an action).
	rr = httptest.NewRecorder()
	PickDir()(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/pick-dir", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET pick-dir should be 405, got %d", rr.Code)
	}
}

func TestProjectsMethodNotAllowed(t *testing.T) {
	h := Projects(newProjStore(t))
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPut, "/v1/projects", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT should be 405, got %d", rr.Code)
	}
}
