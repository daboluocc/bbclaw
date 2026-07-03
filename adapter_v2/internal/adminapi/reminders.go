package adminapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/reminder"
)

// reminderView is the JSON shape the management page renders — the stored fields
// plus times as unix seconds (the SPA formats them locally). FiredAt/Outcome are
// the history fields (ADR-042 §10.3), zero/empty while scheduled.
type reminderView struct {
	ID        string `json:"id"`
	Mode      string `json:"mode"`
	Prompt    string `json:"prompt"`
	RunAtUnix int64  `json:"runAt"`
	State     string `json:"state"`
	DeviceID  string `json:"deviceId,omitempty"` // Origin device (attribution)
	CreatedAt int64  `json:"createdAt"`
	FiredAt   int64  `json:"firedAt,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
}

func toReminderView(r reminder.Reminder) reminderView {
	v := reminderView{
		ID:        r.ID,
		Mode:      r.Mode,
		Prompt:    r.Prompt,
		RunAtUnix: r.RunAt.Unix(),
		State:     r.State,
		DeviceID:  r.Origin.DeviceID,
		CreatedAt: r.CreatedAt.Unix(),
		Outcome:   r.Outcome,
	}
	if !r.FiredAt.IsZero() {
		v.FiredAt = r.FiredAt.Unix()
	}
	return v
}

func toReminderViews(list []reminder.Reminder) []reminderView {
	views := make([]reminderView, 0, len(list))
	for _, rem := range list {
		views = append(views, toReminderView(rem))
	}
	return views
}

// Reminders serves the management page's reminder list + create (ADR-042 §2.4,
// Task #7). GET lists all reminders (soonest first); POST creates one from a
// structured body {prompt, delay, at, mode} — the same time grammar the voice
// path uses (reminder.Resolve), so "delay":"30m" or "at":"tomorrow 09:30".
// origin supplies the device/session a web-created reminder is attributed to (the
// current device), mirroring the voice create — attribution only (ADR-042 §10.1).
//
// GET ?view= selects the list (ADR-042 §10.3): "scheduled" (即将, default),
// "history" (已提醒: done/failed/canceled, newest first), or "all".
func Reminders(store *reminder.Store, origin func() reminder.Origin) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			var views []reminderView
			switch strings.TrimSpace(r.URL.Query().Get("view")) {
			case "history":
				views = toReminderViews(store.History())
			case "all":
				views = toReminderViews(store.List())
			default: // "scheduled" / "" — 即将
				views = toReminderViews(store.Scheduled())
			}
			writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"reminders": views}})
		case http.MethodPost:
			var body struct {
				Prompt string `json:"prompt"`
				Delay  string `json:"delay"` // Go duration, e.g. "30m" / "2h"
				At     string `json:"at"`    // "tomorrow HH:MM"
				Mode   string `json:"mode"`  // notify | task; "" → notify
			}
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
			if err := dec.Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST", Detail: err.Error()})
				return
			}
			now := time.Now()
			runAt, prompt, err := reminder.Resolve(map[string]string{
				"prompt": body.Prompt,
				"delay":  strings.TrimSpace(body.Delay),
				"at":     strings.TrimSpace(body.At),
			}, now)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "REMINDER_TIME_INVALID", Detail: err.Error()})
				return
			}
			var org reminder.Origin
			if origin != nil {
				org = origin()
			}
			rem, err := store.Add(reminder.Reminder{
				Prompt: prompt,
				Mode:   body.Mode,
				RunAt:  runAt,
				Origin: org,
			}, now)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "REMINDER_ADD_FAILED", Detail: err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"reminder": toReminderView(rem)}})
		default:
			w.Header().Set("Allow", "GET, POST")
			writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "METHOD_NOT_ALLOWED"})
		}
	}
}

// ReminderByID cancels a scheduled reminder: DELETE /v1/reminders/{id}.
func ReminderByID(store *reminder.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", "DELETE")
			writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "METHOD_NOT_ALLOWED"})
			return
		}
		id := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/v1/reminders/"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST", Detail: "missing reminder id"})
			return
		}
		if err := store.Cancel(id); err != nil {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "REMINDER_CANCEL_FAILED", Detail: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"canceled": id}})
	}
}
