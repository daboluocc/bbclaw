package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/daboluocc/bbclaw/adapter/internal/asr"
	"github.com/daboluocc/bbclaw/adapter/internal/audio"
	"github.com/daboluocc/bbclaw/adapter/internal/buildinfo"
	"github.com/daboluocc/bbclaw/adapter/internal/config"
	"github.com/daboluocc/bbclaw/adapter/internal/homeadapter"
	"github.com/daboluocc/bbclaw/adapter/internal/httpapi"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/openclaw"
	"github.com/daboluocc/bbclaw/adapter/internal/pipeline"
	"github.com/daboluocc/bbclaw/adapter/internal/tts"
)

func main() {
	if buildinfo.ShouldPrintVersion(os.Args[1:]) {
		fmt.Println(buildinfo.String("bbclaw-adapter"))
		return
	}

	logger := obs.NewLogger()
	metrics := obs.NewMetrics()

	cfg, err := config.LoadFromEnv()
	if err != nil {
		logger.Errorf("load config failed: %v", err)
		os.Exit(1)
	}

	logger.Infof("%s", buildinfo.String("bbclaw-adapter"))

	if cfg.IsCloudMode() {
		runCloud(cfg, logger, metrics)
	} else {
		runLocal(cfg, logger, metrics)
	}
}

func runCloud(cfg config.Config, logger *obs.Logger, metrics *obs.Metrics) {
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
	// Derive HomeSiteID if not set
	if strings.TrimSpace(homeCfg.HomeSiteID) == "" {
		derived, err := homeadapter.EnsureHomeSiteID()
		if err != nil {
			logger.Errorf("derive home_site_id failed: %v", err)
			os.Exit(1)
		}
		homeCfg.HomeSiteID = derived
	}
	if err := homeCfg.Validate(); err != nil {
		logger.Errorf("cloud config invalid: %v", err)
		os.Exit(1)
	}

	sink := pipeline.Wrap(openclaw.NewClient(cfg.OpenClawURL, cfg.HTTPTimeout, openclaw.Options{
		NodeID:             cfg.OpenClawNodeID,
		AuthToken:          cfg.OpenClawAuthToken,
		DeviceIdentityPath: cfg.OpenClawIdentityPath,
		ReplyWaitTimeout:   cfg.OpenClawReplyWait,
	}), logger, metrics)

	adapter := homeadapter.New(homeCfg, sink, logger, metrics)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Infof("starting bbclaw-adapter mode=cloud home_site=%s cloud=%s openclaw=%s",
		homeCfg.HomeSiteID, homeCfg.CloudWSURL, homeCfg.OpenClawURL)
	if err := adapter.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Errorf("adapter stopped: %v", err)
		os.Exit(1)
	}
}

func runLocal(cfg config.Config, logger *obs.Logger, metrics *obs.Metrics) {
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
	sink := pipeline.Wrap(openclaw.NewClient(cfg.OpenClawURL, cfg.HTTPTimeout, openclaw.Options{
		NodeID:             cfg.OpenClawNodeID,
		AuthToken:          cfg.OpenClawAuthToken,
		DeviceIdentityPath: cfg.OpenClawIdentityPath,
		ReplyWaitTimeout:   cfg.OpenClawReplyWait,
	}), logger, metrics)
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
			logger.Errorf("asr readiness: provider %q does not implement Ping", cfg.ASRProvider)
			os.Exit(1)
		}
		pctx, pcancel := context.WithTimeout(context.Background(), cfg.ASRReadinessTimeout)
		err := rp.Ping(pctx)
		pcancel()
		if err != nil {
			logger.Errorf("asr readiness probe failed: %v", err)
			os.Exit(1)
		}
		logger.Infof("asr readiness probe ok provider=%s", cfg.ASRProvider)
	}

	server := httpapi.NewServer(
		httpapi.AppConfig{
			AuthToken:            cfg.AuthToken,
			NodeID:               cfg.OpenClawNodeID,
			SaveAudio:            cfg.SaveAudio,
			SaveInputOnFinish:    cfg.SaveInputOnFinish,
			AudioInDir:           cfg.AudioInDir,
			AudioOutDir:          cfg.AudioOutDir,
			ASRTranscribeTimeout: cfg.ASRTranscribeTimeout,
		},
		streams, asrProvider, ttsProvider, sink, logger, metrics,
	)

	logger.Infof("starting bbclaw-adapter mode=local addr=%s asr_provider=%s tts_provider=%s",
		cfg.Addr, cfg.ASRProvider, cfg.TTSProvider)
	if err := http.ListenAndServe(cfg.Addr, server.Handler()); err != nil {
		logger.Errorf("http server stopped: %v", err)
		os.Exit(1)
	}
}
