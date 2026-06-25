package adminapi

import (
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
	restart := &RestartFlag{}
	h := Projects(store, restart)
	dir := t.TempDir()

	// POST add.
	body := `{"path":"` + dir + `","name":"proj","summary":"测试项目"}`
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPost, "/v1/projects", strings.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("add status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !restart.Load() {
		t.Error("add should flag a restart")
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
	del := ProjectByName(store, &RestartFlag{})
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
	store := newProjStore(t)
	h := Projects(store, &RestartFlag{})
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
	h := Projects(store, &RestartFlag{})
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/projects", nil))
	if !strings.Contains(rr.Body.String(), `"cliReady":true`) {
		t.Errorf("expected cliReady true for an executable bin: %s", rr.Body.String())
	}
}

func TestFSBrowseListsDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "afile"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := FSBrowse()
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/fs?path="+root, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("fs status = %d, body=%s", rr.Code, rr.Body.String())
	}
	b := rr.Body.String()
	if !strings.Contains(b, "alpha") {
		t.Error("directory 'alpha' should be listed")
	}
	if strings.Contains(b, "afile") {
		t.Error("files should NOT be listed (dir picker)")
	}
	if strings.Contains(b, ".hidden") {
		t.Error("dotdirs should be hidden")
	}
}

func TestProjectsMethodNotAllowed(t *testing.T) {
	h := Projects(newProjStore(t), &RestartFlag{})
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPut, "/v1/projects", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT should be 405, got %d", rr.Code)
	}
}
