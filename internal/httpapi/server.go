package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/asr"
	"github.com/daboluocc/bbclaw/adapter/internal/audio"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/openclaw"
)

type AppConfig struct {
	AuthToken            string
	NodeID               string
	LocalIngressEnabled  bool
	CloudRelayEnabled    bool
	CloudStatus          func() map[string]any
	SaveAudio            bool
	SaveInputOnFinish    bool
	AudioInDir           string
	AudioOutDir          string
	ASRTranscribeTimeout time.Duration
}

type ASRProvider interface {
	Transcribe(ctx context.Context, audio []byte, meta asr.Metadata) (asr.Result, error)
}

type OpenClawSink interface {
	SendVoiceTranscript(ctx context.Context, event openclaw.VoiceTranscriptEvent) (openclaw.VoiceTranscriptDelivery, error)
	SendVoiceTranscriptStream(
		ctx context.Context,
		event openclaw.VoiceTranscriptEvent,
		onEvent func(openclaw.VoiceTranscriptStreamEvent),
	) (openclaw.VoiceTranscriptDelivery, error)
}

type TTSProvider interface {
	Synthesize(ctx context.Context, text string) ([]byte, error)
}

// TTSFormatProvider is optionally implemented by TTS providers that output non-mp3 audio.
type TTSFormatProvider interface {
	OutputFormat() string
}

type Server struct {
	cfg     AppConfig
	streams *audio.Manager
	asr     ASRProvider
	tts     TTSProvider
	sink    OpenClawSink
	display *displayTaskQueue
	router  *agent.Router // optional; set via SetAgentDriver / SetAgentRouter
	log     *obs.Logger
	metrics *obs.Metrics

	// agentCtx is a long-lived context for agent sessions so they can survive
	// across HTTP requests. Cancelled via (*Server).Shutdown — main.go wires
	// that into the SIGINT/SIGTERM graceful shutdown path.
	agentCtx      context.Context
	agentCancel   context.CancelFunc
	agentSessions *sessionRegistry
}

func NewServer(cfg AppConfig, streams *audio.Manager, asrProvider ASRProvider, ttsProvider TTSProvider, sink OpenClawSink, logger *obs.Logger, metrics *obs.Metrics) *Server {
	return &Server{
		cfg:     cfg,
		streams: streams,
		asr:     asrProvider,
		tts:     ttsProvider,
		sink:    sink,
		display: newDisplayTaskQueue(128),
		log:     logger,
		metrics: metrics,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /v1/stream/start", s.withAuth(s.handleStart))
	mux.HandleFunc("POST /v1/stream/chunk", s.withAuth(s.handleChunk))
	mux.HandleFunc("POST /v1/stream/finish", s.withAuth(s.handleFinish))
	mux.HandleFunc("POST /v1/tts/synthesize", s.withAuth(s.handleTTSSynthesize))
	mux.HandleFunc("POST /v1/display/task", s.withAuth(s.handleDisplayTask))
	mux.HandleFunc("POST /v1/display/pull", s.withAuth(s.handleDisplayPull))
	mux.HandleFunc("POST /v1/display/ack", s.withAuth(s.handleDisplayAck))
	mux.HandleFunc("POST /v1/agent/message", s.withAuth(s.handleAgentMessage))
	mux.HandleFunc("GET /v1/agent/drivers", s.withAuth(s.handleAgentDrivers))
	// Playground is unauthenticated on purpose — it's a dev-only single-page
	// UI for dogfooding agent drivers. Protect your adapter by not exposing
	// it to the internet.
	mux.HandleFunc("GET /playground", s.handlePlayground)
	return mux
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(s.cfg.AuthToken) == "" {
			next(w, r)
			return
		}
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if auth != "Bearer "+s.cfg.AuthToken {
			writeJSON(w, http.StatusUnauthorized, response{OK: false, Error: "UNAUTHORIZED"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	s.metrics.Inc("healthz_ok")
	s.log.Infof("phase=healthz remote=%s ua=%q", clientHost(r), strings.TrimSpace(r.Header.Get("User-Agent")))
	status := "ok"
	data := map[string]any{
		"status":  status,
		"metrics": s.metrics.Snapshot(),
		"local": map[string]any{
			"enabled": s.cfg.LocalIngressEnabled,
			"ready":   s.cfg.LocalIngressEnabled,
		},
	}
	if s.cfg.CloudRelayEnabled {
		cloud := map[string]any{
			"enabled": true,
		}
		if s.cfg.CloudStatus != nil {
			for k, v := range s.cfg.CloudStatus() {
				cloud[k] = v
			}
			if connected, ok := cloud["connected"].(bool); ok && !connected {
				status = "degraded"
				data["status"] = status
			}
		}
		data["cloud"] = cloud
	}
	writeJSON(w, http.StatusOK, response{
		OK:   true,
		Data: data,
	})
}

func clientHost(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type startRequest struct {
	DeviceID   string `json:"deviceId"`
	SessionKey string `json:"sessionKey"`
	StreamID   string `json:"streamId"`
	Codec      string `json:"codec"`
	SampleRate int    `json:"sampleRate"`
	Channels   int    `json:"channels"`
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST"})
		return
	}
	err := s.streams.Start(audio.StartRequest{
		DeviceID:   req.DeviceID,
		SessionKey: req.SessionKey,
		StreamID:   req.StreamID,
		Codec:      req.Codec,
		SampleRate: req.SampleRate,
		Channels:   req.Channels,
	})
	if err != nil {
		code := "INVALID_REQUEST"
		if errors.Is(err, audio.ErrBusy) {
			code = "TOO_MANY_STREAMS"
		}
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: code})
		return
	}
	s.metrics.Inc("stream_start_ok")
	s.log.Infof("phase=stream_start stream=%s session=%s device=%s codec=%s sample_rate=%d ch=%d",
		req.StreamID, req.SessionKey, req.DeviceID, req.Codec, req.SampleRate, req.Channels)
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"streamId": req.StreamID}})
}

type chunkRequest struct {
	DeviceID    string `json:"deviceId"`
	SessionKey  string `json:"sessionKey"`
	StreamID    string `json:"streamId"`
	Seq         int    `json:"seq"`
	TimestampMs int64  `json:"timestampMs"`
	AudioBase64 string `json:"audioBase64"`
}

func (s *Server) handleChunk(w http.ResponseWriter, r *http.Request) {
	var req chunkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST"})
		return
	}
	payload, err := base64.StdEncoding.DecodeString(req.AudioBase64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_AUDIO_BASE64"})
		return
	}
	err = s.streams.AppendChunk(req.StreamID, audio.Chunk{
		Seq:         req.Seq,
		TimestampMs: req.TimestampMs,
		Payload:     payload,
	})
	if err != nil {
		code := "INVALID_REQUEST"
		switch {
		case errors.Is(err, audio.ErrUnknownStream):
			code = "STREAM_NOT_FOUND"
		case errors.Is(err, audio.ErrInvalidSequence):
			code = "INVALID_SEQUENCE"
		case errors.Is(err, audio.ErrAudioTooLarge):
			code = "AUDIO_TOO_LARGE"
		case errors.Is(err, audio.ErrDurationTooLong):
			code = "AUDIO_TOO_LONG"
		}
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: code})
		return
	}
	s.metrics.Inc("stream_chunk_ok")
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"seq": req.Seq}})
}

type finishRequest struct {
	DeviceID   string `json:"deviceId"`
	SessionKey string `json:"sessionKey"`
	StreamID   string `json:"streamId"`
	ReplyMode  string `json:"replyMode,omitempty"`
}

func (s *Server) handleFinish(w http.ResponseWriter, r *http.Request) {
	var req finishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST"})
		return
	}
	stream, err := s.streams.Finish(req.StreamID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "STREAM_NOT_FOUND"})
		return
	}
	t0 := time.Now()
	s.log.Infof("phase=finish stream=%s session=%s device=%s elapsed_s=0",
		stream.StreamID, stream.SessionKey, stream.DeviceID)

	pcmAudio, codecErrCode, err := normalizeAudioForASR(r.Context(), stream)
	if err != nil {
		s.log.Errorf("phase=pcm_decode_failed stream=%s elapsed_s=%.3f err=%v codec=%s",
			stream.StreamID, time.Since(t0).Seconds(), err, stream.Codec)
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: codecErrCode})
		return
	}
	s.log.Infof("phase=pcm_ready stream=%s elapsed_s=%.3f pcm_bytes=%d codec=%s sr=%d ch=%d",
		stream.StreamID, time.Since(t0).Seconds(), len(pcmAudio), stream.Codec, stream.SampleRate, stream.Channels)

	s.metrics.Inc("stream_finish_received")
	inputSavedPath := ""
	if s.cfg.SaveAudio || s.cfg.SaveInputOnFinish {
		inputSavedPath, err = saveAudioFile(s.cfg.AudioInDir, stream.StreamID, ".pcm", pcmAudio)
		if err != nil {
			s.metrics.Inc("audio_save_failed")
			s.log.Errorf("phase=input_save_failed stream=%s elapsed_s=%.3f err=%v",
				stream.StreamID, time.Since(t0).Seconds(), err)
		} else {
			s.metrics.Inc("audio_save_input_ok")
			s.log.Infof("phase=input_saved stream=%s elapsed_s=%.3f path=%s",
				stream.StreamID, time.Since(t0).Seconds(), inputSavedPath)
		}
	}

	s.log.Infof("phase=asr_start stream=%s elapsed_s=%.3f pcm_bytes=%d asr_timeout_s=%.3f",
		stream.StreamID, time.Since(t0).Seconds(), len(pcmAudio), s.asrTranscribeTimeout().Seconds())

	if strings.EqualFold(strings.TrimSpace(req.ReplyMode), "stream") {
		s.handleFinishStream(w, r, stream, pcmAudio, inputSavedPath, t0)
		return
	}

	asrCtx, asrCancel := context.WithTimeout(r.Context(), s.asrTranscribeTimeout())
	defer asrCancel()
	tASR := time.Now()
	result, err := s.asr.Transcribe(asrCtx, pcmAudio, asr.Metadata{
		DeviceID:   stream.DeviceID,
		SessionKey: stream.SessionKey,
		StreamID:   stream.StreamID,
		Codec:      stream.Codec,
		SampleRate: stream.SampleRate,
		Channels:   stream.Channels,
	})
	if err != nil {
		var apiErr *asr.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "ASR_EMPTY_TRANSCRIPT" {
			s.metrics.Inc("asr_empty")
			s.log.Infof("phase=asr_done stream=%s elapsed_s=%.3f asr_elapsed_s=%.3f empty=1 (ASR_EMPTY_TRANSCRIPT)",
				stream.StreamID, time.Since(t0).Seconds(), time.Since(tASR).Seconds())
			writeJSON(w, http.StatusOK, response{
				OK: true,
				Data: map[string]any{
					"streamId":       stream.StreamID,
					"text":           "",
					"savedInputPath": inputSavedPath,
				},
			})
			return
		}
		s.metrics.Inc("asr_failed")
		code, detail, status := asrHTTPError(err)
		s.log.Errorf("phase=asr_failed stream=%s elapsed_s=%.3f asr_elapsed_s=%.3f code=%s detail=%s err=%v",
			stream.StreamID, time.Since(t0).Seconds(), time.Since(tASR).Seconds(), code, detail, err)
		writeJSON(w, status, response{OK: false, Error: code, Detail: detail})
		return
	}
	s.metrics.Inc("asr_ok")
	trChars := utf8.RuneCountInString(result.Text)
	s.log.Infof("phase=asr_done stream=%s elapsed_s=%.3f asr_elapsed_s=%.3f text_chars=%d text=%q",
		stream.StreamID, time.Since(t0).Seconds(), time.Since(tASR).Seconds(), trChars, result.Text)

	s.log.Infof("phase=openclaw_send stream=%s elapsed_s=%.3f session=%s transcript_chars=%d",
		stream.StreamID, time.Since(t0).Seconds(), stream.SessionKey, trChars)
	delivery, err := s.sink.SendVoiceTranscript(r.Context(), openclaw.VoiceTranscriptEvent{
		Text:       result.Text,
		SessionKey: stream.SessionKey,
		StreamID:   stream.StreamID,
		Source:     "bbclaw.adapter",
		NodeID:     s.cfg.NodeID,
	})
	if err != nil {
		s.metrics.Inc("openclaw_delivery_failed")
		s.log.Errorf("phase=openclaw_failed stream=%s elapsed_s=%.3f err=%v",
			stream.StreamID, time.Since(t0).Seconds(), err)
		writeJSON(w, http.StatusBadGateway, response{OK: false, Error: "OPENCLAW_DELIVERY_FAILED"})
		return
	}
	s.metrics.Inc("openclaw_delivery_ok")
	replyText := strings.TrimSpace(delivery.ReplyText)
	replyPreview := replyText
	if utf8.RuneCountInString(replyPreview) > 120 {
		replyPreview = string([]rune(replyPreview)[:120]) + "…"
	}
	switch {
	case replyText != "":
		s.log.Infof("phase=openclaw_reply stream=%s elapsed_s=%.3f session=%s reply_chars=%d reply=%q",
			stream.StreamID, time.Since(t0).Seconds(), stream.SessionKey, utf8.RuneCountInString(replyText), replyPreview)
	case delivery.ReplyWaitTimedOut:
		s.log.Infof("phase=openclaw_reply stream=%s elapsed_s=%.3f session=%s wait_timeout=1",
			stream.StreamID, time.Since(t0).Seconds(), stream.SessionKey)
	default:
		s.log.Infof("phase=openclaw_reply stream=%s elapsed_s=%.3f session=%s reply_empty=1",
			stream.StreamID, time.Since(t0).Seconds(), stream.SessionKey)
	}

	s.log.Infof("phase=http_response_ok stream=%s elapsed_total_s=%.3f",
		stream.StreamID, time.Since(t0).Seconds())

	writeJSON(w, http.StatusOK, response{
		OK: true,
		Data: map[string]any{
			"streamId":       stream.StreamID,
			"text":           result.Text,
			"replyText":      replyText,
			"savedInputPath": inputSavedPath,
		},
	})
}

type finishStreamWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
}

func newFinishStreamWriter(w http.ResponseWriter) (*finishStreamWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &finishStreamWriter{w: w, flusher: flusher}, true
}

func (sw *finishStreamWriter) write(event map[string]any) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if err := json.NewEncoder(sw.w).Encode(event); err != nil {
		return err
	}
	sw.flusher.Flush()
	return nil
}

func (s *Server) handleFinishStream(
	w http.ResponseWriter,
	r *http.Request,
	stream audio.FinishedStream,
	pcmAudio []byte,
	inputSavedPath string,
	t0 time.Time,
) {
	sw, ok := newFinishStreamWriter(w)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "STREAMING_NOT_SUPPORTED"})
		return
	}

	_ = sw.write(map[string]any{"type": "status", "phase": "transcribing"})

	asrCtx, asrCancel := context.WithTimeout(r.Context(), s.asrTranscribeTimeout())
	defer asrCancel()
	tASR := time.Now()
	result, err := s.asr.Transcribe(asrCtx, pcmAudio, asr.Metadata{
		DeviceID:   stream.DeviceID,
		SessionKey: stream.SessionKey,
		StreamID:   stream.StreamID,
		Codec:      stream.Codec,
		SampleRate: stream.SampleRate,
		Channels:   stream.Channels,
	})
	if err != nil {
		var apiErr *asr.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "ASR_EMPTY_TRANSCRIPT" {
			s.metrics.Inc("asr_empty")
			_ = sw.write(map[string]any{"type": "asr.final", "text": ""})
			_ = sw.write(map[string]any{
				"type":              "done",
				"streamId":          stream.StreamID,
				"text":              "",
				"replyText":         "",
				"replyWaitTimedOut": false,
				"savedInputPath":    inputSavedPath,
			})
			return
		}
		s.metrics.Inc("asr_failed")
		code, detail, _ := asrHTTPError(err)
		s.log.Errorf("phase=asr_failed stream=%s elapsed_s=%.3f asr_elapsed_s=%.3f code=%s detail=%s err=%v",
			stream.StreamID, time.Since(t0).Seconds(), time.Since(tASR).Seconds(), code, detail, err)
		_ = sw.write(map[string]any{"type": "error", "error": code, "detail": detail})
		return
	}
	s.metrics.Inc("asr_ok")
	trChars := utf8.RuneCountInString(result.Text)
	s.log.Infof("phase=asr_done stream=%s elapsed_s=%.3f asr_elapsed_s=%.3f text_chars=%d text=%q",
		stream.StreamID, time.Since(t0).Seconds(), time.Since(tASR).Seconds(), trChars, result.Text)
	_ = sw.write(map[string]any{"type": "asr.final", "text": result.Text})

	if strings.TrimSpace(result.Text) == "" {
		_ = sw.write(map[string]any{
			"type":              "done",
			"streamId":          stream.StreamID,
			"text":              "",
			"replyText":         "",
			"replyWaitTimedOut": false,
			"savedInputPath":    inputSavedPath,
		})
		return
	}

	_ = sw.write(map[string]any{"type": "status", "phase": "processing"})
	s.log.Infof("phase=openclaw_send stream=%s elapsed_s=%.3f session=%s transcript_chars=%d",
		stream.StreamID, time.Since(t0).Seconds(), stream.SessionKey, trChars)
	delivery, err := s.sink.SendVoiceTranscriptStream(r.Context(), openclaw.VoiceTranscriptEvent{
		Text:       result.Text,
		SessionKey: stream.SessionKey,
		StreamID:   stream.StreamID,
		Source:     "bbclaw.adapter",
		NodeID:     s.cfg.NodeID,
	}, func(evt openclaw.VoiceTranscriptStreamEvent) {
		switch evt.Type {
		case "reply.delta":
			if strings.TrimSpace(evt.Text) != "" {
				_ = sw.write(map[string]any{"type": "reply.delta", "text": evt.Text})
			}
		case "thinking":
			_ = sw.write(map[string]any{"type": "thinking", "text": evt.Text})
		case "tool_call":
			_ = sw.write(map[string]any{"type": "tool_call", "name": evt.Text})
		}
	})
	if err != nil {
		s.metrics.Inc("openclaw_delivery_failed")
		s.log.Errorf("phase=openclaw_failed stream=%s elapsed_s=%.3f err=%v",
			stream.StreamID, time.Since(t0).Seconds(), err)
		_ = sw.write(map[string]any{"type": "error", "error": "OPENCLAW_DELIVERY_FAILED", "detail": err.Error()})
		return
	}
	s.metrics.Inc("openclaw_delivery_ok")
	replyText := strings.TrimSpace(delivery.ReplyText)
	replyPreview := replyText
	if utf8.RuneCountInString(replyPreview) > 120 {
		replyPreview = string([]rune(replyPreview)[:120]) + "…"
	}
	switch {
	case replyText != "":
		s.log.Infof("phase=openclaw_reply stream=%s elapsed_s=%.3f session=%s reply_chars=%d reply=%q",
			stream.StreamID, time.Since(t0).Seconds(), stream.SessionKey, utf8.RuneCountInString(replyText), replyPreview)
	case delivery.ReplyWaitTimedOut:
		s.log.Infof("phase=openclaw_reply stream=%s elapsed_s=%.3f session=%s wait_timeout=1",
			stream.StreamID, time.Since(t0).Seconds(), stream.SessionKey)
	default:
		s.log.Infof("phase=openclaw_reply stream=%s elapsed_s=%.3f session=%s reply_empty=1",
			stream.StreamID, time.Since(t0).Seconds(), stream.SessionKey)
	}
	s.log.Infof("phase=http_response_ok stream=%s elapsed_total_s=%.3f",
		stream.StreamID, time.Since(t0).Seconds())
	_ = sw.write(map[string]any{
		"type":              "done",
		"streamId":          stream.StreamID,
		"text":              result.Text,
		"replyText":         replyText,
		"replyWaitTimedOut": delivery.ReplyWaitTimedOut,
		"savedInputPath":    inputSavedPath,
	})
}

type ttsSynthesizeRequest struct {
	Text       string `json:"text"`
	Codec      string `json:"codec,omitempty"`
	SampleRate int    `json:"sampleRate,omitempty"`
	Channels   int    `json:"channels,omitempty"`
}

func (s *Server) handleTTSSynthesize(w http.ResponseWriter, r *http.Request) {
	if s.tts == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "TTS_NOT_CONFIGURED"})
		return
	}
	var req ttsSynthesizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST"})
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "EMPTY_TEXT"})
		return
	}
	tTTS := time.Now()
	textChars := utf8.RuneCountInString(req.Text)
	s.log.Infof("phase=tts_start elapsed_s=0 text_chars=%d", textChars)
	audioBytes, err := s.tts.Synthesize(r.Context(), req.Text)
	if err != nil {
		s.metrics.Inc("tts_failed")
		s.log.Errorf("phase=tts_failed elapsed_s=%.3f err=%v", time.Since(tTTS).Seconds(), err)
		writeJSON(w, http.StatusBadGateway, response{OK: false, Error: "TTS_FAILED"})
		return
	}
	s.metrics.Inc("tts_ok")
	s.log.Infof("phase=tts_done elapsed_s=%.3f audio_bytes=%d", time.Since(tTTS).Seconds(), len(audioBytes))

	outputCodec := strings.ToLower(strings.TrimSpace(req.Codec))
	if outputCodec == "" {
		outputCodec = "mp3"
	}
	ttsFormat := "mp3"
	if fp, ok := s.tts.(TTSFormatProvider); ok && fp.OutputFormat() != "" {
		ttsFormat = fp.OutputFormat()
	}
	outAudio := audioBytes
	outFormat := ttsFormat
	outSampleRate := 0
	outChannels := 0
	if outputCodec == "pcm16" || outputCodec == "pcm_s16le" {
		sr := req.SampleRate
		ch := req.Channels
		if sr <= 0 {
			sr = 16000
		}
		if ch <= 0 {
			ch = 1
		}
		decoded, decodeErr := audio.DecodeMediaToPCM16LE(r.Context(), ttsFormat, sr, ch, audioBytes)
		if decodeErr != nil {
			s.metrics.Inc("tts_failed")
			s.log.Errorf("tts pcm transcode failed err=%v", decodeErr)
			writeJSON(w, http.StatusBadGateway, response{OK: false, Error: "TTS_TRANSCODE_FAILED"})
			return
		}
		outAudio = decoded
		outFormat = "pcm16"
		outSampleRate = sr
		outChannels = ch
	}

	outputSavedPath := ""
	if s.cfg.SaveAudio {
		suffix := "." + outFormat
		outputSavedPath, err = saveAudioFile(s.cfg.AudioOutDir, fmt.Sprintf("tts-%d", time.Now().UnixMilli()), suffix, outAudio)
		if err != nil {
			s.metrics.Inc("audio_save_failed")
			s.log.Errorf("save tts audio failed err=%v", err)
		} else {
			s.metrics.Inc("audio_save_output_ok")
		}
	}
	writeJSON(w, http.StatusOK, response{
		OK: true,
		Data: map[string]any{
			"text":            req.Text,
			"audioBase64":     base64.StdEncoding.EncodeToString(outAudio),
			"format":          outFormat,
			"sampleRate":      outSampleRate,
			"channels":        outChannels,
			"savedOutputPath": outputSavedPath,
		},
	})
}

func (s *Server) handleDisplayTask(w http.ResponseWriter, r *http.Request) {
	var task displayTask
	if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST"})
		return
	}
	task.DeviceID = strings.TrimSpace(task.DeviceID)
	if task.DeviceID == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "DEVICE_ID_REQUIRED"})
		return
	}
	queued := s.display.enqueue(task)
	s.metrics.Inc("display_task_enqueued")
	writeJSON(w, http.StatusOK, response{
		OK: true,
		Data: map[string]any{
			"taskId":   queued.TaskID,
			"deviceId": queued.DeviceID,
		},
	})
}

type displayPullRequest struct {
	DeviceID string `json:"deviceId"`
}

func (s *Server) handleDisplayPull(w http.ResponseWriter, r *http.Request) {
	var req displayPullRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST"})
		return
	}
	deviceID := strings.TrimSpace(req.DeviceID)
	if deviceID == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "DEVICE_ID_REQUIRED"})
		return
	}
	task, ok := s.display.pull(deviceID)
	if !ok {
		writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"task": nil}})
		return
	}
	s.metrics.Inc("display_task_pulled")
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"task": task}})
}

func (s *Server) handleDisplayAck(w http.ResponseWriter, r *http.Request) {
	var ack displayAck
	if err := json.NewDecoder(r.Body).Decode(&ack); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST"})
		return
	}
	if strings.TrimSpace(ack.DeviceID) == "" || strings.TrimSpace(ack.TaskID) == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST"})
		return
	}
	s.metrics.Inc("display_ack")
	writeJSON(w, http.StatusOK, response{
		OK: true,
		Data: map[string]any{
			"deviceId": ack.DeviceID,
			"taskId":   ack.TaskID,
			"actionId": strings.TrimSpace(ack.ActionID),
		},
	})
}

func normalizeAudioForASR(ctx context.Context, stream audio.FinishedStream) ([]byte, string, error) {
	switch strings.ToLower(strings.TrimSpace(stream.Codec)) {
	case "pcm16", "pcm_s16le", "opus":
		out, err := audio.DecodeToPCM16LE(ctx, stream.Codec, stream.SampleRate, stream.Channels, stream.Audio)
		if err != nil {
			return nil, "OPUS_DECODE_FAILED", err
		}
		return out, "", nil
	default:
		return nil, "UNSUPPORTED_CODEC", fmt.Errorf("unsupported codec: %s", stream.Codec)
	}
}

func saveAudioFile(dir, stem, ext string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	safeStem := sanitizeFileName(stem)
	filename := fmt.Sprintf("%s-%d%s", safeStem, time.Now().UnixMilli(), ext)
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write file %s: %w", path, err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return path, nil
	}
	return absPath, nil
}

func sanitizeFileName(input string) string {
	replacer := strings.NewReplacer("/", "-", "\\", "-", " ", "_", ":", "-", "\n", "")
	out := replacer.Replace(strings.TrimSpace(input))
	if out == "" {
		return "audio"
	}
	return out
}

type response struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`
	Data   any    `json:"data,omitempty"`
}

func (s *Server) asrTranscribeTimeout() time.Duration {
	if s.cfg.ASRTranscribeTimeout > 0 {
		return s.cfg.ASRTranscribeTimeout
	}
	return 10 * time.Second
}

func asrHTTPError(err error) (code string, detail string, status int) {
	if errors.Is(err, context.DeadlineExceeded) {
		return "ASR_TIMEOUT", "transcription exceeded ASR_TRANSCRIBE_TIMEOUT_SECONDS", http.StatusGatewayTimeout
	}
	if errors.Is(err, context.Canceled) {
		return "ASR_CANCELED", "request canceled", http.StatusBadGateway
	}
	var apiErr *asr.APIError
	if errors.As(err, &apiErr) {
		msg := apiErr.Message
		if strings.TrimSpace(msg) == "" {
			msg = apiErr.Error()
		}
		return apiErr.Code, msg, http.StatusBadGateway
	}
	return "ASR_FAILED", err.Error(), http.StatusBadGateway
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
