package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/butler"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

func newDispatchTestServer(rec *butler.DispatchRecorder) (*Server, *httptest.Server) {
	srv := NewServer(AppConfig{}, nil, nil, nil, nil, obs.NewLogger(), obs.NewMetrics())
	if rec != nil {
		srv.SetDispatchRecorder(rec)
	}
	ts := httptest.NewServer(srv.Handler())
	return srv, ts
}

// TestHandleButlerDispatchRecent_Empty verifies that an empty ring buffer
// returns a JSON array (not null).
func TestHandleButlerDispatchRecent_Empty(t *testing.T) {
	rec := butler.NewDispatchRecorder()
	_, ts := newDispatchTestServer(rec)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/butler/dispatch/recent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	var entries []butler.DispatchEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if entries == nil {
		t.Error("want non-nil slice (empty array), got nil")
	}
	if len(entries) != 0 {
		t.Errorf("want 0 entries, got %d", len(entries))
	}
}

// TestHandleButlerDispatchRecent_HasEntries verifies that recorded entries
// are returned newest-first and capped at 20.
func TestHandleButlerDispatchRecent_HasEntries(t *testing.T) {
	rec := butler.NewDispatchRecorder()
	// Insert 5 entries
	for i := 1; i <= 5; i++ {
		rec.Record(agent.Event{
			Type: agent.EvDispatchStatus,
			Dispatch: &agent.DispatchStatus{
				Phase:  "started",
				TaskID: "task-" + string(rune('0'+i)),
				Cwd:    "proj",
				Title:  "task title",
			},
		})
	}
	_, ts := newDispatchTestServer(rec)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/butler/dispatch/recent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var entries []butler.DispatchEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("want 5 entries, got %d", len(entries))
	}
	// newest first: task-5 should be first
	if entries[0].TaskID != "task-5" {
		t.Errorf("want task-5 first, got %q", entries[0].TaskID)
	}
}

// TestHandleButlerDispatchRecent_CapAt20 verifies that the endpoint returns
// at most 20 entries by default.
func TestHandleButlerDispatchRecent_CapAt20(t *testing.T) {
	rec := butler.NewDispatchRecorder()
	for i := 0; i < 30; i++ {
		rec.Record(agent.Event{
			Type: agent.EvDispatchStatus,
			Dispatch: &agent.DispatchStatus{
				Phase:  "started",
				TaskID: "t",
				Cwd:    "proj",
				Title:  "t",
			},
		})
	}
	// Use unique IDs to avoid map collision
	rec2 := butler.NewDispatchRecorder()
	for i := 0; i < 30; i++ {
		id := "task-" + string(rune('a'+i%26))
		rec2.Record(agent.Event{
			Type: agent.EvDispatchStatus,
			Dispatch: &agent.DispatchStatus{Phase: "started", TaskID: id, Cwd: "proj", Title: "t"},
		})
	}

	_, ts := newDispatchTestServer(rec2)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/butler/dispatch/recent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var entries []butler.DispatchEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) > 20 {
		t.Errorf("want at most 20 entries, got %d", len(entries))
	}
}

// TestHandleButlerDispatchRecent_NilRecorder verifies graceful handling when
// no recorder has been wired (returns empty array).
func TestHandleButlerDispatchRecent_NilRecorder(t *testing.T) {
	_, ts := newDispatchTestServer(nil)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/butler/dispatch/recent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var entries []butler.DispatchEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("want 0 entries with nil recorder, got %d", len(entries))
	}
}
