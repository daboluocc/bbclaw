package reminder

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLegacyTargetMigrates loads a v1 file (with the old `target` key) and
// checks it backfills Origin (ADR-042 §10.1). cwdName is dropped.
func TestLegacyTargetMigrates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reminders.json")
	v1 := `{
	  "version": 1,
	  "reminders": [
	    {"id":"rem_1","kind":"once","mode":"notify","prompt":"看日志",
	     "runAt":"2999-01-01T00:00:00Z","state":"scheduled",
	     "target":{"deviceId":"BBClaw-AA","sessionId":"s1","cwdName":"proj"},
	     "createdAt":"2026-06-30T00:00:00Z"}
	  ]
	}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got := s.List()
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got[0].Origin.DeviceID != "BBClaw-AA" || got[0].Origin.SessionID != "s1" {
		t.Errorf("origin not migrated: %+v", got[0].Origin)
	}
}

// TestHistoryAndScheduledSplit checks Scheduled() vs History() partitioning and
// that Complete records FiredAt/Outcome (ADR-042 §10.3).
func TestHistoryAndScheduledSplit(t *testing.T) {
	now := baseNow()
	s, _ := Open(filepath.Join(t.TempDir(), "r.json"))
	up, _ := s.Add(Reminder{Prompt: "upcoming", RunAt: now.Add(time.Hour)}, now)
	done, _ := s.Add(Reminder{Prompt: "fired", RunAt: now.Add(time.Minute)}, now)

	firedAt := now.Add(time.Minute)
	if err := s.Complete(done.ID, StateDone, "已跑完", firedAt); err != nil {
		t.Fatalf("complete: %v", err)
	}

	sched := s.Scheduled()
	if len(sched) != 1 || sched[0].ID != up.ID {
		t.Fatalf("scheduled = %+v, want only %s", sched, up.ID)
	}
	hist := s.History()
	if len(hist) != 1 || hist[0].ID != done.ID {
		t.Fatalf("history = %+v, want only %s", hist, done.ID)
	}
	if hist[0].Outcome != "已跑完" || !hist[0].FiredAt.Equal(firedAt) {
		t.Errorf("history record lost outcome/firedAt: %+v", hist[0])
	}
}

// TestHistoryRetentionByAge prunes history older than maxHistAge on persist, and
// keeps scheduled untouched.
func TestHistoryRetentionByAge(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "r.json")
	s, _ := Open(path)

	// One live scheduled (far future) + one ancient fired record.
	s.Add(Reminder{Prompt: "keep", RunAt: now.Add(24 * time.Hour)}, now)
	old, _ := s.Add(Reminder{Prompt: "ancient", RunAt: now.Add(time.Minute)}, now)
	// Fire it 40 days ago → beyond the 30d bound.
	if err := s.Complete(old.ID, StateDone, "x", now.Add(-40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Reload from disk: the aged record must be gone, the scheduled one kept.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(s2.Scheduled()) != 1 {
		t.Errorf("scheduled pruned: %+v", s2.Scheduled())
	}
	if len(s2.History()) != 0 {
		t.Errorf("aged history not pruned: %+v", s2.History())
	}
}
