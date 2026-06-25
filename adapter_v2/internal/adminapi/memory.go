package adminapi

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// memoryFile is one workspace MEMORY/*.md file shown read-only on the admin page.
type memoryFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// memoryOrder is the preferred display order (the butler's canonical dimensions);
// any other *.md files follow alphabetically.
var memoryOrder = []string{"profile.md", "preferences.md", "projects.md", "decisions.md"}

// memoryFileMax bounds a single file's returned content. ADR-022 clamps each
// dimension file to ~4KB, so this is only a runaway guard.
const memoryFileMax = 64 << 10

// Memory returns the butler workspace's MEMORY/*.md files — the assistant's
// accumulated picture of the user (ADR-020 / ADR-022) — for READ-ONLY display on
// the admin page. The workspace path comes from derive().Workspace (the same value
// the page's status block shows), so no extra wiring is needed. Loopback-gated at
// the mux layer; it only ever READS the known markdown files under MEMORY/.
//
//	GET /v1/memory → {"ok":true,"data":{"dir":"…/MEMORY","files":[{name,content}]}}
func Memory(derive func() Derived) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "METHOD_NOT_ALLOWED"})
			return
		}
		ws := strings.TrimSpace(derive().Workspace)
		dir := filepath.Join(ws, "MEMORY")
		ents, err := os.ReadDir(dir)
		if err != nil {
			// No workspace/MEMORY yet (fresh install before the first turn): empty, not
			// an error — the page renders an "暂无记忆" hint.
			writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"dir": dir, "files": []memoryFile{}}})
			return
		}
		byName := map[string]string{}
		var names []string
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				continue
			}
			c := string(b)
			if len(c) > memoryFileMax {
				c = c[:memoryFileMax] + "\n…(截断)"
			}
			byName[e.Name()] = c
			names = append(names, e.Name())
		}
		sort.Strings(names)
		seen := map[string]bool{}
		files := make([]memoryFile, 0, len(names))
		for _, n := range memoryOrder { // canonical dimensions first
			if c, ok := byName[n]; ok {
				files = append(files, memoryFile{Name: n, Content: c})
				seen[n] = true
			}
		}
		for _, n := range names { // any extras the butler created
			if !seen[n] {
				files = append(files, memoryFile{Name: n, Content: byName[n]})
			}
		}
		writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"dir": dir, "files": files}})
	}
}
