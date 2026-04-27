package tts

import (
	"context"
)

// MockProvider returns a short silent PCM16 buffer instead of synthesising
// real speech. Useful for adapter smoke tests, the auto-generated minimal
// .env from scripts/sync_env.py, or any setup where the user wants the
// agent loop to terminate cleanly without configuring a real TTS backend.
//
// Length is loosely proportional to the input text: roughly 60ms of silence
// per character (capped at 3 seconds), enough that the device's playback
// pipeline observes a non-trivial buffer and the chat surface still
// transitions through SPEAKING → IDLE.
//
// Declares OutputFormat() = "pcm16" so the HTTP layer knows the bytes are
// already PCM and skips the ffmpeg transcode step.
type MockProvider struct{}

func NewMockProvider() *MockProvider { return &MockProvider{} }

const (
	mockSampleRate    = 16000
	mockChannels      = 1
	mockMsPerChar     = 60
	mockMinSilenceMs  = 200
	mockMaxSilenceMs  = 3000
)

func (p *MockProvider) Synthesize(_ context.Context, text string) ([]byte, error) {
	durMs := len([]rune(text)) * mockMsPerChar
	if durMs < mockMinSilenceMs {
		durMs = mockMinSilenceMs
	}
	if durMs > mockMaxSilenceMs {
		durMs = mockMaxSilenceMs
	}
	// 16-bit signed mono → 2 bytes per sample. Zero bytes = silence.
	nBytes := mockSampleRate * mockChannels * 2 * durMs / 1000
	return make([]byte, nBytes), nil
}

// OutputFormat tells the HTTP layer this provider already returns PCM16
// little-endian — no ffmpeg transcode required when the device asks for pcm16.
func (p *MockProvider) OutputFormat() string { return "pcm16" }
