// Command bbclaw-adapter-v2 is the PTY-based universal CLI bridge (see
// adapter_v2/DESIGN.md). Phase 1 wires the HTTP/WS surface that lets a phone or
// web client (xterm.js) drive an interactive CLI over a single byte stream and
// reconnect to it after a refresh.
//
// Routes:
//
//	GET /                  the minimal xterm.js web client (embedded static
//	                       index.html); open it in a browser to take over a
//	                       session's real terminal (package web, issue #208).
//	GET /ws?session=<id>   join a terminal session by id, creating it (with the
//	                       configured default CLI argv) if it does not yet exist,
//	                       then streaming raw PTY bytes both ways (termchan).
//	GET /healthz           liveness probe; returns "ok".
//
// The process listens on :18090 by default — distinct from v1's :18080 so both
// adapters can run side by side during the migration. SIGINT/SIGTERM trigger a
// graceful HTTP shutdown.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"nhooyr.io/websocket"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/buildinfo"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/config"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/devicews"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/termchan"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/voicekit"
	"github.com/daboluocc/bbclaw/adapter_v2/web"
)

// shutdownTimeout bounds how long graceful shutdown waits for in-flight HTTP
// handlers (open WebSockets included) to return before the process exits.
const shutdownTimeout = 5 * time.Second

func main() {
	if buildinfo.ShouldPrintVersion(os.Args[1:]) {
		log.SetFlags(0)
		log.Println(buildinfo.String("bbclaw-adapter-v2"))
		return
	}

	cfg := config.LoadFromEnv()
	mgr := session.NewManager()

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: newRouter(mgr, cfg),
	}

	// Run the listener in the background so main can block on signals and then
	// drive a graceful shutdown. serveErr surfaces a startup failure (e.g. the
	// port is already taken) so we don't hang waiting for a signal that the
	// operator has no reason to send.
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("bbclaw-adapter-v2: listening on %s (default CLI: %v)", cfg.Addr, cfg.Argv)
		err := srv.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	// Block until a termination signal or a fatal serve error.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serveErr:
		log.Fatalf("bbclaw-adapter-v2: serve failed: %v", err)
	case s := <-sig:
		log.Printf("bbclaw-adapter-v2: received %s, shutting down", s)
	}

	// Graceful shutdown: stop accepting new connections and give in-flight
	// handlers a bounded window to finish. Sessions themselves outlive the
	// server (their PTYs keep running) — Phase 1 only tears down the HTTP layer.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("bbclaw-adapter-v2: graceful shutdown timed out: %v", err)
	}
}

// newRouter builds the Phase 1 HTTP mux: the terminal WebSocket endpoint and a
// health probe. It is a separate constructor so tests can exercise routing
// without binding a real listener.
func newRouter(mgr *session.Manager, cfg config.Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler(mgr, cfg))
	mux.HandleFunc("/healthz", healthzHandler)

	// bbwire/2 device protocol, Phase A (adapter_v2/docs/device-protocol.md).
	// ASR/TTS come from the shared voice module via voicekit.FromEnv (same env var
	// names as v1: ASR_PROVIDER / TTS_PROVIDER / …). With nothing configured it
	// falls back to a fixed-transcript recognizer + macOS `say` (or silent) TTS, so
	// the endpoint is always live for the sim script / a device. Opus mic audio is
	// decoded to PCM16 before ASR (ffmpeg).
	rec, syn, asrMode, ttsMode := voicekit.FromEnv()
	// Phase B streaming: reply.delta on by default (cosmetic, reply.end is
	// authoritative); per-segment TTS opt-in (ADAPTER_V2_SEGMENT_TTS=1).
	streamDelta := envBool("ADAPTER_V2_STREAM_DELTA", true)
	segmentTTS := envBool("ADAPTER_V2_SEGMENT_TTS", false)
	log.Printf("bbclaw-adapter-v2: device line ASR=%s TTS=%s streamDelta=%v segmentTTS=%v", asrMode, ttsMode, streamDelta, segmentTTS)
	devSrv := devicews.New(mgr, rec, syn, cfg.Argv, cfg.Cwd, devicews.Options{
		Decode:           voicekit.DecodeUplink,
		StreamReplyDelta: streamDelta,
		SegmentTTS:       segmentTTS,
	})
	mux.HandleFunc("/v2/dev/ws", devSrv.Handler())
	// The embedded web client at "/". ServeMux prefers the more specific "/ws"
	// and "/healthz" patterns over this catch-all, so the file server only sees
	// requests those two don't claim.
	mux.Handle("/", web.Handler())
	return mux
}

// envBool reads a boolean env var (1/true/yes/on = true, 0/false/no/off = false),
// returning def when unset or unrecognised.
func envBool(name string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// healthzHandler is the liveness probe.
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// wsHandler upgrades GET /ws?session=<id> to a WebSocket and hands it to
// termchan.Serve. The session is looked up by id and created on first use with
// the configured default CLI argv — so the first client to ask for an id starts
// the CLI and every later client (or a reconnecting one) joins the same live
// session and replays its screen.
func wsHandler(mgr *session.Manager, cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("session")
		if id == "" {
			http.Error(w, "missing session id", http.StatusBadRequest)
			return
		}

		// Create-if-absent, atomically: the first request for an id spawns the
		// CLI; later requests for the same id (and any concurrent first request
		// for the same id) attach to the same session. GetOrCreate serializes the
		// race so two simultaneous first connections cannot each spawn a PTY and
		// leak the loser.
		sess, err := mgr.GetOrCreate(id, ptyhost.Config{
			Argv: cfg.Argv,
			Cwd:  cfg.Cwd,
		})
		if err != nil {
			log.Printf("bbclaw-adapter-v2: create session %q: %v", id, err)
			http.Error(w, "failed to start session", http.StatusInternalServerError)
			return
		}

		// Accept the WebSocket only after we have a session to serve, so a failed
		// spawn surfaces as an HTTP error rather than a confusing post-upgrade
		// close. CompressionDisabled keeps the raw terminal byte stream verbatim.
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			// Accept already wrote the HTTP error response.
			return
		}

		// termchan.Serve owns the conn lifecycle (it closes it on return). A
		// client closing first is the normal exit, not an error worth logging.
		if err := termchan.Serve(sess, conn); err != nil {
			log.Printf("bbclaw-adapter-v2: session %q serve ended: %v", id, err)
		}
	}
}
