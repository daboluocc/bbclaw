package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/asr"
	"github.com/daboluocc/bbclaw/adapter/internal/audio"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/openclaw"
)

type fakeASR struct {
	result asr.Result
	err    error
}

func (f fakeASR) Transcribe(_ context.Context, _ []byte, _ asr.Metadata) (asr.Result, error) {
	return f.result, f.err
}

type fakeSink struct {
	event        openclaw.VoiceTranscriptEvent
	delivery     openclaw.VoiceTranscriptDelivery
	streamEvents []openclaw.VoiceTranscriptStreamEvent
	err          error
}

func (f *fakeSink) SendVoiceTranscript(_ context.Context, event openclaw.VoiceTranscriptEvent) (openclaw.VoiceTranscriptDelivery, error) {
	f.event = event
	return f.delivery, f.err
}

func (f *fakeSink) SendVoiceTranscriptStream(
	_ context.Context,
	event openclaw.VoiceTranscriptEvent,
	onEvent func(openclaw.VoiceTranscriptStreamEvent),
) (openclaw.VoiceTranscriptDelivery, error) {
	f.event = event
	for _, evt := range f.streamEvents {
		onEvent(evt)
	}
	return f.delivery, f.err
}

type fakeTTS struct {
	audio []byte
	err   error
}

func (f fakeTTS) Synthesize(_ context.Context, _ string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.audio, nil
}

func TestStreamLifecycleSuccess(t *testing.T) {
	sink := &fakeSink{
		delivery: openclaw.VoiceTranscriptDelivery{ReplyText: "assistant reply"},
	}
	srv := NewServer(
		AppConfig{AuthToken: "token", NodeID: "node-1"},
		audio.NewManager(1024*1024, 60, 8),
		fakeASR{result: asr.Result{Text: "hello world"}},
		fakeTTS{audio: []byte("mp3data")},
		sink,
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()

	doReq(t, handler, "POST", "/v1/stream/start", map[string]any{
		"deviceId":   "dev-1",
		"sessionKey": "agent:main:main",
		"streamId":   "stream-1",
		"codec":      "pcm16",
		"sampleRate": 16000,
		"channels":   1,
	}, "token", http.StatusOK)

	doReq(t, handler, "POST", "/v1/stream/chunk", map[string]any{
		"deviceId":    "dev-1",
		"sessionKey":  "agent:main:main",
		"streamId":    "stream-1",
		"seq":         1,
		"timestampMs": 1000,
		"audioBase64": base64.StdEncoding.EncodeToString([]byte("abc")),
	}, "token", http.StatusOK)

	finishRec := doReq(t, handler, "POST", "/v1/stream/finish", map[string]any{
		"deviceId":   "dev-1",
		"sessionKey": "agent:main:main",
		"streamId":   "stream-1",
	}, "token", http.StatusOK)
	var finishResp struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(finishRec.Body.Bytes(), &finishResp)
	if !finishResp.OK {
		t.Fatalf("finish body=%s", finishRec.Body.String())
	}
	if got, _ := finishResp.Data["replyText"].(string); got != "assistant reply" {
		t.Fatalf("replyText = %q", got)
	}

	if sink.event.Text != "hello world" {
		t.Fatalf("sink text = %q", sink.event.Text)
	}
	if sink.event.StreamID != "stream-1" {
		t.Fatalf("sink stream = %q", sink.event.StreamID)
	}
}

func TestUnauthorized(t *testing.T) {
	srv := NewServer(
		AppConfig{AuthToken: "token", NodeID: "node-1"},
		audio.NewManager(1024, 60, 2),
		fakeASR{result: asr.Result{Text: "ok"}},
		fakeTTS{audio: []byte("ok")},
		&fakeSink{},
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()
	rec := doReq(t, handler, "POST", "/v1/stream/start", map[string]any{}, "", http.StatusUnauthorized)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["ok"] != false {
		t.Fatalf("ok = %v", resp["ok"])
	}
}

func TestHealthzIncludesCloudStatus(t *testing.T) {
	srv := NewServer(
		AppConfig{
			NodeID:              "node-1",
			LocalIngressEnabled: true,
			CloudRelayEnabled:   true,
			CloudStatus: func() map[string]any {
				return map[string]any{
					"connected":  false,
					"homeSiteId": "home-1",
					"lastError":  "dial cloud ws: boom",
				}
			},
		},
		audio.NewManager(1024, 60, 2),
		fakeASR{result: asr.Result{Text: "ok"}},
		fakeTTS{audio: []byte("ok")},
		&fakeSink{},
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	rec := doReq(t, srv.Handler(), "GET", "/healthz", nil, "", http.StatusOK)
	var resp struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() err = %v", err)
	}
	if got, _ := resp.Data["status"].(string); got != "degraded" {
		t.Fatalf("status = %q", got)
	}
	cloud, _ := resp.Data["cloud"].(map[string]any)
	if connected, _ := cloud["connected"].(bool); connected {
		t.Fatalf("cloud.connected = %v", cloud["connected"])
	}
}

func TestStreamLifecycleStreamMode(t *testing.T) {
	sink := &fakeSink{
		streamEvents: []openclaw.VoiceTranscriptStreamEvent{
			{Type: "reply.delta", Text: "assistant"},
			{Type: "reply.delta", Text: "assistant reply"},
		},
		delivery: openclaw.VoiceTranscriptDelivery{ReplyText: "assistant reply"},
	}
	srv := NewServer(
		AppConfig{AuthToken: "token", NodeID: "node-1"},
		audio.NewManager(1024*1024, 60, 8),
		fakeASR{result: asr.Result{Text: "hello world"}},
		fakeTTS{audio: []byte("mp3data")},
		sink,
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()

	doReq(t, handler, "POST", "/v1/stream/start", map[string]any{
		"deviceId":   "dev-1",
		"sessionKey": "agent:main:main",
		"streamId":   "stream-1",
		"codec":      "pcm16",
		"sampleRate": 16000,
		"channels":   1,
	}, "token", http.StatusOK)
	doReq(t, handler, "POST", "/v1/stream/chunk", map[string]any{
		"deviceId":    "dev-1",
		"sessionKey":  "agent:main:main",
		"streamId":    "stream-1",
		"seq":         1,
		"timestampMs": 1000,
		"audioBase64": base64.StdEncoding.EncodeToString([]byte("abc")),
	}, "token", http.StatusOK)

	body, _ := json.Marshal(map[string]any{
		"deviceId":   "dev-1",
		"sessionKey": "agent:main:main",
		"streamId":   "stream-1",
		"replyMode":  "stream",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/stream/finish", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stream finish status=%d body=%s", rec.Code, rec.Body.String())
	}
	events := decodeNDJSONLines(t, rec.Body.Bytes())
	if len(events) != 6 {
		t.Fatalf("events = %#v", events)
	}
	assertNDJSONEvent(t, events[0], "type", "status")
	assertNDJSONEvent(t, events[0], "phase", "transcribing")
	assertNDJSONEvent(t, events[1], "type", "asr.final")
	assertNDJSONEvent(t, events[1], "text", "hello world")
	assertNDJSONEvent(t, events[2], "type", "status")
	assertNDJSONEvent(t, events[2], "phase", "processing")
	assertNDJSONEvent(t, events[3], "type", "reply.delta")
	assertNDJSONEvent(t, events[3], "text", "assistant")
	assertNDJSONEvent(t, events[4], "type", "reply.delta")
	assertNDJSONEvent(t, events[4], "text", "assistant reply")
	assertNDJSONEvent(t, events[5], "type", "done")
	assertNDJSONEvent(t, events[5], "replyText", "assistant reply")
}

func TestChunkInvalidSequence(t *testing.T) {
	srv := NewServer(
		AppConfig{NodeID: "node-1"},
		audio.NewManager(1024, 60, 2),
		fakeASR{result: asr.Result{Text: "ok"}},
		fakeTTS{audio: []byte("ok")},
		&fakeSink{},
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()

	doReq(t, handler, "POST", "/v1/stream/start", map[string]any{
		"deviceId":   "dev-1",
		"sessionKey": "agent:main:main",
		"streamId":   "stream-1",
		"codec":      "pcm16",
		"sampleRate": 16000,
		"channels":   1,
	}, "", http.StatusOK)

	doReq(t, handler, "POST", "/v1/stream/chunk", map[string]any{
		"deviceId":    "dev-1",
		"sessionKey":  "agent:main:main",
		"streamId":    "stream-1",
		"seq":         2,
		"timestampMs": 1000,
		"audioBase64": base64.StdEncoding.EncodeToString([]byte("abc")),
	}, "", http.StatusBadRequest)
}

func TestTTSSynthesizeSuccess(t *testing.T) {
	srv := NewServer(
		AppConfig{AuthToken: "token", NodeID: "node-1"},
		audio.NewManager(1024, 60, 2),
		fakeASR{result: asr.Result{Text: "ok"}},
		fakeTTS{audio: []byte("mp3data")},
		&fakeSink{},
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()
	rec := doReq(t, handler, "POST", "/v1/tts/synthesize", map[string]any{
		"text": "你好",
	}, "token", http.StatusOK)
	var resp struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.OK {
		t.Fatal("expected ok=true")
	}
	if resp.Data["audioBase64"] == "" {
		t.Fatal("audioBase64 should not be empty")
	}
}

func TestTTSSynthesizeEmptyText(t *testing.T) {
	srv := NewServer(
		AppConfig{NodeID: "node-1"},
		audio.NewManager(1024, 60, 2),
		fakeASR{result: asr.Result{Text: "ok"}},
		fakeTTS{audio: []byte("mp3data")},
		&fakeSink{},
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()
	doReq(t, handler, "POST", "/v1/tts/synthesize", map[string]any{
		"text": "   ",
	}, "", http.StatusBadRequest)
}

func TestTTSSynthesizeNotConfigured(t *testing.T) {
	srv := NewServer(
		AppConfig{NodeID: "node-1"},
		audio.NewManager(1024, 60, 2),
		fakeASR{result: asr.Result{Text: "ok"}},
		nil,
		&fakeSink{},
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()
	doReq(t, handler, "POST", "/v1/tts/synthesize", map[string]any{
		"text": "hello",
	}, "", http.StatusNotImplemented)
}

func TestTTSSynthesizeProviderError(t *testing.T) {
	srv := NewServer(
		AppConfig{NodeID: "node-1"},
		audio.NewManager(1024, 60, 2),
		fakeASR{result: asr.Result{Text: "ok"}},
		fakeTTS{err: errors.New("boom")},
		&fakeSink{},
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()
	doReq(t, handler, "POST", "/v1/tts/synthesize", map[string]any{
		"text": "hello",
	}, "", http.StatusBadGateway)
}

func TestFinishEmptyTranscriptReturnsOK(t *testing.T) {
	sink := &fakeSink{}
	srv := NewServer(
		AppConfig{AuthToken: "token", NodeID: "node-1"},
		audio.NewManager(1024*1024, 60, 8),
		fakeASR{err: &asr.APIError{Code: "ASR_EMPTY_TRANSCRIPT", Message: "empty transcript"}},
		fakeTTS{audio: []byte("mp3data")},
		sink,
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()

	rec := runASRFlow(t, handler, "token")
	var resp struct {
		OK    bool           `json:"ok"`
		Error string         `json:"error"`
		Data  map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !resp.OK {
		t.Fatalf("ok=false body=%s", rec.Body.String())
	}
	if text, _ := resp.Data["text"].(string); text != "" {
		t.Fatalf("expected empty text, got %q", text)
	}
	if sink.event.StreamID != "" {
		t.Fatalf("sink should not receive event on empty transcript")
	}
}

func TestAudioFilesPersisted(t *testing.T) {
	inDir := filepath.Join(t.TempDir(), "in")
	outDir := filepath.Join(t.TempDir(), "out")
	sink := &fakeSink{}
	srv := NewServer(
		AppConfig{
			AuthToken:   "token",
			NodeID:      "node-1",
			SaveAudio:   true,
			AudioInDir:  inDir,
			AudioOutDir: outDir,
		},
		audio.NewManager(1024*1024, 60, 8),
		fakeASR{result: asr.Result{Text: "saved transcript"}},
		fakeTTS{audio: []byte("fake-mp3-bytes")},
		sink,
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()

	finishRec := runASRFlow(t, handler, "token")
	var finishResp struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(finishRec.Body.Bytes(), &finishResp)
	if !finishResp.OK {
		t.Fatalf("finish ok=%v body=%s", finishResp.OK, finishRec.Body.String())
	}
	savedInputPath, _ := finishResp.Data["savedInputPath"].(string)
	if savedInputPath == "" {
		t.Fatalf("savedInputPath should not be empty")
	}
	inputBytes, err := os.ReadFile(savedInputPath)
	if err != nil {
		t.Fatalf("read input file error = %v", err)
	}
	if len(inputBytes) == 0 {
		t.Fatalf("saved input should not be empty")
	}

	ttsRec := doReq(t, handler, "POST", "/v1/tts/synthesize", map[string]any{
		"text": "save tts",
	}, "token", http.StatusOK)
	var ttsResp struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(ttsRec.Body.Bytes(), &ttsResp)
	if !ttsResp.OK {
		t.Fatalf("tts ok=%v body=%s", ttsResp.OK, ttsRec.Body.String())
	}
	savedOutputPath, _ := ttsResp.Data["savedOutputPath"].(string)
	if savedOutputPath == "" {
		t.Fatalf("savedOutputPath should not be empty")
	}
	outputBytes, err := os.ReadFile(savedOutputPath)
	if err != nil {
		t.Fatalf("read output file error = %v", err)
	}
	if len(outputBytes) == 0 {
		t.Fatalf("saved output should not be empty")
	}
}

func TestFinishUnsupportedCodec(t *testing.T) {
	srv := NewServer(
		AppConfig{NodeID: "node-1"},
		audio.NewManager(1024*1024, 60, 8),
		fakeASR{result: asr.Result{Text: "ok"}},
		fakeTTS{audio: []byte("ok")},
		&fakeSink{},
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()

	doReq(t, handler, "POST", "/v1/stream/start", map[string]any{
		"deviceId":   "dev-1",
		"sessionKey": "agent:main:main",
		"streamId":   "stream-unsupported",
		"codec":      "aac",
		"sampleRate": 16000,
		"channels":   1,
	}, "", http.StatusOK)
	doReq(t, handler, "POST", "/v1/stream/chunk", map[string]any{
		"deviceId":    "dev-1",
		"sessionKey":  "agent:main:main",
		"streamId":    "stream-unsupported",
		"seq":         1,
		"timestampMs": 1000,
		"audioBase64": base64.StdEncoding.EncodeToString([]byte("abc")),
	}, "", http.StatusOK)
	rec := doReq(t, handler, "POST", "/v1/stream/finish", map[string]any{
		"deviceId":   "dev-1",
		"sessionKey": "agent:main:main",
		"streamId":   "stream-unsupported",
	}, "", http.StatusBadRequest)
	if !bytes.Contains(rec.Body.Bytes(), []byte("UNSUPPORTED_CODEC")) {
		t.Fatalf("expected UNSUPPORTED_CODEC, got: %s", rec.Body.String())
	}
}

func TestFinishOpusDecodeFailed(t *testing.T) {
	srv := NewServer(
		AppConfig{NodeID: "node-1"},
		audio.NewManager(1024*1024, 60, 8),
		fakeASR{result: asr.Result{Text: "ok"}},
		fakeTTS{audio: []byte("ok")},
		&fakeSink{},
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()

	doReq(t, handler, "POST", "/v1/stream/start", map[string]any{
		"deviceId":   "dev-1",
		"sessionKey": "agent:main:main",
		"streamId":   "stream-opus",
		"codec":      "opus",
		"sampleRate": 16000,
		"channels":   1,
	}, "", http.StatusOK)
	doReq(t, handler, "POST", "/v1/stream/chunk", map[string]any{
		"deviceId":    "dev-1",
		"sessionKey":  "agent:main:main",
		"streamId":    "stream-opus",
		"seq":         1,
		"timestampMs": 1000,
		"audioBase64": base64.StdEncoding.EncodeToString([]byte("not-opus")),
	}, "", http.StatusOK)
	rec := doReq(t, handler, "POST", "/v1/stream/finish", map[string]any{
		"deviceId":   "dev-1",
		"sessionKey": "agent:main:main",
		"streamId":   "stream-opus",
	}, "", http.StatusBadRequest)
	if !bytes.Contains(rec.Body.Bytes(), []byte("OPUS_DECODE_FAILED")) {
		t.Fatalf("expected OPUS_DECODE_FAILED, got: %s", rec.Body.String())
	}
}

func TestFinishOpusSuccessWithFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found, skip opus success test")
	}
	oggOpus, err := exec.Command("ffmpeg",
		"-hide_banner",
		"-loglevel", "error",
		"-f", "lavfi",
		"-i", "sine=frequency=440:duration=0.2:sample_rate=16000",
		"-ac", "1",
		"-c:a", "libopus",
		"-f", "opus",
		"pipe:1").Output()
	if err != nil {
		t.Skipf("ffmpeg opus generation failed: %v", err)
	}

	sink := &fakeSink{}
	srv := NewServer(
		AppConfig{NodeID: "node-1"},
		audio.NewManager(1024*1024, 60, 8),
		fakeASR{result: asr.Result{Text: "opus ok"}},
		fakeTTS{audio: []byte("ok")},
		sink,
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()

	doReq(t, handler, "POST", "/v1/stream/start", map[string]any{
		"deviceId":   "dev-1",
		"sessionKey": "agent:main:main",
		"streamId":   "stream-opus-ok",
		"codec":      "opus",
		"sampleRate": 16000,
		"channels":   1,
	}, "", http.StatusOK)
	doReq(t, handler, "POST", "/v1/stream/chunk", map[string]any{
		"deviceId":    "dev-1",
		"sessionKey":  "agent:main:main",
		"streamId":    "stream-opus-ok",
		"seq":         1,
		"timestampMs": 1000,
		"audioBase64": base64.StdEncoding.EncodeToString(oggOpus),
	}, "", http.StatusOK)
	rec := doReq(t, handler, "POST", "/v1/stream/finish", map[string]any{
		"deviceId":   "dev-1",
		"sessionKey": "agent:main:main",
		"streamId":   "stream-opus-ok",
	}, "", http.StatusOK)
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"text":"opus ok"`)) {
		t.Fatalf("unexpected finish body: %s", rec.Body.String())
	}
}

func TestDisplayTaskEnqueuePullAck(t *testing.T) {
	srv := NewServer(
		AppConfig{AuthToken: "token", NodeID: "node-1"},
		audio.NewManager(1024*1024, 60, 8),
		fakeASR{result: asr.Result{Text: "ok"}},
		fakeTTS{audio: []byte("ok")},
		&fakeSink{},
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()

	enqueueRec := doReq(t, handler, "POST", "/v1/display/task", map[string]any{
		"deviceId": "bbclaw-device-1",
		"title":    "Build Failed",
		"body":     "CI red",
		"blocks": []map[string]any{
			{"type": "kv", "label": "repo", "value": "openclaw"},
			{"type": "text", "text": "main branch failed"},
		},
		"actions": []map[string]any{
			{"id": "ack", "label": "Acknowledge"},
		},
	}, "token", http.StatusOK)

	var enqueueResp struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(enqueueRec.Body.Bytes(), &enqueueResp)
	if !enqueueResp.OK {
		t.Fatalf("enqueue body=%s", enqueueRec.Body.String())
	}
	taskID, _ := enqueueResp.Data["taskId"].(string)
	if taskID == "" {
		t.Fatalf("expected taskId, body=%s", enqueueRec.Body.String())
	}

	pullRec := doReq(t, handler, "POST", "/v1/display/pull", map[string]any{
		"deviceId": "bbclaw-device-1",
	}, "token", http.StatusOK)
	if !bytes.Contains(pullRec.Body.Bytes(), []byte(`"taskId":"`)) {
		t.Fatalf("expected task in pull body=%s", pullRec.Body.String())
	}
	if !bytes.Contains(pullRec.Body.Bytes(), []byte(`"displayText":"Build Failed | CI red | repo: openclaw | main branch failed"`)) {
		t.Fatalf("expected rendered displayText body=%s", pullRec.Body.String())
	}

	ackRec := doReq(t, handler, "POST", "/v1/display/ack", map[string]any{
		"deviceId": "bbclaw-device-1",
		"taskId":   taskID,
		"actionId": "shown",
	}, "token", http.StatusOK)
	if !bytes.Contains(ackRec.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("expected ack ok body=%s", ackRec.Body.String())
	}
}

func TestDisplayPullEmptyQueue(t *testing.T) {
	srv := NewServer(
		AppConfig{NodeID: "node-1"},
		audio.NewManager(1024, 60, 2),
		fakeASR{result: asr.Result{Text: "ok"}},
		fakeTTS{audio: []byte("ok")},
		&fakeSink{},
		obs.NewLogger(),
		obs.NewMetrics(),
	)
	handler := srv.Handler()
	rec := doReq(t, handler, "POST", "/v1/display/pull", map[string]any{
		"deviceId": "bbclaw-device-1",
	}, "", http.StatusOK)
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"task":null`)) {
		t.Fatalf("expected null task body=%s", rec.Body.String())
	}
}

func runASRFlow(t *testing.T, handler http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	doReq(t, handler, "POST", "/v1/stream/start", map[string]any{
		"deviceId":   "dev-1",
		"sessionKey": "agent:main:main",
		"streamId":   "stream-keep",
		"codec":      "pcm16",
		"sampleRate": 16000,
		"channels":   1,
	}, token, http.StatusOK)

	doReq(t, handler, "POST", "/v1/stream/chunk", map[string]any{
		"deviceId":    "dev-1",
		"sessionKey":  "agent:main:main",
		"streamId":    "stream-keep",
		"seq":         1,
		"timestampMs": 1000,
		"audioBase64": base64.StdEncoding.EncodeToString([]byte("abc")),
	}, token, http.StatusOK)

	return doReq(t, handler, "POST", "/v1/stream/finish", map[string]any{
		"deviceId":   "dev-1",
		"sessionKey": "agent:main:main",
		"streamId":   "stream-keep",
	}, token, http.StatusOK)
}

func decodeNDJSONLines(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	var events []map[string]any
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("json.Unmarshal(%q) error = %v", line, err)
		}
		events = append(events, evt)
	}
	return events
}

func assertNDJSONEvent(t *testing.T, evt map[string]any, key string, want any) {
	t.Helper()
	if got := evt[key]; got != want {
		t.Fatalf("event[%s] = %#v, want %#v (event=%#v)", key, got, want, evt)
	}
}

func doReq(t *testing.T, handler http.Handler, method, path string, payload any, token string, expectStatus int) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != expectStatus {
		t.Fatalf("%s %s status=%d body=%s", method, path, rec.Code, rec.Body.String())
	}
	return rec
}
