package deviceapi

import "context"

// This file holds the self-contained stubs that make the device line runnable
// (and testable) before v1's real asr/tts providers are wired in behind the
// Recognizer / Synthesizer / DeviceSink interfaces — the deliberate follow-up
// noted in the package doc and DESIGN.md §7. They are non-test files so other v2
// packages (and a future cmd wiring) can use them as placeholders too.

// StaticRecognizer is a mock Recognizer that returns a fixed transcript for any
// audio. It lets the inject path be exercised without a real ASR backend.
type StaticRecognizer struct {
	// Text is returned by every Transcribe call.
	Text string
	// Err, if set, is returned instead (to test the failure path).
	Err error
}

// Transcribe returns the static text (or the configured error), ignoring audio.
func (r StaticRecognizer) Transcribe(_ context.Context, _ []byte) (string, error) {
	if r.Err != nil {
		return "", r.Err
	}
	return r.Text, nil
}

// SilentSynthesizer is a mock Synthesizer that returns a zero-filled PCM16
// buffer roughly proportional to the text length, declaring format "pcm16". It
// mirrors v1's tts.MockProvider so a test can assert the speak path runs without
// invoking the OS. Useful on non-macOS hosts where SayTTS is unavailable.
type SilentSynthesizer struct{}

const (
	silentSampleRate = 16000 // Hz
	silentChannels   = 1
	silentMsPerChar  = 50
	silentMinMs      = 200
	silentMaxMs      = 3000
)

// Synthesize returns silence sized to the text, never erroring.
func (SilentSynthesizer) Synthesize(_ context.Context, text string) ([]byte, error) {
	ms := len([]rune(text)) * silentMsPerChar
	if ms < silentMinMs {
		ms = silentMinMs
	}
	if ms > silentMaxMs {
		ms = silentMaxMs
	}
	// PCM16 mono: 2 bytes/sample; zero bytes are silence.
	n := silentSampleRate * silentChannels * 2 * ms / 1000
	return make([]byte, n), nil
}

// OutputFormat reports the buffer is already PCM16, so no transcode is needed.
func (SilentSynthesizer) OutputFormat() string { return "pcm16" }

// DiscardSink is a no-op DeviceSink that drops audio. It stands in for the real
// device transport so Run can complete without a device attached.
type DiscardSink struct{}

// Play discards the audio and reports success.
func (DiscardSink) Play(_ context.Context, _ []byte, _ string) error { return nil }
