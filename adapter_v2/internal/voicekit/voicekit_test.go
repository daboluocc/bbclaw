package voicekit

import (
	"context"
	"errors"
	"testing"

	"github.com/daboluocc/bbclaw/voice/asr"
)

// fakeASR is a stand-in asr.Provider that records the metadata it was handed.
type fakeASR struct {
	gotMeta asr.Metadata
	result  string
	err     error
}

func (f *fakeASR) Transcribe(_ context.Context, _ []byte, meta asr.Metadata) (asr.Result, error) {
	f.gotMeta = meta
	if f.err != nil {
		return asr.Result{}, f.err
	}
	return asr.Result{Text: f.result}, nil
}

func TestRecognizerAdaptsResultAndMetadata(t *testing.T) {
	f := &fakeASR{result: "transcribed"}
	r := recognizer{p: f, hotwords: []string{"bbclaw"}}

	got, err := r.Transcribe(context.Background(), []byte("pcm"))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if got != "transcribed" {
		t.Errorf("text = %q, want %q", got, "transcribed")
	}
	// The wrapper reports the post-decode device spec to the provider.
	if f.gotMeta.Codec != pcmCodec || f.gotMeta.SampleRate != deviceRate || f.gotMeta.Channels != deviceChans {
		t.Errorf("meta = %+v, want pcm16/16000/1", f.gotMeta)
	}
	if len(f.gotMeta.Hotwords) != 1 || f.gotMeta.Hotwords[0] != "bbclaw" {
		t.Errorf("hotwords = %v, want [bbclaw]", f.gotMeta.Hotwords)
	}
}

func TestRecognizerPropagatesError(t *testing.T) {
	r := recognizer{p: &fakeASR{err: errors.New("boom")}}
	if _, err := r.Transcribe(context.Background(), []byte("x")); err == nil {
		t.Fatal("want error propagated from provider")
	}
}

// fakeTTS is a stand-in tts.Provider with an optional OutputFormat.
type fakeTTS struct {
	out    []byte
	format string
}

func (f fakeTTS) Synthesize(context.Context, string) ([]byte, error) { return f.out, nil }
func (f fakeTTS) OutputFormat() string                               { return f.format }

func TestWrapTTSPrefersProviderFormat(t *testing.T) {
	s := wrapTTS(fakeTTS{out: []byte("a"), format: "mp3"}, "wav")
	if s.OutputFormat() != "mp3" {
		t.Errorf("OutputFormat = %q, want mp3 (provider's own)", s.OutputFormat())
	}
	audio, _ := s.Synthesize(context.Background(), "hi")
	if string(audio) != "a" {
		t.Errorf("Synthesize = %q, want a", audio)
	}
}

func TestFromEnvDefaultsToMockASR(t *testing.T) {
	// No ASR_PROVIDER set → fixed-transcript recognizer (mode "static-mock").
	t.Setenv("ASR_PROVIDER", "")
	rec, _, asrMode, _ := FromEnv()
	if asrMode != "static-mock" {
		t.Errorf("asrMode = %q, want static-mock with no ASR_PROVIDER", asrMode)
	}
	got, err := rec.Transcribe(context.Background(), []byte("ignored"))
	if err != nil || got == "" {
		t.Errorf("mock recognizer should return a fixed transcript, got %q err=%v", got, err)
	}
}

func TestFromEnvLocalASRWiresRealBackend(t *testing.T) {
	t.Setenv("ASR_PROVIDER", "local")
	t.Setenv("ASR_LOCAL_BIN", "/bin/echo")
	_, _, asrMode, _ := FromEnv()
	if asrMode != "local" {
		t.Errorf("asrMode = %q, want local for ASR_PROVIDER=local with ASR_LOCAL_BIN set", asrMode)
	}
}

func TestFromEnvLocalASRMissingBinFallsBack(t *testing.T) {
	t.Setenv("ASR_PROVIDER", "local")
	t.Setenv("ASR_LOCAL_BIN", "") // required but missing → fall back, not crash
	_, _, asrMode, _ := FromEnv()
	if asrMode != "static-mock" {
		t.Errorf("asrMode = %q, want static-mock when ASR_LOCAL_BIN is missing", asrMode)
	}
}

func TestSplitList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{{"", 0}, {"bbclaw", 1}, {"a, b, c", 3}, {"a b,c", 3}, {" , ", 0}} {
		if got := len(splitList(tc.in)); got != tc.want {
			t.Errorf("splitList(%q) len = %d, want %d", tc.in, got, tc.want)
		}
	}
}
