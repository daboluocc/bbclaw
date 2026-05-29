// Package menu holds the transport-agnostic server-driven menu types and
// pure builders (ADR-019). The adapter computes a fully pre-formatted Menu
// (labels, markers, secondary text, cursor) and the device renders it with a
// single generic menu view — no per-picker state machine on device.
//
// Both transports reuse this package so the menu shape stays identical across
// LAN-direct (httpapi) and cloud-relayed (homeadapter) paths: drift here would
// surface as the device showing different menus depending on connectivity.
package menu

import (
	"fmt"
	"sort"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// Version is the menu protocol version. Devices that don't recognise it fall
// back to their legacy picker (ADR-019 §6).
const Version = 1

// Menu is the device-facing menu descriptor (ADR-019 §1).
type Menu struct {
	ID            string `json:"id"`
	MenuVersion   int    `json:"menuVersion"`
	Title         string `json:"title"`
	SelectedIndex int    `json:"selectedIndex"`
	Rows          []Row  `json:"rows"`
	EmptyText     string `json:"emptyText,omitempty"`
}

// Row is one selectable line. label/secondary/marker are pre-formatted by the
// adapter; the device renders them verbatim.
type Row struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Secondary string `json:"secondary,omitempty"`
	Marker    string `json:"marker,omitempty"` // "active" | ""
	Action    Action `json:"action"`
}

// Action is the closed action set the device echoes back on select (ADR-019 §2).
type Action struct {
	Type      string `json:"type"` // select_session|open_menu|create_session|set_driver|set_model|close
	SessionID string `json:"sessionId,omitempty"`
	MenuID    string `json:"menuId,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Driver    string `json:"driver,omitempty"`
	Model     string `json:"model,omitempty"`
}

// Result is the response to a menu action (ADR-019 §3).
type Result struct {
	Result      string `json:"result"` // closed|navigate|refresh
	NextMenu    *Menu  `json:"nextMenu,omitempty"`
	SessionID   string `json:"sessionId,omitempty"`
	LoadHistory bool   `json:"loadHistory,omitempty"`
}

// SessionItem is the pre-resolved data a session row needs. The caller does the
// logicalsession.List + cwd reverse-lookup; the builder stays pure.
type SessionItem struct {
	ID         string
	Title      string
	Driver     string
	CwdName    string
	LastUsedAt time.Time
}

// Drivers shapes the driver list into a menu. activeDriver gets the "active"
// marker and seeds the cursor. Pure (no I/O).
func Drivers(drivers []agent.DriverInfo, activeDriver string) Menu {
	// Stable order: router.List() order is not guaranteed.
	sort.Slice(drivers, func(i, j int) bool { return drivers[i].Name < drivers[j].Name })
	rows := make([]Row, 0, len(drivers))
	selected := 0
	for i, d := range drivers {
		marker := ""
		if d.Name == activeDriver {
			marker = "active"
			selected = i
		}
		rows = append(rows, Row{
			ID:     d.Name,
			Label:  d.Name,
			Marker: marker,
			Action: Action{Type: "set_driver", Driver: d.Name},
		})
	}
	return Menu{ID: "drivers", MenuVersion: Version, Title: "驱动", SelectedIndex: selected, Rows: rows, EmptyText: "无可用驱动"}
}

// Models shapes one driver's model catalog into a menu. activeModel gets the
// "active" marker and seeds the cursor. Pure.
func Models(driver string, models []agent.ModelInfo, activeModel string) Menu {
	rows := make([]Row, 0, len(models))
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
		rows = append(rows, Row{
			ID:     m.ID,
			Label:  label,
			Marker: marker,
			Action: Action{Type: "set_model", Driver: driver, Model: m.ID},
		})
	}
	return Menu{ID: "models", MenuVersion: Version, Title: "模型", SelectedIndex: selected, Rows: rows, EmptyText: "该驱动无可选模型"}
}

// Sessions shapes the device's logical sessions into a menu. current is the
// device's active logical id (it knows its own; passed via query/payload) and
// gets the "active" marker + cursor. A trailing "+ 新建会话" row navigates to
// the cwd menu. Pure (now injected for deterministic relative-time labels).
func Sessions(items []SessionItem, current string, now time.Time) Menu {
	rows := make([]Row, 0, len(items)+1)
	selected := 0
	for i, it := range items {
		label := it.Title
		if label == "" {
			label = "(无标题)"
		}
		secondary := it.Driver + " · " + FormatRelativeTime(it.LastUsedAt, now)
		if it.CwdName != "" {
			secondary += " · " + it.CwdName
		}
		marker := ""
		if it.ID == current && current != "" {
			marker = "active"
			selected = i
		}
		rows = append(rows, Row{
			ID:        it.ID,
			Label:     label,
			Secondary: secondary,
			Marker:    marker,
			Action:    Action{Type: "select_session", SessionID: it.ID},
		})
	}
	// Always present so an empty session list still offers a way forward.
	rows = append(rows, Row{ID: "__new__", Label: "+ 新建会话", Action: Action{Type: "open_menu", MenuID: "cwd"}})
	return Menu{ID: "sessions", MenuVersion: Version, Title: "会话", SelectedIndex: selected, Rows: rows}
}

// Cwd shapes the configured cwd-pool NAMES into a menu. The action carries the
// pool name (not the path — paths stay server-side, ADR-014). When the pool is
// empty, a single "默认工作区" row creates a session in the adapter's default cwd.
func Cwd(poolNames []string) Menu {
	rows := make([]Row, 0, len(poolNames)+1)
	for _, name := range poolNames {
		rows = append(rows, Row{ID: name, Label: name, Action: Action{Type: "create_session", Cwd: name}})
	}
	if len(rows) == 0 {
		rows = append(rows, Row{ID: "__default__", Label: "默认工作区", Action: Action{Type: "create_session", Cwd: ""}})
	}
	return Menu{ID: "cwd", MenuVersion: Version, Title: "选择项目", SelectedIndex: 0, Rows: rows}
}

// FormatRelativeTime renders a "刚刚 / N 分钟前 / N 小时前 / N 天前 / 1月2日"
// label. now is injected so callers stay pure/testable. This formatting used to
// live on the device (format_relative_time); ADR-018/019 move it here.
func FormatRelativeTime(t, now time.Time) string {
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
