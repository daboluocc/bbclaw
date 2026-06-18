package session

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
)

// newTestManager returns a Manager whose reaper is NOT running, so tests drive
// GC deterministically via reap(). detachTimeout is overridable per test.
func newTestManager(detach time.Duration) *Manager {
	return &Manager{
		sessions:      map[string]*Session{},
		creating:      map[string]*sync.Mutex{},
		detachTimeout: detach,
		gcInterval:    time.Hour, // unused; we call reap() directly
	}
}

// readWithin drains chunks from a client until want is seen in the accumulated
// output or the deadline passes. Returns the accumulated output and whether the
// substring was found.
func readWithin(t *testing.T, c *Client, want string, d time.Duration) (string, bool) {
	t.Helper()
	var sb strings.Builder
	deadline := time.After(d)
	for {
		select {
		case chunk, ok := <-c.Out:
			if !ok {
				return sb.String(), strings.Contains(sb.String(), want)
			}
			sb.Write(chunk)
			if strings.Contains(sb.String(), want) {
				return sb.String(), true
			}
		case <-deadline:
			return sb.String(), strings.Contains(sb.String(), want)
		}
	}
}

// TestCreateBroadcastsToClient is the core happy path: Create spawns a shell,
// pumpOutput feeds the screen and broadcasts raw bytes, and an attached client
// receives the child's output.
func TestCreateBroadcastsToClient(t *testing.T) {
	m := newTestManager(detachTimeout)
	// echo a marker then keep the shell alive briefly so the client can drain it.
	s, err := m.Create("s1", ptyhost.Config{
		Argv:        []string{"bash", "-c", "echo broadcast_marker; sleep 0.3"},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := m.Get("s1"); got != s {
		t.Fatalf("Get(s1) = %p, want %p", got, s)
	}

	c, _, _ := s.Attach()
	out, ok := readWithin(t, c, "broadcast_marker", 3*time.Second)
	if !ok {
		t.Fatalf("client did not receive marker; got %q", out)
	}
}

// TestAttachStatusTransitions covers the Connected ⇄ Detached state machine:
// a fresh session with a client is Connected; detaching the last client flips
// it to Detached; re-attaching flips it back to Connected.
func TestAttachStatusTransitions(t *testing.T) {
	m := newTestManager(detachTimeout)
	s, err := m.Create("s1", ptyhost.Config{
		Argv:        []string{"bash", "-c", "sleep 2"},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { s.kill() })

	tests := []struct {
		name string
		do   func() *Client
		want Status
	}{
		{"connected after first attach", func() *Client { c, _, _ := s.Attach(); return c }, Connected},
	}
	var first *Client
	for _, tt := range tests {
		first = tt.do()
		if got := s.Status(); got != tt.want {
			t.Fatalf("%s: status = %v, want %v", tt.name, got, tt.want)
		}
	}

	// Detaching the only client flips to Detached.
	s.Detach(first)
	if got := s.Status(); got != Detached {
		t.Fatalf("status after last detach = %v, want Detached", got)
	}

	// Re-attaching flips back to Connected and clears the detached clock.
	c2, _, _ := s.Attach()
	if got := s.Status(); got != Connected {
		t.Fatalf("status after re-attach = %v, want Connected", got)
	}
	s.Detach(c2)
}

// TestReconnectSnapshotHasPreviousOutput verifies the reconnect-recovery
// contract: output produced before a disconnect is replayed in the snapshot (or
// scrollback) handed to the reconnecting client.
func TestReconnectSnapshotHasPreviousOutput(t *testing.T) {
	m := newTestManager(detachTimeout)
	s, err := m.Create("s1", ptyhost.Config{
		Argv:        []string{"bash", "-c", "echo SNAPSHOT_LINE; sleep 2"},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { s.kill() })

	// First client: attach and wait until the marker has been broadcast (which
	// means the screen has also Fed it, since both happen under the same lock).
	c1, _, _ := s.Attach()
	if out, ok := readWithin(t, c1, "SNAPSHOT_LINE", 3*time.Second); !ok {
		t.Fatalf("first client never saw marker; got %q", out)
	}
	s.Detach(c1)

	// Reconnect: the snapshot + scrollback must contain the pre-disconnect line.
	_, scrollback, snapshot := s.Attach()
	combined := string(snapshot)
	for _, chunk := range scrollback {
		combined += string(chunk)
	}
	if !strings.Contains(combined, "SNAPSHOT_LINE") {
		t.Fatalf("reconnect snapshot missing pre-disconnect output; got %q", combined)
	}
}

// TestGCReaping is the table for the detach-GC timing contract:
//   - a Connected session is never reaped;
//   - a session Detached for less than detachTimeout survives a GC pass;
//   - a session Detached for longer than detachTimeout is reaped (child killed).
func TestGCReaping(t *testing.T) {
	tests := []struct {
		name       string
		detach     time.Duration // manager's detachTimeout
		setup      func(s *Session)
		wantReaped bool
	}{
		{
			name:   "connected session is never reaped",
			detach: time.Millisecond,
			setup: func(s *Session) {
				c, _, _ := s.Attach() // keep one client → stays Connected
				_ = c
			},
			wantReaped: false,
		},
		{
			name:   "recently detached session survives (within timeout)",
			detach: time.Hour, // long timeout → just-detached is not yet expired
			setup: func(s *Session) {
				c, _, _ := s.Attach()
				s.Detach(c) // detached just now
			},
			wantReaped: false,
		},
		{
			name:   "long-detached session is reaped",
			detach: time.Millisecond, // tiny timeout → detached is already expired
			setup: func(s *Session) {
				c, _, _ := s.Attach()
				s.Detach(c)
				time.Sleep(5 * time.Millisecond) // exceed the tiny timeout
			},
			wantReaped: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManager(tt.detach)
			s, err := m.Create("s1", ptyhost.Config{
				Argv:        []string{"bash", "-c", "sleep 30"},
				InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			t.Cleanup(func() { s.kill() })

			tt.setup(s)
			m.reap()

			reaped := m.Get("s1") == nil
			if reaped != tt.wantReaped {
				t.Fatalf("reaped = %v, want %v", reaped, tt.wantReaped)
			}
		})
	}
}

// TestDetachedNotReapedWithinWindow asserts the spec's concrete timing: a
// session whose clients all dropped is NOT reaped while still inside the detach
// window, even across multiple GC passes. Uses a 5-minute timeout (the real
// default) but a virtual clock by checking that a fresh-detached session
// survives — we cannot wait 5 real minutes, so we assert the boundary logic.
func TestDetachedNotReapedWithinWindow(t *testing.T) {
	m := newTestManager(detachTimeout) // real 5m timeout
	s, err := m.Create("s1", ptyhost.Config{
		Argv:        []string{"bash", "-c", "sleep 30"},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { s.kill() })

	c, _, _ := s.Attach()
	s.Detach(c)

	// Several GC passes within the window must all leave the session alive.
	for i := 0; i < 3; i++ {
		m.reap()
		if m.Get("s1") == nil {
			t.Fatalf("session reaped within %v detach window on pass %d", detachTimeout, i)
		}
	}
}

// TestWriteAfterExitReturnsErrClosed checks that Write fails cleanly once the
// PTY child has exited, rather than panicking on a closed fd.
func TestWriteAfterExitReturnsErrClosed(t *testing.T) {
	m := newTestManager(detachTimeout)
	s, err := m.Create("s1", ptyhost.Config{
		Argv:        []string{"bash", "-c", "exit 0"},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Wait for pumpOutput to observe the exit and evict the session.
	deadline := time.After(3 * time.Second)
	for m.Get("s1") != nil {
		select {
		case <-deadline:
			t.Fatal("session not evicted after child exit")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	if err := s.Write([]byte("x")); err != ErrClosed {
		t.Fatalf("Write after exit = %v, want ErrClosed", err)
	}
}

// TestPTYExitEvictsAndClosesClients verifies that when the child exits,
// pumpOutput removes the session from the Manager and closes attached client
// channels so readers observe end-of-stream.
func TestPTYExitEvictsAndClosesClients(t *testing.T) {
	m := newTestManager(detachTimeout)
	s, err := m.Create("s1", ptyhost.Config{
		Argv:        []string{"bash", "-c", "echo bye; exit 0"},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	c, _, _ := s.Attach()

	// Drain until the channel is closed (EOF) or we time out.
	closed := false
	deadline := time.After(3 * time.Second)
drain:
	for {
		select {
		case _, ok := <-c.Out:
			if !ok {
				closed = true
				break drain
			}
		case <-deadline:
			break drain
		}
	}
	if !closed {
		t.Fatal("client channel not closed after child exit")
	}
	if m.Get("s1") != nil {
		t.Fatal("session not evicted from manager after child exit")
	}
}

// TestSlowClientDoesNotBlockBroadcast verifies the backpressure policy: a client
// that never drains its channel does not stall pumpOutput — a fast client still
// receives output. We fill a slow client's buffer and confirm a second client
// keeps flowing.
func TestSlowClientDoesNotBlockBroadcast(t *testing.T) {
	m := newTestManager(detachTimeout)
	s, err := m.Create("s1", ptyhost.Config{
		// Emit a steady stream so the slow client's buffer overflows.
		Argv:        []string{"bash", "-c", "for i in $(seq 1 500); do echo line_$i; done; sleep 0.5"},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { s.kill() })

	slow, _, _ := s.Attach() // never drained
	_ = slow
	fast, _, _ := s.Attach()

	if out, ok := readWithin(t, fast, "line_1", 3*time.Second); !ok {
		t.Fatalf("fast client starved by slow client; got %q", out)
	}
}

// TestCreateSpawnError ensures a bad argv surfaces the spawn error and does not
// register a half-built session.
func TestCreateSpawnError(t *testing.T) {
	m := newTestManager(detachTimeout)
	if _, err := m.Create("bad", ptyhost.Config{Argv: nil}); err == nil {
		t.Fatal("Create with empty argv: want error, got nil")
	}
	if m.Get("bad") != nil {
		t.Fatal("failed Create registered a session")
	}
}
