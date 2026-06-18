// Package termchan is channel ① — the raw-byte terminal transport. It bridges a
// WebSocket (phone app / web client running xterm.js) to a session.Session:
// client keystrokes → PTY stdin, PTY output → client, verbatim. No parsing.
//
// On (re)connect it replays the session Snapshot so a refreshed page or a phone
// joining mid-session sees the exact current screen. Mirrors dinotty
// src/ws.rs handle_socket (the "join existing session" path).
//
// # Single-writer input policy
//
// Several clients may watch the SAME session at once (a phone and a laptop
// observing the voice device, say). They ALL receive output, but only one may
// type — otherwise two keyboards would interleave bytes into one stdin. We adopt
// dinotty's rule (replace_input_channel): the LATEST connection owns stdin;
// every earlier connection is silently demoted to a read-only observer. A new
// connection therefore "steals" the keyboard from whoever held it. Ownership is
// tracked by a per-session generation counter (see writerSet): a connection may
// write only while it still holds the session's current generation.
package termchan

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"nhooyr.io/websocket"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

// pingInterval is the WebSocket keepalive period. Idle terminals (a session
// waiting on the user) must not be torn down by NAT/proxy idle timeouts, so we
// ping every 30s — matching the dinotty reference.
const pingInterval = 30 * time.Second

// ClientMsg is what a terminal client sends over the wire.
type ClientMsg struct {
	Type string `json:"type"`           // "input" | "resize"
	Data string `json:"data,omitempty"` // input: raw bytes (utf-8)
	Cols uint16 `json:"cols,omitempty"` // resize
	Rows uint16 `json:"rows,omitempty"` // resize
}

// ServerMsg is what the server pushes to a terminal client.
type ServerMsg struct {
	Type string `json:"type"`           // "output" | "reconnected"
	Data string `json:"data,omitempty"` // output bytes
	Cols uint16 `json:"cols,omitempty"` // reconnected
	Rows uint16 `json:"rows,omitempty"` // reconnected
}

// wsConn is the slice of *websocket.Conn that termchan uses. Declaring it as an
// interface lets tests drive Serve with an in-memory fake instead of a real
// network socket. *websocket.Conn satisfies it.
type wsConn interface {
	// Read returns the next message; the MessageType is ignored (we only care
	// about the JSON payload).
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	// Write sends one message of the given type.
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	// Ping sends a WebSocket ping and waits for the pong (keepalive).
	Ping(ctx context.Context) error
	// Close closes the connection with the given status/reason.
	Close(code websocket.StatusCode, reason string) error
}

// writerSet enforces the single-writer policy across all connections to one
// Session. It hands each new connection a fresh generation number and records it
// as the current owner; a connection may write to stdin only while its
// generation still equals the current one. This is the Go analogue of dinotty's
// replace_input_channel — the newest connection wins, older ones go read-only.
//
// Generations are STRICTLY MONOTONIC for the whole time any connection to a
// session is alive: the counter is never rewound while a connection remains,
// even if connections leave out of LIFO order. This matters for two reasons that
// a phone+laptop on one session hit in practice:
//
//   - If the current owner disconnects first while an older connection is still
//     attached, the counter must NOT be reset — otherwise the surviving older
//     connection's token would no longer match (it could never write again).
//   - A reset would also let a brand-new connection be handed a token a still-live
//     connection already holds, making owns() true for BOTH → two writers to one
//     stdin, the exact byte interleaving this set exists to prevent.
//
// So we track a per-session live-connection refcount and only drop the entry
// (allowing the counter to start fresh) once the LAST connection has released.
type writerSet struct {
	mu   sync.Mutex
	gen  map[*session.Session]uint64
	live map[*session.Session]int
}

var writers = &writerSet{
	gen:  map[*session.Session]uint64{},
	live: map[*session.Session]int{},
}

// acquire registers conn as the newest writer for sess and returns its
// generation token. owns(token) is true until another connection acquires. It
// also bumps the live-connection refcount so the generation counter survives
// until the last connection releases.
func (w *writerSet) acquire(sess *session.Session) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	g := w.gen[sess] + 1
	w.gen[sess] = g
	w.live[sess]++
	return g
}

// owns reports whether token is still the current writer generation for sess —
// i.e. no newer connection has stolen the keyboard.
func (w *writerSet) owns(sess *session.Session, token uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gen[sess] == token
}

// release records that one connection to sess has left. The generation counter
// is NOT rewound while other connections remain (see writerSet doc): a departing
// owner must not strand a still-live older connection, and the counter must never
// hand a live token to a future connection. Only when the refcount reaches zero —
// no connection to this session is left — do we delete both entries, so a session
// with no observers does not leak map entries and a later first connection starts
// cleanly from generation 1.
func (w *writerSet) release(sess *session.Session, token uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.live[sess] <= 1 {
		delete(w.live, sess)
		delete(w.gen, sess)
		return
	}
	w.live[sess]--
}

// Serve attaches a terminal client (the WebSocket conn) to sess and runs until
// the connection drops or the PTY exits. It:
//
//  1. Attaches to the session (registering for live output + grabbing a
//     consistent screen snapshot under the session lock).
//  2. Sends reconnected{cols,rows} so the client sizes its xterm.js.
//  3. Replays scrollback chunks, then the visible-screen snapshot, so the
//     client redraws to the exact pre-(re)connect state.
//  4. Becomes the session's sole stdin writer (older connections go read-only).
//  5. Pumps PTY output → WS and WS input/resize → PTY concurrently, with a 30s
//     ping keepalive, until either side closes.
//
// On return the client is detached and (if it still held the keyboard) the
// writer slot is released. Serve owns the conn's lifecycle and closes it.
func Serve(sess *session.Session, conn *websocket.Conn) error {
	return serve(sess, conn)
}

// serve is the wsConn-typed core of Serve, so tests can substitute a fake conn.
func serve(sess *session.Session, conn wsConn) error {
	// A connection-scoped context cancels every loop the moment one of them
	// exits (the reader sees a close, the PTY hits EOF, a write fails …), so the
	// other loops unblock and the goroutine set winds down cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Attach: register for live output and capture the screen atomically.
	client, scrollback, snapshot := sess.Attach()
	defer sess.Detach(client)

	// 4 (claim early, before any input can arrive): newest connection owns stdin.
	token := writers.acquire(sess)
	defer writers.release(sess, token)

	// 2. reconnected{cols,rows}: tell the client the live grid size up front.
	size := sess.Size()
	if err := writeServerMsg(ctx, conn, ServerMsg{
		Type: "reconnected",
		Cols: size.Cols,
		Rows: size.Rows,
	}); err != nil {
		conn.Close(websocket.StatusInternalError, "reconnected")
		return err
	}

	// 3. Replay history then the visible screen, both as "output" frames, so the
	//    client's terminal ends up byte-identical to the server's screen.
	for _, chunk := range scrollback {
		if err := writeOutput(ctx, conn, chunk); err != nil {
			conn.Close(websocket.StatusInternalError, "scrollback")
			return err
		}
	}
	if len(snapshot) > 0 {
		if err := writeOutput(ctx, conn, snapshot); err != nil {
			conn.Close(websocket.StatusInternalError, "snapshot")
			return err
		}
	}

	// 5. Run the three loops; the first to error cancels the rest via ctx.
	var (
		wg      sync.WaitGroup
		once    sync.Once
		loopErr error
	)
	fail := func(err error) {
		// Record the first non-nil, non-cancellation error and tear down.
		if err == nil || errors.Is(err, context.Canceled) {
			cancel()
			return
		}
		once.Do(func() { loopErr = err })
		cancel()
	}

	wg.Add(3)
	go func() { defer wg.Done(); fail(pumpOutput(ctx, conn, client)) }()     // PTY out → WS
	go func() { defer wg.Done(); fail(pumpInput(ctx, conn, sess, token)) }() // WS in → PTY
	go func() { defer wg.Done(); keepAlive(ctx, conn) }()                    // 30s ping

	wg.Wait()
	conn.Close(websocket.StatusNormalClosure, "")
	return loopErr
}

// pumpOutput forwards live PTY output from the session's client channel to the
// WebSocket as "output" frames. It ends when the channel closes (PTY exited),
// the context is cancelled, or a WS write fails.
func pumpOutput(ctx context.Context, conn wsConn, client *session.Client) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunk, ok := <-client.Out:
			if !ok {
				return nil // PTY exited: clean end of stream.
			}
			if err := writeOutput(ctx, conn, chunk); err != nil {
				return err
			}
		}
	}
}

// pumpInput reads client messages and, for the connection that currently owns
// the keyboard, forwards input to PTY stdin and resize to the PTY+screen. A
// connection that has been superseded (older generation) still drains and parses
// messages — so its socket stays healthy and it keeps observing output — but its
// input is dropped on the floor. Resize is honoured regardless of write
// ownership: any viewer reflowing its own terminal should not desync the screen.
func pumpInput(ctx context.Context, conn wsConn, sess *session.Session, token uint64) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			// A normal client close is not an error to propagate.
			if isCloseOrCanceled(err) {
				return nil
			}
			return err
		}

		var msg ClientMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			// Ignore malformed frames rather than killing the session.
			continue
		}

		switch msg.Type {
		case "input":
			// Single-writer gate: only the latest connection writes stdin.
			if !writers.owns(sess, token) {
				continue
			}
			if err := sess.Write([]byte(msg.Data)); err != nil {
				// Session closed under us; stop this connection.
				if errors.Is(err, session.ErrClosed) {
					return nil
				}
				return err
			}
		case "resize":
			_ = sess.Resize(ptyhost.Size{Cols: msg.Cols, Rows: msg.Rows})
		default:
			// Unknown type: ignore for forward compatibility.
		}
	}
}

// keepAlive pings the connection every pingInterval so idle terminals survive
// NAT/proxy idle timeouts. A failed ping cancels the connection's loops.
func keepAlive(ctx context.Context, conn wsConn) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := conn.Ping(ctx); err != nil {
				return
			}
		}
	}
}

// writeOutput marshals one raw byte chunk as an "output" ServerMsg and writes it.
func writeOutput(ctx context.Context, conn wsConn, chunk []byte) error {
	return writeServerMsg(ctx, conn, ServerMsg{Type: "output", Data: string(chunk)})
}

// writeServerMsg JSON-encodes msg and writes it as a single text frame.
func writeServerMsg(ctx context.Context, conn wsConn, msg ServerMsg) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

// isCloseOrCanceled reports whether err is an ordinary end-of-connection signal
// (peer closed the WebSocket, or our own context was cancelled) rather than a
// real transport fault worth surfacing.
func isCloseOrCanceled(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	// A clean WS close yields a CloseStatus; -1 means "not a close frame".
	return websocket.CloseStatus(err) != -1
}
