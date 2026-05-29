package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
)

// Server-driven menu protocol (ADR-019). The adapter computes a fully
// pre-formatted Menu (labels, markers, secondary text) and the device renders
// it with a single generic menu view — no per-picker state machine on device.
//
// This file implements the LAN-direct (httpapi) side for the `drivers` and
// `models` menus (ADR-019 P0之三 first slice). `sessions` / `cwd` and the
// cloud relay come in later slices; unimplemented menu ids return UNKNOWN_MENU
// so old firmware can fall back to its legacy picker.

const menuVersion = 1

// Menu is the device-facing menu descriptor (ADR-019 §1).
type Menu struct {
	ID            string    `json:"id"`
	MenuVersion   int       `json:"menuVersion"`
	Title         string    `json:"title"`
	SelectedIndex int       `json:"selectedIndex"`
	Rows          []MenuRow `json:"rows"`
	EmptyText     string    `json:"emptyText,omitempty"`
}

// MenuRow is one selectable line. label/secondary/marker are pre-formatted by
// the adapter; the device renders them verbatim.
type MenuRow struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Secondary string     `json:"secondary,omitempty"`
	Marker    string     `json:"marker,omitempty"` // "active" | ""
	Action    MenuAction `json:"action"`
}

// MenuAction is the closed action set the device echoes back on select
// (ADR-019 §2).
type MenuAction struct {
	Type      string `json:"type"` // select_session|open_menu|create_session|set_driver|set_model|close
	SessionID string `json:"sessionId,omitempty"`
	MenuID    string `json:"menuId,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Driver    string `json:"driver,omitempty"`
	Model     string `json:"model,omitempty"`
}

// menuActionResult is the response to POST /v1/agent/menu/action (ADR-019 §3).
type menuActionResult struct {
	Result      string `json:"result"` // closed|navigate|refresh
	NextMenu    *Menu  `json:"nextMenu,omitempty"`
	SessionID   string `json:"sessionId,omitempty"`
	LoadHistory bool   `json:"loadHistory,omitempty"`
}

// buildDriversMenu shapes the driver list into a menu. activeDriver gets the
// "active" marker and seeds the initial cursor. Pure (no I/O) for testability.
func buildDriversMenu(drivers []agent.DriverInfo, activeDriver string) Menu {
	sort.Slice(drivers, func(i, j int) bool { return drivers[i].Name < drivers[j].Name })
	rows := make([]MenuRow, 0, len(drivers))
	selected := 0
	for i, d := range drivers {
		marker := ""
		if d.Name == activeDriver {
			marker = "active"
			selected = i
		}
		rows = append(rows, MenuRow{
			ID:     d.Name,
			Label:  d.Name,
			Marker: marker,
			Action: MenuAction{Type: "set_driver", Driver: d.Name},
		})
	}
	return Menu{
		ID:            "drivers",
		MenuVersion:   menuVersion,
		Title:         "驱动",
		SelectedIndex: selected,
		Rows:          rows,
		EmptyText:     "无可用驱动",
	}
}

// buildModelsMenu shapes one driver's model catalog into a menu. activeModel
// gets the "active" marker and seeds the cursor. Pure (no I/O).
func buildModelsMenu(driver string, models []agent.ModelInfo, activeModel string) Menu {
	rows := make([]MenuRow, 0, len(models))
	selected := 0
	for i, m := range models {
		label := m.Label
		if label == "" {
			label = m.ID
		}
		marker := ""
		if m.ID == activeModel {
			marker = "active"
			selected = i
		}
		rows = append(rows, MenuRow{
			ID:     m.ID,
			Label:  label,
			Marker: marker,
			Action: MenuAction{Type: "set_model", Driver: driver, Model: m.ID},
		})
	}
	return Menu{
		ID:            "models",
		MenuVersion:   menuVersion,
		Title:         "模型",
		SelectedIndex: selected,
		Rows:          rows,
		EmptyText:     "该驱动无可选模型",
	}
}

// formatRelativeTime renders a "刚刚 / N 分钟前 / N 小时前 / N 天前 / 1月2日"
// label. `now` is injected so the builder stays pure/testable. This formatting
// used to live on the device (format_relative_time); ADR-018/019 move it here.
func formatRelativeTime(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d/time.Hour))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d 天前", int(d/(24*time.Hour)))
	default:
		return t.Format("1月2日")
	}
}

// sessionMenuItem is the pre-resolved data a session row needs (the handler
// does the logicalsession.List + cwd reverse-lookup; the builder stays pure).
type sessionMenuItem struct {
	ID         string
	Title      string
	Driver     string
	CwdName    string
	LastUsedAt time.Time
}

// buildSessionsMenu shapes the device's logical sessions into a menu. `current`
// is the device's currently-active logical id (it knows its own; passed via the
// `current` query param) and gets the "active" marker + cursor. A trailing
// "+ 新建会话" row navigates to the cwd menu. Pure (now injected).
func buildSessionsMenu(items []sessionMenuItem, current string, now time.Time) Menu {
	rows := make([]MenuRow, 0, len(items)+1)
	selected := 0
	for i, it := range items {
		label := it.Title
		if label == "" {
			label = "(无标题)"
		}
		secondary := it.Driver + " · " + formatRelativeTime(it.LastUsedAt, now)
		if it.CwdName != "" {
			secondary += " · " + it.CwdName
		}
		marker := ""
		if it.ID == current && current != "" {
			marker = "active"
			selected = i
		}
		rows = append(rows, MenuRow{
			ID:        it.ID,
			Label:     label,
			Secondary: secondary,
			Marker:    marker,
			Action:    MenuAction{Type: "select_session", SessionID: it.ID},
		})
	}
	// The "+ 新建会话" row is always present so an empty session list still
	// offers a way forward (it opens the cwd menu).
	rows = append(rows, MenuRow{
		ID:     "__new__",
		Label:  "+ 新建会话",
		Action: MenuAction{Type: "open_menu", MenuID: "cwd"},
	})
	return Menu{
		ID:            "sessions",
		MenuVersion:   menuVersion,
		Title:         "会话",
		SelectedIndex: selected,
		Rows:          rows,
	}
}

// buildCwdMenu shapes the configured cwd-pool NAMES into a menu. The action
// carries the pool name (not the path — paths stay server-side, ADR-014); the
// create_session handler resolves it. When the pool is empty, a single
// "默认工作区" row creates a session in the adapter's default cwd.
func buildCwdMenu(poolNames []string) Menu {
	rows := make([]MenuRow, 0, len(poolNames)+1)
	for _, name := range poolNames {
		rows = append(rows, MenuRow{
			ID:     name,
			Label:  name,
			Action: MenuAction{Type: "create_session", Cwd: name},
		})
	}
	if len(rows) == 0 {
		rows = append(rows, MenuRow{
			ID:     "__default__",
			Label:  "默认工作区",
			Action: MenuAction{Type: "create_session", Cwd: ""},
		})
	}
	return Menu{
		ID:            "cwd",
		MenuVersion:   menuVersion,
		Title:         "选择项目",
		SelectedIndex: 0,
		Rows:          rows,
	}
}

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
		menu := buildDriversMenu(s.router.List(), s.resolveActiveDriver())
		writeJSON(w, http.StatusOK, response{OK: true, Data: menu})

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
		writeJSON(w, http.StatusOK, response{OK: true, Data: buildModelsMenu(driver, models, s.resolveActiveModel(driver))})

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
		items := make([]sessionMenuItem, 0)
		for _, sess := range s.sessions.List(deviceID, driver, 50) {
			// Skip expired sessions so the menu matches the legacy picker (T4).
			if s.cfg.SessionMaxAge > 0 && sess.LastUsedAt.Before(now.Add(-s.cfg.SessionMaxAge)) {
				continue
			}
			items = append(items, sessionMenuItem{
				ID:         string(sess.ID),
				Title:      sess.Title,
				Driver:     sess.Driver,
				CwdName:    s.cwdDisplayName(sess.Cwd),
				LastUsedAt: sess.LastUsedAt,
			})
		}
		writeJSON(w, http.StatusOK, response{OK: true, Data: buildSessionsMenu(items, current, now)})

	case "cwd":
		writeJSON(w, http.StatusOK, response{OK: true, Data: buildCwdMenu(s.cwdPoolNames())})

	default:
		// Unknown menu id → device falls back to its legacy picker.
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNKNOWN_MENU", Detail: id})
	}
}

// handleAgentMenuAction serves POST /v1/agent/menu/action (ADR-019 §2/§3).
//
//	POST /v1/agent/menu/action  {"action":{"type":"set_driver","driver":"opencode"}}
//	→ {"ok":true,"data":{"result":"closed"}}
func (s *Server) handleAgentMenuAction(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}
	var body struct {
		DeviceID string     `json:"deviceId"`
		Action   MenuAction `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST", Detail: err.Error()})
		return
	}
	act := body.Action

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
		// Mirror into the router so the next turn without an explicit driver
		// picks the new selection (same as PUT /v1/agent/active_driver).
		s.router.SetDefault(name)
		s.log.Infof("menu/action: set_driver=%q", name)
		writeJSON(w, http.StatusOK, response{OK: true, Data: menuActionResult{Result: "closed"}})

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
		// model="" clears the override (same semantics as PUT active_model); the
		// id is intentionally not validated against ListModels.
		if err := s.driverState.SetActiveModel(name, strings.TrimSpace(act.Model)); err != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "PERSIST_FAILED", Detail: err.Error()})
			return
		}
		s.log.Infof("menu/action: set_model driver=%q model=%q", name, strings.TrimSpace(act.Model))
		writeJSON(w, http.StatusOK, response{OK: true, Data: menuActionResult{Result: "closed"}})

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
		// and tells the device to (re)load that session's transcript. The id is
		// re-sent on the next /v1/agent/message turn.
		writeJSON(w, http.StatusOK, response{OK: true, Data: menuActionResult{Result: "closed", SessionID: id, LoadHistory: true}})

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
		writeJSON(w, http.StatusOK, response{OK: true, Data: menuActionResult{Result: "closed", SessionID: string(sess.ID), LoadHistory: true}})

	case "open_menu":
		switch strings.TrimSpace(act.MenuID) {
		case "cwd":
			m := buildCwdMenu(s.cwdPoolNames())
			writeJSON(w, http.StatusOK, response{OK: true, Data: menuActionResult{Result: "navigate", NextMenu: &m}})
		default:
			// Context-bearing menus (sessions) are fetched by the device via GET
			// with its own deviceId/current; only context-free nav is inlined here.
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNSUPPORTED_MENU", Detail: act.MenuID})
		}

	case "close":
		writeJSON(w, http.StatusOK, response{OK: true, Data: menuActionResult{Result: "closed"}})

	default:
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNSUPPORTED_ACTION", Detail: act.Type})
	}
}
