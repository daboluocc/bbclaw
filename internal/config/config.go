package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr                 string
	AuthToken            string
	SaveAudio            bool
	SaveInputOnFinish    bool
	AudioInDir           string
	AudioOutDir          string
	ASRProvider          string
	ASRBaseURL           string
	ASRWSURL             string
	ASRAppID             string
	ASRAPIKey            string
	ASRResourceID        string
	ASRModel             string
	ASRLanguage          string
	ASRLocalBin          string
	ASRLocalArgs         []string
	ASRLocalTextPath     string
	TTSProvider          string
	TTSToken             string
	TTSAppID             string
	TTSCluster           string
	TTSVoice             string
	TTSWSURL             string
	TTSLocalBin          string
	TTSLocalArgs         []string
	TTSLocalOutputFormat string
	OpenClawURL          string
	OpenClawAuthToken    string
	OpenClawIdentityPath string
	OpenClawNodeID       string
	OpenClawReplyWait    time.Duration
	MaxStreamSeconds     int
	MaxAudioBytes        int
	MaxConcurrentStreams int
	HTTPTimeout          time.Duration
	ASRTranscribeTimeout time.Duration
	ASRReadinessProbe    bool
	ASRReadinessTimeout  time.Duration
}

func LoadFromEnv() (Config, error) {
	openclawURL := strings.TrimSpace(os.Getenv("OPENCLAW_WS_URL"))
	if openclawURL == "" {
		openclawURL = strings.TrimSpace(os.Getenv("OPENCLAW_RPC_URL"))
	}
	if openclawURL == "" {
		openclawURL = "ws://127.0.0.1:18789"
	}
	cfg := Config{
		Addr:                 getEnvOrDefault("ADAPTER_ADDR", ":18080"),
		AuthToken:            strings.TrimSpace(os.Getenv("ADAPTER_AUTH_TOKEN")),
		SaveAudio:            getEnvBool("SAVE_AUDIO", false),
		SaveInputOnFinish:    getEnvBool("SAVE_INPUT_ON_FINISH", true),
		AudioInDir:           getEnvOrDefault("AUDIO_IN_DIR", "tmp/audio-in"),
		AudioOutDir:          getEnvOrDefault("AUDIO_OUT_DIR", "tmp/audio-out"),
		ASRProvider:          getEnvOrDefault("ASR_PROVIDER", "local"),
		ASRBaseURL:           strings.TrimSpace(os.Getenv("ASR_BASE_URL")),
		ASRWSURL:             getEnvOrDefault("ASR_WS_URL", "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_nostream"),
		ASRAppID:             strings.TrimSpace(os.Getenv("ASR_APP_ID")),
		ASRAPIKey:            strings.TrimSpace(os.Getenv("ASR_API_KEY")),
		ASRModel:             getEnvOrDefault("ASR_MODEL", "gpt-4o-mini-transcribe"),
		ASRResourceID:        getEnvOrDefault("ASR_RESOURCE_ID", "volc.bigasr.sauc.duration"),
		ASRLanguage:          getEnvOrDefault("ASR_LANGUAGE", "zh-CN"),
		ASRLocalBin:          strings.TrimSpace(os.Getenv("ASR_LOCAL_BIN")),
		ASRLocalArgs:         splitLocalArgs(os.Getenv("ASR_LOCAL_ARGS")),
		ASRLocalTextPath:     strings.TrimSpace(os.Getenv("ASR_LOCAL_TEXT_PATH")),
		TTSProvider:          getEnvOrDefault("TTS_PROVIDER", "doubao_native"),
		TTSToken:             strings.TrimSpace(os.Getenv("TTS_TOKEN")),
		TTSAppID:             strings.TrimSpace(os.Getenv("TTS_APP_ID")),
		TTSCluster:           getEnvOrDefault("TTS_CLUSTER", "volcano_tts"),
		TTSVoice:             getEnvOrDefault("TTS_VOICE", "zh_female_wanwanxiaohe_moon_bigtts"),
		TTSWSURL:             getEnvOrDefault("TTS_WS_URL", "wss://openspeech.bytedance.com/api/v1/tts/ws_binary"),
		TTSLocalBin:          strings.TrimSpace(os.Getenv("TTS_LOCAL_BIN")),
		TTSLocalArgs:         splitLocalArgs(os.Getenv("TTS_LOCAL_ARGS")),
		TTSLocalOutputFormat: getEnvOrDefault("TTS_LOCAL_OUTPUT_FORMAT", "wav"),
		OpenClawURL:          openclawURL,
		OpenClawAuthToken:    strings.TrimSpace(os.Getenv("OPENCLAW_AUTH_TOKEN")),
		OpenClawIdentityPath: strings.TrimSpace(os.Getenv("OPENCLAW_DEVICE_IDENTITY_PATH")),
		OpenClawNodeID:       getEnvOrDefault("OPENCLAW_NODE_ID", "bbclaw-adapter"),
		OpenClawReplyWait:    time.Duration(getEnvInt("OPENCLAW_REPLY_WAIT_SECONDS", 25)) * time.Second,
		MaxStreamSeconds:     getEnvInt("MAX_STREAM_SECONDS", 90),
		MaxAudioBytes:        getEnvInt("MAX_AUDIO_BYTES", 4*1024*1024),
		MaxConcurrentStreams: getEnvInt("MAX_CONCURRENT_STREAMS", 16),
		HTTPTimeout:          time.Duration(getEnvInt("HTTP_TIMEOUT_SECONDS", 30)) * time.Second,
		ASRTranscribeTimeout: time.Duration(getEnvInt("ASR_TRANSCRIBE_TIMEOUT_SECONDS", 10)) * time.Second,
		ASRReadinessProbe:    getEnvBool("ASR_READINESS_PROBE", true),
		ASRReadinessTimeout:  time.Duration(getEnvInt("ASR_READINESS_TIMEOUT_SECONDS", 8)) * time.Second,
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Addr) == "" {
		return errors.New("ADAPTER_ADDR is required")
	}
	if c.SaveAudio || c.SaveInputOnFinish {
		if strings.TrimSpace(c.AudioInDir) == "" {
			return errors.New("AUDIO_IN_DIR is required when SAVE_AUDIO=true or SAVE_INPUT_ON_FINISH=true")
		}
	}
	if c.SaveAudio {
		if strings.TrimSpace(c.AudioOutDir) == "" {
			return errors.New("AUDIO_OUT_DIR is required when SAVE_AUDIO=true")
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.ASRProvider)) {
	case "local":
		if strings.TrimSpace(c.ASRLocalBin) == "" {
			return errors.New("ASR_LOCAL_BIN is required for ASR_PROVIDER=local")
		}
	case "openai_compatible":
		if strings.TrimSpace(c.ASRBaseURL) == "" {
			return errors.New("ASR_BASE_URL is required")
		}
		if strings.TrimSpace(c.ASRAPIKey) == "" {
			return errors.New("ASR_API_KEY is required")
		}
		if strings.TrimSpace(c.ASRModel) == "" {
			return errors.New("ASR_MODEL is required")
		}
		if _, err := url.ParseRequestURI(c.ASRBaseURL); err != nil {
			return fmt.Errorf("ASR_BASE_URL is invalid: %w", err)
		}
	case "doubao_native":
		if strings.TrimSpace(c.ASRWSURL) == "" {
			return errors.New("ASR_WS_URL is required for doubao_native")
		}
		if strings.TrimSpace(c.ASRAppID) == "" {
			return errors.New("ASR_APP_ID is required for doubao_native")
		}
		if strings.TrimSpace(c.ASRAPIKey) == "" {
			return errors.New("ASR_API_KEY is required for doubao_native")
		}
		if strings.TrimSpace(c.ASRResourceID) == "" {
			return errors.New("ASR_RESOURCE_ID is required for doubao_native")
		}
		if _, err := url.ParseRequestURI(c.ASRWSURL); err != nil {
			return fmt.Errorf("ASR_WS_URL is invalid: %w", err)
		}
	default:
		return fmt.Errorf("ASR_PROVIDER is unsupported: %s", c.ASRProvider)
	}
	if strings.TrimSpace(c.OpenClawURL) == "" {
		return errors.New("OPENCLAW_WS_URL or OPENCLAW_RPC_URL is required")
	}
	if strings.TrimSpace(c.OpenClawNodeID) == "" {
		return errors.New("OPENCLAW_NODE_ID is required")
	}
	if c.OpenClawReplyWait <= 0 {
		return errors.New("OPENCLAW_REPLY_WAIT_SECONDS must be > 0")
	}
	if c.MaxStreamSeconds <= 0 {
		return errors.New("MAX_STREAM_SECONDS must be > 0")
	}
	if c.MaxAudioBytes <= 0 {
		return errors.New("MAX_AUDIO_BYTES must be > 0")
	}
	if c.MaxConcurrentStreams <= 0 {
		return errors.New("MAX_CONCURRENT_STREAMS must be > 0")
	}
	if c.HTTPTimeout <= 0 {
		return errors.New("HTTP_TIMEOUT_SECONDS must be > 0")
	}
	if c.ASRTranscribeTimeout <= 0 {
		return errors.New("ASR_TRANSCRIBE_TIMEOUT_SECONDS must be > 0")
	}
	if c.ASRReadinessProbe && c.ASRReadinessTimeout <= 0 {
		return errors.New("ASR_READINESS_TIMEOUT_SECONDS must be > 0 when ASR_READINESS_PROBE is enabled")
	}
	openclawEndpoint, err := url.ParseRequestURI(c.OpenClawURL)
	if err != nil {
		return fmt.Errorf("OPENCLAW endpoint is invalid: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(openclawEndpoint.Scheme)) {
	case "http", "https", "ws", "wss":
	default:
		return fmt.Errorf("OPENCLAW endpoint scheme must be one of http/https/ws/wss, got: %s", openclawEndpoint.Scheme)
	}
	switch strings.ToLower(strings.TrimSpace(c.TTSProvider)) {
	case "mock":
		// No external credentials needed in mock mode.
	case "local_command":
		if strings.TrimSpace(c.TTSLocalBin) == "" {
			return errors.New("TTS_LOCAL_BIN is required for TTS_PROVIDER=local_command")
		}
	case "", "doubao_native":
		if strings.TrimSpace(c.TTSWSURL) == "" {
			return errors.New("TTS_WS_URL is required for doubao_native")
		}
		if strings.TrimSpace(c.TTSAppID) == "" {
			return errors.New("TTS_APP_ID is required for doubao_native")
		}
		if strings.TrimSpace(c.TTSToken) == "" {
			return errors.New("TTS_TOKEN is required for doubao_native")
		}
		if strings.TrimSpace(c.TTSCluster) == "" {
			return errors.New("TTS_CLUSTER is required for doubao_native")
		}
		if strings.TrimSpace(c.TTSVoice) == "" {
			return errors.New("TTS_VOICE is required for doubao_native")
		}
		if _, err := url.ParseRequestURI(c.TTSWSURL); err != nil {
			return fmt.Errorf("TTS_WS_URL is invalid: %w", err)
		}
	default:
		return fmt.Errorf("TTS_PROVIDER is unsupported: %s", c.TTSProvider)
	}
	return nil
}

func splitLocalArgs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

func getEnvOrDefault(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func getEnvInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func getEnvBool(name string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
