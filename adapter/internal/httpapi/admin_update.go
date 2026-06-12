package httpapi

import (
	"bytes"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	cmdpkg "github.com/daboluocc/bbclaw/adapter/internal/cmd"
)

// updateState guards the upgrade flow against concurrent triggers and lets the
// admin page show a "进行中" badge until the response lands. The mutex is per
// process — that's the right scope, since we re-exec on success.
var (
	updateMu     sync.Mutex
	updateActive atomic.Bool
)

// handleAdminVersion reports the running build's tag, the latest GitHub
// release tag, and whether an upgrade is available. The latest lookup is
// short-timeout (10s in latestReleaseAsset) and treated as best-effort — a
// flaky network must not block the admin page from rendering, so we return
// what we have and leave the rest empty.
//
//	GET /v1/admin/version
//	→ {"ok":true,"data":{"current":"v0.5.0","latest":"v0.5.1","update_available":true,"checking":false}}
func (s *Server) handleAdminVersion(w http.ResponseWriter, r *http.Request) {
	current := s.version
	if current == "" {
		current = "dev"
	}
	latest := cmdpkg.FetchLatestTag()
	avail := false
	if latest != "" {
		avail = cmdpkg.IsNewerVersion(current, latest)
	}
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
		"current":          current,
		"latest":           latest,
		"update_available": avail,
		"updating":         updateActive.Load(),
	}})
}

// handleAdminUpdate downloads the latest release binary, atomically replaces
// the running executable, and re-execs in place so the new image picks up the
// existing settings.json. The response goes out before the re-exec so the
// browser can see the success status; the SPA then polls /healthz until the
// new process answers and reloads the page (same recipe used by the existing
// /v1/admin/restart flow).
//
//	POST /v1/admin/update
//	→ {"ok":true,"data":{"output":"...","upgraded":true,"restarting":true}}
func (s *Server) handleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	if !updateActive.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusConflict, response{
			OK: false, Error: "UPDATE_BUSY",
			Detail: "an update is already in progress",
		})
		return
	}
	// updateMu serialises the rest of the flow so concurrent callers can't race
	// inside DownloadLatestBinary; the atomic flag above just gives a fast 409.
	updateMu.Lock()
	defer func() {
		updateMu.Unlock()
		updateActive.Store(false)
	}()

	preTag := s.version
	var buf bytes.Buffer
	if err := cmdpkg.DownloadLatestBinary(&buf); err != nil {
		s.log.Errorf("admin: update failed: %v\n%s", err, buf.String())
		writeJSON(w, http.StatusInternalServerError, response{
			OK: false, Error: "UPDATE_FAILED",
			Detail: err.Error() + "\n" + buf.String(),
		})
		return
	}
	out := buf.String()
	s.log.Infof("admin: update completed (%s → latest)\n%s", preTag, out)

	// "已是最新" branch: DownloadLatestBinary printed the [ok] line and didn't
	// touch the binary. Tell the page so it can show "已是最新" instead of
	// triggering the restart-and-reload dance.
	if !bytes.Contains(buf.Bytes(), []byte("[ok] binary updated")) {
		writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
			"output":     out,
			"upgraded":   false,
			"restarting": false,
		}})
		return
	}

	exe, err := os.Executable()
	if err != nil {
		// Binary was already swapped; tell the user to restart manually.
		writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
			"output":     out + "\n[warn] could not resolve executable path; please restart manually",
			"upgraded":   true,
			"restarting": false,
		}})
		return
	}
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
		"output":     out,
		"upgraded":   true,
		"restarting": true,
	}})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.log.Infof("admin: re-exec after update → %s", exe)
	go func() {
		// Same delay as handleAdminRestart — let the response flush before we
		// replace the process image. Browser polls /healthz to reconnect.
		time.Sleep(300 * time.Millisecond)
		argv := append([]string{exe}, os.Args[1:]...)
		if err := syscall.Exec(exe, argv, os.Environ()); err != nil {
			s.log.Errorf("admin: post-update re-exec failed: %v", err)
		}
	}()
}
