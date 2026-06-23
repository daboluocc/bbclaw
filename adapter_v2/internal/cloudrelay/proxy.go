package cloudrelay

import (
	"encoding/json"
	"strings"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/butler"
)

// proxyDriver is the single logical driver adapter_v2 advertises to the device's
// settings UI. adapter_v2 has no driver matrix — it runs one claude PTY — so it
// reports exactly one driver, always active. (The device is moving to a read-only
// driver display anyway; this keeps the current firmware's settings page working
// in cloud_saas instead of timing out.)
const proxyDriver = "claude-code"

// handleAgentProxy answers the cloud-relayed agent.* settings/UI requests the
// device fires from its settings page (driver list, history, sessions, menu).
// Without these, adapter_v2's old silent no-op left the device's HTTP calls to
// hang until ESP_ERR_HTTP_EAGAIN. The cloud matches replies by MessageID and
// reshapes our flat payload into the device's {ok,data} body, so we only need the
// exact field names — values are minimal/static. Returns false if the kind is not
// one we proxy (caller then ignores it).
func (r *Relay) handleAgentProxy(write func(Envelope) error, env Envelope) bool {
	reply := func(kind string, payload map[string]any) bool {
		_ = write(Envelope{
			Type: "reply", MessageID: env.MessageID, DeviceID: env.DeviceID,
			HomeSiteID: r.cfg.HomeSiteID, Kind: kind, Payload: payload,
		})
		return true
	}

	switch strings.ToLower(strings.TrimSpace(env.Kind)) {
	case "agent.drivers":
		return reply("agent.drivers.reply", map[string]any{
			"drivers":        []map[string]any{driverEntry()},
			"active_driver":  proxyDriver,
			"butler_driver":  proxyDriver,
			"butler_capable": true,
		})
	case "chat.drivers":
		return reply("chat.drivers.reply", map[string]any{
			"drivers": []map[string]any{{"name": proxyDriver, "capabilities": driverCaps()}},
		})
	case "agent.messages":
		// Real conversation history (ADR-032 P3): parse the requested session's
		// claude .jsonl. Empty sessionId defaults to the active conversation. The
		// firmware/web reads content as a STRING under key "content".
		var p struct {
			SessionID string `json:"sessionId"`
			Before    int    `json:"before"`
			Limit     int    `json:"limit"`
		}
		raw, _ := json.Marshal(env.Payload)
		_ = json.Unmarshal(raw, &p)
		sid := strings.TrimSpace(p.SessionID)
		if sid == "" {
			sid = r.devActiveID()
		}
		var all []butler.ConversationMessage
		if r.dev != nil && sid != "" {
			all, _ = r.dev.Messages(sid)
		}
		page, total, hasMore := butler.PageMessages(all, p.Before, p.Limit)
		msgs := make([]map[string]any, 0, len(page))
		for _, m := range page {
			msgs = append(msgs, map[string]any{
				"role": m.Role, "content": m.Content, "timestamp": m.Timestamp, "seq": m.Seq,
			})
		}
		return reply("agent.messages.reply", map[string]any{
			"messages": msgs, "total": total, "hasMore": hasMore,
		})
	case "agent.sessions", "agent.sessions.list.logical":
		// Real conversation history (ADR-032): the workspace's claude .jsonl files,
		// most-recent first, with the active one flagged.
		active := r.devActiveID()
		sessions := []map[string]any{}
		for _, s := range r.devList() {
			sessions = append(sessions, map[string]any{
				"id": s.ID, "title": s.Title, "lastUsedAt": s.ModUnixSec,
				"active": s.ID == active,
			})
		}
		return reply("agent.sessions.reply", map[string]any{"sessions": sessions, "active": active})
	case "agent.sessions.create":
		// Start a fresh conversation: respawn the default session with a new id.
		id := ""
		if r.dev != nil {
			id = r.dev.New()
		}
		r.log("cloudrelay: session.new device=%s id=%s", env.DeviceID, id)
		return reply("agent.sessions.create.reply", map[string]any{"session": map[string]any{
			"id": id, "driver": proxyDriver, "title": "新对话", "active": true,
		}})
	case "agent.sessions.activate", "agent.sessions.select":
		// Resume an existing conversation: respawn with --resume <id>.
		var p struct {
			SessionID string `json:"sessionId"`
			ID        string `json:"id"`
		}
		raw, _ := json.Marshal(env.Payload)
		_ = json.Unmarshal(raw, &p)
		id := strings.TrimSpace(p.SessionID)
		if id == "" {
			id = strings.TrimSpace(p.ID)
		}
		if id != "" && r.dev != nil {
			r.dev.Resume(id)
			r.log("cloudrelay: session.resume device=%s id=%s", env.DeviceID, id)
		}
		return reply("agent.sessions.activate.reply", map[string]any{"ok": id != "", "active": r.devActiveID()})
	case "agent.cwd_pool":
		return reply("agent.cwd_pool.reply", map[string]any{"pool": []any{}})
	case "agent.active_driver.set":
		return reply("agent.active_driver.set.reply", map[string]any{
			"ok": true, "active_driver": proxyDriver,
		})
	case "agent.active_model.set":
		return reply("agent.active_model.set.reply", map[string]any{
			"ok": true, "driver": proxyDriver, "active_model": "",
		})
	case "agent.menu":
		return reply("agent.menu.reply", menuPayload(env))
	case "agent.menu.action":
		return reply("agent.menu.action.reply", map[string]any{"result": "closed"})
	default:
		return false
	}
}

// devList returns the device session's conversation list (empty on nil/error).
func (r *Relay) devList() []butler.ConversationInfo {
	if r.dev == nil {
		return nil
	}
	list, _ := r.dev.List()
	return list
}

func (r *Relay) devActiveID() string {
	if r.dev == nil {
		return ""
	}
	return r.dev.ActiveID()
}

func driverCaps() map[string]any {
	return map[string]any{
		"toolApproval": false, "resume": true, "streaming": true,
		"maxInputBytes": 0, "butler": true,
	}
}

func driverEntry() map[string]any {
	return map[string]any{
		"name":           proxyDriver,
		"capabilities":   driverCaps(),
		"butler_capable": true,
		"installed":      true,
	}
}

// menuPayload returns a minimal server-driven menu (ADR-019). Only the driver and
// cwd menus get a real row; everything else is an empty menu — enough for the
// device to render without hanging while driver/model selection is being retired.
func menuPayload(env Envelope) map[string]any {
	var p struct {
		ID string `json:"id"`
	}
	raw, _ := json.Marshal(env.Payload)
	_ = json.Unmarshal(raw, &p)

	base := func(id, title string, rows []map[string]any, empty string) map[string]any {
		return map[string]any{
			"id": id, "menuVersion": 1, "title": title, "selectedIndex": 0,
			"rows": rows, "emptyText": empty,
		}
	}
	switch strings.TrimSpace(p.ID) {
	case "drivers":
		return base("drivers", "驱动", []map[string]any{{
			"id": proxyDriver, "label": proxyDriver, "marker": "active",
			"action": map[string]any{"type": "set_driver", "driver": proxyDriver},
		}}, "无可用驱动")
	case "cwd":
		return base("cwd", "选择项目", []map[string]any{{
			"id": "__default__", "label": "默认工作区",
			"action": map[string]any{"type": "create_session", "cwd": ""},
		}}, "无可用项目")
	default:
		return base(strings.TrimSpace(p.ID), "", []map[string]any{}, "暂无内容")
	}
}
