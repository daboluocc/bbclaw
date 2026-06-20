package deviceapi

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SayTTS is a self-contained Synthesizer backed by macOS's built-in `say`
// command. It exists so the Phase 2 device line is testable end-to-end on a dev
// Mac without standing up v1's HTTP TTS providers (Doubao / OpenAI). It writes a
// real RIFF/WAVE PCM16 file and returns its bytes.
//
// This is intentionally a stopgap: production reuses v1's tts providers behind
// the Synthesizer interface (see the package doc and DESIGN.md §7). SayTTS is
// macOS-only — Ping() reports the binary is missing on other platforms.
type SayTTS struct {
	// bin is the `say` executable (default "say"); overridable for tests.
	bin string
	// voice, if set, selects a named system voice (`say -v`). Empty = default.
	voice string
	// sampleRate is the PCM sample rate written into the WAVE header.
	sampleRate int
}

// SayOption configures a SayTTS.
type SayOption func(*SayTTS)

// WithSayVoice selects a named macOS voice (e.g. "Samantha").
func WithSayVoice(v string) SayOption { return func(s *SayTTS) { s.voice = v } }

// WithSayBin overrides the `say` binary path (used by tests to point at a fake).
func WithSayBin(bin string) SayOption { return func(s *SayTTS) { s.bin = bin } }

// NewSayTTS builds a macOS `say` synthesizer. Defaults: bin "say", 16000 Hz
// (the BBClaw device speaker rate, so its WAV needs no resample — only header
// stripping — to reach the device as raw PCM16), system default voice.
func NewSayTTS(opts ...SayOption) *SayTTS {
	s := &SayTTS{bin: "say", sampleRate: 16000}
	for _, o := range opts {
		o(s)
	}
	return s
}

// OutputFormat reports the encoding of Synthesize's bytes. `say` with the WAVE
// file-format and LEI16 data-format yields standard PCM16 WAV.
func (s *SayTTS) OutputFormat() string { return "wav" }

// Synthesize renders text to a WAV buffer via `say -o file --file-format=WAVE
// --data-format=LEI16@<rate>`. Empty text is an error (nothing to speak).
func (s *SayTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("deviceapi: say tts: empty text")
	}

	dir, err := os.MkdirTemp("", "bbclaw-v2-tts-*")
	if err != nil {
		return nil, fmt.Errorf("deviceapi: say tts temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	out := filepath.Join(dir, "reply.wav")
	args := []string{
		"-o", out,
		"--file-format=WAVE",
		fmt.Sprintf("--data-format=LEI16@%d", s.sampleRate),
	}
	if s.voice != "" {
		args = append(args, "-v", s.voice)
	}
	args = append(args, text)

	cmd := exec.CommandContext(ctx, s.bin, args...)
	if errOut, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("deviceapi: say tts: %w: %s", err, strings.TrimSpace(string(errOut)))
	}

	audio, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("deviceapi: say tts read output: %w", err)
	}
	if len(audio) == 0 {
		return nil, fmt.Errorf("deviceapi: say tts: empty audio")
	}
	return audio, nil
}

// Ping reports whether the `say` binary is available, so callers can fall back
// to a mock Synthesizer on non-macOS hosts at wiring time.
func (s *SayTTS) Ping() error {
	if _, err := exec.LookPath(s.bin); err != nil {
		return fmt.Errorf("deviceapi: say tts unavailable: %w", err)
	}
	return nil
}
