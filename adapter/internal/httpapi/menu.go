package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
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

// handleAgentMenu serves GET /v1/agent/menu/{id} (ADR-019 §3).
//
//	GET /v1/agent/menu/drivers
//	GET /v1/agent/menu/models?driver=claude-code   (driver defaults to active)
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

	default:
		// sessions / cwd not implemented in this slice; device falls back to
		// its legacy picker on UNKNOWN_MENU.
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
		Action MenuAction `json:"action"`
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

	case "close":
		writeJSON(w, http.StatusOK, response{OK: true, Data: menuActionResult{Result: "closed"}})

	default:
		// select_session / open_menu / create_session belong to the sessions/cwd
		// menus, not yet wired in this slice.
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNSUPPORTED_ACTION", Detail: act.Type})
	}
}
