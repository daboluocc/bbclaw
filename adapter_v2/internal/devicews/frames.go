// Package devicews implements the bbwire/2 device protocol (Phase A): one
// client-dialed WebSocket per device, where the WS frame opcode is the type
// discriminator — TEXT frames carry JSON control objects, BINARY frames carry an
// 8-byte header + raw codec audio (no base64). See adapter_v2/docs/device-protocol.md.
//
// Phase A scope: the required path only — hello handshake, PTT uplink → batch
// ASR → inject into the PTY (deviceapi.Bridge), reply.end-only screen text
// (no streaming reply.delta), and one-shot TTS streamed back as BINARY frames.
// Streaming deltas, resume/ack, and the Cloud relay are later phases.
package devicews

import "encoding/binary"

// Proto is the bbwire protocol version announced in the hello handshake.
const Proto = 2

// streamKind — binary header byte 0.
const (
	streamUplinkMic   = 0x01 // device → adapter: mic audio
	streamDownlinkTTS = 0x02 // adapter → device: synthesized reply audio
)

// codec — binary header byte 1.
const (
	codecOpus  = 0x01
	codecPCM16 = 0x02
)

// flags — binary header bytes 6-7 (bitfield).
const (
	flagFinal    = 1 << 0 // last frame of this turn's stream
	flagKeyframe = 1 << 1 // opus config / keyframe present
)

// binHeaderLen is the fixed little-endian header on every BINARY frame.
const binHeaderLen = 8

// binHeader is the 8-byte prefix of a BINARY audio frame.
type binHeader struct {
	StreamKind byte
	Codec      byte
	TurnSeq    uint16 // mirrors the JSON turn's `u`
	FrameSeq   uint16 // per-turn monotonic; gap/reorder/resume
	Flags      uint16
}

// encode prepends the header to payload, returning one BINARY frame body.
func (h binHeader) encode(payload []byte) []byte {
	b := make([]byte, binHeaderLen+len(payload))
	b[0] = h.StreamKind
	b[1] = h.Codec
	binary.LittleEndian.PutUint16(b[2:4], h.TurnSeq)
	binary.LittleEndian.PutUint16(b[4:6], h.FrameSeq)
	binary.LittleEndian.PutUint16(b[6:8], h.Flags)
	copy(b[binHeaderLen:], payload)
	return b
}

// decodeBinFrame splits a BINARY frame into its header and raw audio payload.
// ok is false if the frame is too short to hold a header.
func decodeBinFrame(b []byte) (binHeader, []byte, bool) {
	if len(b) < binHeaderLen {
		return binHeader{}, nil, false
	}
	h := binHeader{
		StreamKind: b[0],
		Codec:      b[1],
		TurnSeq:    binary.LittleEndian.Uint16(b[2:4]),
		FrameSeq:   binary.LittleEndian.Uint16(b[4:6]),
		Flags:      binary.LittleEndian.Uint16(b[6:8]),
	}
	return h, b[binHeaderLen:], true
}

// codecName maps a wire codec byte to the audio-package codec string.
func codecName(c byte) string {
	switch c {
	case codecOpus:
		return "opus"
	case codecPCM16:
		return "pcm16"
	default:
		return ""
	}
}

// codecByte maps a codec string (e.g. a Synthesizer's OutputFormat) to the wire
// byte. Anything that is not opus is marked pcm16 (the device's default speaker
// codec); the actual transcode/format negotiation is a later phase.
func codecByte(name string) byte {
	if name == "opus" {
		return codecOpus
	}
	return codecPCM16
}

// ── Control frames (TEXT/JSON) ──────────────────────────────────────────────

// audioSpec describes a mic or speaker stream in the hello handshake.
type audioSpec struct {
	Codec string `json:"codec"`
	Rate  int    `json:"rate"`
	Ch    int    `json:"ch,omitempty"`
}

// ctrlIn is the union of every uplink (device → adapter) control object. Only
// the fields relevant to its `t` are populated; the rest stay zero.
type ctrlIn struct {
	T      string     `json:"t"`
	Proto  int        `json:"proto,omitempty"`
	Dev    string     `json:"dev,omitempty"`
	Auth   string     `json:"auth,omitempty"`
	Mic    *audioSpec `json:"mic,omitempty"`
	Spk    *audioSpec `json:"spk,omitempty"`
	Resume string     `json:"resume,omitempty"`
	TurnID string     `json:"turnId,omitempty"`
	U      uint16     `json:"u,omitempty"`
	Frames int        `json:"frames,omitempty"`
	UpTo   uint16     `json:"upTo,omitempty"`
}

// Downlink (adapter → device) control objects. Each sets its own `t`.

type helloOK struct {
	T         string    `json:"t"`
	Proto     int       `json:"proto"`
	SessionID string    `json:"sessionId"`
	Spk       audioSpec `json:"spk"`
	Resumed   bool      `json:"resumed"`
}

type asrFinal struct {
	T      string `json:"t"`
	TurnID string `json:"turnId"`
	Text   string `json:"text"`
}

type replyEnd struct {
	T      string `json:"t"`
	TurnID string `json:"turnId"`
	Text   string `json:"text"`
}

type turnState struct {
	T      string `json:"t"`
	TurnID string `json:"turnId"`
	State  string `json:"state"` // thinking | speaking | idle
}

type errFrame struct {
	T      string `json:"t"`
	TurnID string `json:"turnId,omitempty"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// turn states.
const (
	stateThinking = "thinking"
	stateSpeaking = "speaking"
	stateIdle     = "idle"
)

// error codes.
const (
	codeUnauth     = "UNAUTH"
	codeASREmpty   = "ASR_EMPTY"
	codeASRTimeout = "ASR_TIMEOUT"
	codeTTSFailed  = "TTS_FAILED"
	codeBadProto   = "BAD_PROTO"
	codeBusy       = "BUSY" // a ptt.start arrived while a turn was still in flight
)
