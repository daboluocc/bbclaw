package memory

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/workspace"
)

// TestWriterRunsConsolidationOnThreshold wires a Writer with both a distiller
// and a consolidator on the single worker, then asserts that once a distilled
// append pushes the inbox past the threshold, consolidation runs on that same
// worker (archiving + clearing the inbox). This exercises the engine→worker→
// consolidate path and the "one worker serializes distill + consolidate"
// guarantee (ADR-022 §4).
func TestWriterRunsConsolidationOnThreshold(t *testing.T) {
	path := writeCLAUDE(t, seededWithEmptyBlock())

	// Distiller appends a sizeable note so the inbox crosses the (deliberately
	// tiny) threshold after one turn.
	d := &stubDistiller{
		items: []Item{{Category: "preference", Text: strings.Repeat("长偏好", 10)}},
		calls: make(chan struct{}, 1),
	}
	sum := &stubSummarizer{result: map[string][]string{
		"preference": {"整理后的偏好"},
	}}

	w := newWriter(NewStore(path), d, nil)
	w.consolidator = NewConsolidator(path, sum, nil)
	w.tickInterval = 0 // deterministic: no time-based ticker, threshold only
	w.trig = triggerConfig{
		maxBytes:       40,  // tiny cap so one note crosses 75%
		thresholdRatio: 0.5, // → fire at >=20 bytes
		cooldown:       0,
	}
	w.start()

	w.RecordTurn("一句足够长的用户话语触发蒸馏", "管家回复", "/ws")

	deadline := time.After(3 * time.Second)
	for {
		pref, ok := readProfile(t, filepath.Dir(path), "preference")
		inner, _ := workspace.ManagedBlock(readFile(t, path))
		if ok && strings.Contains(pref, "整理后的偏好") && strings.TrimSpace(inner) == "" {
			return // consolidated + inbox cleared
		}
		select {
		case <-deadline:
			t.Fatalf("consolidation did not run; profile_ok=%v inbox=%q", ok, inner)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
