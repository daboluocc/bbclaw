package memory

import "time"

// triggerConfig holds the consolidation trigger thresholds (ADR-022 §1). All
// fields are env-overridable; see config.go for resolution and defaults.
type triggerConfig struct {
	maxBytes       int           // inbox cap the threshold is measured against
	thresholdRatio float64       // fire when inboxBytes >= ratio*maxBytes (≥75%)
	idleGap        time.Duration // fire when idle this long with a non-empty inbox
	maxGap         time.Duration // backstop: fire this long after last consolidation
	cooldown       time.Duration // per-key: never fire within this of the last run
}

// triggerState is the runtime snapshot the decision reads. The Writer assembles
// it (reading inbox size off disk) and hands it to decideTrigger, keeping the
// decision a pure, table-testable function.
type triggerState struct {
	now               time.Time
	inboxBytes        int
	hasInbox          bool
	lastTurnAt        time.Time
	lastConsolidateAt time.Time
}

// decideTrigger is the pure trigger decision (ADR-022 §1). It returns whether a
// consolidation should run now and which of the four classes fired. Precedence:
// cooldown gate → threshold → idle → backstop. An empty inbox never fires.
func decideTrigger(s triggerState, cfg triggerConfig) (bool, string) {
	if !s.hasInbox {
		return false, ""
	}
	// per-key cooldown: suppress everything within the window of the last run.
	if cfg.cooldown > 0 && !s.lastConsolidateAt.IsZero() && s.now.Sub(s.lastConsolidateAt) < cfg.cooldown {
		return false, "cooldown"
	}
	// threshold: inbox is filling up (≥ ratio of the FIFO cap).
	if cfg.maxBytes > 0 && cfg.thresholdRatio > 0 &&
		float64(s.inboxBytes) >= cfg.thresholdRatio*float64(cfg.maxBytes) {
		return true, "threshold"
	}
	// idle: device has been quiet long enough that archiving won't interrupt.
	if cfg.idleGap > 0 && !s.lastTurnAt.IsZero() && s.now.Sub(s.lastTurnAt) >= cfg.idleGap {
		return true, "idle"
	}
	// backstop: ensure we consolidate at least once per maxGap regardless.
	if cfg.maxGap > 0 && s.now.Sub(s.lastConsolidateAt) >= cfg.maxGap {
		return true, "backstop"
	}
	return false, ""
}
