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
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent/claudecode"
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
	run(cfg, logger, metrics)
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

func buildLocalServer(cfg config.Config, sink pipeline.Sink, cloudRelay *homeadapter.Adapter, logger *obs.Logger, metrics *obs.Metrics) (*http.Server, error) {
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
			return nil, fmt.Errorf("asr readiness: provider %q does not implement Ping", cfg.ASRProvider)
		}
		pctx, pcancel := context.WithTimeout(context.Background(), cfg.ASRReadinessTimeout)
		err := rp.Ping(pctx)
		pcancel()
		if err != nil {
			return nil, fmt.Errorf("asr readiness probe failed: %w", err)
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
	server.SetAgentDriver(claudecode.New(claudecode.Options{}, logger))
	return &http.Server{
		Addr:    cfg.Addr,
		Handler: server.Handler(),
	}, nil
}

func run(cfg config.Config, logger *obs.Logger, metrics *obs.Metrics) {
	sink := buildSink(cfg, logger, metrics)
	var cloudRelay *homeadapter.Adapter
	var err error
	if cfg.EnableCloudRelay() {
		cloudRelay, err = buildCloudRelay(cfg, sink, logger, metrics)
		if err != nil {
			logger.Errorf("%v", err)
			os.Exit(1)
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
		server, err := buildLocalServer(cfg, sink, cloudRelay, logger, metrics)
		if err != nil {
			logger.Errorf("%v", err)
			os.Exit(1)
		}
		logger.Infof("starting bbclaw-adapter local_ingress=enabled addr=%s asr_provider=%s tts_provider=%s cloud_relay=%t",
			cfg.Addr, cfg.ASRProvider, cfg.TTSProvider, cfg.EnableCloudRelay())
		go func() {
			err := server.ListenAndServe()
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
			_ = server.Shutdown(shutdownCtx)
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
