package memory

import (
	"testing"
	"time"
)

func baseCfg() triggerConfig {
	return triggerConfig{
		maxBytes:       4096,
		thresholdRatio: 0.75,
		idleGap:        5 * time.Minute,
		maxGap:         6 * time.Hour,
		cooldown:       10 * time.Minute,
	}
}

func TestDecideTriggerEmptyInboxNeverFires(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s := triggerState{now: now, inboxBytes: 0, hasInbox: false}
	if fire, _ := decideTrigger(s, baseCfg()); fire {
		t.Error("empty inbox must never fire")
	}
}

func TestDecideTriggerThreshold(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cfg := baseCfg()
	// 75% of 4096 = 3072. Just over should fire as "threshold".
	s := triggerState{
		now:               now,
		inboxBytes:        3100,
		hasInbox:          true,
		lastTurnAt:        now,                        // not idle
		lastConsolidateAt: now.Add(-20 * time.Minute), // past cooldown
	}
	fire, reason := decideTrigger(s, cfg)
	if !fire || reason != "threshold" {
		t.Errorf("got (%v,%q), want (true,threshold)", fire, reason)
	}
	// Just under should not fire (nothing else triggers).
	s.inboxBytes = 3000
	if fire, _ := decideTrigger(s, cfg); fire {
		t.Error("below threshold must not fire on size alone")
	}
}

func TestDecideTriggerIdle(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	cfg := baseCfg()
	s := triggerState{
		now:               now,
		inboxBytes:        100, // below threshold
		hasInbox:          true,
		lastTurnAt:        now.Add(-6 * time.Minute),  // idle > 5m
		lastConsolidateAt: now.Add(-20 * time.Minute), // past cooldown
	}
	fire, reason := decideTrigger(s, cfg)
	if !fire || reason != "idle" {
		t.Errorf("got (%v,%q), want (true,idle)", fire, reason)
	}
	// Recent turn → not idle.
	s.lastTurnAt = now.Add(-1 * time.Minute)
	if fire, r := decideTrigger(s, cfg); fire {
		t.Errorf("recent turn must not fire idle, got reason=%q", r)
	}
}

func TestDecideTriggerBackstop(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	cfg := baseCfg()
	s := triggerState{
		now:               now,
		inboxBytes:        50, // below threshold
		hasInbox:          true,
		lastTurnAt:        now.Add(-1 * time.Minute), // not idle
		lastConsolidateAt: now.Add(-7 * time.Hour),   // past maxGap (6h)
	}
	fire, reason := decideTrigger(s, cfg)
	if !fire || reason != "backstop" {
		t.Errorf("got (%v,%q), want (true,backstop)", fire, reason)
	}
}

func TestDecideTriggerCooldownSuppresses(t *testing.T) {
	now := time.Unix(4_000_000, 0)
	cfg := baseCfg()
	// Inbox is over threshold AND idle, but a consolidation just ran 1m ago.
	s := triggerState{
		now:               now,
		inboxBytes:        4000,
		hasInbox:          true,
		lastTurnAt:        now.Add(-10 * time.Minute),
		lastConsolidateAt: now.Add(-1 * time.Minute), // within 10m cooldown
	}
	fire, reason := decideTrigger(s, cfg)
	if fire || reason != "cooldown" {
		t.Errorf("got (%v,%q), want (false,cooldown)", fire, reason)
	}
}

func TestDecideTriggerFirstRunNoPriorConsolidation(t *testing.T) {
	now := time.Unix(5_000_000, 0)
	cfg := baseCfg()
	// Zero lastConsolidateAt: cooldown gate is skipped, backstop (now-zero huge)
	// would fire even below threshold as long as inbox is non-empty.
	s := triggerState{
		now:        now,
		inboxBytes: 10,
		hasInbox:   true,
		lastTurnAt: now, // not idle
	}
	fire, reason := decideTrigger(s, cfg)
	if !fire || reason != "backstop" {
		t.Errorf("first run got (%v,%q), want (true,backstop)", fire, reason)
	}
}
