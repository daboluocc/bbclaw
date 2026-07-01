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
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"nhooyr.io/websocket"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/adminapi"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/buildinfo"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/butler"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/cloudrelay"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/config"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/curdevice"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/deviceapi"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/devicehub"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/devicews"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/proactive"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/projectstore"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/reminder"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/settingsstore"
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

	// `device …` is a short-lived CLI (set-volume / set-miyu via the cloud config
	// API), NOT the server. Dispatch it before any server setup and exit with its
	// code. The butler shells out to this so it can adjust the current device.
	if len(os.Args) > 1 && os.Args[1] == "device" {
		os.Exit(runDeviceCmd(os.Args[2:]))
	}

	// Export this binary's absolute path so the butler's claude (which inherits the
	// adapter's env via the PTY) can invoke `"$BBCLAW_ADAPTER_V2_BIN" device …`
	// regardless of PATH — the persona's device-control section references it.
	if exe, err := os.Executable(); err == nil {
		_ = os.Setenv("BBCLAW_ADAPTER_V2_BIN", exe)
	}

	// Web-first config (ADR-025, env-overlay variant). This MUST run before the
	// config/voicekit/cloudrelay readers below: it seeds settings.json from the
	// environment on first boot, then exports the file's values back into the
	// process env so the EXISTING FromEnv/LoadFromEnv/LoadConfig readers see the
	// file-sourced configuration with ZERO changes to those packages.
	seed := settingsstore.FromEnv()
	settingsPath := filepath.Join(settingsstore.DataDir(), "settings.json")
	if err := settingsstore.Bootstrap(settingsPath, seed); err != nil {
		log.Printf("bbclaw-adapter-v2: settings bootstrap failed (continuing on env): %v", err)
	}
	store, err := settingsstore.Open(settingsPath, seed)
	if err != nil {
		// Corrupt/unreadable file: Open still returns a usable store holding the
		// env-derived base, so we log and continue rather than blocking startup.
		log.Printf("bbclaw-adapter-v2: settings load error (using env defaults): %v", err)
	}
	store.ExportEnv() // env now reflects settings.json; the readers below pick it up

	// Project loader (ADR-036): the registered project list feeds the butler's
	// system prompt (so it knows the user's projects from boot — preventing the
	// "asked but doesn't know" failure) and merges project names into ASR_HOTWORDS
	// (so the recognizer hears them). Opened HERE — after ExportEnv, before the voice
	// readers and DeviceClaudeArgs below — so a fresh boot / a post-add re-exec both
	// pick up the current projects. A missing/corrupt file degrades to an empty list.
	projectsPath := filepath.Join(settingsstore.DataDir(), "projects.json")
	projStore, perr := projectstore.Open(projectsPath)
	if perr != nil {
		log.Printf("bbclaw-adapter-v2: projects load error (using empty list): %v", perr)
	}
	mergeProjectHotwords(projStore.List()) // appends project names to ASR_HOTWORDS env (before voicekit reads it)

	restartFlag := &adminapi.RestartFlag{}

	cfg := config.LoadFromEnv()
	mgr := session.NewManager()

	// The shared "default active" session is a butler session (ported from v1):
	// claude runs IN the butler workspace (so it loads CLAUDE.md natively) with the
	// device persona appended. The device (LAN + cloud relay) and the web terminal
	// all spawn session.DefaultID with THIS exact argv + cwd, attaching to one
	// identical PTY. ADAPTER_V2_CWD overrides the workspace dir; otherwise the
	// shared ~/.bbclaw-adapter/workspace (v1's) is used, keeping the user's memory.
	defaultCwd, err := butler.EnsureWorkspace(cfg.Cwd)
	if err != nil {
		log.Fatalf("bbclaw-adapter-v2: butler workspace: %v", err)
	}
	baseArgv := butler.DeviceClaudeArgs(cfg.Argv, defaultCwd, projStore.List())
	// DeviceSession owns the default conversation's lifecycle (ADR-032): it appends
	// the resume flag (--resume <active>/--continue/--session-id) to baseArgv, so
	// every entry point spawns session.DefaultID identically and resume / new /
	// switch stay coherent. Loads the persisted active conversation on boot.
	devSess := butler.NewDeviceSession(mgr, baseArgv, defaultCwd)
	log.Printf("bbclaw-adapter-v2: butler workspace %s (active session %q)", defaultCwd, devSess.ActiveID())

	// derive supplies the read-only block the admin page shows (build version,
	// resolved home_site_id, workspace, settings file). homeSiteId is resolved the
	// same way cloudrelay does: the HOME_SITE_ID env (now reflecting settings.json)
	// or the persisted identity.json — computed per-request so a just-saved id shows.
	derive := func() adminapi.Derived {
		return adminapi.Derived{
			Version:      buildinfo.Tag,
			HomeSiteID:   resolveHomeSiteID(),
			Workspace:    defaultCwd,
			SettingsFile: store.Path(),
		}
	}

	// ADR-042 command router + reminder scheduler. hub is the single active-bridge
	// slot both device transports (LAN + cloud relay) register into; cmdHooks gives
	// every device bridge quick-command interception ("停止"/"状态"/reminders); the
	// scheduler injects a due reminder's prompt into whatever bridge is live, or —
	// when none is — marks it failed (the notify outbox / offline-defer is M3).
	hub := devicehub.New()
	remStore, rerr := reminder.Open(filepath.Join(settingsstore.DataDir(), "reminders.json"))
	if rerr != nil {
		log.Printf("bbclaw-adapter-v2: reminders load error (using empty store): %v", rerr)
	}
	cmdHooks := buildCommandHooks(remStore, devSess)
	// relay is assigned below if cloud_saas is enabled; the scheduler closure
	// captures the variable and reads it at fire time.
	var relay *cloudrelay.Relay
	// ensureRunner lazily spawns the isolated worker session for ModeTask reminders
	// (ADR-042 §3.3) — only the first task reminder pays the extra claude process,
	// so a device that only sets alarms never spawns it. context.Background: the
	// worker outlives the HTTP server like every other session (main's shutdown
	// leaves sessions running).
	var runnerMu sync.Mutex
	var runner *proactive.Runner
	var runnerTried bool
	ensureRunner := func() *proactive.Runner {
		runnerMu.Lock()
		defer runnerMu.Unlock()
		if runnerTried {
			return runner
		}
		runnerTried = true
		rn, err := proactive.New(context.Background(), mgr, "reminder-worker",
			butler.WorkerConfig(baseArgv, defaultCwd), session.DefaultGridCols, session.DefaultGridRows)
		if err != nil {
			log.Printf("bbclaw-adapter-v2: reminder worker spawn failed: %v", err)
			return nil
		}
		runner = rn
		return runner
	}
	scheduler := reminder.NewScheduler(remStore, reminder.InjectorFunc(func(_ context.Context, r reminder.Reminder) error {
		if relay == nil {
			return fmt.Errorf("reminder %s: cloud relay disabled (LAN proactive deferred)", r.ID)
		}
		// Resolve the target device at FIRE time, not create time: the reminder may
		// have been created while no device was connected (right after a restart, or
		// the device offline), in which case Target.DeviceID is stale/empty and would
		// mis-route (verified live: a reminder set before the device connected was
		// enqueued under a stale id and never delivered). BBClaw is single-device
		// today, so "the device connected now" (curdevice) is the right target; fall
		// back to the stored id only if nothing is currently connected.
		deviceID := curdevice.Get()
		if deviceID == "" {
			deviceID = r.Target.DeviceID
		}
		if deviceID == "" {
			return fmt.Errorf("reminder %s: no device to deliver to", r.ID)
		}
		preview, ttsText := "提醒："+r.Prompt, "提醒，"+r.Prompt
		if r.Mode == reminder.ModeTask {
			// Run the prompt as a headless Agent turn and report its RESULT (ADR-042
			// §3.3), instead of just echoing the reminder text. The worker inherits the
			// device persona, so replies are already short + speakable.
			rn := ensureRunner()
			if rn == nil {
				return fmt.Errorf("reminder %s: task mode but no worker", r.ID)
			}
			log.Printf("bbclaw-adapter-v2: running task reminder %s: %q", r.ID, r.Prompt)
			reply, err := rn.RunOnce(context.Background(), r.Prompt, 0)
			if err != nil {
				return fmt.Errorf("reminder %s task failed: %w", r.ID, err)
			}
			if strings.TrimSpace(reply) == "" {
				reply = "任务已执行，没有产生可播报的结果。"
			}
			preview, ttsText = "提醒结果："+capRunes(reply, 40), reply
		}
		log.Printf("bbclaw-adapter-v2: firing reminder %s → device %s", r.ID, deviceID)
		// preview = toast text; ttsText asks the device to also speak it (ADR-042 §3.2).
		return relay.Notify(deviceID, preview, ttsText, r.Target.SessionID)
	}), 0, nil)

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: newRouter(mgr, cfg, devSess, store, projStore, restartFlag, derive, cmdHooks, hub, remStore),
	}
	log.Printf("bbclaw-adapter-v2: settings file %s", store.Path())
	log.Printf("bbclaw-adapter-v2: admin at http://127.0.0.1%s/admin", cfg.Addr)

	// Cloud relay (SaaS): when CLOUD_WS_URL is set, register with the BBClaw Cloud
	// as a HomeAdapter so the device can pick adapter_v2 in its sites.list and have
	// voice relayed here (cloud does ASR/TTS; we inject the transcript into a PTY
	// and return the reply text). Runs alongside the LAN /v2/dev/ws device line.
	// relayCtx is cancelled on shutdown so the relay stops dialing + closes its WS.
	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	if cloudrelay.Enabled() {
		if rc, err := cloudrelay.LoadConfig(); err != nil {
			log.Printf("bbclaw-adapter-v2: cloud relay disabled (config error): %v", err)
		} else {
			relay = cloudrelay.New(mgr, devSess, rc, log.Printf)
			relay.SetCommandRouter(cmdHooks, hub) // ADR-042: cloud path gets the same router + hub
			go relay.Run(relayCtx)
		}
	}

	// Reminder scheduler (ADR-042): polls the store and fires due reminders into
	// the active device bridge. Shares the app-lifetime relayCtx so it stops on
	// shutdown.
	go scheduler.Run(relayCtx)

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
	stopRelay() // stop the cloud relay's dial/reconnect loop and close its WS

	// Graceful shutdown: stop accepting new connections and give in-flight
	// handlers a bounded window to finish. Sessions themselves outlive the
	// server (their PTYs keep running) — Phase 1 only tears down the HTTP layer.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("bbclaw-adapter-v2: graceful shutdown timed out: %v", err)
	}
}

// mergeProjectHotwords appends the registered project names to the ASR_HOTWORDS
// env var (comma-separated, the format voicekit.splitList accepts) so the
// recognizer is biased toward hearing them — e.g. so a spoken "Buildhub" is
// transcribed correctly (ADR-036 §决策四). It runs after ExportEnv and before
// voicekit reads the env. Existing hotwords are preserved; project names already
// present are not duplicated. Project names are delimiter-safe by construction
// (projectstore.Add forbids ',' and ':').
func mergeProjectHotwords(projects []projectstore.Project) {
	if len(projects) == 0 {
		return
	}
	seen := map[string]struct{}{}
	var words []string
	add := func(w string) {
		w = strings.TrimSpace(w)
		if w == "" {
			return
		}
		if _, dup := seen[w]; dup {
			return
		}
		seen[w] = struct{}{}
		words = append(words, w)
	}
	// Preserve any existing hotwords (comma- or space-separated) first.
	for _, w := range strings.FieldsFunc(os.Getenv("ASR_HOTWORDS"), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		add(w)
	}
	for _, p := range projects {
		add(p.Name)
	}
	if len(words) > 0 {
		_ = os.Setenv("ASR_HOTWORDS", strings.Join(words, ","))
	}
}

// newRouter builds the Phase 1 HTTP mux: the terminal WebSocket endpoint and a
// health probe. It is a separate constructor so tests can exercise routing
// without binding a real listener.
func newRouter(mgr *session.Manager, cfg config.Config, devSess *butler.DeviceSession, store *settingsstore.Store, projStore *projectstore.Store, restart *adminapi.RestartFlag, derive func() adminapi.Derived, cmdHooks *deviceapi.CommandHooks, hub *devicehub.Hub, remStore *reminder.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler(mgr, cfg, devSess))
	mux.HandleFunc("/healthz", healthzHandler)

	// Web-first config admin surface (ADR-025), loopback-only. Registered before
	// the "/" catch-all so ServeMux's longest-prefix match routes these first.
	mux.HandleFunc("/admin", adminapi.LocalOnly(web.AdminHandler().ServeHTTP))
	mux.HandleFunc("/admin/", adminapi.LocalOnly(web.AdminHandler().ServeHTTP))
	mux.HandleFunc("/v1/settings", adminapi.LocalOnly(adminapi.Settings(store, restart, derive)))
	mux.HandleFunc("/v1/settings/restart", adminapi.LocalOnly(adminapi.Restart()))

	// Project loader (ADR-036), loopback-only. GET/POST the project list, DELETE one
	// by name, and a NATIVE OS folder chooser (pick-dir) so non-programmers select a
	// directory via the Finder dialog instead of typing a path. Adding/removing does
	// NOT force a restart (revised UX): it persists and takes effect at the next boot.
	// The MEMORY/projects.md enrichment scanner is a separate follow-up (§决策三).
	mux.HandleFunc("/v1/projects", adminapi.LocalOnly(adminapi.Projects(projStore)))
	mux.HandleFunc("/v1/projects/", adminapi.LocalOnly(adminapi.ProjectByName(projStore)))
	// Reminders management (ADR-042 §2.4, Task #7): list / create / cancel. A
	// web-created reminder targets the current device (curdevice) + active
	// conversation, mirroring the voice create. Loopback-only like the rest.
	reminderTarget := func() reminder.Target {
		return reminder.Target{DeviceID: curdevice.Get(), SessionID: devSess.ActiveID()}
	}
	mux.HandleFunc("/v1/reminders", adminapi.LocalOnly(adminapi.Reminders(remStore, reminderTarget)))
	mux.HandleFunc("/v1/reminders/", adminapi.LocalOnly(adminapi.ReminderByID(remStore)))
	mux.HandleFunc("/v1/admin/pick-dir", adminapi.LocalOnly(adminapi.PickDir()))
	// Read-only view of the butler's user-profile / memory files (ADR-020/022):
	// workspace MEMORY/*.md, so the operator can see what the butler knows.
	mux.HandleFunc("/v1/memory", adminapi.LocalOnly(adminapi.Memory(derive)))
	// Device control (密语 / 音量) for the current device, via the SAME cloud config
	// path the `device set-volume/set-miyu` CLI uses — so the admin page can control
	// the device like the butler does over voice. Loopback-only.
	mux.HandleFunc("/v1/admin/device-config", adminapi.LocalOnly(adminDeviceConfigHandler()))

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
	devSrv := devicews.New(mgr, rec, syn, devSess, devicews.Options{
		Decode:           voicekit.DecodeUplink,
		StreamReplyDelta: streamDelta,
		SegmentTTS:       segmentTTS,
		CommandHooks:     cmdHooks,
		Hub:              hub,
	})
	mux.HandleFunc("/v2/dev/ws", devSrv.Handler())
	// Local HTTP session browser for the web SPA: list / view transcript / switch.
	agentSessionsRoutes(mux, mgr, devSess)
	// The embedded web client at "/". ServeMux prefers the more specific "/ws"
	// and "/healthz" patterns over this catch-all, so the file server only sees
	// requests those two don't claim.
	mux.Handle("/", web.Handler())
	return mux
}

// buildCommandHooks assembles the ADR-042 command-router side-effects the device
// bridges call: status.show summarises the runtime; reminder.create/list persist
// and read from the reminder store, binding each new reminder to the active
// conversation (so it fires back there). turn.cancel and session.new are owned by
// the Bridge itself and need no hook.
func buildCommandHooks(remStore *reminder.Store, devSess *butler.DeviceSession) *deviceapi.CommandHooks {
	return &deviceapi.CommandHooks{
		Status: func() string {
			mode := "本地"
			if cloudrelay.Enabled() {
				mode = "云端配对"
			}
			return fmt.Sprintf("当前%s模式，会话 %s 正常。", mode, shortID(devSess.ActiveID()))
		},
		ReminderCreate: func(args map[string]string) (string, error) {
			now := time.Now()
			runAt, prompt, err := reminder.Resolve(args, now)
			if err != nil {
				return "", err
			}
			r, err := remStore.Add(reminder.Reminder{
				Prompt: prompt,
				Mode:   args["mode"], // "" → store defaults to ModeNotify
				RunAt:  runAt,
				// Bind to the device + conversation that set it, so firing routes the
				// notification back to this device (ADR-042 §3). DeviceID comes from
				// curdevice (the last device the adapter saw); empty on a dev box with
				// no paired device, in which case firing reports "no target".
				Target: reminder.Target{
					DeviceID:  curdevice.Get(),
					SessionID: devSess.ActiveID(),
				},
			}, now)
			if err != nil {
				return "", err
			}
			return reminder.ConfirmText(r, now), nil
		},
		ReminderList: func() string {
			now := time.Now()
			var pending []reminder.Reminder
			for _, r := range remStore.List() {
				if r.State == reminder.StateScheduled {
					pending = append(pending, r)
				}
			}
			if len(pending) == 0 {
				return "当前没有待提醒的任务。"
			}
			head := pending[0]
			if len(pending) == 1 {
				return "有 1 条提醒：" + reminder.ConfirmText(head, now)
			}
			return fmt.Sprintf("有 %d 条提醒，最近一条是%s", len(pending), reminder.ConfirmText(head, now))
		},
	}
}

// capRunes truncates s to at most n runes (adding "…" if cut), for a toast
// preview built from a possibly-long task result.
func capRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// shortID trims a conversation UUID to its first segment for a speakable status.
func shortID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
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

// resolveHomeSiteID reports the home_site_id the cloud relay would use, for the
// admin page's read-only block: the HOME_SITE_ID env (which now reflects
// settings.json after ExportEnv), else the UUID persisted in
// ~/.bbclaw-adapter-v2/identity.json. It only READS — never creates the file
// (cloudrelay owns that on first cloud connect), so a LAN-only adapter shows the
// env value or "(none)" without minting an identity it never uses.
func resolveHomeSiteID() string {
	if id := strings.TrimSpace(os.Getenv("HOME_SITE_ID")); id != "" {
		return id
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "(none)"
	}
	path := filepath.Join(home, ".bbclaw-adapter-v2", "identity.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "(none)"
	}
	var f struct {
		HomeSiteID string `json:"homeSiteId"`
	}
	if json.Unmarshal(raw, &f) == nil && strings.TrimSpace(f.HomeSiteID) != "" {
		return strings.TrimSpace(f.HomeSiteID)
	}
	return "(none)"
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
func wsHandler(mgr *session.Manager, cfg config.Config, devSess *butler.DeviceSession) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// No ?session= → the shared default session (the one the device drives), so
		// opening the web UI joins the device's live conversation. An explicit
		// ?session=<id> selects another session (the future web session-browser);
		// only the default session carries the butler persona + workspace cwd +
		// the active-conversation resume flag (from DeviceSession).
		id := r.URL.Query().Get("session")
		var cfgPty ptyhost.Config
		if id == "" || id == session.DefaultID {
			id = session.DefaultID
			cfgPty = devSess.Config()
		} else {
			cfgPty = ptyhost.Config{Argv: cfg.Argv, Cwd: cfg.Cwd}
		}

		// Accept the WebSocket upgrade FIRST, before touching the session, so a
		// non-WebSocket probe (a bare GET /ws, a health check) is rejected with
		// 426 and NEVER spawns a CLI. CompressionDisabled keeps the raw terminal
		// byte stream verbatim.
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			// Accept already wrote the HTTP error response.
			return
		}

		// Create-if-absent, atomically: the first connection for an id spawns the
		// CLI; later connections for the same id attach to the same session.
		// GetOrCreate serializes the race so two simultaneous first connections
		// cannot each spawn a PTY and leak the loser. A spawn failure now closes
		// the just-accepted WebSocket (we are past the HTTP response).
		sess, err := mgr.GetOrCreate(id, cfgPty)
		if err != nil {
			log.Printf("bbclaw-adapter-v2: create session %q: %v", id, err)
			conn.Close(websocket.StatusInternalError, "failed to start session")
			return
		}

		// termchan.Serve owns the conn lifecycle (it closes it on return). A
		// client closing first is the normal exit, not an error worth logging.
		if err := termchan.Serve(sess, conn); err != nil {
			log.Printf("bbclaw-adapter-v2: session %q serve ended: %v", id, err)
		}
	}
}
