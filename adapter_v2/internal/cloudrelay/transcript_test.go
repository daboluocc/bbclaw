package cloudrelay

import (
	"context"
	"testing"
)

// TestCloudBridgePreempt covers the barge-in handle mechanism: preempt cancels the
// in-flight turn, and disarm only clears its OWN turn (a stale handle must not
// clear a newer turn that already replaced it). The end-to-end barge-in (a newer
// transcript interrupting claude) is exercised against the real CLI; this pins the
// pointer-identity bookkeeping that routes the cancel correctly.
func TestCloudBridgePreempt(t *testing.T) {
	cb := &cloudBridge{}

	// preempt with nothing in flight is a no-op (the common sequential case).
	cb.preempt()

	// arm a turn; preempt cancels exactly it.
	ctx1, c1 := context.WithCancel(context.Background())
	h1 := cb.arm(c1)
	cb.preempt()
	select {
	case <-ctx1.Done():
	default:
		t.Fatal("preempt did not cancel the in-flight turn")
	}

	// A stale disarm (h1) after a newer arm (h2) must NOT clear h2 — else a
	// finishing superseded turn would unregister the live one and the next barge-in
	// couldn't preempt it.
	_, c2 := context.WithCancel(context.Background())
	h2 := cb.arm(c2)
	cb.disarm(h1)
	cb.actMu.Lock()
	active := cb.active
	cb.actMu.Unlock()
	if active != h2 {
		t.Fatal("stale disarm cleared the active (newer) turn")
	}

	// disarm of the active handle clears it.
	cb.disarm(h2)
	cb.actMu.Lock()
	active = cb.active
	cb.actMu.Unlock()
	if active != nil {
		t.Fatal("disarm of the active handle should clear it")
	}
}
