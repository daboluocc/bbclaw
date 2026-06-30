package reminder

import (
	"context"
	"log"
	"time"
)

// Injector fires one due reminder: it routes the reminder's Prompt back to its
// Target session's live bridge (ADR-042 §3) and returns nil once the turn has
// been accepted. An offline target should NOT error — the wiring defers it to the
// notify outbox (M3) and still returns nil so the reminder is marked done (the
// outbox owns redelivery). A returned error marks the reminder failed.
type Injector interface {
	Fire(ctx context.Context, r Reminder) error
}

// InjectorFunc adapts a function to Injector.
type InjectorFunc func(ctx context.Context, r Reminder) error

func (f InjectorFunc) Fire(ctx context.Context, r Reminder) error { return f(ctx, r) }

// Scheduler polls the Store and fires due reminders through the Injector. One
// goroutine, started by Run, stopped by cancelling its ctx. The clock is
// injectable so tests drive time deterministically.
type Scheduler struct {
	store *Store
	inj   Injector
	tick  time.Duration
	now   func() time.Time
}

// NewScheduler builds a scheduler. tick<=0 defaults to 10s — fine for minute-
// granularity reminders (mirrors the SMS-reminder UX; sub-second precision is a
// non-goal). now defaults to time.Now.
func NewScheduler(store *Store, inj Injector, tick time.Duration, now func() time.Time) *Scheduler {
	if tick <= 0 {
		tick = 10 * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &Scheduler{store: store, inj: inj, tick: tick, now: now}
}

// Run blocks until ctx is cancelled, firing due reminders each tick. It also
// fires once immediately on start so a reminder whose RunAt already passed while
// the adapter was down is delivered promptly on restart (late > never).
func (s *Scheduler) Run(ctx context.Context) {
	s.fireDue(ctx)
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.fireDue(ctx)
		}
	}
}

// fireDue fires every reminder due as of now, sequentially. Each transitions
// scheduled→running→done/failed and is persisted, so a crash mid-fire never
// double-fires (a running reminder is not re-picked by Pending). Sequential
// firing keeps the device from being flooded if several come due at once.
func (s *Scheduler) fireDue(ctx context.Context) {
	for _, r := range s.store.Pending(s.now()) {
		if err := s.store.SetState(r.ID, StateRunning); err != nil {
			log.Printf("reminder: mark running %s: %v", r.ID, err)
			continue
		}
		if err := s.inj.Fire(ctx, r); err != nil {
			log.Printf("reminder: fire %s failed: %v", r.ID, err)
			_ = s.store.SetState(r.ID, StateFailed)
			continue
		}
		_ = s.store.SetState(r.ID, StateDone)
	}
}
