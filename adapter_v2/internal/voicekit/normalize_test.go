package voicekit

import (
	"context"
	"encoding/binary"
	"testing"
)

// buildWAV makes a minimal canonical PCM16 RIFF/WAVE buffer for the given spec.
func buildWAV(rate, ch int, data []byte) []byte {
	var b []byte
	put32 := func(v uint32) { var x [4]byte; binary.LittleEndian.PutUint32(x[:], v); b = append(b, x[:]...) }
	put16 := func(v uint16) { var x [2]byte; binary.LittleEndian.PutUint16(x[:], v); b = append(b, x[:]...) }
	b = append(b, "RIFF"...)
	put32(uint32(36 + len(data)))
	b = append(b, "WAVE"...)
	b = append(b, "fmt "...)
	put32(16)
	put16(1)              // PCM
	put16(uint16(ch))     // channels
	put32(uint32(rate))   // sample rate
	put32(uint32(rate * ch * 2)) // byte rate
	put16(uint16(ch * 2)) // block align
	put16(16)             // bits/sample
	b = append(b, "data"...)
	put32(uint32(len(data)))
	b = append(b, data...)
	return b
}

func TestWavToPCM16StripsHeader(t *testing.T) {
	samples := []byte{1, 0, 2, 0, 3, 0, 4, 0} // 4 PCM16 samples
	wav := buildWAV(deviceRate, deviceChans, samples)

	pcm, rate, ch, ok := wavToPCM16(wav)
	if !ok {
		t.Fatal("wavToPCM16 should parse a canonical PCM16 WAV")
	}
	if rate != deviceRate || ch != deviceChans {
		t.Errorf("rate/ch = %d/%d, want %d/%d", rate, ch, deviceRate, deviceChans)
	}
	if string(pcm) != string(samples) {
		t.Errorf("pcm = %v, want %v", pcm, samples)
	}
}

func TestWavToPCM16RejectsNonWav(t *testing.T) {
	if _, _, _, ok := wavToPCM16([]byte("not a wav at all")); ok {
		t.Error("non-WAV input should not parse")
	}
	if _, _, _, ok := wavToPCM16(buildWAV(16000, 1, nil)[:20]); ok {
		t.Error("truncated WAV should not parse")
	}
}

func TestToPCM16Passthrough(t *testing.T) {
	in := []byte{9, 9, 9}
	for _, format := range []string{"", "pcm16", "pcm_s16le"} {
		got, err := toPCM16(context.Background(), format, in)
		if err != nil || string(got) != string(in) {
			t.Errorf("toPCM16(%q) = %v err=%v, want passthrough %v", format, got, err, in)
		}
	}
}

func TestToPCM16StripsDeviceSpecWav(t *testing.T) {
	samples := []byte{5, 0, 6, 0}
	wav := buildWAV(deviceRate, deviceChans, samples)
	got, err := toPCM16(context.Background(), "wav", wav)
	if err != nil {
		t.Fatalf("toPCM16(wav): %v", err)
	}
	if string(got) != string(samples) {
		t.Errorf("got %v, want stripped samples %v (no ffmpeg for device-spec WAV)", got, samples)
	}
}

// fakeFmtTTS returns canned bytes in a declared format.
type fakeFmtTTS struct {
	out    []byte
	format string
}

func (f fakeFmtTTS) Synthesize(context.Context, string) ([]byte, error) { return f.out, nil }
func (f fakeFmtTTS) OutputFormat() string                               { return f.format }

func TestNormalizingSynthYieldsPCM16(t *testing.T) {
	samples := []byte{7, 0, 8, 0}
	wav := buildWAV(deviceRate, deviceChans, samples)
	syn := normalizing(fakeFmtTTS{out: wav, format: "wav"})

	if syn.OutputFormat() != pcmCodec {
		t.Errorf("OutputFormat = %q, want %q", syn.OutputFormat(), pcmCodec)
	}
	got, err := syn.Synthesize(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if string(got) != string(samples) {
		t.Errorf("normalized output = %v, want stripped PCM16 %v", got, samples)
	}
}
