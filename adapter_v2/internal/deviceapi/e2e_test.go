//go:build e2e

// Device end-to-end smoke (issue #212).
//
// This is the no-external-deps acceptance test the Makefile `e2e` target runs
// (`make -C adapter_v2 e2e` → `go test -tags e2e ./internal/deviceapi/...`). It
// differs from the unit-level integration test in deviceapi_test.go in ONE
// deliberate way: the upstream CLI is a REAL, separately-compiled process
// (cmd/mockcli) spawned under a genuine PTY, not an inline shell snippet. That
// makes it a faithful stand-in for production's `claude` TUI — the same spawn →
// inject → reply → boundary → TTS path, end to end, with nothing mocked but the
// agent's brain.
//
// It is build-tagged `e2e` so the default `go test ./...` (run on every commit
// and in plain CI) stays fast and never depends on `go build` of a child binary
// at test time; the heavier process-spawning smoke runs on demand via the
// Makefile target.
//
// What it asserts (the spec's round-trip):
//
//	injected ASR text  ─▶ mock CLI process  ─▶ canned "ANSWER: <text>" reply
//	                                            ─▶ turn boundary fires
//	                                            ─▶ TTS invoked with the reply
//	                                            ─▶ synthesised audio reaches sink
//
// It reuses the recordingSink / countingTTS / waitForPlays helpers from
// deviceapi_test.go (same package).
package deviceapi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

// buildMockCLI compiles cmd/mockcli into a temp dir once per test run and returns
// the binary path. Compiling (rather than `go run`) keeps the spawned upstream a
// single clean process with no `go` toolchain wrapper in the PTY, exactly like a
// real installed agent CLI.
func buildMockCLI(t *testing.T) string {
	t.Helper()

	goBin := goToolPath()
	bin := filepath.Join(t.TempDir(), "mockcli")
	moduleRoot := moduleRoot(t, goBin)

	cmd := exec.Command(goBin, "build", "-o", bin, "./cmd/mockcli")
	cmd.Dir = moduleRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockcli: %v\n%s", err, out)
	}
	return bin
}

// moduleRoot resolves the adapter_v2 module root (the dir holding go.mod) by
// asking the toolchain (`go env GOMOD`). The test's cwd is the package dir
// (internal/deviceapi), so a hand-counted "../.." is brittle; deriving it from
// GOMOD is robust to where the test happens to run from.
func moduleRoot(t *testing.T, goBin string) string {
	t.Helper()
	cmd := exec.Command(goBin, "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		t.Fatalf("no go.mod found from %s", goBin)
	}
	return filepath.Dir(gomod)
}

// goToolPath finds the `go` binary. Portable CI normally has `go` on PATH, so we
// prefer that; this dev environment does not, so we fall back to the well-known
// Homebrew / standard install locations.
func goToolPath() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	for _, cand := range []string{"/opt/homebrew/bin/go", "/usr/local/go/bin/go", "/usr/local/bin/go"} {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return "go" // last resort; exec.Command will surface a clear error if missing
}

// TestE2EVoiceRoundTrip drives one full device turn against the real mock CLI
// process: inject a transcript, let the CLI reply, watch the boundary fire, and
// assert TTS was invoked with the reply text and its audio reached the device
// sink. Table-driven over transcript variants so a regression in extraction or
// boundary detection surfaces on more than one phrasing.
func TestE2EVoiceRoundTrip(t *testing.T) {
	bin := buildMockCLI(t)

	cases := []struct {
		name       string
		transcript string
		wantSpoken string // substring the synthesised reply text must contain
	}{
		{name: "greeting", transcript: "hello bbclaw", wantSpoken: "ANSWER: hello bbclaw"},
		{name: "question", transcript: "what is the weather", wantSpoken: "ANSWER: what is the weather"},
		{name: "punctuation", transcript: "turn on the light, please.", wantSpoken: "ANSWER: turn on the light, please."},
	}
	// NOTE: this smoke uses ASCII transcripts on purpose. The server-side VT
	// emulator (hinshun/vt10x in internal/vtscreen) does not yet render
	// double-width CJK glyphs into VisibleText — a "你好" reply currently extracts
	// as empty. Faithful wide-character rendering is a vtscreen concern, tracked
	// separately from this device-e2e issue; the round-trip path itself is
	// transcript-agnostic and fully exercised by the ASCII cases above.

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := session.NewManager()
			s, err := m.Create("e2e", ptyhost.Config{
				Argv:        []string{bin},
				InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
			})
			if err != nil {
				t.Fatalf("spawn mock CLI session: %v", err)
			}

			tts := &countingTTS{format: "wav", out: []byte("WAVDATA")}
			sink := &recordingSink{}
			bridge := New(s, StaticRecognizer{Text: tc.transcript}, tts, sink,
				Config{Cols: 80, Rows: 24, PollInterval: 40 * time.Millisecond})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			runErr := make(chan error, 1)
			go func() { runErr <- bridge.Run(ctx) }()

			// Let Run attach and baseline the initial idle prompt before injecting.
			time.Sleep(120 * time.Millisecond)

			if err := bridge.SubmitVoiceTurn(tc.transcript); err != nil {
				t.Fatalf("SubmitVoiceTurn: %v", err)
			}

			plays := waitForPlays(sink, 1, 5*time.Second)
			if len(plays) == 0 {
				t.Fatalf("TTS audio never reached the device sink; tts calls=%v", tts.calls())
			}

			calls := tts.calls()
			if len(calls) == 0 {
				t.Fatalf("Synthesize was never called")
			}
			if !strings.Contains(calls[0], tc.wantSpoken) {
				t.Errorf("spoken text = %q, want substring %q", calls[0], tc.wantSpoken)
			}
			if string(plays[0].audio) != "WAVDATA" {
				t.Errorf("sink audio = %q, want %q", plays[0].audio, "WAVDATA")
			}
			if plays[0].format != "wav" {
				t.Errorf("sink format = %q, want wav", plays[0].format)
			}

			cancel()
			<-runErr
			_ = m
		})
	}
}

// TestE2ESayTTSRoundTrip is the fullest possible local smoke: the real mock CLI
// process AND the real macOS `say` synthesiser, proving the extracted reply
// becomes genuine WAV audio at the device sink with nothing mocked but the agent.
// Skipped where `say` is unavailable (non-macOS), since it is the documented
// local-only stopgap synthesiser.
func TestE2ESayTTSRoundTrip(t *testing.T) {
	say := NewSayTTS()
	if err := say.Ping(); err != nil {
		t.Skipf("say unavailable: %v", err)
	}
	bin := buildMockCLI(t)

	m := session.NewManager()
	s, err := m.Create("e2e-say", ptyhost.Config{
		Argv:        []string{bin},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("spawn mock CLI session: %v", err)
	}

	sink := &recordingSink{}
	bridge := New(s, StaticRecognizer{Text: "speak this out loud"}, say, sink,
		Config{Cols: 80, Rows: 24, PollInterval: 40 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- bridge.Run(ctx) }()

	time.Sleep(120 * time.Millisecond)
	if err := bridge.SubmitVoiceTurn("speak this out loud"); err != nil {
		t.Fatalf("SubmitVoiceTurn: %v", err)
	}

	plays := waitForPlays(sink, 1, 6*time.Second)
	if len(plays) == 0 {
		t.Fatalf("say-synthesised audio never reached the sink")
	}
	if plays[0].format != "wav" {
		t.Errorf("format = %q, want wav", plays[0].format)
	}
	// A real WAV has at least the 44-byte RIFF/WAVE header.
	if len(plays[0].audio) < 44 || string(plays[0].audio[:4]) != "RIFF" {
		t.Errorf("sink did not receive a real WAV buffer (len=%d)", len(plays[0].audio))
	}

	cancel()
	<-runErr
	_ = m
}
