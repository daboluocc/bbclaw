package adminapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/projectstore"
)

// projectView is the admin-page shape of a project: the stored fields plus two
// computed ones — cliReady (does the project's CLI resolve right now) and editable
// (always true in P1; all entries are admin-managed). It mirrors v1's admin list.
type projectView struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Source   string    `json:"source"`
	Summary  string    `json:"summary,omitempty"`
	CLIBin   string    `json:"cliBin,omitempty"`
	CLIReady bool      `json:"cliReady"`
	Editable bool      `json:"editable"`
	AddedAt  time.Time `json:"addedAt,omitempty"`
}

func toView(p projectstore.Project) projectView {
	return projectView{
		Name:     p.Name,
		Path:     p.Path,
		Source:   p.Source,
		Summary:  p.Summary,
		CLIBin:   p.CLIBin,
		CLIReady: projectstore.CLIReady(p.CLIBin),
		Editable: true,
		AddedAt:  p.AddedAt,
	}
}

func viewList(ps []projectstore.Project) []projectView {
	out := make([]projectView, 0, len(ps))
	for _, p := range ps {
		out = append(out, toView(p))
	}
	return out
}

// Projects dispatches GET (list) / POST (add) on /v1/projects (ADR-036). Adding a
// project flags a restart, because the project list bakes into the butler system
// prompt at boot — it reaches the live butler only after the re-exec the page then
// offers. Loopback-gated at the mux layer.
//
//	GET  /v1/projects → {"ok":true,"data":{"projects":[...]}}
//	POST /v1/projects   body {path, name?, summary?, cliBin?}
//	     → {"ok":true,"data":{"project":{...},"restartRequired":true}}
func Projects(store *projectstore.Store, restart *RestartFlag) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
				"projects": viewList(store.List()),
			}})
		case http.MethodPost:
			var body struct {
				Path    string `json:"path"`
				Name    string `json:"name"`
				Summary string `json:"summary"`
				CLIBin  string `json:"cliBin"`
			}
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
			if err := dec.Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST", Detail: err.Error()})
				return
			}
			proj, err := store.Add(projectstore.Project{
				Name:    body.Name,
				Path:    body.Path,
				Summary: body.Summary,
				CLIBin:  body.CLIBin,
			})
			if err != nil {
				// Validation failures (bad/missing path, duplicate) are client errors.
				writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "PROJECT_ADD_FAILED", Detail: err.Error()})
				return
			}
			restart.Set()
			writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
				"project":         toView(proj),
				"restartRequired": true,
			}})
		default:
			w.Header().Set("Allow", "GET, POST")
			writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "METHOD_NOT_ALLOWED"})
		}
	}
}

// ProjectByName handles DELETE /v1/projects/{name} (ADR-036). The name is the path
// suffix after /v1/projects/. Removing flags a restart for the same reason as Add.
//
//	DELETE /v1/projects/{name} → {"ok":true,"data":{"removed":bool,"restartRequired":true}}
func ProjectByName(store *projectstore.Store, restart *RestartFlag) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", "DELETE")
			writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "METHOD_NOT_ALLOWED"})
			return
		}
		name := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/v1/projects/"))
		if name == "" {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST", Detail: "missing project name"})
			return
		}
		removed, err := store.Remove(name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "PROJECT_REMOVE_FAILED", Detail: err.Error()})
			return
		}
		if removed {
			restart.Set()
		}
		writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
			"removed":         removed,
			"restartRequired": removed,
		}})
	}
}

// fsEntry is one subdirectory in the directory picker.
type fsEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// FSBrowse is a server-side directory picker (ADR-036 §决策四): it lists the
// subdirectories of an absolute path so a non-programmer can browse to a project
// folder on the admin page instead of typing an absolute path. Directories only
// (a project is a directory). Defaults to the user's home dir. Loopback-gated at
// the mux layer; still, it only ever READS directory names — never file contents.
//
//	GET /v1/admin/fs?path=/abs/dir
//	→ {"ok":true,"data":{"path":"/abs/dir","parent":"/abs","dirs":[{name,path}]}}
func FSBrowse() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "METHOD_NOT_ALLOWED"})
			return
		}
		path := strings.TrimSpace(r.URL.Query().Get("path"))
		if path == "" || !filepath.IsAbs(path) {
			if home, err := os.UserHomeDir(); err == nil {
				path = home
			} else {
				path = string(filepath.Separator)
			}
		}
		path = filepath.Clean(path)
		fi, err := os.Stat(path)
		if err != nil || !fi.IsDir() {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "FS_NOT_A_DIR", Detail: "path is not an accessible directory"})
			return
		}
		ents, err := os.ReadDir(path)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "FS_READ_FAILED", Detail: err.Error()})
			return
		}
		dirs := make([]fsEntry, 0, len(ents))
		for _, e := range ents {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue // hide dotfiles/dirs from the picker by default
			}
			if !e.IsDir() {
				// A symlink to a directory reports !IsDir() from DirEntry; resolve it.
				if e.Type()&os.ModeSymlink == 0 {
					continue
				}
				if st, serr := os.Stat(filepath.Join(path, name)); serr != nil || !st.IsDir() {
					continue
				}
			}
			dirs = append(dirs, fsEntry{Name: name, Path: filepath.Join(path, name)})
		}
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
		parent := filepath.Dir(path)
		if parent == path {
			parent = "" // at filesystem root: no parent
		}
		writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
			"path":   path,
			"parent": parent,
			"dirs":   dirs,
		}})
	}
}
