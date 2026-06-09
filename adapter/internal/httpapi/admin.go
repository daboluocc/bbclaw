package httpapi

import (
	_ "embed"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/prewarm"
	"github.com/daboluocc/bbclaw/adapter/internal/projectstore"
	"github.com/daboluocc/bbclaw/adapter/internal/workspace"
)

// adminHTML is the single-file, zero-dependency local management page. It shows
// read-only runtime status and lets the operator add/remove project directories
// the butler may dispatch into. Vanilla JS so the binary stays self-contained.
//
//go:embed admin.html
var adminHTML []byte

// adminLocalOnly restricts a handler to loopback callers. Adding a project to the
// allow-list grants the butler authority to run agentic tasks — including command
// and file execution — in that directory, so this surface must never be reachable
// from the LAN or the cloud relay. The gate is enforced per-request on the peer
// address (the adapter itself may bind 0.0.0.0 for device traffic), independent
// of the device auth token.
func (s *Server) adminLocalOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackAddr(r.RemoteAddr) {
			writeJSON(w, http.StatusForbidden, response{
				OK:     false,
				Error:  "ADMIN_LOCAL_ONLY",
				Detail: "admin endpoints are restricted to localhost",
			})
			return
		}
		next(w, r)
	}
}

// isLoopbackAddr reports whether addr (host:port, as in http.Request.RemoteAddr)
// is a loopback peer. A malformed or non-IP address is treated as non-loopback —
// fail closed.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		host = strings.TrimSpace(addr) // some transports omit the port
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleAdminPage serves the embedded admin HTML.
func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if s.projects == nil {
		writeJSON(w, http.StatusNotImplemented, response{
			OK:     false,
			Error:  "ADMIN_DISABLED",
			Detail: "project store not configured",
		})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(adminHTML)
}

// adminProject is the wire shape for a project row (the admin page is local, so
// the full path is returned here — unlike the device-facing /v1/agent/cwd-pool).
type adminProject struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Source   string `json:"source"`
	Editable bool   `json:"editable"` // false for env-defined entries
}

// handleAdminProjectsList returns the effective project allow-list with paths.
//
//	GET /v1/admin/projects
//	response: {"ok":true,"data":{"projects":[{name,path,source,editable}, ...]}}
func (s *Server) handleAdminProjectsList(w http.ResponseWriter, r *http.Request) {
	if s.projects == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "ADMIN_DISABLED"})
		return
	}
	list := s.projects.List()
	out := make([]adminProject, 0, len(list))
	for _, p := range list {
		out = append(out, adminProject{
			Name:     p.Name,
			Path:     p.Path,
			Source:   p.Source,
			Editable: true, // every project is web-managed and removable
		})
	}
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"projects": out}})
}

// handleAdminProjectsAdd registers a new project directory and kicks off a
// best-effort prewarm scan that seeds MEMORY/projects.md.
//
//	POST /v1/admin/projects  {"name":"blog","path":"/abs/path"}
func (s *Server) handleAdminProjectsAdd(w http.ResponseWriter, r *http.Request) {
	if s.projects == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "ADMIN_DISABLED"})
		return
	}
	var body struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST", Detail: err.Error()})
		return
	}

	// The admin UI selects a directory and lets the name auto-derive from its
	// base name; an explicit name is still honoured for API callers.
	var proj projectstore.Project
	var err error
	if strings.TrimSpace(body.Name) == "" {
		proj, err = s.projects.AddPath(body.Path)
	} else {
		proj, err = s.projects.Add(body.Name, body.Path)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "ADD_REJECTED", Detail: err.Error()})
		return
	}
	s.log.Infof("admin: added project name=%q path=%q", proj.Name, proj.Path)

	// Prewarm: scan the repo and seed MEMORY/projects.md so the butler already
	// "knows" this project on the first voice turn. Best-effort + async; a failure
	// never affects the add.
	if mdPath, perr := workspace.MemoryFilePath("projects.md"); perr == nil {
		prewarm.RecordAsync(proj.Name, proj.Path, mdPath, s.log)
	} else {
		s.log.Warnf("admin: resolve projects.md path failed, skipping prewarm: %v", perr)
	}

	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
		"project": adminProject{Name: proj.Name, Path: proj.Path, Source: proj.Source, Editable: true},
	}})
}

// handleAdminProjectsDelete removes an admin-added project. Env-defined projects
// cannot be removed (they are operator configuration).
//
//	DELETE /v1/admin/projects/{name}
func (s *Server) handleAdminProjectsDelete(w http.ResponseWriter, r *http.Request) {
	if s.projects == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "ADMIN_DISABLED"})
		return
	}
	name := r.PathValue("name")
	removed, err := s.projects.Remove(name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "REMOVE_FAILED", Detail: err.Error()})
		return
	}
	if !removed {
		writeJSON(w, http.StatusNotFound, response{OK: false, Error: "NOT_FOUND", Detail: name})
		return
	}
	s.log.Infof("admin: removed project name=%q", name)
	writeJSON(w, http.StatusOK, response{OK: true})
}

// fsEntry is one sub-directory in the server-side directory browser.
type fsEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// handleAdminFS lists the immediate sub-directories of an absolute path so the
// admin page can offer a native-feeling directory picker. Browsers can't reveal
// a chosen directory's absolute path, so the picker walks the host filesystem
// server-side instead — safe because the route is loopback-only and the operator
// owns this machine. Only directory names/paths are returned; no file contents.
//
//	GET /v1/admin/fs?path=/abs/dir   (empty path → the user's home directory)
//	response: {"ok":true,"data":{"path","parent","dirs":[{name,path}, ...]}}
func (s *Server) handleAdminFS(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "NO_HOME", Detail: err.Error()})
			return
		}
		path = home
	}
	if !filepath.IsAbs(path) {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "BAD_PATH", Detail: "path must be absolute"})
		return
	}
	path = filepath.Clean(path)

	entries, err := os.ReadDir(path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "READ_FAILED", Detail: err.Error()})
		return
	}
	dirs := make([]fsEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // directories only; hide dotfolders to cut noise
		}
		dirs = append(dirs, fsEntry{Name: e.Name(), Path: filepath.Join(path, e.Name())})
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })

	parent := filepath.Dir(path)
	if parent == path {
		parent = "" // at filesystem root
	}
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
		"path":   path,
		"parent": parent,
		"dirs":   dirs,
	}})
}

// fsSearchMaxResults / fsSearchMaxDepth bound the recursive directory search so a
// keyword over a deep tree can't run away. fsSearchSkip are noisy/heavy folders
// never worth descending into.
const (
	fsSearchMaxResults = 200
	fsSearchMaxDepth   = 6
)

var fsSearchSkip = map[string]struct{}{
	"node_modules": {}, ".git": {}, "vendor": {}, "dist": {}, "build": {},
	"target": {}, ".cache": {}, "Library": {}, ".Trash": {},
}

// handleAdminFSSearch recursively finds directories under a root whose name
// contains the keyword (case-insensitive), for the picker's keyword search. It is
// bounded in depth and result count and skips heavy folders; dotfolders are
// hidden. Loopback-only like the rest of the admin surface.
//
//	GET /v1/admin/fs/search?path=/abs/root&q=keyword
//	response: {"ok":true,"data":{"dirs":[{name,path}, ...],"truncated":bool}}
func (s *Server) handleAdminFSSearch(w http.ResponseWriter, r *http.Request) {
	root := strings.TrimSpace(r.URL.Query().Get("path"))
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "EMPTY_QUERY"})
		return
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "NO_HOME", Detail: err.Error()})
			return
		}
		root = home
	}
	if !filepath.IsAbs(root) {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "BAD_PATH", Detail: "path must be absolute"})
		return
	}
	root = filepath.Clean(root)

	var hits []fsEntry
	truncated := false
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if truncated || depth > fsSearchMaxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if _, skip := fsSearchSkip[e.Name()]; skip {
				continue
			}
			full := filepath.Join(dir, e.Name())
			if strings.Contains(strings.ToLower(e.Name()), q) {
				hits = append(hits, fsEntry{Name: e.Name(), Path: full})
				if len(hits) >= fsSearchMaxResults {
					truncated = true
					return
				}
			}
			walk(full, depth+1)
		}
	}
	walk(root, 0)
	sort.Slice(hits, func(i, j int) bool { return hits[i].Path < hits[j].Path })

	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
		"dirs":      hits,
		"truncated": truncated,
	}})
}

// workspacePreviewFiles is the whitelist of butler workspace files the admin page
// may preview (read-only). Anything outside this set is rejected, so the endpoint
// can never read arbitrary host files. Paths are relative to the workspace dir.
var workspacePreviewFiles = []string{
	"CLAUDE.md",
	"MEMORY/profile.md",
	"MEMORY/preferences.md",
	"MEMORY/projects.md",
	"MEMORY/decisions.md",
}

// maxPreviewBytes caps how much of a workspace file the preview returns.
const maxPreviewBytes = 256 << 10

// workspaceFileMeta is one previewable file's listing entry.
type workspaceFileMeta struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
	Size   int64  `json:"size"`
}

// handleAdminWorkspaceFiles lists the previewable workspace files with existence
// and size, so the page can render the file menu.
//
//	GET /v1/admin/workspace-files
func (s *Server) handleAdminWorkspaceFiles(w http.ResponseWriter, r *http.Request) {
	dir, err := workspace.Dir()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "NO_WORKSPACE", Detail: err.Error()})
		return
	}
	out := make([]workspaceFileMeta, 0, len(workspacePreviewFiles))
	for _, name := range workspacePreviewFiles {
		meta := workspaceFileMeta{Name: name}
		if fi, statErr := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); statErr == nil {
			meta.Exists = true
			meta.Size = fi.Size()
		}
		out = append(out, meta)
	}
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"files": out}})
}

// handleAdminWorkspaceFile returns one whitelisted workspace file's content.
//
//	GET /v1/admin/workspace-file?name=MEMORY/projects.md
//	response: {"ok":true,"data":{"name","content","exists","truncated"}}
func (s *Server) handleAdminWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if !slices.Contains(workspacePreviewFiles, name) {
		writeJSON(w, http.StatusForbidden, response{OK: false, Error: "NOT_ALLOWED", Detail: "file is not previewable"})
		return
	}
	dir, err := workspace.Dir()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "NO_WORKSPACE", Detail: err.Error()})
		return
	}
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if os.IsNotExist(err) {
		writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"name": name, "exists": false, "content": ""}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "READ_FAILED", Detail: err.Error()})
		return
	}
	truncated := false
	if len(raw) > maxPreviewBytes {
		raw = raw[:maxPreviewBytes]
		truncated = true
	}
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
		"name": name, "exists": true, "content": string(raw), "truncated": truncated,
	}})
}
