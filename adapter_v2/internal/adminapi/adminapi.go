// Package adminapi is adapter_v2's slim, loopback-only web admin/config surface
// (ADR-025). It is self-contained: it depends only on the settingsstore package
// (and the standard library) — nothing from v1.
//
// Endpoints (all loopback-gated by LocalOnly at the mux layer):
//
//	GET    /v1/settings          → the full settings doc + derived read-only block
//	PUT    /v1/settings          ← a full Settings doc; persists + flags restart
//	POST   /v1/settings/restart  → re-exec the process in place (apply changes)
//
// Secrets (API keys/tokens) are returned/accepted in PLAINTEXT on purpose: the
// route is loopback-only and the persisted file is plaintext too (ADR-025).
package adminapi

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/settingsstore"
)

// response is the uniform JSON envelope: {ok, data?, error?, detail?}.
type response struct {
	OK     bool   `json:"ok"`
	Data   any    `json:"data,omitempty"`
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// RestartFlag is a tiny shared latch: PUT sets it, GET reads it. It is an atomic
// bool so the HTTP handlers (which run concurrently) can touch it safely without
// the caller wiring a mutex. Construct one in main and pass it to the handlers.
type RestartFlag struct{ v atomic.Bool }

// Set marks that a settings change is pending and needs a restart to apply.
func (r *RestartFlag) Set() { r.v.Store(true) }

// Load reports whether a restart is pending.
func (r *RestartFlag) Load() bool { return r.v.Load() }

// Derived carries the read-only, system-generated values the page displays but
// never lets the user edit (build version, resolved device identity, workspace,
// settings file path).
type Derived struct {
	Version      string `json:"version"`
	HomeSiteID   string `json:"homeSiteId"`
	Workspace    string `json:"workspace"`
	SettingsFile string `json:"settingsFile"`
}

// LocalOnly restricts a handler to loopback callers. The admin surface mutates
// runtime config (including secrets) and can re-exec the process, so it must
// never be reachable from the LAN or the cloud relay — even though the adapter
// itself may bind 0.0.0.0 for device traffic. The gate is per-request on the
// peer address. A malformed/non-IP peer is treated as non-loopback (fail-closed).
func LocalOnly(next http.HandlerFunc) http.HandlerFunc {
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
// is a loopback peer (127.0.0.0/8 or ::1). A malformed or non-IP address is
// treated as non-loopback — fail closed. Ported from v1 (module-agnostic).
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		host = strings.TrimSpace(addr) // some transports omit the port
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SettingsGet returns the full settings document plus the restart-pending flag
// and the derived read-only block. derive is called per-request so the resolved
// homeSiteId/version/workspace reflect the live process.
//
//	GET /v1/settings
//	→ {"ok":true,"data":{"settings":{...},"restartRequired":bool,"derived":{...}}}
func SettingsGet(store *settingsstore.Store, restart *RestartFlag, derive func() Derived) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
			"settings":        store.Snapshot(),
			"restartRequired": restart.Load(),
			"derived":         derive(),
		}})
	}
}

// SettingsPut persists a new settings document. The page sends the FULL doc; it
// is decoded over the current snapshot so a partial body still merges cleanly.
// On success the restart flag is set (the change applies on the next boot) and
// the page is told to surface the restart banner.
//
//	PUT /v1/settings   body = settingsstore.Settings
func SettingsPut(store *settingsstore.Store, restart *RestartFlag) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next := store.Snapshot()
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		if err := dec.Decode(&next); err != nil {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST", Detail: err.Error()})
			return
		}
		if err := store.Replace(next); err != nil {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "SETTINGS_WRITE_FAILED", Detail: err.Error()})
			return
		}
		restart.Set()
		writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"restartRequired": true}})
	}
}

// Settings dispatches GET/PUT on /v1/settings to one handler so the route can be
// registered once and still split on method (the v2 mux pattern is path-only).
func Settings(store *settingsstore.Store, restart *RestartFlag, derive func() Derived) http.HandlerFunc {
	get := SettingsGet(store, restart, derive)
	put := SettingsPut(store, restart)
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			get(w, r)
		case http.MethodPut:
			put(w, r)
		default:
			w.Header().Set("Allow", "GET, PUT")
			writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "METHOD_NOT_ALLOWED"})
		}
	}
}

// Restart re-executes the adapter in place so a settings change takes effect. It
// replies 200 first, flushes, then (after a short delay so the response reaches
// the socket) syscall.Exec replaces the process image with a fresh copy that
// re-reads settings.json on boot. Ported from v1's handleAdminRestart — it is
// module-agnostic. The page also polls /healthz to know when to reload.
//
//	POST /v1/settings/restart
func Restart() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "METHOD_NOT_ALLOWED"})
			return
		}
		exe, err := os.Executable()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "RESTART_FAILED", Detail: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"restarting": true}})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go func() {
			// Give the HTTP response time to reach the socket before we replace the
			// process image. Best-effort; the page also polls /healthz to reconnect.
			time.Sleep(300 * time.Millisecond)
			argv := append([]string{exe}, os.Args[1:]...)
			_ = syscall.Exec(exe, argv, os.Environ())
		}()
	}
}
