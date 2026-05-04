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
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/ollama"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/openclawdriver"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/opencode"
	"github.com/daboluocc/bbclaw/adapter/internal/asr"
	"github.com/daboluocc/bbclaw/adapter/internal/audio"
	"github.com/daboluocc/bbclaw/adapter/internal/buildinfo"
	"github.com/daboluocc/bbclaw/adapter/internal/cmd"
	"github.com/daboluocc/bbclaw/adapter/internal/config"
	"github.com/daboluocc/bbclaw/adapter/internal/homeadapter"
	"github.com/daboluocc/bbclaw/adapter/internal/httpapi"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/openclaw"
	"github.com/daboluocc/bbclaw/adapter/internal/pipeline"
	"github.com/daboluocc/bbclaw/adapter/internal/tts"
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

func buildLocalServer(cfg config.Config, sink pipeline.Sink, cloudRelay *homeadapter.Adapter, agentRouter *agent.Router, sessionMgr *logicalsession.Manager, logger *obs.Logger, metrics *obs.Metrics) (*http.Server, *httpapi.Server, error) {
	streams := audio.NewManager(cfg.MaxAudioBytes, cfg.MaxStreamSeconds, cfg.MaxConcurrentStreams)
	var asrProvider asr.Provider
	switch strings.ToLower(strings.TrimSpace(cfg.ASRProvider)) {
	case "doubao_native":
		asrProvider = asr.NewDoubaoNativeProvider(
			cfg.ASRWSURL, cfg.ASRAppID, cfg.ASRAPIKey, cfg.ASRResourceID, cfg.ASRModel, cfg.ASRLanguage,
		)
	case "local":
		asrProvider = asr.NewLocalCommandProvider(cfg.ASRLocalBin, cfg.ASRLocalArgs, cfg.ASRLocalTextPath)
	default:
		asrProvider = asr.NewOpenAICompatibleProvider(
			cfg.ASRBaseURL, cfg.ASRAPIKey, cfg.ASRModel, &http.Client{Timeout: cfg.HTTPTimeout},
		)
	}
	var ttsProvider tts.Provider
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
		},
		streams, asrProvider, ttsProvider, sink, logger, metrics,
	)
	server.SetAgentRouter(agentRouter)
	if sessionMgr != nil {
		server.SetSessionManager(sessionMgr)
	}
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
			return claudecode.New(claudecode.Options{}, logger), nil
		},
		autoEnable: func(cfg config.Config) bool { return true },
	},
	{
		name: "opencode",
		construct: func(cfg config.Config, logger *obs.Logger) (agent.Driver, error) {
			return opencode.New(opencode.Options{
				Bin:       os.Getenv("AGENT_OPENCODE_BIN"),
				ExtraArgs: parseArgList(os.Getenv("AGENT_OPENCODE_EXTRA_ARGS")),
			}, logger), nil
		},
		autoEnable: func(cfg config.Config) bool { return true },
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
	logger.Infof("logicalsession: ready path=%s default_cwd=%q", path, defaultCwd)
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

func run(cfg config.Config, logger *obs.Logger, metrics *obs.Metrics) {
	sink := buildSink(cfg, logger, metrics)
	agentRouter := buildAgentRouter(cfg, logger)
	sessionMgr := buildSessionManager(logger)
	var cloudRelay *homeadapter.Adapter
	var err error
	if cfg.EnableCloudRelay() {
		cloudRelay, err = buildCloudRelay(cfg, sink, logger, metrics)
		if err != nil {
			logger.Errorf("%v", err)
			os.Exit(1)
		}
		cloudRelay.SetRouter(agentRouter)
		if sessionMgr != nil {
			cloudRelay.SetSessionManager(sessionMgr)
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

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
		httpSrv, agentSrv, err := buildLocalServer(cfg, sink, cloudRelay, agentRouter, sessionMgr, logger, metrics)
		if err != nil {
			logger.Errorf("%v", err)
			os.Exit(1)
		}
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
		go func() {
			<-ctx.Done()
			shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelShutdown()
			// Tear down live agent sessions first so in-flight drivers get a
			// chance to flush; then drop the HTTP listener. Both honour the
			// 5-second deadline.
			_ = agentSrv.Shutdown(shutdownCtx)
			_ = httpSrv.Shutdown(shutdownCtx)
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
