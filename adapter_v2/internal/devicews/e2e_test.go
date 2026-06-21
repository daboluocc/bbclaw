//go:build e2e

// bbwire/2 device protocol — Phase A acceptance gate (adapter_v2/docs/device-protocol.md).
//
// This is THE sign-off test: a WS client impersonates the ESP32 device and drives
// one full PTT round-trip against the real /v2/dev/ws handler, with no hardware
// and no network beyond loopback. The only mocks are the device's mouth/ear:
//   - ASR is a StaticRecognizer (transcript = "hello") standing in for real ASR,
//   - TTS is the SilentSynthesizer (zero-filled PCM16) standing in for real TTS,
//   - the agent is cmd/mockcli, a real separately-compiled process under a PTY.
//
// Everything between — the protocol framing, session/Bridge wiring, inject, PTY
// reply scrape, boundary, and the binary-audio downlink — is the production path.
//
// Asserts the round-trip the protocol promises:
//
//	hello → hello.ok
//	ptt.start + BINARY mic frames + ptt.stop
//	  → asr.final{"hello"} → reply.end (contains "ANSWER: hello")
//	  → ≥1 BINARY downlink frame (streamKind=tts, flags.final) → turn{idle}
//
// Build-tagged `e2e` so plain `go test ./...` stays fast and dep-free; run via
// `make -C adapter_v2 e2e`.
package devicews

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/deviceapi"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

func TestE2EDeviceRoundTrip(t *testing.T) {
	bin := buildMockCLI(t)

	mgr := session.NewManager()
	// StreamReplyDelta on so the round-trip also exercises the Phase B live-subtitle
	// path (reply.delta) ahead of the authoritative reply.end.
	srv := New(mgr, deviceapi.StaticRecognizer{Text: "hello"}, deviceapi.SilentSynthesizer{}, []string{bin}, "", Options{StreamReplyDelta: true})
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/v2/dev/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")
	conn.SetReadLimit(maxFrame)

	// hello → hello.ok
	writeJSON(t, ctx, conn, map[string]any{
		"t": "hello", "proto": Proto, "dev": "sim",
		"mic": map[string]any{"codec": "pcm16", "rate": 16000, "ch": 1},
		"spk": map[string]any{"codec": "pcm16", "rate": 16000},
	})
	if got := readCtrlType(t, ctx, conn); got != "hello.ok" {
		t.Fatalf("first downlink frame t=%q, want hello.ok", got)
	}

	// PTT: start, two mic frames (last marked final), stop.
	writeJSON(t, ctx, conn, map[string]any{"t": "ptt.start", "turnId": "u1", "u": 1})
	pcm := make([]byte, 320) // a dummy ~10ms PCM16 mic frame; content is irrelevant (ASR is static)
	writeBinFrame(t, ctx, conn, binHeader{StreamKind: streamUplinkMic, Codec: codecPCM16, TurnSeq: 1, FrameSeq: 0}, pcm)
	writeBinFrame(t, ctx, conn, binHeader{StreamKind: streamUplinkMic, Codec: codecPCM16, TurnSeq: 1, FrameSeq: 1, Flags: flagFinal}, pcm)
	writeJSON(t, ctx, conn, map[string]any{"t": "ptt.stop", "turnId": "u1", "u": 1, "frames": 2})

	// Collect downlink frames until turn{idle} or the context expires.
	var (
		sawASR, sawReplyEnd, sawIdle bool
		sawReplyDelta                bool
		replyText                    string
		ttsFinalFrames               int
	)
	for !sawIdle {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		if typ == websocket.MessageBinary {
			if h, _, ok := decodeBinFrame(data); ok && h.StreamKind == streamDownlinkTTS && h.Flags&flagFinal != 0 {
				ttsFinalFrames++
			}
			continue
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m["t"] {
		case "asr.final":
			if m["text"] == "hello" {
				sawASR = true
			}
		case "reply.delta":
			if s, _ := m["text"].(string); strings.Contains(s, "ANSWER") {
				sawReplyDelta = true
			}
		case "reply.end":
			if s, _ := m["text"].(string); s != "" {
				replyText = s
				sawReplyEnd = true
			}
		case "turn":
			if m["state"] == "idle" {
				sawIdle = true
			}
		case "error":
			t.Fatalf("unexpected error frame: %v", m)
		}
	}

	if !sawASR {
		t.Errorf("never received asr.final{text:\"hello\"}")
	}
	if !sawReplyEnd || !strings.Contains(replyText, "ANSWER: hello") {
		t.Errorf("reply.end text = %q, want a substring \"ANSWER: hello\"", replyText)
	}
	if !sawReplyDelta {
		t.Errorf("StreamReplyDelta on but no reply.delta frame carrying the reply arrived")
	}
	if ttsFinalFrames == 0 {
		t.Errorf("never received a downlink TTS binary frame with flags.final")
	}
	if !sawIdle {
		t.Errorf("never received turn{state:\"idle\"}")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func writeJSON(t *testing.T, ctx context.Context, conn *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write text: %v", err)
	}
}

func writeBinFrame(t *testing.T, ctx context.Context, conn *websocket.Conn, h binHeader, payload []byte) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageBinary, h.encode(payload)); err != nil {
		t.Fatalf("write binary: %v", err)
	}
}

func readCtrlType(t *testing.T, ctx context.Context, conn *websocket.Conn) string {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal ctrl %q: %v", data, err)
	}
	s, _ := m["t"].(string)
	return s
}

// buildMockCLI compiles cmd/mockcli into a temp dir and returns the binary path
// (mirrors the deviceapi e2e helper; duplicated because that one is package-private).
func buildMockCLI(t *testing.T) string {
	t.Helper()
	goBin := goToolPath()
	bin := filepath.Join(t.TempDir(), "mockcli")
	cmd := exec.Command(goBin, "build", "-o", bin, "./cmd/mockcli")
	cmd.Dir = moduleRoot(t, goBin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockcli: %v\n%s", err, out)
	}
	return bin
}

func moduleRoot(t *testing.T, goBin string) string {
	t.Helper()
	out, err := exec.Command(goBin, "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		t.Fatalf("no go.mod found from %s", goBin)
	}
	return filepath.Dir(gomod)
}

func goToolPath() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	for _, cand := range []string{"/opt/homebrew/bin/go", "/usr/local/go/bin/go", "/usr/local/bin/go"} {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return "go"
}
