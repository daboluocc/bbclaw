package butler

import (
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

func TestDispatchRingEmpty(t *testing.T) {
	r := NewDispatchRing()
	got := r.Recent(20)
	if len(got) != 0 {
		t.Errorf("want empty slice, got %d entries", len(got))
	}
}

func TestDispatchRingRecordAndRecent(t *testing.T) {
	r := NewDispatchRing()
	for i, phase := range []string{"started", "done", "error"} {
		r.Record(&agent.DispatchStatus{
			Phase:  phase,
			TaskID: "task-" + string(rune('A'+i)),
			Cwd:    "proj",
		})
	}
	got := r.Recent(20)
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d", len(got))
	}
	// Recent returns newest-first.
	if got[0].TaskID != "task-C" {
		t.Errorf("want newest task-C first, got %q", got[0].TaskID)
	}
	if got[2].TaskID != "task-A" {
		t.Errorf("want oldest task-A last, got %q", got[2].TaskID)
	}
}

func TestDispatchRingUpsertSameTaskID(t *testing.T) {
	r := NewDispatchRing()
	r.Record(&agent.DispatchStatus{Phase: "started", TaskID: "t1", Cwd: "proj"})
	r.Record(&agent.DispatchStatus{Phase: "done", TaskID: "t1", ElapsedMs: 500})

	got := r.Recent(20)
	if len(got) != 1 {
		t.Fatalf("upsert: want 1 entry, got %d", len(got))
	}
	if got[0].Status != "done" {
		t.Errorf("want status=done after upsert, got %q", got[0].Status)
	}
	if got[0].ElapsedMs != 500 {
		t.Errorf("want elapsedMs=500 after upsert, got %d", got[0].ElapsedMs)
	}
}

func TestDispatchRingCapacityEviction(t *testing.T) {
	r := NewDispatchRing()
	// Insert dispatchRingCapacity+5 entries with distinct task IDs.
	for i := 0; i < dispatchRingCapacity+5; i++ {
		r.Record(&agent.DispatchStatus{
			Phase:  "started",
			TaskID: "task-" + string(rune('a'+i%26)) + string(rune('0'+i%10)),
		})
	}
	got := r.Recent(100)
	if len(got) != dispatchRingCapacity {
		t.Errorf("want %d entries after eviction, got %d", dispatchRingCapacity, len(got))
	}
}

func TestDispatchRingRecentLimit(t *testing.T) {
	r := NewDispatchRing()
	for i := 0; i < 10; i++ {
		r.Record(&agent.DispatchStatus{Phase: "started", TaskID: "t" + string(rune('0'+i))})
	}
	got := r.Recent(5)
	if len(got) != 5 {
		t.Errorf("want 5 entries with limit=5, got %d", len(got))
	}
}

func TestTruncateDispatchTitle(t *testing.T) {
	cases := []struct {
		input    string
		maxCJK   int
		wantLen  int // rune length of expected result (including possible "…")
		wantTail string
	}{
		{"hello", 24, 5, "hello"},
		{"重构 auth 模块", 24, 10, "重构 auth 模块"},
		{"这是一个很长的派发任务标题超过了二十四个字符的限制", 24, 25, "…"},
	}
	for _, tc := range cases {
		got := truncateDispatchTitle(tc.input, tc.maxCJK)
		runes := []rune(got)
		if len(runes) != tc.wantLen {
			t.Errorf("truncateDispatchTitle(%q, %d): want %d runes, got %d (%q)",
				tc.input, tc.maxCJK, tc.wantLen, len(runes), got)
		}
		if tc.wantTail == "…" && runes[len(runes)-1] != '…' {
			t.Errorf("truncateDispatchTitle(%q, %d): want trailing …, got %q", tc.input, tc.maxCJK, got)
		}
	}
}
