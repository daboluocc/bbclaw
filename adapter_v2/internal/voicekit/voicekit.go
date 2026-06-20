// Package voicekit wires the shared voice providers (github.com/daboluocc/bbclaw/voice)
// behind adapter_v2's narrow deviceapi.Recognizer / deviceapi.Synthesizer
// interfaces, and builds them from the environment.
//
// This is the "extraction adaptation": v1's asr/tts/audio were lifted into the
// shared `voice` module (so v1 and v2 share one implementation), and this package
// adapts their richer interfaces down to what the device line needs — ASR is just
// audio→text, TTS is just text→audio+format. The env var names mirror v1's
// (ASR_PROVIDER, TTS_PROVIDER, …) so an operator's existing .env Just Works.
//
// Audio reaching the Recognizer is always PCM16: the device-WS transport decodes
// Opus uplink to PCM16 before ASR (see DecodeUplink), matching v1's normalize-
// before-ASR flow, so the recognizer wrapper reports a fixed pcm16/16k/mono spec.
package voicekit

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/deviceapi"
	"github.com/daboluocc/bbclaw/voice/asr"
	"github.com/daboluocc/bbclaw/voice/audio"
	"github.com/daboluocc/bbclaw/voice/tts"
)

// Device audio spec the Recognizer sees after the transport has decoded uplink to
// PCM16. BBClaw mics are 16 kHz mono; these are the metadata the ASR providers want.
const (
	pcmCodec    = "pcm16"
	deviceRate  = 16000
	deviceChans = 1
)

// recognizer adapts an asr.Provider (audio+Metadata→Result) to the device line's
// deviceapi.Recognizer (audio→text). The audio is already PCM16 (transport-decoded).
type recognizer struct {
	p        asr.Provider
	hotwords []string
}

func (r recognizer) Transcribe(ctx context.Context, audio []byte) (string, error) {
	res, err := r.p.Transcribe(ctx, audio, asr.Metadata{
		Codec:      pcmCodec,
		SampleRate: deviceRate,
		Channels:   deviceChans,
		Hotwords:   r.hotwords,
	})
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// synthesizer adapts a tts.Provider (text→bytes) to deviceapi.Synthesizer by
// pairing it with its declared output format.
type synthesizer struct {
	p      tts.Provider
	format string
}

func (s synthesizer) Synthesize(ctx context.Context, text string) ([]byte, error) {
	return s.p.Synthesize(ctx, text)
}
func (s synthesizer) OutputFormat() string { return s.format }

// formatProvider is the optional OutputFormat() that tts providers expose.
type formatProvider interface{ OutputFormat() string }

// DecodeUplink decodes one PTT utterance's mic audio to PCM16 for ASR. codec is
// the device's declared mic codec ("pcm16" or "opus"); rate/ch its mic spec. It
// is the seam the device-WS transport calls before handing audio to the
// Recognizer, mirroring v1's normalize-before-ASR. PCM16 passes through untouched;
// Opus shells out to ffmpeg (voice/audio), so an Opus device needs ffmpeg on PATH.
func DecodeUplink(ctx context.Context, codec string, rate, ch int, payload []byte) ([]byte, error) {
	return audio.DecodeToPCM16LE(ctx, codec, rate, ch, payload)
}

// FromEnv builds the device-line Recognizer + Synthesizer from the environment,
// using the same var names as v1. Unset/unknown providers fall back to a safe
// default so `make run` works with zero config:
//   - ASR: a StaticRecognizer (a fixed transcript) when ASR_PROVIDER is unset —
//     real recognition needs a configured backend.
//   - TTS: the macOS `say` synthesiser when TTS_PROVIDER is unset and `say` is
//     available (real audio, no keys), else the SilentSynthesizer.
// asrMode / ttsMode name the actual backend chosen (e.g. "local", "doubao_native",
// "say", "static-mock", "silent") so the caller can log honestly what the device
// line will really do — a `say` fallback IS real audio, not a mock.
func FromEnv() (rec deviceapi.Recognizer, syn deviceapi.Synthesizer, asrMode, ttsMode string) {
	if r, mode := buildASR(); r != nil {
		rec, asrMode = r, mode
	} else {
		rec, asrMode = deviceapi.StaticRecognizer{Text: "你好"}, "static-mock"
	}
	if s, mode := buildTTS(); s != nil {
		syn, ttsMode = s, mode
	} else if say := deviceapi.NewSayTTS(); say.Ping() == nil {
		syn, ttsMode = say, "say" // real macOS `say` audio, zero config
	} else {
		syn, ttsMode = deviceapi.SilentSynthesizer{}, "silent"
	}
	// Every synthesizer's output is normalised to canonical PCM16 before it
	// reaches the device, so the transport's pcm16 codec label is always honest
	// (a WAV/MP3 reply would otherwise play as noise). See normalize.go.
	syn = normalizing(syn)
	return rec, syn, asrMode, ttsMode
}

// buildASR constructs the ASR provider named by ASR_PROVIDER, returning its mode
// name. Returns nil when unset/unsupported/misconfigured (caller falls back to
// the mock recognizer).
func buildASR() (deviceapi.Recognizer, string) {
	hotwords := splitList(os.Getenv("ASR_HOTWORDS"))
	provider := strings.TrimSpace(os.Getenv("ASR_PROVIDER"))
	switch provider {
	case "local":
		bin := strings.TrimSpace(os.Getenv("ASR_LOCAL_BIN"))
		if bin == "" {
			return nil, ""
		}
		p := asr.NewLocalCommandProvider(bin, splitArgs(os.Getenv("ASR_LOCAL_ARGS")), strings.TrimSpace(os.Getenv("ASR_LOCAL_TEXT_PATH")))
		return recognizer{p: p, hotwords: hotwords}, provider
	case "openai_compatible", "openai":
		base := strings.TrimSpace(os.Getenv("ASR_BASE_URL"))
		if base == "" {
			return nil, ""
		}
		p := asr.NewOpenAICompatibleProvider(base, strings.TrimSpace(os.Getenv("ASR_API_KEY")), strings.TrimSpace(os.Getenv("ASR_MODEL")), &http.Client{Timeout: 60 * time.Second})
		return recognizer{p: p, hotwords: hotwords}, provider
	case "doubao_native":
		url := strings.TrimSpace(os.Getenv("ASR_BASE_URL"))
		appID := strings.TrimSpace(os.Getenv("ASR_APP_ID"))
		token := strings.TrimSpace(os.Getenv("ASR_API_KEY"))
		if url == "" || appID == "" || token == "" {
			return nil, ""
		}
		p := asr.NewDoubaoNativeProvider(url, appID, token,
			strings.TrimSpace(os.Getenv("ASR_RESOURCE_ID")),
			strings.TrimSpace(os.Getenv("ASR_MODEL")),
			getOrDefault("ASR_LANGUAGE", "zh-CN"),
			asr.DoubaoOptions{
				BoostingTable: strings.TrimSpace(os.Getenv("ASR_BOOSTING_TABLE")),
				CorrectTable:  strings.TrimSpace(os.Getenv("ASR_CORRECT_TABLE")),
				Hotwords:      hotwords,
			})
		return recognizer{p: p, hotwords: hotwords}, provider
	default:
		return nil, ""
	}
}

// buildTTS constructs the TTS provider named by TTS_PROVIDER, returning its mode
// name. Returns nil when unset/unsupported/misconfigured (caller falls back to
// `say`/silent).
func buildTTS() (deviceapi.Synthesizer, string) {
	provider := strings.TrimSpace(os.Getenv("TTS_PROVIDER"))
	switch provider {
	case "local", "local_command":
		bin := strings.TrimSpace(os.Getenv("TTS_LOCAL_BIN"))
		if bin == "" {
			return nil, ""
		}
		format := getOrDefault("TTS_LOCAL_OUTPUT_FORMAT", "wav")
		p := tts.NewLocalCommandProvider(bin, splitArgs(os.Getenv("TTS_LOCAL_ARGS")), format)
		return wrapTTS(p, format), provider
	case "doubao_native":
		url := strings.TrimSpace(os.Getenv("TTS_BASE_URL"))
		appID := strings.TrimSpace(os.Getenv("TTS_APP_ID"))
		token := strings.TrimSpace(os.Getenv("TTS_TOKEN"))
		// Require the URL too (symmetric with buildASR): without it the provider
		// would dial "" and fail every turn instead of falling back to say/silent.
		if url == "" || appID == "" || token == "" {
			return nil, ""
		}
		p := tts.NewDoubaoNativeProvider(url, appID, token,
			strings.TrimSpace(os.Getenv("TTS_CLUSTER")),
			strings.TrimSpace(os.Getenv("TTS_VOICE")))
		return wrapTTS(p, "mp3"), provider
	default:
		return nil, ""
	}
}

// wrapTTS pairs a tts.Provider with its output format, preferring the provider's
// own OutputFormat() when it exposes one.
func wrapTTS(p tts.Provider, fallback string) deviceapi.Synthesizer {
	format := fallback
	if fp, ok := p.(formatProvider); ok && fp.OutputFormat() != "" {
		format = fp.OutputFormat()
	}
	return synthesizer{p: p, format: format}
}

func getOrDefault(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// splitArgs splits a space-separated arg string (sufficient for the simple
// local-provider command lines; quoting is not needed at this layer).
func splitArgs(s string) []string { return strings.Fields(s) }

// splitList splits a comma/space-separated list (e.g. hotwords) into trimmed,
// non-empty items.
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' })
	out := fields[:0]
	for _, f := range fields {
		if t := strings.TrimSpace(f); t != "" {
			out = append(out, t)
		}
	}
	return out
}
