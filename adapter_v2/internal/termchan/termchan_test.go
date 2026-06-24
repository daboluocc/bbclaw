package termchan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

// ── test harness ────────────────────────────────────────────────────────────

// newServeServer stands up an httptest server whose handler upgrades to a
// WebSocket and runs Serve against the given session. Every client that dials it
// joins the SAME session, exactly as the production /ws route will.
func newServeServer(t *testing.T, sess *session.Session) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		// Serve owns the conn lifecycle; ignore the returned error in tests
		// (a client closing first is normal).
		_ = Serve(sess, conn)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// dial opens a client WebSocket to the test server.
func dial(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

// readUntil drains server messages from conn until the accumulated "output"
// payload contains want, or the deadline passes. Returns the accumulated output
// text and whether want was seen. reconnected frames are skipped.
func readUntil(t *testing.T, conn *websocket.Conn, want string, d time.Duration) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var sb strings.Builder
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return sb.String(), strings.Contains(sb.String(), want)
		}
		var msg ServerMsg
		if json.Unmarshal(data, &msg) == nil && msg.Type == "output" {
			sb.WriteString(msg.Data)
			if strings.Contains(sb.String(), want) {
				return sb.String(), true
			}
		}
	}
}

// sendInput writes one "input" ClientMsg.
func sendInput(t *testing.T, conn *websocket.Conn, data string) {
	t.Helper()
	b, _ := json.Marshal(ClientMsg{Type: "input", Data: data})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write input: %v", err)
	}
}

func newSession(t *testing.T, argv ...string) (*session.Manager, *session.Session) {
	t.Helper()
	m := session.NewManager()
	s, err := m.Create("s1", ptyhost.Config{
		Argv:        argv,
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return m, s
}

// ── integration: two clients both observe output ────────────────────────────

// TestTwoClientsBothReceiveOutput is the spec's first acceptance criterion: two
// WS clients join the same session and BOTH receive the PTY output stream (the
// older one is a read-only observer, not cut off from output).
func TestTwoClientsBothReceiveOutput(t *testing.T) {
	// `cat` echoes stdin back, and stays alive so both clients can observe.
	_, s := newSession(t, "cat")
	t.Cleanup(func() { _ = s.Resize(ptyhost.Size{Cols: 80, Rows: 24}) })
	srv := newServeServer(t, s)

	c1 := dial(t, srv)
	c2 := dial(t, srv) // c2 is the newer connection (the writer)

	// Let both Serve goroutines finish Attach before producing output.
	time.Sleep(100 * time.Millisecond)

	// The newest client (c2) types; cat echoes it back to the shared screen.
	sendInput(t, c2, "hello_both\n")

	if out, ok := readUntil(t, c1, "hello_both", 3*time.Second); !ok {
		t.Fatalf("observer client c1 did not receive output; got %q", out)
	}
	if out, ok := readUntil(t, c2, "hello_both", 3*time.Second); !ok {
		t.Fatalf("writer client c2 did not receive output; got %q", out)
	}
}

// TestOnlyNewestWritesStdin is the spec's second acceptance criterion: when two
// clients are connected, only the newest one's input reaches PTY stdin; the
// older connection is demoted to read-only and its input is dropped.
func TestOnlyNewestWritesStdin(t *testing.T) {
	_, s := newSession(t, "cat")
	srv := newServeServer(t, s)

	c1 := dial(t, srv)
	time.Sleep(100 * time.Millisecond)
	c2 := dial(t, srv) // c2 supersedes c1 as the writer
	time.Sleep(100 * time.Millisecond)

	// c1 (now an observer) types FROM_OLD; it must be ignored.
	sendInput(t, c1, "FROM_OLD\n")
	// c2 (the owner) types FROM_NEW; it must reach cat and echo back.
	sendInput(t, c2, "FROM_NEW\n")

	// We should see FROM_NEW; FROM_OLD must NOT appear before it (cat echoes in
	// arrival order, so if the old write had gone through it would precede NEW).
	out, ok := readUntil(t, c2, "FROM_NEW", 3*time.Second)
	if !ok {
		t.Fatalf("writer input never echoed; got %q", out)
	}
	if strings.Contains(out, "FROM_OLD") {
		t.Fatalf("observer (old) input leaked to stdin; got %q", out)
	}
}

// TestReconnectReplaysSnapshot is the spec's third acceptance criterion: a
// client that connects after output was produced sees that output replayed
// (snapshot + scrollback) on connect, so a refreshed page restores its screen.
func TestReconnectReplaysSnapshot(t *testing.T) {
	// Echo a marker, then stay alive so a late joiner can attach and replay it.
	_, s := newSession(t, "bash", "-c", "echo RESTORE_ME; sleep 5")
	t.Cleanup(func() { _ = s.Resize(ptyhost.Size{Cols: 80, Rows: 24}) })

	// Drive the session directly until the marker is on the screen, so the
	// snapshot the next client gets is guaranteed to contain it.
	first, _, _ := s.Attach()
	deadline := time.After(3 * time.Second)
seen:
	for {
		select {
		case chunk := <-first.Out:
			if strings.Contains(string(chunk), "RESTORE_ME") {
				break seen
			}
		case <-deadline:
			t.Fatal("marker never produced on screen")
		}
	}
	s.Detach(first)

	// Now a fresh WS client connects: the replay (scrollback + snapshot) must
	// carry the pre-connect marker even though it was produced before this
	// client existed.
	srv := newServeServer(t, s)
	c := dial(t, srv)
	if out, ok := readUntil(t, c, "RESTORE_ME", 3*time.Second); !ok {
		t.Fatalf("reconnecting client did not get replayed snapshot; got %q", out)
	}
}

// TestReconnectedMessageHasSize asserts the first frame is reconnected{cols,rows}
// reflecting the live grid, so the client can size xterm.js before the replay.
func TestReconnectedMessageHasSize(t *testing.T) {
	_, s := newSession(t, "bash", "-c", "sleep 5")
	t.Cleanup(func() { _ = s.Resize(ptyhost.Size{Cols: 80, Rows: 24}) })
	srv := newServeServer(t, s)
	c := dial(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	var msg ServerMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal first frame: %v", err)
	}
	if msg.Type != "reconnected" {
		t.Fatalf("first frame type = %q, want reconnected", msg.Type)
	}
	if msg.Cols != 80 || msg.Rows != 24 {
		t.Fatalf("reconnected size = %dx%d, want 80x24", msg.Cols, msg.Rows)
	}
}

// ── unit: single-writer generation policy (table-driven) ─────────────────────

// TestWriterPolicy is the table for the writerSet ownership semantics that back
// the single-writer rule, exercised without any sockets.
func TestWriterPolicy(t *testing.T) {
	tests := []struct {
		name string
		// run drives a sequence of acquires and asserts ownership via the
		// returned tokens.
		run func(t *testing.T, ws *writerSet, sess *session.Session)
	}{
		{
			name: "sole writer owns",
			run: func(t *testing.T, ws *writerSet, sess *session.Session) {
				tok := ws.acquire(sess)
				if !ws.owns(sess, tok) {
					t.Fatal("sole connection should own stdin")
				}
			},
		},
		{
			name: "newer connection steals from older",
			run: func(t *testing.T, ws *writerSet, sess *session.Session) {
				old := ws.acquire(sess)
				neu := ws.acquire(sess)
				if ws.owns(sess, old) {
					t.Fatal("older connection must lose ownership when a newer one acquires")
				}
				if !ws.owns(sess, neu) {
					t.Fatal("newest connection must own stdin")
				}
			},
		},
		{
			name: "three connections: only newest owns",
			run: func(t *testing.T, ws *writerSet, sess *session.Session) {
				t1 := ws.acquire(sess)
				t2 := ws.acquire(sess)
				t3 := ws.acquire(sess)
				if ws.owns(sess, t1) || ws.owns(sess, t2) {
					t.Fatal("only the newest of three should own stdin")
				}
				if !ws.owns(sess, t3) {
					t.Fatal("newest of three must own stdin")
				}
			},
		},
		{
			name: "release by active writer frees the slot",
			run: func(t *testing.T, ws *writerSet, sess *session.Session) {
				tok := ws.acquire(sess)
				ws.release(sess, tok)
				// A subsequent acquire still works (counter resets cleanly).
				next := ws.acquire(sess)
				if !ws.owns(sess, next) {
					t.Fatal("acquire after release should own")
				}
			},
		},
		{
			name: "release by superseded writer does not disturb owner",
			run: func(t *testing.T, ws *writerSet, sess *session.Session) {
				old := ws.acquire(sess)
				neu := ws.acquire(sess)
				ws.release(sess, old) // old leaving must not steal ownership back
				if !ws.owns(sess, neu) {
					t.Fatal("releasing a superseded writer must not disturb the current owner")
				}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			// Fresh writerSet per case so they don't share generation state.
			ws := &writerSet{
				gen:  map[*session.Session]uint64{},
				live: map[*session.Session]int{},
			}
			// A non-nil unique key; we never dereference it.
			sess := &session.Session{}
			tt.run(t, ws, sess)
		})
	}
}

// TestPumpInputGatesByOwnership drives pumpInput with a fake conn to prove that
// input from a non-owning (superseded) connection is dropped, and that a client
// resize is IGNORED — the default session's PTY is a fixed grid (ADR-035), so a
// browser viewer never reflows the shared PTY. All without a real socket.
func TestPumpInputGatesByOwnership(t *testing.T) {
	m := session.NewManager()
	s, err := m.Create("gate", ptyhost.Config{
		Argv:        []string{"cat"},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	client, _, _ := s.Attach()
	defer s.Detach(client)

	// This connection acquires, then a second acquire supersedes it: its token
	// is now stale, so its input must be dropped.
	stale := writers.acquire(s)
	_ = writers.acquire(s) // supersede
	defer writers.release(s, stale)

	fake := newFakeConn(
		mustJSON(ClientMsg{Type: "input", Data: "SHOULD_DROP\n"}),
		mustJSON(ClientMsg{Type: "resize", Cols: 100, Rows: 40}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- pumpInput(ctx, fake, s, stale) }()

	// pumpInput returns nil once the fake conn is drained (its Read yields a
	// clean close after the queued frames).
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pumpInput did not finish")
	}

	// The dropped input must not have echoed via cat onto the screen.
	if out, ok := readClient(client, "SHOULD_DROP", 300*time.Millisecond); ok {
		t.Fatalf("stale-owner input reached stdin; observed %q", out)
	}

	// The resize was IGNORED (fixed-grid design, ADR-035): the PTY keeps its spawn
	// size, not the client-requested 100x40.
	if got := s.Size(); got.Cols != 80 || got.Rows != 24 {
		t.Fatalf("client resize should be ignored: size = %dx%d, want 80x24", got.Cols, got.Rows)
	}
}

// ── fakeConn: an in-memory wsConn for unit tests ─────────────────────────────

// fakeConn is a scripted wsConn: Read replays a queue of frames then returns a
// clean WebSocket close; Write/Ping are recorded.
type fakeConn struct {
	mu      sync.Mutex
	in      [][]byte // queued inbound frames, consumed in order
	written [][]byte // outbound frames (for assertions)
	pings   int
	closed  bool
}

func newFakeConn(in ...[]byte) *fakeConn { return &fakeConn{in: in} }

func (f *fakeConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.in) == 0 {
		// Emulate a peer close so pumpInput exits with nil. A value CloseError
		// is what websocket.CloseStatus matches via errors.As.
		return 0, nil, websocket.CloseError{Code: websocket.StatusNormalClosure}
	}
	frame := f.in[0]
	f.in = f.in[1:]
	return websocket.MessageText, frame, nil
}

func (f *fakeConn) Write(ctx context.Context, typ websocket.MessageType, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, append([]byte(nil), p...))
	return nil
}

func (f *fakeConn) Ping(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pings++
	return nil
}

func (f *fakeConn) Close(code websocket.StatusCode, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// readClient drains a session client channel looking for want.
func readClient(c *session.Client, want string, d time.Duration) (string, bool) {
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
