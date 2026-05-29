package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/menu"
)

// Server-driven menu protocol (ADR-019), LAN-direct (httpapi) side. The
// transport-agnostic Menu types + builders live in internal/agent/menu so the
// cloud-relayed (homeadapter) path produces byte-identical menus.

// cwdDisplayName reverse-looks-up a cwd path to its pool name, falling back to
// the path basename. Empty cwd → empty string. Shared by the sessions menu and
// handleAgentSessionsLogical.
func (s *Server) cwdDisplayName(cwd string) string {
	if cwd == "" {
		return ""
	}
	for _, e := range s.cfg.CwdPool {
		if e.Path == cwd {
			return e.Name
		}
	}
	return filepath.Base(cwd)
}

// cwdPoolNames returns the configured cwd-pool entry names (paths stay server-side).
func (s *Server) cwdPoolNames() []string {
	names := make([]string, 0, len(s.cfg.CwdPool))
	for _, e := range s.cfg.CwdPool {
		names = append(names, e.Name)
	}
	return names
}

// handleAgentMenu serves GET /v1/agent/menu/{id} (ADR-019 §3).
//
//	GET /v1/agent/menu/drivers
//	GET /v1/agent/menu/models?driver=claude-code   (driver defaults to active)
//	GET /v1/agent/menu/sessions?deviceId=X&driver=Y&current=ls-Z
//	GET /v1/agent/menu/cwd
func (s *Server) handleAgentMenu(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	switch id {
	case "drivers":
		writeJSON(w, http.StatusOK, response{OK: true, Data: menu.Drivers(s.router.List(), s.resolveActiveDriver())})

	case "models":
		driver := strings.TrimSpace(r.URL.Query().Get("driver"))
		if driver == "" {
			driver = s.resolveActiveDriver()
		}
		drv, ok := s.router.Get(driver)
		if !ok {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNKNOWN_DRIVER", Detail: driver})
			return
		}
		var models []agent.ModelInfo
		if ml, isLister := drv.(agent.ModelLister); isLister {
			if m, err := ml.ListModels(r.Context()); err == nil {
				models = m
			} else {
				s.log.Warnf("menu/models: driver %s ListModels failed: %v", driver, err)
			}
		}
		writeJSON(w, http.StatusOK, response{OK: true, Data: menu.Models(driver, models, s.resolveActiveModel(driver))})

	case "sessions":
		if s.sessions == nil {
			writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "LOGICAL_SESSIONS_DISABLED"})
			return
		}
		deviceID := strings.TrimSpace(r.URL.Query().Get("deviceId"))
		driver := strings.TrimSpace(r.URL.Query().Get("driver"))
		if driver == "" {
			driver = s.resolveActiveDriver()
		}
		current := strings.TrimSpace(r.URL.Query().Get("current"))
		now := time.Now()
		items := make([]menu.SessionItem, 0)
		for _, sess := range s.sessions.List(deviceID, driver, 50) {
			if s.cfg.SessionMaxAge > 0 && sess.LastUsedAt.Before(now.Add(-s.cfg.SessionMaxAge)) {
				continue // skip expired (matches legacy picker, T4)
			}
			items = append(items, menu.SessionItem{
				ID:         string(sess.ID),
				Title:      sess.Title,
				Driver:     sess.Driver,
				CwdName:    s.cwdDisplayName(sess.Cwd),
				LastUsedAt: sess.LastUsedAt,
			})
		}
		writeJSON(w, http.StatusOK, response{OK: true, Data: menu.Sessions(items, current, now)})

	case "cwd":
		writeJSON(w, http.StatusOK, response{OK: true, Data: menu.Cwd(s.cwdPoolNames())})

	default:
		// Unknown menu id → device falls back to its legacy picker.
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNKNOWN_MENU", Detail: id})
	}
}

// handleAgentMenuAction serves POST /v1/agent/menu/action (ADR-019 §2/§3).
//
//	POST /v1/agent/menu/action  {"deviceId":"...","action":{"type":"set_driver","driver":"opencode"}}
//	→ {"ok":true,"data":{"result":"closed"}}
func (s *Server) handleAgentMenuAction(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}
	var body struct {
		DeviceID string      `json:"deviceId"`
		Action   menu.Action `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST", Detail: err.Error()})
		return
	}
	act := body.Action
	if strings.TrimSpace(act.Type) == "" {
		// Match the cloud proxy: a missing/empty action is EMPTY_ACTION, not
		// UNSUPPORTED_ACTION (keeps the error code identical across paths).
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "EMPTY_ACTION"})
		return
	}

	switch act.Type {
	case "set_driver":
		if s.driverState == nil {
			writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "DRIVERSTATE_NOT_CONFIGURED"})
			return
		}
		name := strings.TrimSpace(act.Driver)
		if name == "" {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "EMPTY_NAME"})
			return
		}
		if _, ok := s.router.Get(name); !ok {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNKNOWN_DRIVER", Detail: name})
			return
		}
		if err := s.driverState.SetActiveDriver(name); err != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "PERSIST_FAILED", Detail: err.Error()})
			return
		}
		s.router.SetDefault(name) // mirror so next turn picks it
		s.log.Infof("menu/action: set_driver=%q", name)
		writeJSON(w, http.StatusOK, response{OK: true, Data: menu.Result{Result: "closed"}})

	case "set_model":
		if s.driverState == nil {
			writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "DRIVERSTATE_NOT_CONFIGURED"})
			return
		}
		name := strings.TrimSpace(act.Driver)
		if name == "" {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "EMPTY_NAME"})
			return
		}
		if _, ok := s.router.Get(name); !ok {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNKNOWN_DRIVER", Detail: name})
			return
		}
		if err := s.driverState.SetActiveModel(name, strings.TrimSpace(act.Model)); err != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "PERSIST_FAILED", Detail: err.Error()})
			return
		}
		s.log.Infof("menu/action: set_model driver=%q model=%q", name, strings.TrimSpace(act.Model))
		writeJSON(w, http.StatusOK, response{OK: true, Data: menu.Result{Result: "closed"}})

	case "select_session":
		if s.sessions == nil {
			writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "LOGICAL_SESSIONS_DISABLED"})
			return
		}
		id := strings.TrimSpace(act.SessionID)
		if id == "" {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "EMPTY_SESSION_ID"})
			return
		}
		if _, ok := s.sessions.Get(logicalsession.ID(id)); !ok {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNKNOWN_LOGICAL_SESSION", Detail: id})
			return
		}
		// Selection is device-side state; the adapter just confirms the choice
		// and tells the device to (re)load that session's transcript.
		writeJSON(w, http.StatusOK, response{OK: true, Data: menu.Result{Result: "closed", SessionID: id, LoadHistory: true}})

	case "create_session":
		if s.sessions == nil {
			writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "LOGICAL_SESSIONS_DISABLED"})
			return
		}
		driver := s.resolveActiveDriver()
		if driver == "" {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "DRIVER_REQUIRED"})
			return
		}
		cwd := "" // empty → manager's default cwd
		if cwdName := strings.TrimSpace(act.Cwd); cwdName != "" {
			resolved, ok := s.resolveCwdByName(cwdName)
			if !ok {
				writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNKNOWN_CWD_NAME", Detail: cwdName})
				return
			}
			cwd = resolved
		}
		sess, err := s.sessions.Create(strings.TrimSpace(body.DeviceID), driver, cwd, "")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "CREATE_SESSION_FAILED", Detail: err.Error()})
			return
		}
		s.log.Infof("menu/action: create_session logical=%s driver=%s cwd=%q", sess.ID, driver, cwd)
		writeJSON(w, http.StatusOK, response{OK: true, Data: menu.Result{Result: "closed", SessionID: string(sess.ID), LoadHistory: true}})

	case "open_menu":
		switch strings.TrimSpace(act.MenuID) {
		case "cwd":
			m := menu.Cwd(s.cwdPoolNames())
			writeJSON(w, http.StatusOK, response{OK: true, Data: menu.Result{Result: "navigate", NextMenu: &m}})
		default:
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNSUPPORTED_MENU", Detail: act.MenuID})
		}

	case "close":
		writeJSON(w, http.StatusOK, response{OK: true, Data: menu.Result{Result: "closed"}})

	default:
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNSUPPORTED_ACTION", Detail: act.Type})
	}
}
