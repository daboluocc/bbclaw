package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/reminder"
)

func newTestReminderStore(t *testing.T) *reminder.Store {
	t.Helper()
	s, err := reminder.Open(filepath.Join(t.TempDir(), "reminders.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func TestRemindersCreateAndList(t *testing.T) {
	store := newTestReminderStore(t)
	origin := func() reminder.Origin { return reminder.Origin{DeviceID: "dev-9"} }
	h := Reminders(store, origin)

	// POST a reminder.
	req := httptest.NewRequest(http.MethodPost, "/v1/reminders",
		strings.NewReader(`{"prompt":"看日志","delay":"30m","mode":"task"}`))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// GET lists it with the mapped fields.
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/reminders", nil))
	var got struct {
		OK   bool `json:"ok"`
		Data struct {
			Reminders []reminderView `json:"reminders"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data.Reminders) != 1 {
		t.Fatalf("want 1 reminder, got %d", len(got.Data.Reminders))
	}
	r := got.Data.Reminders[0]
	if r.Prompt != "看日志" || r.Mode != "task" || r.State != reminder.StateScheduled || r.DeviceID != "dev-9" {
		t.Errorf("reminder view wrong: %+v", r)
	}
}

func TestRemindersRejectsBadTime(t *testing.T) {
	h := Reminders(newTestReminderStore(t), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/reminders",
		strings.NewReader(`{"prompt":"x"}`)) // no delay/at
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing time", rec.Code)
	}
}

func TestReminderCancel(t *testing.T) {
	store := newTestReminderStore(t)
	create := Reminders(store, nil)
	rec := httptest.NewRecorder()
	create(rec, httptest.NewRequest(http.MethodPost, "/v1/reminders",
		strings.NewReader(`{"prompt":"x","delay":"1h"}`)))
	id := store.List()[0].ID

	del := ReminderByID(store)
	rec = httptest.NewRecorder()
	del(rec, httptest.NewRequest(http.MethodDelete, "/v1/reminders/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if store.List()[0].State != reminder.StateCanceled {
		t.Errorf("reminder not canceled: %s", store.List()[0].State)
	}
}
