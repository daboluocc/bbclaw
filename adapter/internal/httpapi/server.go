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
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/daboluocc/bbclaw/adapter/internal/adminui"
	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/driverstate"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/voice/asr"
	"github.com/daboluocc/bbclaw/voice/audio"
	"github.com/daboluocc/bbclaw/adapter/internal/butler"
	"github.com/daboluocc/bbclaw/adapter/internal/config"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/openclaw"
	"github.com/daboluocc/bbclaw/adapter/internal/projectstore"
	"github.com/daboluocc/bbclaw/adapter/internal/settingsstore"
	"github.com/daboluocc/bbclaw/voice/tts"
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
	SessionReuseWindow   time.Duration     // 0 disables reuse
	SessionMaxAge        time.Duration     // 0 disables sweep
	CwdPool              []config.CwdEntry // populated from BBCLAW_CWD_POOL / BBCLAW_DEFAULT_CWD
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

	// sessions is the logical-session table (ADR-014). Optional: when nil
	// the LAN-direct agent path falls back to legacy behaviour. Set via
	// SetSessionManager from main.go.
	sessions *logicalsession.Manager

	// driverState persists user-mutable driver preferences (active driver
	// name + per-driver active model). Optional: when nil the active driver
	// is the router's default and active_model falls back to driver-default.
	// Set via SetDriverState from main.go.
	driverState *driverstate.Store

	// butlerWorkspace is the per-device butler workspace cwd (ADR-021 §1). When
	// non-empty (wired by main.go), every /v1/agent/message turn is routed to
	// the device's butler logical session instead of honouring the device's
	// requested driver/session. Empty keeps the legacy multi-session behaviour.
	butlerWorkspace string
	// butlerMCPServers are the butler's dispatch MCP servers (ADR-021 §2 /
	// ADR-024 §5, format-neutral spec), passed to the engine so the butler
	// session can dispatch workers via whichever driver backs it.
	butlerMCPServers []agent.MCPServerSpec
	// memoryWriter is the butler long-term-memory write side (ADR-021 §4).
	// Optional: nil means the engine skips the memory step. Wired by main.go from
	// the shared butlerInfra when the pipeline is enabled; the same writer is also
	// wired into the cloud-relay engine so memory works regardless of ingress.
	memoryWriter butler.MemoryWriter

	// dispatchRing is the in-memory ring buffer for butler dispatch tasks
	// (ADR-021-firmware-ui §1.4). Non-nil when butler mode is active; wired
	// via SetDispatchRing from main.go. Used by GET /v1/butler/dispatch/recent.
	dispatchRing *butler.DispatchRing

	// inflight is the process-level in-flight turn registry for barge-in
	// (ADR-028 §2.5.1). Wired via SetInflight from main.go and shared with the
	// cloud relay; backs POST /v1/agent/cancel. Optional: nil disables cancel.
	inflight *butler.InflightRegistry

	// WebSocket hub for local_home device connections + notification queue.
	wsHub      *WSHub
	notifQueue *NotificationQueue

	// dispatchRecorder is the process-level ring buffer for
	// GET /v1/butler/dispatch/recent (ADR-021-firmware-ui §1.4).
	// Wired via SetDispatchRecorder from main.go. Optional: nil disables the endpoint.
	dispatchRecorder *butler.DispatchRecorder

	// projects is the mutable project allow-list backing the local admin page
	// (GET /admin). Optional: when nil the cwd-pool surface falls back to the
	// immutable cfg.CwdPool snapshot. Wired via SetProjectStore from main.go.
	projects *projectstore.Store

	// settings is the web-mutable runtime configuration store (ADR-025): ASR/TTS,
	// Anthropic endpoint, cloud relay, OpenClaw, topology toggles. Optional: when
	// nil the settings admin endpoints return 501. Wired via SetSettingsStore.
	settings *settingsstore.Store
	// settingsRestartReq is set after a successful settings PUT and reported by
	// GET /v1/admin/settings as restart_required, so the page can show the
	// "restart to apply" banner. Cleared naturally on the next process start.
	settingsRestartReq atomic.Bool

	// Read-only identity/diagnostics surfaced on the admin page (ADR-025):
	// system-generated values the user can't usefully edit. homeSiteID is the
	// resolved device identity (env or ~/.bbclaw-adapter/identity.json), version
	// is the build tag, and logFile is the persistent runtime log path returned
	// by GET /v1/admin/logs. Wired via SetIdentity from main.go.
	homeSiteID string
	version    string
	logFile    string
}

// SetIdentity records read-only diagnostics shown on the admin page: the
// resolved home_site_id, the build version, and the runtime log file path.
// Optional; unset values simply render blank.
func (s *Server) SetIdentity(homeSiteID, version, logFile string) {
	s.homeSiteID = homeSiteID
	s.version = version
	s.logFile = logFile
}

// SetProjectStore wires the mutable project allow-list used by the local admin
// page and the live cwd-pool surface. Pass nil to keep the legacy env-only pool.
func (s *Server) SetProjectStore(store *projectstore.Store) {
	s.projects = store
}

// asrHotwords returns the live project names to bias ASR toward, so a spoken
// project reference (e.g. "bbclaw") isn't mistranscribed beyond the butler's
// ability to match it. Providers that support biasing (Whisper `prompt`) use it;
// others ignore it. The butler's own fuzzy matching is the provider-agnostic
// backstop (see workspace persona).
func (s *Server) asrHotwords() []string {
	return s.cwdPoolNames()
}

// effectivePool returns the live project allow-list: the projectstore's merged
// (env ∪ admin-added) view when wired, else the immutable env snapshot from
// config. Callers that only need names/paths read through this so an admin add
// is reflected without a restart.
func (s *Server) effectivePool() []config.CwdEntry {
	if s.projects != nil {
		list := s.projects.List()
		out := make([]config.CwdEntry, 0, len(list))
		for _, p := range list {
			out = append(out, config.CwdEntry{Name: p.Name, Path: p.Path})
		}
		return out
	}
	return s.cfg.CwdPool
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
	mux.HandleFunc("POST /v1/agent/cancel", s.withAuth(s.handleAgentCancel))
	mux.HandleFunc("GET /v1/agent/drivers", s.withAuth(s.handleAgentDrivers))
	mux.HandleFunc("GET /v1/agent/environment", s.withAuth(s.handleAgentEnvironment))
	mux.HandleFunc("PUT /v1/agent/active_driver", s.withAuth(s.handleAgentActiveDriverPut))
	mux.HandleFunc("PUT /v1/agent/drivers/{name}/active_model", s.withAuth(s.handleAgentActiveModelPut))
	mux.HandleFunc("GET /v1/agent/menu/{id}", s.withAuth(s.handleAgentMenu))
	mux.HandleFunc("POST /v1/agent/menu/action", s.withAuth(s.handleAgentMenuAction))
	mux.HandleFunc("GET /v1/agent/sessions", s.withAuth(s.handleAgentSessions))
	mux.HandleFunc("GET /v1/agent/cwd-pool", s.withAuth(s.handleAgentCwdPool))
	mux.HandleFunc("POST /v1/agent/sessions", s.withAuth(s.handleAgentSessionCreate))
	mux.HandleFunc("GET /v1/agent/sessions/{id}", s.withAuth(s.handleAgentSessionGet))
	mux.HandleFunc("PATCH /v1/agent/sessions/{id}", s.withAuth(s.handleAgentSessionUpdate))
	mux.HandleFunc("GET /v1/agent/sessions/{id}/messages", s.withAuth(s.handleAgentSessionMessages))
	mux.HandleFunc("POST /v1/agent/sessions/{id}/approve", s.withAuth(s.handleAgentSessionApprove))
	mux.HandleFunc("DELETE /v1/agent/sessions/{id}", s.withAuth(s.handleAgentDeleteSession))
	mux.HandleFunc("GET /v1/butler/dispatch/recent", s.withAuth(s.handleButlerDispatchRecent))
	mux.HandleFunc("GET /ws", s.handleWebSocket)
	// Playground is unauthenticated on purpose — it's a dev-only single-page
	// UI for dogfooding agent drivers. Protect your adapter by not exposing
	// it to the internet.
	mux.HandleFunc("GET /playground", s.handlePlayground)
	// Local admin page (ADR-021 §拓展): basic status + project allow-list
	// management. Adding a project grants the butler authority to run agentic
	// tasks (with command/file execution) in that directory, so these routes are
	// gated to loopback callers only via adminLocalOnly — never exposed to the
	// LAN or cloud, regardless of the auth token.
	// Embedded Vue SPA (adapter/web → internal/adminui/dist). Both /admin and the
	// /admin/ subtree (assets) serve from the bundle; localhost-only.
	mux.HandleFunc("GET /admin", s.adminLocalOnly(adminui.ServeHTTP))
	mux.HandleFunc("GET /admin/", s.adminLocalOnly(adminui.ServeHTTP))
	mux.HandleFunc("GET /v1/admin/projects", s.adminLocalOnly(s.handleAdminProjectsList))
	mux.HandleFunc("POST /v1/admin/projects", s.adminLocalOnly(s.handleAdminProjectsAdd))
	mux.HandleFunc("DELETE /v1/admin/projects/{name}", s.adminLocalOnly(s.handleAdminProjectsDelete))
	mux.HandleFunc("GET /v1/admin/fs", s.adminLocalOnly(s.handleAdminFS))
	mux.HandleFunc("GET /v1/admin/fs/search", s.adminLocalOnly(s.handleAdminFSSearch))
	mux.HandleFunc("GET /v1/admin/fs/resolve-drop", s.adminLocalOnly(s.handleAdminFSResolveDrop))
	mux.HandleFunc("GET /v1/admin/workspace-files", s.adminLocalOnly(s.handleAdminWorkspaceFiles))
	mux.HandleFunc("GET /v1/admin/workspace-file", s.adminLocalOnly(s.handleAdminWorkspaceFile))
	// Read-only conversation surface for the admin SPA — same handlers as the
	// device-facing routes but behind the localhost gate (no device token needed).
	mux.HandleFunc("GET /v1/admin/sessions", s.adminLocalOnly(s.handleAgentSessions))
	mux.HandleFunc("GET /v1/admin/sessions/{id}/messages", s.adminLocalOnly(s.handleAgentSessionMessages))
	mux.HandleFunc("GET /v1/admin/sessions/{id}/parts", s.adminLocalOnly(s.handleAgentSessionParts))
	mux.HandleFunc("GET /v1/admin/dispatch/recent", s.adminLocalOnly(s.handleButlerDispatchRecent))
	// Driver management for the admin SPA (ADR-024) — same handlers as the
	// device-facing /v1/agent/* routes but behind the localhost gate, since the
	// page has no device token. A single active_driver drives everything
	// (butler + worker + memory); there is no separate butler_driver.
	mux.HandleFunc("GET /v1/admin/drivers", s.adminLocalOnly(s.handleAgentDrivers))
	mux.HandleFunc("GET /v1/admin/environment", s.adminLocalOnly(s.handleAgentEnvironment))
	mux.HandleFunc("PUT /v1/admin/active_driver", s.adminLocalOnly(s.handleAgentActiveDriverPut))
	// Web-first runtime configuration (ADR-025): read/write settings.json and a
	// one-click self-restart to apply. Loopback-only — these hold plaintext keys.
	mux.HandleFunc("GET /v1/admin/settings", s.adminLocalOnly(s.handleAdminSettingsGet))
	mux.HandleFunc("PUT /v1/admin/settings", s.adminLocalOnly(s.handleAdminSettingsPut))
	mux.HandleFunc("POST /v1/admin/restart", s.adminLocalOnly(s.handleAdminRestart))
	// Self-upgrade (loopback-only): check for and download the latest GitHub
	// release binary, then re-exec to load it. Mirrors the clawflow CLI's
	// /api/version + /api/update pair so the admin page can offer a one-click
	// upgrade matching the dashboard banner pattern users already know.
	mux.HandleFunc("GET /v1/admin/version", s.adminLocalOnly(s.handleAdminVersion))
	mux.HandleFunc("POST /v1/admin/update", s.adminLocalOnly(s.handleAdminUpdate))
	// Recent runtime logs for the admin 日志 page (ADR-025) — read from the
	// logger's in-memory ring; loopback-only.
	mux.HandleFunc("GET /v1/admin/logs", s.adminLocalOnly(s.handleAdminLogs))
	return withCORS(mux)
}

// withCORS wraps a handler with permissive CORS so a browser-hosted client
// (e.g. the BBClaw web portal at a different port during local dev) can reach
// the LAN-direct API. The Adapter is intended for trusted local networks, so
// `*` is acceptable; tighten this later if the surface becomes public.
//
// Behaviour:
//   - Adds `Access-Control-Allow-Origin: *`, exposed methods, headers, and a
//     long max-age on every response.
//   - Short-circuits OPTIONS preflight requests with 204 No Content so the
//     net/http ServeMux's default 405 doesn't leak through. (Without this the
//     browser sees a preflight failure and blocks the real request.)
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Echo the Origin when present so credentialed requests work; fall
		// back to "*" when no Origin is sent (curl, server-to-server).
		if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
		} else {
			h.Set("Access-Control-Allow-Origin", "*")
		}
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		h.Set("Access-Control-Max-Age", "600")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
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
	if s.asr == nil {
		// LAN voice pipeline is disabled (ADR-025 §3: cloud-default deployments do
		// ASR/TTS in the cloud). Reject early instead of buffering audio we can't
		// transcribe.
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "VOICE_NOT_CONFIGURED",
			Detail: "local voice pipeline is disabled; enable it on the admin page (设置 → 部署模式 → 本地模式)"})
		return
	}
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

	mode := strings.ToLower(strings.TrimSpace(req.ReplyMode))
	if mode == "stream" || mode == "asr" {
		s.handleFinishStream(w, r, stream, pcmAudio, inputSavedPath, t0, mode == "asr")
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
		Hotwords:   s.asrHotwords(),
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
	asrOnly bool,
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
		Hotwords:   s.asrHotwords(),
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

	if strings.TrimSpace(result.Text) == "" || asrOnly {
		if asrOnly {
			s.log.Infof("phase=asr_only stream=%s elapsed_s=%.3f (skip openclaw delivery)",
				stream.StreamID, time.Since(t0).Seconds())
		}
		_ = sw.write(map[string]any{
			"type":              "done",
			"streamId":          stream.StreamID,
			"text":              result.Text,
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
	// Issue #169: optional segment index sent by firmware tts_playback_task so
	// adapter logs can correlate each synth call with its subtitle cutover time.
	SegIdx int `json:"segIdx,omitempty"`
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
	rawChars := utf8.RuneCountInString(req.Text)
	cleanText := tts.Sanitize(req.Text)
	if cleanText == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "EMPTY_TEXT"})
		return
	}
	tTTS := time.Now()
	textChars := utf8.RuneCountInString(cleanText)
	s.log.Infof("phase=tts_start elapsed_s=0 text_chars=%d raw_chars=%d", textChars, rawChars)
	// Issue #169: structured per-segment log so adapter logs can be correlated
	// with firmware subtitle cutover timestamps (firmware logs tts_subtitle:
	// seg_idx=N wall_ms=M just before posting to the subtitle bar).
	s.log.Infof("phase=tts_synth_seg seg_idx=%d text_len=%d wall_ms=%d",
		req.SegIdx, textChars, time.Now().UnixMilli())
	audioBytes, err := s.tts.Synthesize(r.Context(), cleanText)
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
		// Skip ffmpeg when the provider already emits PCM16. Mock TTS does;
		// real backends like doubao_native emit MP3 and need the transcode.
		ttsFormatNorm := strings.ToLower(strings.TrimSpace(ttsFormat))
		if ttsFormatNorm == "pcm16" || ttsFormatNorm == "pcm_s16le" {
			outAudio = audioBytes
		} else {
			decoded, decodeErr := audio.DecodeMediaToPCM16LE(r.Context(), ttsFormat, sr, ch, audioBytes)
			if decodeErr != nil {
				s.metrics.Inc("tts_failed")
				s.log.Errorf("tts pcm transcode failed err=%v", decodeErr)
				writeJSON(w, http.StatusBadGateway, response{OK: false, Error: "TTS_TRANSCODE_FAILED"})
				return
			}
			outAudio = decoded
		}
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
