package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
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
// project just persists it — it does NOT force a restart: the project list bakes
// into the butler system prompt at the NEXT boot, and configuring projects is a
// low-urgency setup task the operator doesn't want a disruptive re-exec for
// (revised UX 2026-06-25). It therefore takes effect when the adapter next
// restarts, not immediately. Loopback-gated at the mux layer.
//
//	GET  /v1/projects → {"ok":true,"data":{"projects":[...]}}
//	POST /v1/projects   body {path, name?, summary?, cliBin?}
//	     → {"ok":true,"data":{"project":{...}}}
func Projects(store *projectstore.Store) http.HandlerFunc {
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
			writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"project": toView(proj)}})
		default:
			w.Header().Set("Allow", "GET, POST")
			writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "METHOD_NOT_ALLOWED"})
		}
	}
}

// ProjectByName handles DELETE /v1/projects/{name} (ADR-036). The name is the path
// suffix after /v1/projects/. Like Add, it does NOT force a restart (takes effect
// next boot).
//
//	DELETE /v1/projects/{name} → {"ok":true,"data":{"removed":bool}}
func ProjectByName(store *projectstore.Store) http.HandlerFunc {
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
		writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"removed": removed}})
	}
}

// dirPickTimeout bounds how long we wait for the user to pick a folder in the
// native dialog before giving up (treated as cancelled), so a walked-away user
// can't leave the request hanging forever.
const dirPickTimeout = 2 * time.Minute

// pickDir is the native OS folder chooser, overridable in tests. It returns
// (path, true, nil) on a pick, ("", false, nil) on user cancel, and an error only
// when no native picker is available (unsupported OS / missing tool) so the page
// can fall back to manual path entry.
var pickDir = pickDirNative

// PickDir opens the operating system's native "choose folder" dialog ON THE
// ADAPTER HOST and returns the chosen ABSOLUTE path (ADR-036 §决策四, revised
// 2026-06-25). This works because the admin page is loopback-only — the browser
// and the adapter are the same machine — so the dialog appears on the user's
// screen. It deliberately replaces the old in-browser directory tree: browsers hide
// a picked directory's absolute path for security, and the operator asked for a
// native picker rather than a rendered tree. A non-programmer clicks the button, a
// Finder folder chooser pops up, and the real path comes back.
//
//	POST /v1/admin/pick-dir
//	→ {"ok":true,"data":{"path":"/abs/dir"}}      picked
//	→ {"ok":true,"data":{"cancelled":true}}        user cancelled
//	→ {"ok":false,"error":"PICKER_UNAVAILABLE"}    no native picker (type a path)
func PickDir() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "METHOD_NOT_ALLOWED"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), dirPickTimeout)
		defer cancel()
		path, ok, err := pickDir(ctx)
		if err != nil {
			writeJSON(w, http.StatusOK, response{OK: false, Error: "PICKER_UNAVAILABLE", Detail: err.Error()})
			return
		}
		if !ok {
			writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"cancelled": true}})
			return
		}
		writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"path": path}})
	}
}

// pickDirNative shells out to the platform's native folder chooser. macOS uses
// AppleScript (osascript), Linux tries zenity. The prompt is a fixed literal — no
// user input reaches the shell. A user cancel surfaces as ok=false (not an error);
// a missing tool / unsupported OS is an error so the caller can fall back.
func pickDirNative(ctx context.Context) (string, bool, error) {
	switch runtime.GOOS {
	case "darwin":
		// `choose folder` returns an HFS path; `POSIX path of` converts to an
		// absolute /-path. Cancel exits non-zero with "User canceled. (-128)".
		out, err := exec.CommandContext(ctx, "osascript", "-e",
			`POSIX path of (choose folder with prompt "选择项目目录")`).Output()
		if err != nil {
			if isUserCancel(err) {
				return "", false, nil
			}
			return "", false, err
		}
		p := strings.TrimSpace(string(out))
		if p == "" {
			return "", false, nil
		}
		return filepath.Clean(p), true, nil
	case "linux":
		out, err := exec.CommandContext(ctx, "zenity", "--file-selection", "--directory",
			"--title=选择项目目录").Output()
		if err != nil {
			// zenity returns exit code 1 on cancel; distinguish "not installed".
			if _, lookErr := exec.LookPath("zenity"); lookErr != nil {
				return "", false, lookErr
			}
			return "", false, nil // treat any run error with zenity present as cancel
		}
		p := strings.TrimSpace(string(out))
		if p == "" {
			return "", false, nil
		}
		return filepath.Clean(p), true, nil
	default:
		return "", false, errNoPicker
	}
}

// errNoPicker signals no native directory picker is available on this OS.
var errNoPicker = errPicker("native directory picker not supported on this OS")

type errPicker string

func (e errPicker) Error() string { return string(e) }

// isUserCancel reports whether an osascript error is the user pressing Cancel
// (exit -128 / "User canceled"), as opposed to a real failure.
func isUserCancel(err error) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		s := string(ee.Stderr)
		return strings.Contains(s, "User canceled") || strings.Contains(s, "-128")
	}
	return false
}
