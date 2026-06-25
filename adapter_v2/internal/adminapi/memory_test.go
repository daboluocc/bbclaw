package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemoryReadsFilesInOrder(t *testing.T) {
	ws := t.TempDir()
	mem := filepath.Join(ws, "MEMORY")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write out of canonical order + an extra file.
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(mem, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("decisions.md", "# 决策")
	write("profile.md", "# 用户档案\n- 称呼: 周老板")
	write("zzz-extra.md", "extra")
	write("notes.txt", "ignored") // non-md ignored

	derive := func() Derived { return Derived{Workspace: ws} }
	rr := httptest.NewRecorder()
	Memory(derive)(rr, httptest.NewRequest(http.MethodGet, "/v1/memory", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK   bool `json:"ok"`
		Data struct {
			Dir   string `json:"dir"`
			Files []struct {
				Name    string `json:"name"`
				Content string `json:"content"`
			} `json:"files"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range resp.Data.Files {
		names = append(names, f.Name)
	}
	// profile first (canonical order), then decisions, then the extra; .txt excluded.
	got := strings.Join(names, ",")
	if got != "profile.md,decisions.md,zzz-extra.md" {
		t.Errorf("order = %q, want profile.md,decisions.md,zzz-extra.md", got)
	}
	if resp.Data.Files[0].Content != "# 用户档案\n- 称呼: 周老板" {
		t.Errorf("profile content not returned: %q", resp.Data.Files[0].Content)
	}
}

func TestMemoryMissingDirIsEmpty(t *testing.T) {
	derive := func() Derived { return Derived{Workspace: filepath.Join(t.TempDir(), "nope")} }
	rr := httptest.NewRecorder()
	Memory(derive)(rr, httptest.NewRequest(http.MethodGet, "/v1/memory", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("missing dir should be ok, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"files":[]`) {
		t.Errorf("missing dir should return empty files: %s", rr.Body.String())
	}
}

func TestMemoryMethodNotAllowed(t *testing.T) {
	derive := func() Derived { return Derived{} }
	rr := httptest.NewRecorder()
	Memory(derive)(rr, httptest.NewRequest(http.MethodPost, "/v1/memory", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST should be 405, got %d", rr.Code)
	}
}
