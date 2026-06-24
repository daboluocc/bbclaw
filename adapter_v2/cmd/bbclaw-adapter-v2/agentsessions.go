package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/butler"
)

// agentSessionsRoutes registers the LOCAL HTTP endpoints the web SPA uses to browse
// conversations: list, view a transcript, and switch the active one. They drive
// butler.DeviceSession directly — the same data the cloud-relay proxy exposes over
// the cloud WS, here served to the LAN/local web. Not loopback-gated: these carry
// no secrets (titles + transcript text), same exposure as the terminal at "/".
func agentSessionsRoutes(mux *http.ServeMux, dev *butler.DeviceSession) {
	// GET /v1/agent/sessions → {sessions:[{id,title,lastUsedAt,active}], active}
	mux.HandleFunc("GET /v1/agent/sessions", func(w http.ResponseWriter, r *http.Request) {
		active := dev.ActiveID()
		list, _ := dev.List()
		sessions := make([]map[string]any, 0, len(list))
		for _, s := range list {
			sessions = append(sessions, map[string]any{
				"id": s.ID, "title": s.Title, "lastUsedAt": s.ModUnixSec, "active": s.ID == active,
			})
		}
		writeAgentJSON(w, map[string]any{"sessions": sessions, "active": active})
	})

	// GET /v1/agent/sessions/{id}/messages?before=&limit= → {messages,total,hasMore}
	mux.HandleFunc("GET /v1/agent/sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.PathValue("id"))
		before := atoiDefault(r.URL.Query().Get("before"), 0)
		limit := atoiDefault(r.URL.Query().Get("limit"), 0)
		all, _ := dev.Messages(id)
		page, total, hasMore := butler.PageMessages(all, before, limit)
		msgs := make([]map[string]any, 0, len(page))
		for _, m := range page {
			msgs = append(msgs, map[string]any{
				"role": m.Role, "content": m.Content, "timestamp": m.Timestamp, "seq": m.Seq,
			})
		}
		writeAgentJSON(w, map[string]any{"messages": msgs, "total": total, "hasMore": hasMore})
	})

	// POST /v1/agent/sessions/{id}/activate → switch the default session onto this
	// conversation (DeviceSession.Resume respawns --resume <id>); the terminal at
	// "/" reconnects to it. {active}
	mux.HandleFunc("POST /v1/agent/sessions/{id}/activate", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.PathValue("id"))
		if id == "" {
			http.Error(w, "session id required", http.StatusBadRequest)
			return
		}
		dev.Resume(id)
		writeAgentJSON(w, map[string]any{"active": dev.ActiveID()})
	})

	// POST /v1/agent/sessions/new → start a fresh conversation. {active}
	mux.HandleFunc("POST /v1/agent/sessions/new", func(w http.ResponseWriter, r *http.Request) {
		id := dev.New()
		writeAgentJSON(w, map[string]any{"active": id})
	})

	// DELETE /v1/agent/sessions/{id} → remove a conversation. Deleting the active
	// one moves the active id onto the next conversation (or a fresh one). {active}
	mux.HandleFunc("DELETE /v1/agent/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.PathValue("id"))
		if id == "" {
			http.Error(w, "session id required", http.StatusBadRequest)
			return
		}
		active, err := dev.Delete(id)
		if err != nil {
			writeAgentError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeAgentJSON(w, map[string]any{"active": active})
	})
}

func writeAgentJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
}

func writeAgentError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}
