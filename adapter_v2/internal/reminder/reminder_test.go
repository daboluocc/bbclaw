package reminder

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func baseNow() time.Time {
	// Anchored to today (not a fixed calendar date): a fixed past date drifts out
	// of persistLocked's real-time 30-day history window as the test suite ages,
	// silently pruning records a test just wrote (List/History empty → index
	// panic). Still deterministic within a single test run.
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), 14, 0, 0, 0, time.Local)
}

func TestResolveDelay(t *testing.T) {
	now := baseNow()
	runAt, prompt, err := Resolve(map[string]string{"delay": "30m", "prompt": "看日志"}, now)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !runAt.Equal(now.Add(30 * time.Minute)) {
		t.Errorf("runAt = %v, want +30m", runAt)
	}
	if prompt != "看日志" {
		t.Errorf("prompt = %q", prompt)
	}
}

func TestResolveDelayDefaultsPrompt(t *testing.T) {
	_, prompt, err := Resolve(map[string]string{"delay": "1h"}, baseNow())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prompt != defaultPrompt {
		t.Errorf("prompt = %q, want default", prompt)
	}
}

func TestResolveTomorrow(t *testing.T) {
	now := baseNow()
	runAt, _, err := Resolve(map[string]string{"at": "tomorrow 09:30"}, now)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	tomorrow := now.AddDate(0, 0, 1)
	want := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 9, 30, 0, 0, time.Local)
	if !runAt.Equal(want) {
		t.Errorf("runAt = %v, want %v", runAt, want)
	}
}

func TestResolveRejectsBadInput(t *testing.T) {
	cases := []map[string]string{
		{},                        // no time
		{"delay": "abc"},          // bad duration
		{"delay": "-5m"},          // non-positive
		{"at": "next week 09:00"}, // unsupported form
		{"at": "tomorrow 99:00"},  // out of range
	}
	for _, args := range cases {
		if _, _, err := Resolve(args, baseNow()); err == nil {
			t.Errorf("Resolve(%v) = nil err, want error", args)
		}
	}
}

func TestConfirmText(t *testing.T) {
	now := baseNow()
	r := Reminder{Prompt: "检查烧录日志", RunAt: now.Add(30 * time.Minute)}
	got := ConfirmText(r, now)
	if got != "已设置30 分钟后提醒：检查烧录日志" {
		t.Errorf("ConfirmText = %q", got)
	}
}

func TestStoreAddListCancelPersist(t *testing.T) {
	now := baseNow()
	path := filepath.Join(t.TempDir(), "reminders.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r, err := s.Add(Reminder{Prompt: "看日志", RunAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if r.ID == "" || r.State != StateScheduled {
		t.Fatalf("Add returned %+v", r)
	}
	// Reopen → reminder survives the round-trip.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.List(); len(got) != 1 || got[0].ID != r.ID {
		t.Fatalf("after reopen List = %+v", got)
	}
	// Cancel removes it from Pending.
	if err := s2.Cancel(r.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if due := s2.Pending(now.Add(2 * time.Hour)); len(due) != 0 {
		t.Errorf("canceled reminder still pending: %+v", due)
	}
}

func TestStoreAddRejectsPastTime(t *testing.T) {
	now := baseNow()
	s, _ := Open(filepath.Join(t.TempDir(), "r.json"))
	if _, err := s.Add(Reminder{Prompt: "x", RunAt: now.Add(-time.Minute)}, now); err == nil {
		t.Error("Add past-time = nil err, want error")
	}
}

func TestSchedulerFiresDueOnce(t *testing.T) {
	now := baseNow()
	s, _ := Open(filepath.Join(t.TempDir(), "r.json"))
	if _, err := s.Add(Reminder{Prompt: "ping", RunAt: now.Add(time.Minute)}, now); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var fired []Reminder
	inj := InjectorFunc(func(_ context.Context, r Reminder) (string, error) {
		mu.Lock()
		fired = append(fired, r)
		mu.Unlock()
		return "", nil
	})

	// Clock advanced past RunAt; tiny tick so Run loops fast.
	clock := now.Add(2 * time.Minute)
	sch := NewScheduler(s, inj, 5*time.Millisecond, func() time.Time { return clock })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sch.Run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(fired)
		mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("reminder never fired")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// It must fire exactly once: state is now done, not re-picked.
	cancel()
	if got := s.List(); got[0].State != StateDone {
		t.Errorf("state = %s, want done", got[0].State)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 {
		t.Errorf("fired %d times, want 1", len(fired))
	}
}

func TestSchedulerMarksFailedOnInjectorError(t *testing.T) {
	now := baseNow()
	s, _ := Open(filepath.Join(t.TempDir(), "r.json"))
	s.Add(Reminder{Prompt: "ping", RunAt: now.Add(time.Minute)}, now)

	inj := InjectorFunc(func(_ context.Context, _ Reminder) (string, error) { return "", context.DeadlineExceeded })
	clock := now.Add(2 * time.Minute)
	sch := NewScheduler(s, inj, time.Millisecond, func() time.Time { return clock })
	sch.fireDue(context.Background()) // synchronous single pass

	if got := s.List(); got[0].State != StateFailed {
		t.Errorf("state = %s, want failed", got[0].State)
	}
}
