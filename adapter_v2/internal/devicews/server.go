package devicews

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"nhooyr.io/websocket"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/butler"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/deviceapi"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

// maxFrame bounds a single inbound WS frame (mic audio frames can be a few KB).
const maxFrame = 1 << 20

// Canonical ASR audio spec. Uplink is normalised to this (16 kHz mono PCM16)
// before recognition, matching the fixed metadata the voicekit recognizer reports
// — so a non-16 kHz mic (e.g. 48 kHz opus) is resampled, not mislabelled.
const (
	asrRate  = 16000
	asrChans = 1
)

// anonSeq names sessions for hello frames that omit a device id.
var anonSeq atomic.Uint64

// wsConn is the slice of *websocket.Conn devicews uses; an interface so tests can
// drive serve() with an in-memory fake. *websocket.Conn satisfies it.
type wsConn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Close(code websocket.StatusCode, reason string) error
}

// Server serves the bbwire/2 device protocol (Phase A) at one HTTP route. One
// WebSocket connection = one device = one PTY session driven via a deviceapi.Bridge.
type Server struct {
	mgr         *session.Manager
	asr         deviceapi.Recognizer // batch ASR over the buffered PTT utterance
	tts         deviceapi.Synthesizer
	dev         *butler.DeviceSession // default conversation's spawn config (ADR-032)
	auth        string                // shared secret; "" disables auth (dev/LAN)
	cols        int
	rows        int
	decode      func(ctx context.Context, codec string, rate, ch int, payload []byte) ([]byte, error)
	streamDelta bool
	segmentTTS  bool
}

// Options carries the optional knobs for New.
type Options struct {
	Auth string // shared secret expected in hello.auth; "" disables the check
	Cols int    // PTY grid; defaults to 80
	Rows int    // PTY grid; defaults to 24
	// Decode, if set, converts a PTT utterance's mic audio to PCM16 before ASR
	// (codec is the device's declared mic codec). nil means pcm16-passthrough
	// only — an opus device then gets an ASR_TIMEOUT error. Wired to
	// voicekit.DecodeUplink (which shells to ffmpeg for opus).
	Decode func(ctx context.Context, codec string, rate, ch int, payload []byte) ([]byte, error)

	// StreamReplyDelta (Phase B) streams live reply.delta frames as the reply
	// grows. Safe to default on (reply.end stays authoritative).
	StreamReplyDelta bool
	// SegmentTTS (Phase B) speaks the reply sentence-by-sentence instead of once at
	// turn end. Opt-in (higher risk). Off → one-shot TTS (Phase A behaviour).
	SegmentTTS bool
}

// New builds a device-WS server. asr/tts are the voice providers (a mock ASR and
// local TTS suffice for Phase A); dev supplies the default conversation's spawn
// config (the LAN device line drives the same default session as the cloud relay).
func New(mgr *session.Manager, asr deviceapi.Recognizer, tts deviceapi.Synthesizer, dev *butler.DeviceSession, opt Options) *Server {
	if opt.Cols <= 0 {
		opt.Cols = 80
	}
	if opt.Rows <= 0 {
		opt.Rows = 24
	}
	return &Server{mgr: mgr, asr: asr, tts: tts, dev: dev, auth: opt.Auth, cols: opt.Cols, rows: opt.Rows, decode: opt.Decode, streamDelta: opt.StreamReplyDelta, segmentTTS: opt.SegmentTTS}
}

// Handler upgrades GET /v2/dev/ws and runs one device session on it.
func (s *Server) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return // Accept wrote the HTTP error
		}
		conn.SetReadLimit(maxFrame)
		s.serve(r.Context(), conn)
	}
}

// deviceConn holds the per-connection state and is BOTH the deviceapi.DeviceSink
// (downlink TTS → binary frames) AND the deviceapi.Events observer (turn lifecycle
// → control frames). All writes go through writeMu since the read loop and
// Bridge.Run write concurrently and a WS conn is not safe for concurrent writes.
// Per-connection turn FSM. Phase A serves exactly ONE turn at a time: a new
// ptt.start is refused (error BUSY) until the prior turn reaches idle. This is
// what keeps the turn identity (curTurnID/curU) stable for the whole reply, so
// Bridge.Run's ReplyComplete/Play/TurnIdle can't stamp an in-flight turn's frames
// with a newer turn's id — and it bounds the uplink buffer to one utterance.
// Barge-in (overlapping turns) is a later phase.
const (
	connIdle      = iota // ready for ptt.start
	connCapturing        // ptt.start seen; buffering uplink mic frames
	connBusy             // ptt.stop seen; reply in flight (ASR→inject→reply→TTS)
)

type deviceConn struct {
	ctx  context.Context
	conn wsConn
	spk  audioSpec
	mic  audioSpec // device's declared uplink mic spec (codec/rate/ch), for decode + ASR

	writeMu sync.Mutex

	mu        sync.Mutex
	fsm       int
	curTurnID string
	curU      uint16
	dnSeq     uint16 // downlink TTS frame counter, reset per turn
	deltaSeq  uint16 // reply.delta sequence counter, reset per turn
	toolSeq   uint16 // tool.step sequence counter, reset per turn (independent of deltaSeq)
}

// tryBeginCapture opens a new turn iff the connection is idle, recording its
// identity. Returns false (caller should reply BUSY) if a turn is already open.
func (c *deviceConn) tryBeginCapture(turnID string, u uint16) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fsm != connIdle {
		return false
	}
	c.fsm = connCapturing
	c.curTurnID, c.curU, c.dnSeq, c.deltaSeq, c.toolSeq = turnID, u, 0, 0, 0
	return true
}

// captureMatch reports whether a mic frame belongs to the open turn — true only
// while capturing and the frame's turnSeq matches. Stray frames (no open turn, or
// a mismatched/zero turnSeq) are dropped, so the uplink buffer can't grow outside
// a turn.
func (c *deviceConn) captureMatch(turnSeq uint16) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fsm == connCapturing && turnSeq == c.curU
}

// tryBeginReply transitions capturing → busy on ptt.stop. Returns false for a
// stray ptt.stop with no turn open.
func (c *deviceConn) tryBeginReply() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fsm != connCapturing {
		return false
	}
	c.fsm = connBusy
	return true
}

// setIdle returns the connection to idle, re-enabling ptt.start.
func (c *deviceConn) setIdle() {
	c.mu.Lock()
	c.fsm = connIdle
	c.mu.Unlock()
}

func (c *deviceConn) turn() (string, uint16) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curTurnID, c.curU
}

func (c *deviceConn) writeCtrl(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Write(c.ctx, websocket.MessageText, b)
}

func (c *deviceConn) writeBin(b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Write(c.ctx, websocket.MessageBinary, b)
}

// Play implements deviceapi.DeviceSink: send one synthesized audio unit as a
// BINARY downlink frame, marked final. "Final" means end-of-playable-unit, not
// end-of-turn (see flagFinal): under one-shot TTS the unit is the whole reply;
// under per-segment TTS (Phase B) the Bridge calls Play once per sentence, so each
// sentence is its own final frame the device plays on arrival. Turn end is the
// separate turn{idle} control frame.
func (c *deviceConn) Play(_ context.Context, audio []byte, format string) error {
	c.mu.Lock()
	u, seq := c.curU, c.dnSeq
	c.dnSeq++
	c.mu.Unlock()
	h := binHeader{
		StreamKind: streamDownlinkTTS,
		Codec:      codecByte(format),
		TurnSeq:    u,
		FrameSeq:   seq,
		Flags:      flagFinal,
	}
	return c.writeBin(h.encode(audio))
}

// ReplyDelta implements deviceapi.Events: a live snapshot of the growing reply →
// reply.delta. text is the full current reply (the device replaces its subtitle),
// each tagged with an incrementing per-turn seq.
func (c *deviceConn) ReplyDelta(text string) {
	c.mu.Lock()
	turnID, seq := c.curTurnID, c.deltaSeq
	c.deltaSeq++
	c.mu.Unlock()
	_ = c.writeCtrl(replyDelta{T: "reply.delta", TurnID: turnID, Seq: seq, Text: text})
}

// ReplyComplete implements deviceapi.Events: the turn's final text → reply.end.
func (c *deviceConn) ReplyComplete(text string) {
	turnID, _ := c.turn()
	_ = c.writeCtrl(replyEnd{T: "reply.end", TurnID: turnID, Text: text})
}

// ToolStep implements deviceapi.Events: a display-only tool-progress frame
// (tool.step). Shown as a dimmed chip on the device; never spoken.
func (c *deviceConn) ToolStep(name, hint string) {
	c.mu.Lock()
	turnID, seq := c.curTurnID, c.toolSeq
	c.toolSeq++
	c.mu.Unlock()
	_ = c.writeCtrl(toolStep{T: "tool.step", TurnID: turnID, Seq: seq, Name: name, Hint: hint})
}

// TurnIdle implements deviceapi.Events: the turn is fully spoken → turn{idle},
// and the connection returns to idle so the next ptt.start is accepted.
func (c *deviceConn) TurnIdle() {
	turnID, _ := c.turn()
	_ = c.writeCtrl(turnState{T: "turn", TurnID: turnID, State: stateIdle})
	c.setIdle()
}

// serve runs one device connection: hello handshake, then the PTT loop, until the
// socket drops. The session/PTY outlives the connection (reaped by the session GC
// once detached); Phase A has no resume, so a reconnect with the same device id
// simply rejoins the live session.
func (s *Server) serve(parent context.Context, conn wsConn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	dc := &deviceConn{ctx: ctx, conn: conn, spk: defaultSpk(), mic: defaultMic()}

	// 1. hello handshake (first frame, TEXT).
	typ, data, err := conn.Read(ctx)
	if err != nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return
	}
	var hello ctrlIn
	if typ != websocket.MessageText || json.Unmarshal(data, &hello) != nil || hello.T != "hello" {
		_ = dc.writeCtrl(errFrame{T: "error", Code: codeBadProto, Detail: "expected hello"})
		conn.Close(websocket.StatusProtocolError, "bad hello")
		return
	}
	if hello.Proto != Proto {
		// The capability handshake hinges on a known proto; reject a mismatch
		// rather than silently treating a v1/v3 device as v2.
		_ = dc.writeCtrl(errFrame{T: "error", Code: codeBadProto, Detail: "unsupported proto"})
		conn.Close(websocket.StatusProtocolError, "proto")
		return
	}
	if s.auth != "" && hello.Auth != s.auth {
		_ = dc.writeCtrl(errFrame{T: "error", Code: codeUnauth})
		conn.Close(websocket.StatusPolicyViolation, "unauthorized")
		return
	}
	if hello.Spk != nil && hello.Spk.Codec != "" {
		dc.spk = *hello.Spk
	}
	if hello.Mic != nil && hello.Mic.Codec != "" {
		dc.mic = *hello.Mic
	}

	deviceID := strings.TrimSpace(hello.Dev)
	if deviceID == "" {
		deviceID = "dev-anon-" + strconv.FormatUint(anonSeq.Add(1), 10)
	}

	// 2. session + bridge (the bridge's sink AND events are this conn). The LAN
	// device drives the shared DEFAULT session (the one the web terminal joins and
	// the cloud relay also targets), not a per-device session — so device and web
	// share one PTY. deviceID is kept for logging/echo only. (P1: single default
	// session; per-device sessions are a later phase.)
	pcfg := s.dev.Config()
	pcfg.InitialSize = ptyhost.Size{Cols: uint16(s.cols), Rows: uint16(s.rows)}
	sess, err := s.mgr.GetOrCreate(session.DefaultID, pcfg)
	if err != nil {
		_ = dc.writeCtrl(errFrame{T: "error", Code: codeBadProto, Detail: "session create failed"})
		conn.Close(websocket.StatusInternalError, "session")
		return
	}
	// asr is nil on the Bridge: the handler runs ASR itself (so it can emit
	// asr.final between recognition and injection); the Bridge only needs tts+sink.
	bridge := deviceapi.New(sess, nil, s.tts, dc, deviceapi.Config{
		Cols:             s.cols,
		Rows:             s.rows,
		StreamReplyDelta: s.streamDelta,
		SegmentTTS:       s.segmentTTS,
		Warmup:           true,
	})
	bridge.SetEvents(dc)
	go bridge.Run(ctx)

	// 3. hello.ok.
	_ = dc.writeCtrl(helloOK{T: "hello.ok", Proto: Proto, SessionID: deviceID, Spk: dc.spk, Resumed: false})

	// 4. PTT loop.
	var uplink []byte
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		switch typ {
		case websocket.MessageBinary:
			h, payload, ok := decodeBinFrame(data)
			if !ok || h.StreamKind != streamUplinkMic {
				continue
			}
			if dc.captureMatch(h.TurnSeq) {
				uplink = append(uplink, payload...)
			}
		case websocket.MessageText:
			var m ctrlIn
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.T {
			case "ptt.start":
				if !dc.tryBeginCapture(m.TurnID, m.U) {
					// A turn is already in flight: Phase A serves one at a time.
					_ = dc.writeCtrl(errFrame{T: "error", TurnID: m.TurnID, Code: codeBusy})
					continue
				}
				uplink = uplink[:0]
			case "ptt.stop":
				if !dc.tryBeginReply() {
					continue // stray stop with no turn open
				}
				if !s.finishTurn(ctx, dc, bridge, m.TurnID, uplink) {
					// Nothing was injected (empty/failed ASR): hand control back.
					_ = dc.writeCtrl(turnState{T: "turn", TurnID: m.TurnID, State: stateIdle})
					dc.setIdle()
				}
				uplink = uplink[:0]
			case "ping", "ack":
				// Phase A: WS-layer keepalive covers ping; ack is Phase C.
			}
		}
	}

	cancel()
	conn.Close(websocket.StatusNormalClosure, "")
}

// finishTurn transcribes the buffered utterance, emits asr.final + turn{thinking},
// and injects the transcript into the PTY. Returns true iff a turn was actually
// injected (so the caller leaves the connection BUSY until the Bridge fires
// TurnIdle); false on empty/failed ASR or a failed inject, where the caller hands
// control back to idle. The reply.end / TTS audio / turn{idle} frames of a
// successful turn are emitted later by the Bridge through dc's Events/Sink.
func (s *Server) finishTurn(ctx context.Context, dc *deviceConn, bridge *deviceapi.Bridge, turnID string, audio []byte) bool {
	var text string
	if s.asr != nil && len(audio) > 0 {
		// Normalise mic audio to the canonical ASR rate (PCM16 16 kHz mono) before
		// ASR — matching v1's normalize-before-ASR and the fixed spec the voicekit
		// recognizer reports. Opus is decoded+resampled to asrRate/asrChans by the
		// configured decoder (ffmpeg); PCM16 passes through (the device contract is
		// 16 kHz mono, so no resample is needed for it). The target is canonical, NOT
		// the device's declared mic rate, so a 48 kHz opus mic still reaches ASR as
		// 16 kHz — otherwise the recognizer's fixed 16 kHz metadata would lie.
		pcm := audio
		mic := strings.ToLower(strings.TrimSpace(dc.mic.Codec))
		if mic != "" && mic != "pcm16" && mic != "pcm_s16le" {
			if s.decode == nil {
				_ = dc.writeCtrl(errFrame{T: "error", TurnID: turnID, Code: codeBadAudio, Detail: "no decoder for codec " + mic})
				return false
			}
			d, err := s.decode(ctx, mic, asrRate, asrChans, audio)
			if err != nil {
				_ = dc.writeCtrl(errFrame{T: "error", TurnID: turnID, Code: codeBadAudio, Detail: "decode: " + err.Error()})
				return false
			}
			pcm = d
		}
		t, err := s.asr.Transcribe(ctx, pcm)
		if err != nil {
			_ = dc.writeCtrl(errFrame{T: "error", TurnID: turnID, Code: codeASRTimeout, Detail: err.Error()})
			return false
		}
		text = t
	}
	if strings.TrimSpace(text) == "" {
		// Nothing recognised: do not poke the CLI.
		_ = dc.writeCtrl(errFrame{T: "error", TurnID: turnID, Code: codeASREmpty})
		return false
	}
	_ = dc.writeCtrl(asrFinal{T: "asr.final", TurnID: turnID, Text: text})
	_ = dc.writeCtrl(turnState{T: "turn", TurnID: turnID, State: stateThinking})
	if err := bridge.SubmitVoiceTurn(text); err != nil {
		_ = dc.writeCtrl(errFrame{T: "error", TurnID: turnID, Code: codeTTSFailed, Detail: err.Error()})
		return false
	}
	return true
}

// defaultSpk is the assumed device speaker stream when hello omits it.
func defaultSpk() audioSpec { return audioSpec{Codec: "pcm16", Rate: 16000, Ch: 1} }

// defaultMic is the assumed device mic stream when hello omits it (PCM16 16k mono,
// so the no-codec path needs no decoder).
func defaultMic() audioSpec { return audioSpec{Codec: "pcm16", Rate: 16000, Ch: 1} }
