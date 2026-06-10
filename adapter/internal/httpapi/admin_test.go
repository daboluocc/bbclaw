package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/prewarm"
	"github.com/daboluocc/bbclaw/adapter/internal/projectstore"
)

func newAdminServer(t *testing.T, seed []projectstore.Project) (*Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	t.Setenv("BBCLAW_DATA_DIR", dataDir) // prewarm + store resolve here
	// Adding a project kicks off prewarm.RecordAsync, which writes MEMORY/
	// projects.md under dataDir. Await it before t.TempDir's RemoveAll runs
	// (cleanups are LIFO, so registering here runs before the temp-dir removal).
	t.Cleanup(prewarm.Wait)
	srv := NewServer(AppConfig{}, nil, nil, nil, nil, obs.NewLogger(), obs.NewMetrics())
	path := filepath.Join(dataDir, "projects.json")
	if _, err := projectstore.Bootstrap(path, seed); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	store, err := projectstore.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	srv.SetProjectStore(store)
	return srv, dataDir
}

func TestAdminLocalOnlyRejectsRemotePeer(t *testing.T) {
	srv, _ := newAdminServer(t, nil)
	h := srv.adminLocalOnly(srv.handleAdminProjectsList)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/projects", nil)
	req.RemoteAddr = "203.0.113.7:44321" // non-loopback
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote peer status = %d, want 403", rec.Code)
	}
}

func TestAdminLocalOnlyAllowsLoopback(t *testing.T) {
	srv, _ := newAdminServer(t, nil)
	h := srv.adminLocalOnly(srv.handleAdminProjectsList)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/projects", nil)
	req.RemoteAddr = "127.0.0.1:5050"
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("loopback status = %d, want 200", rec.Code)
	}
}

func TestAdminProjectsAddListDelete(t *testing.T) {
	srv, _ := newAdminServer(t, []projectstore.Project{{Name: "envproj", Path: "/tmp/envproj"}})
	ts := httptest.NewServer(srv.Handler()) // binds loopback → passes the gate
	defer ts.Close()

	projDir := t.TempDir()

	// Add a valid project.
	body, _ := json.Marshal(map[string]string{"name": "blog", "path": projDir})
	res, err := http.Post(ts.URL+"/v1/admin/projects", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("add status = %d, want 200", res.StatusCode)
	}
	res.Body.Close()

	// List must include both the seeded entry and the new admin entry.
	names := listProjectNames(t, ts.URL)
	if !names["blog"] || !names["envproj"] {
		t.Fatalf("list = %v, want blog+envproj", names)
	}

	// In the web-first model every project is removable — including the one
	// originally seeded from the environment.
	if code := deleteProject(t, ts.URL, "envproj"); code != http.StatusOK {
		t.Fatalf("delete seeded project status = %d, want 200", code)
	}
	if code := deleteProject(t, ts.URL, "blog"); code != http.StatusOK {
		t.Fatalf("delete admin project status = %d, want 200", code)
	}
	if names := listProjectNames(t, ts.URL); names["blog"] || names["envproj"] {
		t.Fatalf("projects still present after delete: %v", names)
	}
}

func TestAdminFSListsSubdirs(t *testing.T) {
	srv, _ := newAdminServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	root := t.TempDir()
	for _, d := range []string{"alpha", "beta", ".hidden"} {
		if err := mkdir(t, root, d); err != nil {
			t.Fatal(err)
		}
	}
	res, err := http.Get(ts.URL + "/v1/admin/fs?path=" + root)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Data struct {
			Path   string    `json:"path"`
			Parent string    `json:"parent"`
			Dirs   []fsEntry `json:"dirs"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, d := range out.Data.Dirs {
		names[d.Name] = true
	}
	if !names["alpha"] || !names["beta"] {
		t.Fatalf("fs dirs = %v, want alpha+beta", names)
	}
	if names[".hidden"] {
		t.Error("dotfolders should be hidden")
	}
	if out.Data.Parent == "" {
		t.Error("parent should be set for a non-root path")
	}
}

func TestAdminProjectsAddPathOnlyDerivesName(t *testing.T) {
	srv, _ := newAdminServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	parent := t.TempDir()
	dir := parent + "/myrepo"
	if err := mkdir(t, parent, "myrepo"); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"path": dir}) // no name
	res, err := http.Post(ts.URL+"/v1/admin/projects", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("add path-only status = %d, want 200", res.StatusCode)
	}
	if names := listProjectNames(t, ts.URL); !names["myrepo"] {
		t.Fatalf("name not derived from basename: %v", names)
	}
}

func TestAdminProjectsAddRejectsBadPath(t *testing.T) {
	srv, _ := newAdminServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"name": "x", "path": "relative/not/abs"})
	res, err := http.Post(ts.URL+"/v1/admin/projects", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad path status = %d, want 400", res.StatusCode)
	}
}

func TestAdminWorkspaceFilePreviewAndWhitelist(t *testing.T) {
	srv, dataDir := newAdminServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Seed a workspace CLAUDE.md under the temp data dir.
	wsMem := filepath.Join(dataDir, "workspace", "MEMORY")
	if err := os.MkdirAll(wsMem, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "workspace", "CLAUDE.md"), []byte("# 管家人设\nhello"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Whitelisted, existing file returns content.
	res, err := http.Get(ts.URL + "/v1/admin/workspace-file?name=CLAUDE.md")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data struct {
			Content string `json:"content"`
			Exists  bool   `json:"exists"`
		} `json:"data"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	if !out.Data.Exists || !strings.Contains(out.Data.Content, "管家人设") {
		t.Fatalf("CLAUDE.md preview = %+v", out.Data)
	}

	// Path-traversal / non-whitelisted name is rejected (403).
	for _, bad := range []string{"../../etc/passwd", "MEMORY/../../secret", "/etc/hosts", "random.md"} {
		r, err := http.Get(ts.URL + "/v1/admin/workspace-file?name=" + bad)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Errorf("workspace-file name=%q status = %d, want 403", bad, r.StatusCode)
		}
	}
}

func TestAdminFSSearchRecursive(t *testing.T) {
	srv, _ := newAdminServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	root := t.TempDir()
	// root/a/target-proj and root/b/c/another should both be found by "proj".
	for _, d := range []string{"a/target-proj", "b/c/sideproj", "node_modules/skip-proj"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	res, err := http.Get(ts.URL + "/v1/admin/fs/search?path=" + root + "&q=proj")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Data struct {
			Dirs []fsEntry `json:"dirs"`
		} `json:"data"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	names := map[string]bool{}
	for _, d := range out.Data.Dirs {
		names[d.Name] = true
	}
	if !names["target-proj"] || !names["sideproj"] {
		t.Fatalf("recursive search dirs = %v, want target-proj + sideproj", names)
	}
	if names["skip-proj"] {
		t.Error("node_modules should be skipped in search")
	}
}

func listProjectNames(t *testing.T, base string) map[string]bool {
	t.Helper()
	res, err := http.Get(base + "/v1/admin/projects")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Data struct {
			Projects []adminProject `json:"projects"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, p := range out.Data.Projects {
		names[p.Name] = true
	}
	return names
}

func mkdir(t *testing.T, parent, name string) error {
	t.Helper()
	return os.MkdirAll(filepath.Join(parent, name), 0o755)
}

func deleteProject(t *testing.T, base, name string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, base+"/v1/admin/projects/"+name, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}
