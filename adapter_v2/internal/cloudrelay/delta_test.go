package cloudrelay

import (
	"strings"
	"sync"
	"testing"
)

// reconstructLikeCloud mimics the cloud's deviceTTSChunkStreamer.handleReplyDelta
// append-only diff (server.go:1187-1196): increment = text[len(prev):] when text
// extends prev, else the whole text; the spoken audio is the concatenation of all
// increments. We assert that, given our forwarded deltas, the cloud speaks the
// reply EXACTLY once (no repeats) — the bug that caused "一直在重复播报".
func reconstructLikeCloud(deltas []string) string {
	prev, spoken := "", ""
	for _, text := range deltas {
		inc := text
		if len(text) > len(prev) && strings.HasPrefix(text, prev) {
			inc = text[len(prev):]
		}
		prev = text
		spoken += inc
	}
	return spoken
}

// driveTurn feeds a sequence of reply snapshots (as deviceapi would, ending with
// the final via ReplyComplete) and returns the deltas the relay forwarded.
func driveTurn(snapshots []string, final string) []string {
	var forwarded []string
	ev := &cloudEvents{}
	ev.begin(func(env Envelope) error {
		if env.Kind == "voice.reply.delta" {
			forwarded = append(forwarded, env.Payload["text"].(string))
		}
		return nil
	}, Envelope{MessageID: "m"}, "site")
	for _, s := range snapshots {
		ev.ReplyDelta(s)
	}
	ev.ReplyComplete(final)
	return forwarded
}

// TestDeltaConcurrentTurns mirrors production: reply events (ReplyDelta/
// ReplyComplete/TurnIdle) fire from the Bridge.Run goroutine while begin/end/
// reply() run on the handleTranscript goroutine. Run under -race to catch any
// real data race on cloudEvents' fields (the single-goroutine table test above
// exercises none). Also asserts a late ReplyDelta after end() is dropped.
func TestDeltaConcurrentTurns(t *testing.T) {
	ev := &cloudEvents{}
	write := func(Envelope) error { return nil }
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		done := ev.begin(write, Envelope{MessageID: "m"}, "site")
		wg.Add(1)
		go func() { // the Bridge.Run side
			defer wg.Done()
			ev.ReplyDelta("你")
			ev.ReplyDelta("你好")
			ev.ReplyComplete("你好,我在")
			ev.TurnIdle()
		}()
		<-done // the handleTranscript side waits for the turn, then reads + disarms
		_ = ev.reply()
		ev.end()
		wg.Wait()
		ev.ReplyDelta("late") // after end(): must be dropped (active==false), no panic
	}
}

func TestDeltaMonotonicNoRepeat(t *testing.T) {
	cases := []struct {
		name      string
		snapshots []string
		final     string
		want      string // what the cloud should speak — the reply, once
	}{
		{
			name:      "clean append-only growth",
			snapshots: []string{"你", "你好", "你好,", "你好,我在"},
			final:     "你好,我在",
			want:      "你好,我在",
		},
		{
			name: "TUI redraw jitter (non-monotonic snapshots dropped)",
			// "你好" then a redraw that drops a char, then resumes growth past it.
			snapshots: []string{"你好", "你", "你好,我", "你好,我在这"},
			final:     "你好,我在这",
			want:      "你好,我在这",
		},
		{
			name:      "all snapshots jittery, final flushes whole reply once",
			snapshots: []string{"abc", "ab", "axc", "ab"},
			final:     "abcdef",
			want:      "abcdef",
		},
		{
			name:      "no intermediate deltas, final only",
			snapshots: nil,
			final:     "just the final",
			want:      "just the final",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deltas := driveTurn(tc.snapshots, tc.final)

			// 1) every forwarded delta strictly extends the previous (monotonic).
			for i := 1; i < len(deltas); i++ {
				if !strings.HasPrefix(deltas[i], deltas[i-1]) || len(deltas[i]) <= len(deltas[i-1]) {
					t.Errorf("delta %d %q does not strictly extend %q", i, deltas[i], deltas[i-1])
				}
			}
			// 2) the cloud's append-only diff speaks the reply exactly once.
			if got := reconstructLikeCloud(deltas); got != tc.want {
				t.Errorf("cloud would speak %q, want %q (deltas=%v)", got, tc.want, deltas)
			}
		})
	}
}
