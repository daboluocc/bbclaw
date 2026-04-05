package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/zhoushoujianwork/bbclaw/adapter/internal/asr"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/audio"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/buildinfo"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/config"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/httpapi"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/obs"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/openclaw"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/pipeline"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/tts"
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

	streams := audio.NewManager(cfg.MaxAudioBytes, cfg.MaxStreamSeconds, cfg.MaxConcurrentStreams)
	var asrProvider asr.Provider
	switch strings.ToLower(strings.TrimSpace(cfg.ASRProvider)) {
	case "doubao_native":
		asrProvider = asr.NewDoubaoNativeProvider(
			cfg.ASRWSURL,
			cfg.ASRAppID,
			cfg.ASRAPIKey,
			cfg.ASRResourceID,
			cfg.ASRModel,
			cfg.ASRLanguage,
		)
	case "local":
		asrProvider = asr.NewLocalCommandProvider(cfg.ASRLocalBin, cfg.ASRLocalArgs, cfg.ASRLocalTextPath)
	default:
		asrProvider = asr.NewOpenAICompatibleProvider(
			cfg.ASRBaseURL,
			cfg.ASRAPIKey,
			cfg.ASRModel,
			&http.Client{Timeout: cfg.HTTPTimeout},
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
		ttsProvider = tts.NewLocalCommandProvider(
			cfg.TTSLocalBin,
			cfg.TTSLocalArgs,
			cfg.TTSLocalOutputFormat,
		)
	default:
		ttsProvider = tts.NewDoubaoNativeProvider(
			cfg.TTSWSURL,
			cfg.TTSAppID,
			cfg.TTSToken,
			cfg.TTSCluster,
			cfg.TTSVoice,
		)
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
			logger.Errorf("asr readiness probe failed (check ASR_* env / network): %v", err)
			os.Exit(1)
		}
		if strings.EqualFold(strings.TrimSpace(cfg.ASRProvider), "local") {
			logger.Infof("asr readiness probe ok provider=%s %s transcribe_timeout=%s readiness_timeout=%s readiness_probe=%v",
				cfg.ASRProvider,
				asr.LocalConfigSummary(cfg.ASRLocalBin, cfg.ASRLocalArgs, cfg.ASRLocalTextPath),
				cfg.ASRTranscribeTimeout,
				cfg.ASRReadinessTimeout,
				cfg.ASRReadinessProbe,
			)
		} else {
			logger.Infof("asr readiness probe ok provider=%s", cfg.ASRProvider)
		}
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
		streams,
		asrProvider,
		ttsProvider,
		sink,
		logger,
		metrics,
	)

	logger.Infof("%s", buildinfo.String("bbclaw-adapter"))
	logger.Infof("starting bbclaw-adapter addr=%s asr_provider=%s tts_provider=%s asr_transcribe_timeout=%s readiness_probe=%v",
		cfg.Addr, cfg.ASRProvider, cfg.TTSProvider, cfg.ASRTranscribeTimeout, cfg.ASRReadinessProbe)
	if strings.EqualFold(strings.TrimSpace(cfg.ASRProvider), "local") && !cfg.ASRReadinessProbe {
		logger.Infof("asr %s transcribe_timeout=%s readiness_probe=off",
			asr.LocalConfigSummary(cfg.ASRLocalBin, cfg.ASRLocalArgs, cfg.ASRLocalTextPath), cfg.ASRTranscribeTimeout)
	}
	if err := http.ListenAndServe(cfg.Addr, server.Handler()); err != nil {
		logger.Errorf("http server stopped: %v", fmt.Errorf("listen: %w", err))
		os.Exit(1)
	}
}
