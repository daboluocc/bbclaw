package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/aider"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/claudecode"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/codex"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/driverstate"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/ollama"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/openclawdriver"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/opencode"
	"github.com/daboluocc/bbclaw/voice/asr"
	"github.com/daboluocc/bbclaw/voice/audio"
	"github.com/daboluocc/bbclaw/adapter/internal/buildinfo"
	"github.com/daboluocc/bbclaw/adapter/internal/butler"
	"github.com/daboluocc/bbclaw/adapter/internal/butler/memory"
	"github.com/daboluocc/bbclaw/adapter/internal/butlermcp"
	"github.com/daboluocc/bbclaw/adapter/internal/cmd"
	"github.com/daboluocc/bbclaw/adapter/internal/config"
	"github.com/daboluocc/bbclaw/adapter/internal/homeadapter"
	"github.com/daboluocc/bbclaw/adapter/internal/httpapi"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/openclaw"
	"github.com/daboluocc/bbclaw/adapter/internal/pipeline"
	"github.com/daboluocc/bbclaw/adapter/internal/projectstore"
	"github.com/daboluocc/bbclaw/adapter/internal/settingsstore"
	"github.com/daboluocc/bbclaw/voice/tts"
	"github.com/daboluocc/bbclaw/adapter/internal/workspace"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := cmd.NewRootCmd()

	// Override the default run function to execute the adapter service
	rootCmd.RunE = func(c *cobra.Command, args []string) error {
		logger := obs.NewLogger()
		metrics := obs.NewMetrics()

		cfg, err := config.LoadFromEnv()
		if err != nil {
			logger.Errorf("load config failed: %v", err)
			os.Exit(1)
		}

		logger.Infof("%s", buildinfo.String("bbclaw-adapter"))
		run(cfg, logger, metrics)
		return nil
	}

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func buildSink(cfg config.Config, logger *obs.Logger, metrics *obs.Metrics) pipeline.Sink {
	return pipeline.Wrap(openclaw.NewClient(cfg.OpenClawURL, cfg.HTTPTimeout, openclaw.Options{
		NodeID:             cfg.OpenClawNodeID,
		AuthToken:          cfg.OpenClawAuthToken,
		DeviceIdentityPath: cfg.OpenClawIdentityPath,
		ReplyWaitTimeout:   cfg.OpenClawReplyWait,
	}), logger, metrics)
}

func buildCloudRelay(cfg config.Config, sink pipeline.Sink, logger *obs.Logger, metrics *obs.Metrics) (*homeadapter.Adapter, error) {
	homeCfg := homeadapter.Config{
		CloudWSURL:           cfg.CloudWSURL,
		CloudAuthToken:       cfg.CloudAuthToken,
		HomeSiteID:           cfg.HomeSiteID,
		ReconnectDelay:       cfg.ReconnectDelay,
		HTTPTimeout:          cfg.HTTPTimeout,
		OpenClawURL:          cfg.OpenClawURL,
		OpenClawAuthToken:    cfg.OpenClawAuthToken,
		OpenClawNodeID:       cfg.OpenClawNodeID,
		OpenClawReplyWait:    cfg.OpenClawReplyWait,
		OpenClawIdentityPath: cfg.OpenClawIdentityPath,
	}
	if strings.TrimSpace(homeCfg.HomeSiteID) == "" {
		derived, err := homeadapter.EnsureHomeSiteID()
		if err != nil {
			return nil, fmt.Errorf("derive home_site_id failed: %w", err)
		}
		homeCfg.HomeSiteID = derived
	}
	if err := homeCfg.Validate(); err != nil {
		return nil, fmt.Errorf("cloud config invalid: %w", err)
	}
	return homeadapter.New(homeCfg, sink, logger, metrics), nil
}

// butlerInfra bundles the process-shared butler observability + long-term memory
// so BOTH the cloud-relay and the local-ingress butler engines wire the *same*
// instances. Previously these were created inside buildLocalServer and only the
// local path got them, so a device talking through the cloud relay never fed the
// memory writer (managed block stayed empty, MEMORY/*.md never filled) and its
// dispatches weren't recorded (ADR-021 §4). One home adapter == one home, so
// sharing one memory writer is correct (the multi-tenant concern lives in the
// cloud backend, not here).
type butlerInfra struct {
	memory   butler.MemoryWriter      // nil when memory pipeline is off/disabled
	ring     *butler.DispatchRing     // nil when butler workspace is unavailable
	recorder *butler.DispatchRecorder // always non-nil (process-level)
}

// buildButlerInfra constructs the shared butler memory + dispatch infra once.
// The dispatch recorder is always built (process-level). The dispatch ring and
// memory writer require a butler workspace + session manager, mirroring the
// previous gating.
func buildButlerInfra(butlerWorkspace string, sessionMgr *logicalsession.Manager, agentRouter *agent.Router, logger *obs.Logger) butlerInfra {
	infra := butlerInfra{recorder: butler.NewDispatchRecorder()}
	if sessionMgr == nil || butlerWorkspace == "" {
		return infra
	}
	// Butler dispatch ring buffer (ADR-021-firmware-ui §1.4). Backed by a JSON
	// snapshot in BBCLAW_DATA_DIR so the firmware Task List / admin survive an
	// adapter restart (issue #138 P0). Falls back to in-memory-only when the data
	// dir cannot be resolved.
	if dataDir, derr := workspace.DataDir(); derr != nil {
		logger.Warnf("butler-dispatch: resolve data dir failed, ring not persisted: %v", derr)
		infra.ring = butler.NewDispatchRing()
	} else {
		infra.ring = butler.NewPersistentDispatchRing(filepath.Join(dataDir, "dispatch_ring.json"), logger)
	}
	logger.Infof("butler-dispatch: ring buffer enabled")
	// Butler long-term memory (ADR-021 §4); enabled by default, see memory.Enabled.
	if mdPath, perr := workspace.ClaudeMDPath(); perr != nil {
		logger.Warnf("butler-memory: resolve CLAUDE.md path failed, memory disabled: %v", perr)
	} else if w, on := memory.NewWithRunner(mdPath, memoryRunner(agentRouter, logger), logger); on {
		infra.memory = w
		logger.Infof("butler-memory: long-term memory enabled path=%s", mdPath)
	}
	return infra
}

func buildLocalServer(cfg config.Config, sink pipeline.Sink, cloudRelay *homeadapter.Adapter, agentRouter *agent.Router, sessionMgr *logicalsession.Manager, driverStateStore *driverstate.Store, settingsStore *settingsstore.Store, butlerWorkspace string, butlerMCPServers []agent.MCPServerSpec, infra butlerInfra, logger *obs.Logger, metrics *obs.Metrics) (*http.Server, *httpapi.Server, error) {
	streams := audio.NewManager(cfg.MaxAudioBytes, cfg.MaxStreamSeconds, cfg.MaxConcurrentStreams)
	// ADR-025 §3: the LAN voice pipeline is opt-in AND must be fully configured.
	// When off (cloud-default — the cloud does ASR/TTS) or enabled-but-incomplete,
	// no provider is built and /v1/stream/* + /v1/tts/* degrade to 501. An enabled
	// but incomplete config warns rather than crashing, so the user can switch to
	// local mode first and fill ASR/TTS on the AI page afterwards.
	var asrProvider asr.Provider
	var ttsProvider tts.Provider
	if cfg.LocalVoiceEnabled && !cfg.VoiceReady() {
		logger.Warnf("local voice enabled but ASR/TTS incomplete (%v); voice disabled until configured at /admin (设置)", cfg.VoiceConfigError())
	}
	if cfg.VoiceReady() {
		switch strings.ToLower(strings.TrimSpace(cfg.ASRProvider)) {
		case "doubao_native":
			asrProvider = asr.NewDoubaoNativeProvider(
				cfg.ASRWSURL, cfg.ASRAppID, cfg.ASRAPIKey, cfg.ASRResourceID, cfg.ASRModel, cfg.ASRLanguage,
				asr.DoubaoOptions{
					EnableDDC:     cfg.ASREnableDDC,
					BoostingTable: cfg.ASRBoostingTable,
					CorrectTable:  cfg.ASRCorrectTable,
					Hotwords:      cfg.ASRHotwords,
				},
			)
		case "local":
			asrProvider = asr.NewLocalCommandProvider(cfg.ASRLocalBin, cfg.ASRLocalArgs, cfg.ASRLocalTextPath)
		default:
			asrProvider = asr.NewOpenAICompatibleProvider(
				cfg.ASRBaseURL, cfg.ASRAPIKey, cfg.ASRModel, &http.Client{Timeout: cfg.HTTPTimeout},
			)
		}
		switch strings.ToLower(strings.TrimSpace(cfg.TTSProvider)) {
		case "mock":
			ttsProvider = tts.NewMockProvider()
		case "local_command":
			ttsProvider = tts.NewLocalCommandProvider(cfg.TTSLocalBin, cfg.TTSLocalArgs, cfg.TTSLocalOutputFormat)
		default:
			ttsProvider = tts.NewDoubaoNativeProvider(cfg.TTSWSURL, cfg.TTSAppID, cfg.TTSToken, cfg.TTSCluster, cfg.TTSVoice)
		}

		if cfg.ASRReadinessProbe {
			rp, ok := asrProvider.(asr.ReadinessProbe)
			if !ok {
				return nil, nil, fmt.Errorf("asr readiness: provider %q does not implement Ping", cfg.ASRProvider)
			}
			pctx, pcancel := context.WithTimeout(context.Background(), cfg.ASRReadinessTimeout)
			err := rp.Ping(pctx)
			pcancel()
			if err != nil {
				return nil, nil, fmt.Errorf("asr readiness probe failed: %w", err)
			}
			logger.Infof("asr readiness probe ok provider=%s", cfg.ASRProvider)
		}
	} else if !cfg.LocalVoiceEnabled {
		logger.Infof("local voice pipeline disabled (cloud does ASR/TTS); /v1/stream/* + /v1/tts/* return 501")
	}

	var cloudStatus func() map[string]any
	if cloudRelay != nil {
		cloudStatus = func() map[string]any {
			status := cloudRelay.Status()
			return map[string]any{
				"connected":    status.Connected,
				"homeSiteId":   status.HomeSiteID,
				"lastError":    status.LastError,
				"lastChangeAt": status.LastChangeAt.Format(time.RFC3339),
			}
		}
	}

	server := httpapi.NewServer(
		httpapi.AppConfig{
			AuthToken:            cfg.AuthToken,
			NodeID:               cfg.OpenClawNodeID,
			LocalIngressEnabled:  cfg.EnableLocalIngress(),
			CloudRelayEnabled:    cfg.EnableCloudRelay(),
			CloudStatus:          cloudStatus,
			SaveAudio:            cfg.SaveAudio,
			SaveInputOnFinish:    cfg.SaveInputOnFinish,
			AudioInDir:           cfg.AudioInDir,
			AudioOutDir:          cfg.AudioOutDir,
			ASRTranscribeTimeout: cfg.ASRTranscribeTimeout,
			SessionReuseWindow:   cfg.SessionReuseWindow,
			SessionMaxAge:        cfg.SessionMaxAge,
			CwdPool:              cfg.CwdPool,
		},
		streams, asrProvider, ttsProvider, sink, logger, metrics,
	)
	server.SetAgentRouter(agentRouter)
	// Project allow-list backing the local admin page (/admin). projects.json is
	// the source of truth; BBCLAW_CWD_POOL only seeds a fresh file on first run,
	// after which projects are managed entirely through the web page (and shared
	// live with the mcp-server subprocess, which opens the same file).
	if dataDir, derr := workspace.DataDir(); derr != nil {
		logger.Warnf("project-store: resolve data dir failed, admin project mgmt disabled: %v", derr)
	} else {
		storePath := filepath.Join(dataDir, "projects.json")
		seed := make([]projectstore.Project, 0, len(cfg.CwdPool))
		for _, e := range cfg.CwdPool {
			seed = append(seed, projectstore.Project{Name: e.Name, Path: e.Path})
		}
		switch status, serr := projectstore.Bootstrap(storePath, seed); {
		case serr != nil:
			logger.Warnf("project-store: bootstrap failed: %v", serr)
		case status == projectstore.BootstrapSeeded:
			logger.Infof("project-store: seeded %d project(s) from BBCLAW_CWD_POOL into %s (env is a one-time bootstrap; manage projects at /admin)", len(seed), storePath)
		case status == projectstore.BootstrapMigrated:
			logger.Infof("project-store: migrated %s to web-managed format and merged BBCLAW_CWD_POOL projects (you can now remove BBCLAW_CWD_POOL from .env)", storePath)
		}
		if store, oerr := projectstore.Open(storePath); oerr != nil {
			logger.Warnf("project-store: open failed, admin project mgmt disabled: %v", oerr)
		} else {
			server.SetProjectStore(store)
			logger.Infof("project-store: ready (%d project(s)); local admin page at /admin", len(store.List()))
		}
	}
	if sessionMgr != nil {
		server.SetSessionManager(sessionMgr)
		// Route local agent turns to the per-device butler session (ADR-021).
		server.SetButlerWorkspace(butlerWorkspace, butlerMCPServers)
		// Shared butler dispatch ring + long-term memory (built once in run(),
		// also wired into the cloud-relay engine so memory/dispatch work no matter
		// how the device reaches this adapter).
		if infra.ring != nil {
			server.SetDispatchRing(infra.ring)
		}
		if infra.memory != nil {
			server.SetMemoryWriter(infra.memory)
		}
	}
	if driverStateStore != nil {
		server.SetDriverState(driverStateStore)
	}
	if settingsStore != nil {
		server.SetSettingsStore(settingsStore)
	}
	// Process-level dispatch recorder for GET /v1/butler/dispatch/recent (shared).
	server.SetDispatchRecorder(infra.recorder)
	return &http.Server{
		Addr:    cfg.Addr,
		Handler: server.Handler(),
	}, server, nil
}

// driverReg is one row in k_driver_registry. Each row knows everything
// needed to decide whether to register a driver and how to construct it.
//
//   - name        : the Driver.Name() this entry will register under. Used
//     both for the AGENT_ENABLED_DRIVERS comma-list match and
//     for log messages.
//   - construct   : builds the actual driver. Allowed to fail (e.g. invalid
//     config); on error the row is skipped with a warning.
//   - autoEnable  : in auto mode (AGENT_ENABLED_DRIVERS empty), should this
//     row be registered? Lets each driver carry its own
//     gating predicate (cfg field set, TCP probe, etc.).
//   - forceEnv    : optional env var name. If set to a non-empty value,
//     forces registration even when autoEnable would skip
//     (used to bypass a flaky probe on developer machines).
type driverReg struct {
	name       string
	construct  func(cfg config.Config, logger *obs.Logger) (agent.Driver, error)
	autoEnable func(cfg config.Config) bool
	forceEnv   string
}

// ollamaProbeAddr is the TCP endpoint probed in auto mode to decide if
// ollama should be registered. Exposed as a package var so tests can
// redirect it to a deterministic address; production code keeps the
// default 127.0.0.1:11434.
var ollamaProbeAddr = "127.0.0.1:11434"

// ollamaProbeTimeout is the per-probe TCP dial timeout. Same rationale as
// ollamaProbeAddr — overridable from tests.
var ollamaProbeTimeout = 500 * time.Millisecond

// k_driver_registry is the static list of all known agent drivers, in the
// order they are registered (which is also the AGENT_DEFAULT_DRIVER fallback
// order: the first successfully-registered driver wins). Adding a new
// driver = adding one row here.
var k_driver_registry = []driverReg{
	{
		name: "claude-code",
		construct: func(cfg config.Config, logger *obs.Logger) (agent.Driver, error) {
			opts := claudecode.Options{
				ExtraArgs: parseArgList(os.Getenv("AGENT_CLAUDE_CODE_EXTRA_ARGS")),
				Env:       map[string]string{},
				// AGENT_THINKING (default on): surface the butler's extended
				// thinking on the admin conversation page (ADR-029 §2.2).
				Thinking: envEnabledDefault(os.Getenv("AGENT_THINKING")),
			}
			// Disable Claude Code's native auto-memory (~/.claude/projects/<slug>/
			// memory) for every adapter-spawned claude session (ADR-021 §4,
			// 方向 B). The butler owns its long-term memory in the workspace
			// MEMORY/*.md files (persona writes them directly; the distill +
			// consolidate pipeline backstops them); letting the CLI's own memory
			// fork writes into ~/.claude would split memory across two unreconciled
			// stores and keep it off the admin page. Operator-set EXTRA_ARGS/Env
			// can still override if ever needed.
			opts.Env["CLAUDE_CODE_DISABLE_AUTO_MEMORY"] = "1"
			if cfg.ClaudeBaseURL != "" {
				opts.Env["ANTHROPIC_BASE_URL"] = cfg.ClaudeBaseURL
			}
			if cfg.ClaudeAuthToken != "" {
				opts.Env["ANTHROPIC_AUTH_TOKEN"] = cfg.ClaudeAuthToken
			}
			return claudecode.New(opts, logger), nil
		},
		// ADR-023: only register claude-code when the CLI is actually on PATH,
		// so the page's driver list / `installed` flag reflects reality instead
		// of advertising a driver that fails at first call. AGENT_CLAUDE_CODE_FORCE
		// bypasses the probe for non-PATH installs.
		autoEnable: func(cfg config.Config) bool { _, err := exec.LookPath("claude"); return err == nil },
		forceEnv:   "AGENT_CLAUDE_CODE_FORCE",
	},
	{
		name: "opencode",
		construct: func(cfg config.Config, logger *obs.Logger) (agent.Driver, error) {
			opts := opencode.Options{
				Bin:       os.Getenv("AGENT_OPENCODE_BIN"),
				ExtraArgs: parseArgList(os.Getenv("AGENT_OPENCODE_EXTRA_ARGS")),
				// ADR-031 P1-3: scoped provider creds for the serve process
				// (cloud_saas channel). Empty → serve inherits the adapter env.
				ProviderEnv: parseEnvMap(os.Getenv("AGENT_OPENCODE_PROVIDER_ENV")),
			}
			// ADR-031: serve+SDK backend (long-lived `opencode serve`, version
			// handshake, native streaming/interrupt/model-listing). Toggled from
			// the admin page (ai.opencode_serve, settings.json) or the
			// AGENT_OPENCODE_SERVE env; falls back to the legacy CLI-scrape driver
			// when off, so the migration is reversible ("migrate, don't flip").
			if cfg.OpencodeServeEnabled() {
				return opencode.NewServe(opts, logger), nil
			}
			return opencode.New(opts, logger), nil
		},
		// ADR-023: gate on the opencode CLI being present (see claude-code above).
		autoEnable: func(cfg config.Config) bool { _, err := exec.LookPath("opencode"); return err == nil },
		forceEnv:   "AGENT_OPENCODE_FORCE",
	},
	{
		name: "openclaw",
		construct: func(cfg config.Config, logger *obs.Logger) (agent.Driver, error) {
			return openclawdriver.New(openclawdriver.Options{
				URL:                strings.TrimSpace(cfg.OpenClawURL),
				AuthToken:          cfg.OpenClawAuthToken,
				NodeID:             cfg.OpenClawNodeID,
				DeviceIdentityPath: cfg.OpenClawIdentityPath,
				ReplyWaitTimeout:   cfg.OpenClawReplyWait,
				HTTPTimeout:        cfg.HTTPTimeout,
			}, logger), nil
		},
		autoEnable: func(cfg config.Config) bool {
			return strings.TrimSpace(cfg.OpenClawURL) != ""
		},
		forceEnv: "AGENT_OPENCLAW_FORCE",
	},
	{
		name: "ollama",
		construct: func(cfg config.Config, logger *obs.Logger) (agent.Driver, error) {
			return ollama.New(ollama.Options{}, logger), nil
		},
		autoEnable: func(cfg config.Config) bool {
			return probeTCP(ollamaProbeAddr, ollamaProbeTimeout)
		},
		forceEnv: "AGENT_OLLAMA_FORCE",
	},
	{
		name: "aider",
		construct: func(cfg config.Config, logger *obs.Logger) (agent.Driver, error) {
			return aider.New(aider.Options{
				Bin:       os.Getenv("AGENT_AIDER_BIN"),
				ExtraArgs: parseArgList(os.Getenv("AGENT_AIDER_EXTRA_ARGS")),
			}, logger), nil
		},
		autoEnable: func(cfg config.Config) bool {
			_, err := exec.LookPath("aider")
			return err == nil
		},
		forceEnv: "AGENT_AIDER_FORCE",
	},
	{
		name: "codex",
		construct: func(cfg config.Config, logger *obs.Logger) (agent.Driver, error) {
			return codex.New(codex.Options{
				Bin:       os.Getenv("AGENT_CODEX_BIN"),
				ExtraArgs: parseArgList(os.Getenv("AGENT_CODEX_EXTRA_ARGS")),
			}, logger), nil
		},
		autoEnable: func(cfg config.Config) bool {
			_, err := exec.LookPath("codex")
			return err == nil
		},
		forceEnv: "AGENT_CODEX_FORCE",
	},
}

// buildAgentRouter constructs the Router using these two env vars (both
// optional; zero-config means "auto-detect what's available"):
//
//	AGENT_ENABLED_DRIVERS  comma list (e.g. "claude-code,openclaw,ollama");
//	                       empty = auto mode — each driver's autoEnable
//	                       predicate decides (claude-code/opencode always,
//	                       openclaw only when cfg.OpenClawURL is set,
//	                       ollama only when 127.0.0.1:11434 listens).
//	AGENT_DEFAULT_DRIVER   request without explicit driver routes to this
//	                       one; empty = first registered driver.
//
// Registration order (which determines the default when AGENT_DEFAULT_DRIVER
// is unset) is the order of k_driver_registry above.
//
// Each driver may also expose a forceEnv (e.g. AGENT_OLLAMA_FORCE=1) to
// bypass its autoEnable predicate when the auto-detect heuristic is wrong
// on a developer machine. Everything else is hardcoded by design (see
// feedback_config_minimalism).
func buildAgentRouter(cfg config.Config, logger *obs.Logger) *agent.Router {
	return buildAgentRouterFromRegistry(cfg, logger, k_driver_registry, os.Getenv)
}

// memoryRunner builds the driver-aware PromptRunner for the long-term memory
// distill/consolidate step (ADR-024 §6): the memory follows the active driver.
// claude-code keeps its cheap `claude -p --model Haiku` path; codex/opencode
// distill through their own driver (a one-shot turn via the worker runner with
// no cwd). The active driver is the router's current default (kept in lock-step
// with active_driver). Falls back to the claude path when the active driver is
// unresolved or unregistered.
func memoryRunner(router *agent.Router, logger *obs.Logger) memory.PromptRunner {
	claudeRun := memory.ClaudePromptRunnerFromEnv(os.Getenv("AGENT_CLAUDE_CODE_BIN"))
	return func(ctx context.Context, prompt string) (string, error) {
		name := ""
		if router != nil {
			if d := router.Default(); d != nil {
				name = d.Name()
			}
		}
		if name == "" || name == "claude-code" {
			return claudeRun(ctx, prompt)
		}
		drv, ok := router.Get(name)
		if !ok {
			return claudeRun(ctx, prompt)
		}
		res, _, err := butlermcp.NewWorkerRunner(drv, 0).Run(ctx, "", prompt)
		return res, err
	}
}

// buildDriverState constructs the persistent driver-preference store. The
// state file lives under BBCLAW_DATA_DIR/driver_state.json (default
// ~/.bbclaw-adapter/driver_state.json), alongside the logical-session table.
//
// Returns nil when the store cannot be created (which only happens when the
// home directory is unresolvable — the adapter still runs without
// persistence, falling back to the router's first-registered default).
func buildDriverState(logger *obs.Logger) *driverstate.Store {
	dataDir := strings.TrimSpace(os.Getenv("BBCLAW_DATA_DIR"))
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			logger.Warnf("driverstate: cannot resolve home dir, persistence disabled: %v", err)
			return nil
		}
		dataDir = filepath.Join(home, ".bbclaw-adapter")
	}
	path := filepath.Join(dataDir, "driver_state.json")
	store, err := driverstate.NewStore(path, logger)
	if err != nil {
		logger.Warnf("driverstate: load failed at %s, persistence disabled: %v", path, err)
		return nil
	}
	return store
}

// buildSettingsStore loads the web-mutable runtime configuration (ADR-025) and
// overlays it onto cfg in place. settings.json lives next to driver_state.json
// under BBCLAW_DATA_DIR. On first run it is seeded from the env-derived cfg
// (BBCLAW_CWD_POOL-style one-time bootstrap); thereafter the file is the source
// of truth and the page is the home for ASR/TTS/cloud/openclaw/Anthropic config.
//
// After overlay the effective cfg is re-validated, so an invalid persisted
// combination is caught here (the PUT handler validates before writing, so this
// is normally unreachable). Returns nil when the data dir is unresolvable — the
// adapter then runs on env-only config.
func buildSettingsStore(cfg *config.Config, logger *obs.Logger) *settingsstore.Store {
	dataDir := strings.TrimSpace(os.Getenv("BBCLAW_DATA_DIR"))
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			logger.Warnf("settingsstore: cannot resolve home dir, web config disabled: %v", err)
			return nil
		}
		dataDir = filepath.Join(home, ".bbclaw-adapter")
	}
	path := filepath.Join(dataDir, "settings.json")

	seed := settingsstore.FromConfig(*cfg)
	switch status, serr := settingsstore.Bootstrap(path, seed); {
	case serr != nil:
		logger.Warnf("settingsstore: bootstrap failed at %s: %v", path, serr)
	case status == settingsstore.BootstrapSeeded:
		logger.Infof("settingsstore: seeded %s from .env (env is now a one-time bootstrap; manage config at /admin)", path)
	}

	store, err := settingsstore.Open(path, settingsstore.FromConfig(*cfg))
	if err != nil {
		// A corrupt file degrades to the env-derived base inside Open; log and
		// continue with whatever it returned rather than disabling web config.
		logger.Warnf("settingsstore: open %s degraded: %v", path, err)
	}
	if store == nil {
		return nil
	}

	store.Snapshot().ApplyTo(cfg)
	if verr := cfg.Validate(); verr != nil {
		logger.Errorf("settingsstore: effective config invalid after applying %s: %v", path, verr)
		os.Exit(1)
	}
	logger.Infof("settingsstore: ready path=%s cloud_relay=%t local_voice=%t", path, cfg.EnableCloudRelay(), cfg.LocalVoiceEnabled)
	return store
}

// applyDriverStateDefault reconciles the router's runtime default with the
// persisted active_driver. Called once at startup after both have been
// initialised. When the persisted name isn't a registered driver (e.g. the
// operator removed it from AGENT_ENABLED_DRIVERS since last run), we leave
// the router's auto-default in place and log a warning.
func applyDriverStateDefault(router *agent.Router, store *driverstate.Store, logger *obs.Logger) {
	if router == nil || store == nil {
		return
	}
	want := store.ActiveDriver()
	if want == "" {
		return
	}
	if router.SetDefault(want) {
		logger.Infof("driverstate: router default overridden to %q from persisted active_driver", want)
		return
	}
	logger.Warnf("driverstate: persisted active_driver=%q is not registered, keeping router default=%q",
		want, router.DefaultName())
}

// buildSessionManager constructs the logical-session table (ADR-014). The
// persistence path is BBCLAW_DATA_DIR/sessions.json (default
// ~/.bbclaw-adapter), and the default cwd for sessions created without an
// explicit cwd comes from BBCLAW_DEFAULT_CWD (empty falls through to the
// process's working directory at the time the driver spawns the CLI).
//
// Returns nil only on unrecoverable failure (we still want the adapter to
// run for ASR/TTS even if the session table can't be loaded).
func buildSessionManager(logger *obs.Logger) *logicalsession.Manager {
	dataDir := strings.TrimSpace(os.Getenv("BBCLAW_DATA_DIR"))
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			logger.Warnf("logicalsession: cannot resolve home dir, manager disabled: %v", err)
			return nil
		}
		dataDir = filepath.Join(home, ".bbclaw-adapter")
	}
	defaultCwd := strings.TrimSpace(os.Getenv("BBCLAW_DEFAULT_CWD"))
	path := filepath.Join(dataDir, "sessions.json")
	mgr, err := logicalsession.NewManager(path, defaultCwd, logger)
	if err != nil {
		logger.Warnf("logicalsession: load failed at %s, manager disabled: %v", path, err)
		return nil
	}
	if defaultCwd != "" {
		logger.Infof("logicalsession: ready path=%s default_cwd=%q", path, defaultCwd)
	} else {
		logger.Infof("logicalsession: ready path=%s", path)
	}
	return mgr
}

// buildAgentRouterFromRegistry is the testable core of buildAgentRouter. It
// takes the registry slice and an env-getter as parameters so unit tests
// can drive it deterministically without touching the real environment or
// the production registry.
func buildAgentRouterFromRegistry(cfg config.Config, logger *obs.Logger, registry []driverReg, getenv func(string) string) *agent.Router {
	router := agent.NewRouter()
	enabled := parseEnabledDrivers(getenv("AGENT_ENABLED_DRIVERS"))

	for _, reg := range registry {
		// Decide enabled in priority order:
		//   1. explicit AGENT_ENABLED_DRIVERS list — wins if non-nil
		//   2. forceEnv set to non-empty — bypasses autoEnable
		//   3. auto mode — autoEnable predicate
		var (
			enable bool
			reason string
		)
		switch {
		case enabled != nil:
			if enabled[reg.name] {
				enable = true
				reason = "explicitly enabled via AGENT_ENABLED_DRIVERS"
			} else {
				reason = "not listed in AGENT_ENABLED_DRIVERS"
			}
		case reg.forceEnv != "" && strings.TrimSpace(getenv(reg.forceEnv)) != "":
			enable = true
			reason = fmt.Sprintf("forced via %s", reg.forceEnv)
		default:
			if reg.autoEnable != nil && reg.autoEnable(cfg) {
				enable = true
				reason = "auto-detected"
			} else {
				reason = "auto-detect predicate false"
			}
		}

		if !enable {
			logger.Infof("agent router: %s skipped (%s)", reg.name, reason)
			continue
		}

		drv, err := reg.construct(cfg, logger)
		if err != nil {
			logger.Warnf("agent router: %s construct failed: %v", reg.name, err)
			continue
		}
		router.Register(drv, logger)
		logger.Infof("agent router: %s registered (%s)", reg.name, reason)
	}

	if want := strings.TrimSpace(getenv("AGENT_DEFAULT_DRIVER")); want != "" {
		if !router.SetDefault(want) {
			logger.Warnf("AGENT_DEFAULT_DRIVER=%q is not a registered driver; keeping %q", want, router.DefaultName())
		} else {
			logger.Infof("agent router: default overridden to %q via AGENT_DEFAULT_DRIVER", want)
		}
	}

	var names []string
	for _, info := range router.List() {
		names = append(names, info.Name)
	}
	logger.Infof("agent router ready drivers=%s default=%s", strings.Join(names, ","), router.DefaultName())
	return router
}

// parseEnabledDrivers turns a comma list into a set. Empty input returns
// nil (signalling "auto mode"); explicit empty entries are ignored.
func parseEnabledDrivers(raw string) map[string]bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out[part] = true
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseArgList splits a comma-separated string into a string slice, trimming
// whitespace and dropping empty segments. Used for CLI extra-args env vars.
func parseArgList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

// parseEnvMap parses a "KEY=VALUE,KEY2=VALUE2" list into a map. Used for
// AGENT_OPENCODE_PROVIDER_ENV (ADR-031 P1-3). Entries without '=' are skipped.
// Values may themselves contain '=' (only the first splits).
func parseEnvMap(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		k, v, ok := strings.Cut(part, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// envEnabledDefault parses a default-on boolean env var: empty/unset means
// enabled; only an explicit falsey value ("0" / "false" / "off" / "no") disables.
func envEnabledDefault(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// butlerMCPEnv collects the environment the butler's --mcp-config server entry
// embeds so the spawned `mcp-server` subprocess sees a deterministic project
// allowlist + credentials regardless of how the parent claude process scrubs
// its environment (ADR-021 §2). Returns nil when nothing needs embedding (the
// subprocess then relies purely on inherited env).
func butlerMCPEnv(cfg config.Config) map[string]string {
	env := map[string]string{}
	if v := strings.TrimSpace(os.Getenv("BBCLAW_CWD_POOL")); v != "" {
		env["BBCLAW_CWD_POOL"] = v
	}
	// Propagate the data-dir override so the mcp-server subprocess opens the same
	// projectstore file as the main process (admin-added projects survive there).
	if v := strings.TrimSpace(os.Getenv("BBCLAW_DATA_DIR")); v != "" {
		env["BBCLAW_DATA_DIR"] = v
	}
	if cfg.DefaultCwd != "" {
		env["BBCLAW_DEFAULT_CWD"] = cfg.DefaultCwd
	}
	if cfg.ClaudeBaseURL != "" {
		env["ANTHROPIC_BASE_URL"] = cfg.ClaudeBaseURL
	}
	if cfg.ClaudeAuthToken != "" {
		env["ANTHROPIC_AUTH_TOKEN"] = cfg.ClaudeAuthToken
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_CLAUDE_CODE_EXTRA_ARGS")); v != "" {
		env["AGENT_CLAUDE_CODE_EXTRA_ARGS"] = v
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_CLAUDE_CODE_BIN")); v != "" {
		env["AGENT_CLAUDE_CODE_BIN"] = v
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// probeTCP dials addr with the given timeout and returns whether the dial
// succeeded. Immediately closes the connection on success.
func probeTCP(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// logFileMaxBytes caps the runtime log file: on startup, if it already exceeds
// this, it's rotated to <path>.1 (single generation) so disk use stays bounded
// at ~2× this without a full rotation scheme.
const logFileMaxBytes = 16 << 20 // 16 MiB

// setupFileLog opens <DataDir>/adapter-runtime.log (append) and tees the logger
// into it so runtime output is persisted at a stable, documented path. Returns
// the path, or "" if it couldn't be set up (logging then stays stdout-only).
// Best-effort: a failure here must never block startup.
func setupFileLog(logger *obs.Logger) string {
	dir, err := workspace.DataDir()
	if err != nil {
		logger.Warnf("logfile: resolve data dir failed, file logging disabled: %v", err)
		return ""
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Warnf("logfile: mkdir %s failed, file logging disabled: %v", dir, err)
		return ""
	}
	path := filepath.Join(dir, "adapter-runtime.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > logFileMaxBytes {
		_ = os.Rename(path, path+".1") // best-effort single-generation rotation
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		logger.Warnf("logfile: open %s failed, file logging disabled: %v", path, err)
		return ""
	}
	// Held open for the process lifetime; the OS reclaims it on exit/re-exec.
	logger.Tee(f)
	logger.Infof("logfile: persisting runtime logs to %s", path)
	return path
}

func run(cfg config.Config, logger *obs.Logger, metrics *obs.Metrics) {
	// Persist logs to a stable file under the data dir and mirror them there in
	// addition to stdout, so the admin 日志 page and AI/CLI can read runtime
	// output without watching the binary's stdout (ADR-025). Best-effort.
	logPath := setupFileLog(logger)

	// Overlay web-managed settings (ADR-025) onto the env-derived cfg before
	// anything reads it, so ASR/TTS/cloud/openclaw/Anthropic config from the
	// admin page wins over .env. Mutates cfg in place + re-validates.
	settingsStore := buildSettingsStore(&cfg, logger)

	sink := buildSink(cfg, logger, metrics)
	agentRouter := buildAgentRouter(cfg, logger)
	sessionMgr := buildSessionManager(logger)
	driverStateStore := buildDriverState(logger)
	applyDriverStateDefault(agentRouter, driverStateStore, logger)

	// Ensure the butler workspace + default CLAUDE.md exist (ADR-021 §4). The
	// butler session runs with cwd=workspace so Claude loads this persona +
	// long-term memory natively. Non-fatal: degrade like driverstate if the
	// scaffold can't be written.
	butlerWorkspace := ""
	if dir, err := workspace.EnsureScaffold(); err != nil {
		logger.Warnf("workspace: scaffold failed, butler CLAUDE.md unavailable: %v", err)
	} else {
		butlerWorkspace = dir
		logger.Infof("workspace: ready dir=%s", dir)
	}

	// Build the butler's dispatch MCP server spec so its session can dispatch
	// coding work to worker agents through the `mcp-server` subcommand (ADR-021
	// §2). Format-neutral (ADR-024 §5): each driver renders it into its own
	// config shape, so codex/opencode butlers dispatch the same way claude does.
	// Only the per-device butler session carries it; worker sessions don't.
	// Non-fatal: degrade to a butler without dispatch.
	var butlerMCPServers []agent.MCPServerSpec
	if butlerWorkspace != "" {
		if self, err := os.Executable(); err != nil {
			logger.Warnf("butler-mcp: resolve executable failed, dispatch disabled: %v", err)
		} else {
			butlerMCPServers = []agent.MCPServerSpec{{
				Name:    butlermcp.ServerName,
				Command: self,
				Args:    []string{"mcp-server"},
				Env:     butlerMCPEnv(cfg),
			}}
			logger.Infof("butler-mcp: dispatch server ready command=%s", self)
		}
	}

	if len(cfg.CwdPool) > 0 {
		names := make([]string, len(cfg.CwdPool))
		for i, e := range cfg.CwdPool {
			names[i] = e.Name
		}
		logger.Infof("cwd-pool: projects=%s", strings.Join(names, ","))
	}

	// T4: Session expiration sweep — run once at startup and then every 24h.
	if sessionMgr != nil && cfg.SessionMaxAge > 0 {
		n := sessionMgr.Sweep(cfg.SessionMaxAge)
		if n > 0 {
			logger.Infof("logicalsession: startup sweep removed %d expired sessions (max_age=%s)", n, cfg.SessionMaxAge)
		}
	}

	// Shared butler memory + dispatch infra — built once and wired into BOTH the
	// cloud-relay and local-ingress butler engines (ADR-021 §4). Without this,
	// cloud-relayed butler turns silently skipped memory distillation + dispatch
	// recording, so a remote device's memory files never updated.
	butlerDeps := buildButlerInfra(butlerWorkspace, sessionMgr, agentRouter, logger)

	// In-flight turn registry for barge-in (ADR-028 §2.5.1) — one per process,
	// shared by the local HTTP server and the cloud relay so a device's cancel
	// always finds the running turn regardless of ingress.
	inflight := butler.NewInflightRegistry()

	var cloudRelay *homeadapter.Adapter
	var err error
	if cfg.EnableCloudRelay() {
		cloudRelay, err = buildCloudRelay(cfg, sink, logger, metrics)
		if err != nil {
			logger.Errorf("%v", err)
			os.Exit(1)
		}
		cloudRelay.SetRouter(agentRouter)
		// Cloud-relay butler engine shares the same memory writer + dispatch
		// observability as the local path (otherwise remote turns never persist
		// long-term memory or show up in dispatch history).
		cloudRelay.SetButlerInfra(butlerDeps.memory, butlerDeps.ring, butlerDeps.recorder)
		cloudRelay.SetInflight(inflight)
		if sessionMgr != nil {
			cloudRelay.SetSessionManager(sessionMgr)
			// Route cloud voice turns to the per-device butler session (ADR-021).
			cloudRelay.SetButlerWorkspace(butlerWorkspace, butlerMCPServers)
		}
		if driverStateStore != nil {
			cloudRelay.SetDriverState(driverStateStore)
		}
		// Wire CWD pool so cloud-proxied GET /v1/agent/cwd-pool works.
		if len(cfg.CwdPool) > 0 {
			pool := make([]homeadapter.CwdPoolEntry, len(cfg.CwdPool))
			for i, e := range cfg.CwdPool {
				pool[i] = homeadapter.CwdPoolEntry{Name: e.Name, Path: e.Path}
			}
			cloudRelay.SetCwdPool(pool)
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// T4: Periodic session sweep goroutine (every 24h).
	if sessionMgr != nil && cfg.SessionMaxAge > 0 {
		go runSessionSweepTicker(ctx, sessionMgr, cfg.SessionMaxAge, logger)
	}

	errCh := make(chan error, 2)
	active := 0

	if cfg.EnableCloudRelay() {
		active++
		cloudStatus := cloudRelay.Status()
		logger.Infof("starting bbclaw-adapter cloud_relay=enabled home_site=%s cloud=%s openclaw=%s",
			cloudStatus.HomeSiteID, cfg.CloudWSURL, cfg.OpenClawURL)
		go func() {
			errCh <- cloudRelay.Run(ctx)
		}()
	}

	if cfg.EnableLocalIngress() {
		active++
		httpSrv, agentSrv, err := buildLocalServer(cfg, sink, cloudRelay, agentRouter, sessionMgr, driverStateStore, settingsStore, butlerWorkspace, butlerMCPServers, butlerDeps, logger, metrics)
		if err != nil {
			logger.Errorf("%v", err)
			os.Exit(1)
		}
		agentSrv.SetInflight(inflight)
		// Surface read-only identity/diagnostics on the admin page: the resolved
		// device identity (env or identity.json), the build version, and the
		// persistent log path. These are shown but never edited (ADR-025).
		homeSiteID := strings.TrimSpace(cfg.HomeSiteID)
		if homeSiteID == "" {
			if id, derr := homeadapter.EnsureHomeSiteID(); derr == nil {
				homeSiteID = id
			}
		}
		agentSrv.SetIdentity(homeSiteID, buildinfo.Tag, logPath)
		logger.Infof("starting bbclaw-adapter local_ingress=enabled addr=%s asr_provider=%s tts_provider=%s cloud_relay=%t",
			cfg.Addr, cfg.ASRProvider, cfg.TTSProvider, cfg.EnableCloudRelay())
		go func() {
			err := httpSrv.ListenAndServe()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
				return
			}
			errCh <- nil
		}()
		// Pop the local admin page once the listener is up (best-effort, opt-out
		// via BBCLAW_OPEN_ADMIN=0). Headless hosts just log and continue.
		go maybeOpenAdminBrowser(cfg.Addr, logger)
		go func() {
			<-ctx.Done()
			shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelShutdown()
			// Tear down live agent sessions first so in-flight drivers get a
			// chance to flush; then drop the HTTP listener. Both honour the
			// 5-second deadline.
			_ = agentSrv.Shutdown(shutdownCtx)
			_ = httpSrv.Shutdown(shutdownCtx)
			// Release process-level driver resources (claude-code warm pool,
			// opencode serve, …) — any driver implementing agent.Shutdowner.
			for _, info := range agentRouter.List() {
				if drv, ok := agentRouter.Get(info.Name); ok {
					if sd, ok := drv.(agent.Shutdowner); ok {
						sd.Shutdown()
					}
				}
			}
		}()
	}

	if !cfg.EnableLocalIngress() && !cfg.EnableCloudRelay() {
		logger.Errorf("adapter has no enabled capabilities")
		os.Exit(1)
	}

	for active > 0 {
		select {
		case <-ctx.Done():
			return
		case err := <-errCh:
			active--
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.Errorf("adapter stopped: %v", err)
				os.Exit(1)
			}
		}
	}
}

// runSessionSweepTicker runs the logical-session expiration sweep every 24h
// until ctx is cancelled.
func runSessionSweepTicker(ctx context.Context, mgr *logicalsession.Manager, maxAge time.Duration, logger *obs.Logger) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n := mgr.Sweep(maxAge)
			if n > 0 {
				logger.Infof("logicalsession: periodic sweep removed %d expired sessions (max_age=%s)", n, maxAge)
			}
		}
	}
}
