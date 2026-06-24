package devicews

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/butler"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/deviceapi"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

func TestBinHeaderRoundTrip(t *testing.T) {
	h := binHeader{StreamKind: streamDownlinkTTS, Codec: codecOpus, TurnSeq: 7, FrameSeq: 1234, Flags: flagFinal | flagKeyframe}
	payload := []byte("audio-bytes")
	frame := h.encode(payload)
	if len(frame) != binHeaderLen+len(payload) {
		t.Fatalf("frame len = %d, want %d", len(frame), binHeaderLen+len(payload))
	}
	got, gotPayload, ok := decodeBinFrame(frame)
	if !ok {
		t.Fatal("decodeBinFrame !ok on a well-formed frame")
	}
	if got != h {
		t.Errorf("header roundtrip = %+v, want %+v", got, h)
	}
	if string(gotPayload) != string(payload) {
		t.Errorf("payload roundtrip = %q, want %q", gotPayload, payload)
	}
}

func TestDecodeBinFrameTooShort(t *testing.T) {
	if _, _, ok := decodeBinFrame([]byte{0x02, 0x01, 0x00}); ok {
		t.Error("decodeBinFrame returned ok for a frame shorter than the header")
	}
}

func TestCodecMapping(t *testing.T) {
	for _, tc := range []struct {
		b    byte
		name string
	}{{codecOpus, "opus"}, {codecPCM16, "pcm16"}} {
		if got := codecName(tc.b); got != tc.name {
			t.Errorf("codecName(%#x) = %q, want %q", tc.b, got, tc.name)
		}
		if got := codecByte(tc.name); got != tc.b {
			t.Errorf("codecByte(%q) = %#x, want %#x", tc.name, got, tc.b)
		}
	}
	// Unknown formats default to the pcm16 marker (device's default speaker codec).
	if got := codecByte("mp3"); got != codecPCM16 {
		t.Errorf("codecByte(mp3) = %#x, want pcm16 marker %#x", got, codecPCM16)
	}
}

// TestTurnFSM locks the one-turn-at-a-time guard that prevents the downlink
// turn-misattribution race (a second ptt.start mid-reply rewriting the turn id)
// and bounds the uplink buffer (mic frames accepted only while capturing).
func TestTurnFSM(t *testing.T) {
	c := &deviceConn{}

	if !c.tryBeginCapture("u1", 1) {
		t.Fatal("first ptt.start should open a turn from idle")
	}
	if c.tryBeginCapture("u2", 2) {
		t.Error("a second ptt.start while capturing must be refused (BUSY)")
	}
	if !c.captureMatch(1) {
		t.Error("a mic frame for the open turn (turnSeq=1) should match")
	}
	if c.captureMatch(2) {
		t.Error("a mic frame for a different turnSeq must be dropped")
	}

	if !c.tryBeginReply() {
		t.Fatal("ptt.stop should move capturing→busy")
	}
	if c.tryBeginReply() {
		t.Error("a second ptt.stop must be refused")
	}
	if c.captureMatch(1) {
		t.Error("no mic frames may be buffered while busy (turn in flight)")
	}
	if c.tryBeginCapture("u2", 2) {
		t.Error("a ptt.start while busy must be refused (BUSY)")
	}

	c.setIdle()
	if !c.tryBeginCapture("u2", 2) {
		t.Error("after the turn reaches idle, a new ptt.start should open a turn")
	}
	if id, u := c.turn(); id != "u2" || u != 2 {
		t.Errorf("turn() = (%q,%d) after reopen, want (\"u2\",2)", id, u)
	}
}

func TestServeRejectsBadFirstFrame(t *testing.T) {
	srv := New(session.NewManager(), deviceapi.StaticRecognizer{Text: "x"}, deviceapi.SilentSynthesizer{}, butler.NewDeviceSession(session.NewManager(), []string{"cat"}, ""), Options{})
	f := newFakeConn(inFrame{typ: websocket.MessageBinary, data: []byte{0x01, 0x02, 0x03}})

	runServe(srv, f)

	if got := f.firstCtrlType(t); got != "error" {
		t.Fatalf("want an error frame for a non-hello first frame, got t=%q", got)
	}
}

func TestServeRejectsBadAuth(t *testing.T) {
	srv := New(session.NewManager(), deviceapi.StaticRecognizer{Text: "x"}, deviceapi.SilentSynthesizer{}, butler.NewDeviceSession(session.NewManager(), []string{"cat"}, ""), Options{Auth: "s3cret"})
	hello, _ := json.Marshal(map[string]any{"t": "hello", "proto": Proto, "auth": "wrong"})
	f := newFakeConn(inFrame{typ: websocket.MessageText, data: hello})

	runServe(srv, f)

	got := f.findCtrl(t, "error")
	if got["code"] != codeUnauth {
		t.Fatalf("want error code %q, got %v", codeUnauth, got)
	}
}

func TestServeHelloOK(t *testing.T) {
	srv := New(session.NewManager(), deviceapi.StaticRecognizer{Text: "x"}, deviceapi.SilentSynthesizer{}, butler.NewDeviceSession(session.NewManager(), []string{"cat"}, ""), Options{})
	hello, _ := json.Marshal(map[string]any{"t": "hello", "proto": Proto, "dev": "unit"})
	f := newFakeConn(inFrame{typ: websocket.MessageText, data: hello})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.serve(ctx, f); close(done) }()

	// After the hello, the read loop blocks; we only need to see hello.ok.
	if got := f.waitForCtrl(t, "hello.ok", 3*time.Second); got["sessionId"] != "unit" {
		t.Errorf("hello.ok sessionId = %v, want \"unit\"", got["sessionId"])
	}
	cancel()
	f.close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
}

// promptMenuCLI renders a claude-style permission menu then idles (cat), so the
// forwarded-prompt round-trip can be exercised end-to-end over the device WS.
// "❯" is U+276F (e2 9d af). Yes/No labels make ParsePrompt classify it permission.
const promptMenuCLI = "printf 'Do you want to proceed?\\r\\n'; " +
	"printf '\\xe2\\x9d\\xaf 1. Yes\\r\\n'; " +
	"printf '  2. No\\r\\n'; " +
	"printf 'Esc to cancel\\r\\n'; cat >/dev/null"

// TestServePromptForwardAndSelect (ADR-033 P1): a blocking menu on the shared
// session is forwarded as prompt.open; the device answers with prompt.select and
// the Bridge confirms it with prompt.close{answered}.
func TestServePromptForwardAndSelect(t *testing.T) {
	t.Setenv("ADAPTER_V2_CONFIRM_ON_DEVICE", "1") // enable forward-to-device
	mgr := session.NewManager()
	dev := butler.NewDeviceSession(mgr, []string{"bash", "-c", promptMenuCLI}, "")
	srv := New(mgr, deviceapi.StaticRecognizer{Text: "x"}, deviceapi.SilentSynthesizer{}, dev, Options{Cols: 80, Rows: 24})
	hello, _ := json.Marshal(map[string]any{"t": "hello", "proto": Proto, "dev": "unit"})
	f := newFakeConn(inFrame{typ: websocket.MessageText, data: hello})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { srv.serve(ctx, f); close(done) }()

	open := f.waitForCtrl(t, "prompt.open", 5*time.Second)
	pid, _ := open["promptId"].(string)
	if pid == "" || open["question"] != "Do you want to proceed?" {
		t.Fatalf("bad prompt.open frame: %v", open)
	}
	if opts, _ := open["options"].([]any); len(opts) != 2 {
		t.Fatalf("prompt.open options = %v, want 2", open["options"])
	}

	// Device confirms option 1; the Bridge validates the id+key, injects the digit,
	// and reports prompt.close{answered}.
	sel, _ := json.Marshal(map[string]any{"t": "prompt.select", "promptId": pid, "optionKey": "1"})
	f.push(inFrame{typ: websocket.MessageText, data: sel})

	closed := f.waitForCtrl(t, "prompt.close", 3*time.Second)
	if closed["reason"] != "answered" {
		t.Errorf("prompt.close reason = %v, want answered", closed["reason"])
	}

	cancel()
	f.close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
}

// ── fakeConn: an in-memory wsConn for driving serve() without a network ──────

type inFrame struct {
	typ  websocket.MessageType
	data []byte
}

type fakeConn struct {
	mu     sync.Mutex
	in     []inFrame
	idx    int
	writes [][]byte // captured TEXT frame payloads (control); binary ignored here
	closed chan struct{}
	more   chan struct{} // signals a push() so a blocked Read re-checks `in`
}

func newFakeConn(in ...inFrame) *fakeConn {
	return &fakeConn{in: in, closed: make(chan struct{}), more: make(chan struct{}, 8)}
}

// push appends an inbound frame at runtime and wakes a blocked Read — lets a test
// send a frame only AFTER observing a write (e.g. prompt.select after prompt.open).
func (f *fakeConn) push(fr inFrame) {
	f.mu.Lock()
	f.in = append(f.in, fr)
	f.mu.Unlock()
	select {
	case f.more <- struct{}{}:
	default:
	}
}

func (f *fakeConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	for {
		f.mu.Lock()
		if f.idx < len(f.in) {
			fr := f.in[f.idx]
			f.idx++
			f.mu.Unlock()
			return fr.typ, fr.data, nil
		}
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-f.closed:
			return 0, nil, io.EOF
		case <-f.more:
		}
	}
}

func (f *fakeConn) Write(_ context.Context, typ websocket.MessageType, p []byte) error {
	if typ == websocket.MessageText {
		f.mu.Lock()
		f.writes = append(f.writes, append([]byte(nil), p...))
		f.mu.Unlock()
	}
	return nil
}

func (f *fakeConn) Close(websocket.StatusCode, string) error {
	f.close()
	return nil
}

func (f *fakeConn) close() {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
}

// runServe runs serve to completion against an already-terminating fake (the
// fake's input is exhausted and serve will block then we close it). Used for the
// rejection tests where serve returns on its own after writing the error+close.
func runServe(srv *Server, f *fakeConn) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { srv.serve(ctx, f); close(done) }()
	// Rejection paths Close() themselves; for safety also close after a beat.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		f.close()
		<-done
	}
}

func (f *fakeConn) ctrls(t *testing.T) []map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, 0, len(f.writes))
	for _, w := range f.writes {
		var m map[string]any
		if json.Unmarshal(w, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeConn) firstCtrlType(t *testing.T) string {
	t.Helper()
	c := f.ctrls(t)
	if len(c) == 0 {
		t.Fatal("no control frame was written")
	}
	s, _ := c[0]["t"].(string)
	return s
}

func (f *fakeConn) findCtrl(t *testing.T, typ string) map[string]any {
	t.Helper()
	for _, m := range f.ctrls(t) {
		if m["t"] == typ {
			return m
		}
	}
	t.Fatalf("no %q control frame written; got %v", typ, f.ctrls(t))
	return nil
}

func (f *fakeConn) waitForCtrl(t *testing.T, typ string, d time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, m := range f.ctrls(t) {
			if m["t"] == typ {
				return m
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("control frame %q not written within %s; got %v", typ, d, f.ctrls(t))
	return nil
}
