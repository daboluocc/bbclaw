package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/workspace"
)

// stubDistiller returns fixed items and signals each invocation on calls.
type stubDistiller struct {
	items []Item
	calls chan struct{}
	block chan struct{} // when non-nil, Distill waits on it before returning
}

func (s *stubDistiller) Distill(ctx context.Context, userText, replyText string) ([]Item, error) {
	if s.calls != nil {
		s.calls <- struct{}{}
	}
	if s.block != nil {
		<-s.block
	}
	return s.items, nil
}

func TestWriterAppendsDistilledNotes(t *testing.T) {
	path := writeCLAUDE(t, seededWithEmptyBlock())
	d := &stubDistiller{
		items: []Item{{Category: "preference", Text: "异步要点"}},
		calls: make(chan struct{}, 1),
	}
	w := NewWriter(NewStore(path), d, nil)

	w.RecordTurn("用户说了一句够长的话", "管家回复", "/ws")

	select {
	case <-d.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("distiller was never invoked")
	}

	// Wait for the append to land (worker writes after Distill returns).
	deadline := time.After(2 * time.Second)
	for {
		inner, _ := workspace.ManagedBlock(readFile(t, path))
		if strings.Contains(inner, "异步要点") {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("note never appended; block=%q", inner)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestWriterSkipsShortUtterance(t *testing.T) {
	path := writeCLAUDE(t, seededWithEmptyBlock())
	d := &stubDistiller{
		items: []Item{{Category: "preference", Text: "should-not-appear"}},
		calls: make(chan struct{}, 1),
	}
	w := NewWriter(NewStore(path), d, nil)

	w.RecordTurn("hi", "ok", "/ws") // below defaultMinUserChars

	select {
	case <-d.calls:
		t.Fatal("distiller should not run on a too-short utterance")
	case <-time.After(300 * time.Millisecond):
		// expected: no call
	}
}

func TestRecordTurnNonBlockingWhenQueueFull(t *testing.T) {
	path := writeCLAUDE(t, seededWithEmptyBlock())
	block := make(chan struct{})
	d := &stubDistiller{
		items: []Item{{Category: "p", Text: "x"}},
		block: block,
	}
	w := NewWriter(NewStore(path), d, nil)
	// Deliberately never close(block): unblocking at test end would let the
	// worker drain the queued memos and write into t.TempDir concurrently
	// with its RemoveAll cleanup — a "directory not empty" flake (seen on CI
	// linux-amd64). Parking the worker inside Distill guarantees no writes;
	// the goroutine dies with the test binary.
	_ = block

	// Saturate: 1 in-flight (worker blocked in Distill) + fill the buffered
	// channel. Further RecordTurn calls must return immediately (drop), never
	// block the caller.
	for i := 0; i < defaultQueueDepth+5; i++ {
		done := make(chan struct{})
		go func() {
			w.RecordTurn("一句足够长的用户话语", "reply", "/ws")
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("RecordTurn blocked on call %d (queue full should drop)", i)
		}
	}
}
