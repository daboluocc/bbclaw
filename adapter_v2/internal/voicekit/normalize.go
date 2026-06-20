package voicekit

import (
	"context"
	"encoding/binary"
	"strings"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/deviceapi"
	"github.com/daboluocc/bbclaw/voice/audio"
)

// The device speaker wants raw PCM16 at deviceRate/deviceChans. TTS providers
// emit assorted formats (say → WAV, doubao → MP3, local → configurable), so the
// device line must normalise every reply to canonical PCM16 before it goes on the
// wire — otherwise a WAV/MP3 container labelled "pcm16" plays as noise on the
// device (its RIFF/MP3 header becomes audible garbage and samples misalign).
//
// pcmSynth wraps any Synthesizer and converts its output to PCM16 16 kHz mono,
// reporting OutputFormat()="pcm16" so the transport's codec byte is honest.

// pcmSynth normalises a Synthesizer's output to canonical PCM16.
type pcmSynth struct{ inner deviceapi.Synthesizer }

// normalizing wraps syn so its Synthesize output is always canonical PCM16.
// A nil inner is returned unchanged (defensive).
func normalizing(syn deviceapi.Synthesizer) deviceapi.Synthesizer {
	if syn == nil {
		return nil
	}
	return pcmSynth{inner: syn}
}

func (p pcmSynth) Synthesize(ctx context.Context, text string) ([]byte, error) {
	raw, err := p.inner.Synthesize(ctx, text)
	if err != nil {
		return nil, err
	}
	return toPCM16(ctx, p.inner.OutputFormat(), raw)
}

func (p pcmSynth) OutputFormat() string { return pcmCodec }

// toPCM16 converts audio in `format` to raw PCM16LE at deviceRate/deviceChans.
//   - pcm16 / empty: passthrough.
//   - wav already at the device spec: strip the header in pure Go (no ffmpeg),
//     so the zero-config macOS `say` path (WAV @ 16 kHz mono) needs no ffmpeg.
//   - any other wav (different rate/channels) or mp3/etc.: transcode via ffmpeg
//     (voice/audio.DecodeMediaToPCM16LE) — the same path v1 uses; the operator
//     who configured such a provider is expected to have ffmpeg, as in v1.
func toPCM16(ctx context.Context, format string, b []byte) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "pcm16", "pcm_s16le":
		return b, nil
	case "wav", "wave":
		if pcm, rate, ch, ok := wavToPCM16(b); ok && rate == deviceRate && ch == deviceChans {
			return pcm, nil // already 16 kHz mono — just drop the container header
		}
		return audio.DecodeMediaToPCM16LE(ctx, "wav", deviceRate, deviceChans, b)
	default:
		return audio.DecodeMediaToPCM16LE(ctx, format, deviceRate, deviceChans, b)
	}
}

// wavToPCM16 extracts the PCM samples and format from a canonical RIFF/WAVE
// buffer in pure Go. ok is false for anything not a simple PCM WAV (compressed,
// truncated, or no data chunk), so the caller falls back to ffmpeg.
func wavToPCM16(b []byte) (pcm []byte, sampleRate, channels int, ok bool) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, 0, false
	}
	var rate, ch, bits int
	var haveFmt bool
	i := 12
	for i+8 <= len(b) {
		id := string(b[i : i+4])
		size := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		body := i + 8
		if size < 0 || body+size > len(b) {
			return nil, 0, 0, false // malformed/truncated chunk
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, 0, 0, false
			}
			audioFormat := binary.LittleEndian.Uint16(b[body : body+2])
			ch = int(binary.LittleEndian.Uint16(b[body+2 : body+4]))
			rate = int(binary.LittleEndian.Uint32(b[body+4 : body+8]))
			bits = int(binary.LittleEndian.Uint16(b[body+14 : body+16]))
			if audioFormat != 1 || bits != 16 { // 1 = PCM; we only handle PCM16
				return nil, 0, 0, false
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return nil, 0, 0, false
			}
			return b[body : body+size], rate, ch, true
		}
		i = body + size
		if size%2 == 1 {
			i++ // chunks are word-aligned
		}
	}
	return nil, 0, 0, false
}
