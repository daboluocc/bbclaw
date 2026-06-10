package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/config"
	"github.com/daboluocc/bbclaw/adapter/internal/settingsstore"
)

// SetSettingsStore wires the web-mutable runtime configuration store (ADR-025).
// Pass nil to keep the .env-only behaviour (the settings admin endpoints then
// return 501). Loopback-gated at the route layer.
func (s *Server) SetSettingsStore(store *settingsstore.Store) {
	s.settings = store
}

// handleAdminSettingsGet returns the full settings document plus whether a
// restart is pending to apply a prior write.
//
//	GET /v1/admin/settings
//	→ {"ok":true,"data":{"settings":{...}, "restart_required":bool}}
//
// Secrets are returned in plaintext on purpose — the route is loopback-only and
// ADR-025 chose plaintext read/write for v1.
func (s *Server) handleAdminSettingsGet(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "SETTINGS_DISABLED"})
		return
	}
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
		"settings":         s.settings.Snapshot(),
		"restart_required": s.settingsRestartReq.Load(),
		// Read-only, system-generated values the page shows but never lets the
		// user edit (ADR-025): the resolved device identity, build version, and
		// where logs are persisted.
		"derived": map[string]any{
			"home_site_id": s.homeSiteID,
			"version":      s.version,
			"log_file":     s.logFile,
		},
	}})
}

// handleAdminSettingsPut persists a new settings document. The page sends the
// full document; a partial body is also accepted (it merges over the current
// snapshot). The proposed settings are validated against the *effective* config
// (env overlaid with the new settings) before being written, so an invalid
// combination (e.g. local voice on but ASR keys missing) is rejected up front
// rather than failing the next boot.
//
//	PUT /v1/admin/settings   body = settingsstore.Settings (full or partial)
func (s *Server) handleAdminSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "SETTINGS_DISABLED"})
		return
	}
	// Decode over the current snapshot so a partial PUT only changes the keys it
	// carries (present-then-modify); the page's full-doc PUT overwrites everything.
	next := s.settings.Snapshot()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&next); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST", Detail: err.Error()})
		return
	}

	// Validate the effective view: env defaults + the proposed settings overlay.
	cfg, err := config.LoadFromEnv()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "ENV_INVALID", Detail: err.Error()})
		return
	}
	next.ApplyTo(&cfg)
	// Validate rejects only structural errors (bad mode/cloud/openclaw URL, …).
	// Incomplete voice config is NOT a structural error — local mode saves fine and
	// the pipeline degrades to 501 until ASR/TTS is filled (ADR-025 §3). The page
	// is told via voice_incomplete so it can nudge the user to the AI page.
	if err := cfg.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_SETTINGS", Detail: err.Error()})
		return
	}
	voiceIncomplete := cfg.LocalVoiceEnabled && cfg.VoiceConfigError() != nil

	if err := s.settings.Replace(next); err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "SETTINGS_WRITE_FAILED", Detail: err.Error()})
		return
	}
	s.settingsRestartReq.Store(true)
	s.log.Infof("admin: settings updated (restart required to apply; voice_incomplete=%t)", voiceIncomplete)
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
		"restart_required": true,
		"voice_incomplete": voiceIncomplete,
	}})
}

// handleAdminRestart re-executes the adapter in place so a settings change takes
// effect (ADR-025 §4). It replies 200 first, then re-execs after a short delay
// to let the response flush; syscall.Exec replaces the process image (works the
// same under systemd / `&` — the new image re-reads settings.json on start).
//
//	POST /v1/admin/restart
func (s *Server) handleAdminRestart(w http.ResponseWriter, r *http.Request) {
	exe, err := os.Executable()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "RESTART_FAILED", Detail: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"restarting": true}})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.log.Infof("admin: self-restart requested, re-exec %s", exe)
	go func() {
		// Give the HTTP response time to reach the socket before we replace the
		// process image. Best-effort; the page also polls /healthz to reconnect.
		time.Sleep(300 * time.Millisecond)
		argv := append([]string{exe}, os.Args[1:]...)
		if err := syscall.Exec(exe, argv, os.Environ()); err != nil {
			s.log.Errorf("admin: re-exec failed: %v", err)
		}
	}()
}
