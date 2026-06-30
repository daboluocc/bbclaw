package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/butler"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

// agentSessionsRoutes registers the LOCAL HTTP endpoints the web SPA uses to browse
// conversations: list, view a transcript, switch the active one, and type into it.
// They drive butler.DeviceSession directly — the same data the cloud-relay proxy
// exposes over the cloud WS, here served to the LAN/local web. Not loopback-gated:
// these carry no secrets (titles + transcript text), same exposure as the terminal
// at "/". mgr is used by the text-input route to reach the live default PTY.
func agentSessionsRoutes(mux *http.ServeMux, mgr *session.Manager, dev *butler.DeviceSession) {
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

	// POST /v1/agent/sessions/{id}/input → type a text turn into the conversation,
	// the web composer's equivalent of the device's voice turn. Body: {"text":"…"}.
	//
	// The active conversation runs in the shared default session (session.DefaultID).
	// That PTY is spawned LAZILY (on the first /ws connect or device attach) and the
	// GC reaps it after 5min with no clients — so a #sessions page open on its own,
	// with no terminal tab or device, often has NO live PTY ("no live session" is
	// just "nobody started it / it was reaped", not a crash). So instead of failing,
	// we GetOrCreate the default session here exactly like the /ws handler, then —
	// only when WE had to spawn it — wait for claude to reach its idle "❯" prompt
	// before injecting, so the first turn isn't swallowed by startup (the same
	// readiness gate deviceapi warmup uses). Input aimed at any non-active id is
	// rejected with 409 so the client switches first (POST .../activate).
	mux.HandleFunc("POST /v1/agent/sessions/{id}/input", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.PathValue("id"))
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAgentError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		// Strip any trailing newline the composer sent; we append our own Enter so
		// the turn submits as one keystroke (matching the device's idle-prompt path).
		text := strings.TrimRight(body.Text, "\r\n")
		if strings.TrimSpace(text) == "" {
			writeAgentError(w, http.StatusBadRequest, "text required")
			return
		}
		if id == "" || id != dev.ActiveID() {
			writeAgentError(w, http.StatusConflict, "session is not active; switch to it first")
			return
		}
		// Spawn-if-absent: a #sessions-only page may have no live default PTY yet.
		spawned := mgr.Get(session.DefaultID) == nil
		sess, err := mgr.GetOrCreate(session.DefaultID, dev.Config())
		if err != nil {
			writeAgentError(w, http.StatusServiceUnavailable, "failed to start session: "+err.Error())
			return
		}
		// A freshly-spawned claude needs a moment to clear its startup (the
		// configured auto-Enter keys dismiss the trust/upsell dialogs) and paint its
		// idle prompt; injecting before then loses the turn. An already-live session
		// (terminal open / device attached) is assumed ready, like a human typing.
		if spawned {
			waitForIdlePrompt(r.Context(), sess)
		}
		if err := sess.Write([]byte(text + "\r")); err != nil {
			writeAgentError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeAgentJSON(w, map[string]any{"sent": true})
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

// waitForIdlePrompt blocks until a freshly-spawned claude session has reached its
// idle "❯" input prompt — so the first injected turn lands at the prompt instead
// of in claude's startup trust/upsell dialogs (which the configured auto-Enter
// keys clear) and is lost. Best-effort and bounded: on timeout (or a dead PTY /
// cancelled request) it returns anyway and lets the write try — the same
// "proceed rather than wedge" stance as deviceapi warmup. The "❯" heuristic
// mirrors that warmup's readiness check.
func waitForIdlePrompt(ctx context.Context, sess *session.Session) {
	const (
		timeout = 12 * time.Second
		poll    = 150 * time.Millisecond
	)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		if strings.Contains(sess.VisibleText(), "❯") {
			return // idle prompt up → ready
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-tick.C:
		}
	}
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
