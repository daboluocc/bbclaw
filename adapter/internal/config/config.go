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

// CwdEntry is one entry in the CWD pool: a human-readable name and the
// absolute filesystem path it maps to.
type CwdEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type Config struct {
	// AdapterMode: "auto" (default, local HTTP always on + cloud relay when configured),
	// "local" (force local only), or "cloud" (force cloud relay only).
	AdapterMode string

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

	// Session management tunables.
	SessionReuseWindow time.Duration // BBCLAW_SESSION_REUSE_WINDOW — reuse recent session within this window
	SessionMaxAge      time.Duration // BBCLAW_SESSION_MAX_AGE — sweep sessions older than this

	// CWD pool (issue #30 / T3).
	// BBCLAW_CWD_POOL="name:path,name:path,..." — multi-project working directory selection.
	// BBCLAW_DEFAULT_CWD is kept as a single-entry fallback when the pool is empty.
	DefaultCwd string     // BBCLAW_DEFAULT_CWD
	CwdPool    []CwdEntry // parsed from BBCLAW_CWD_POOL

	// Cloud relay fields.
	CloudWSURL     string
	CloudAuthToken string
	HomeSiteID     string
	ReconnectDelay time.Duration
}

func (c Config) normalizedMode() string {
	mode := strings.ToLower(strings.TrimSpace(c.AdapterMode))
	if mode == "" {
		return "auto"
	}
	return mode
}

func (c Config) cloudConfigured() bool {
	return strings.TrimSpace(c.CloudWSURL) != ""
}

// EnableLocalIngress returns true when the adapter should serve the local HTTP API.
func (c Config) EnableLocalIngress() bool {
	switch c.normalizedMode() {
	case "cloud":
		return false
	default:
		return true
	}
}

// EnableCloudRelay returns true when the adapter should connect to Cloud as a home adapter.
func (c Config) EnableCloudRelay() bool {
	switch c.normalizedMode() {
	case "cloud":
		return true
	case "local":
		return false
	default:
		return c.cloudConfigured()
	}
}

func LoadFromEnv() (Config, error) {
	openclawURL := strings.TrimSpace(os.Getenv("OPENCLAW_WS_URL"))
	if openclawURL == "" {
		openclawURL = strings.TrimSpace(os.Getenv("OPENCLAW_RPC_URL"))
	}
	if openclawURL == "" {
		openclawURL = "ws://127.0.0.1:18789"
	}
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("ADAPTER_MODE")))
	if mode == "" {
		mode = "auto"
	}
	cfg := Config{
		AdapterMode:          mode,
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
		// Default points at the production SaaS so a fresh adapter installs and
		// connects with zero config. Cloud relays are claim_required until the
		// portal user types in the registration code, so unauthenticated traffic
		// can't move. Set ADAPTER_MODE=local to opt out entirely.
		CloudWSURL:           getEnvOrDefault("CLOUD_WS_URL", "wss://bbclaw.daboluo.cc/ws"),
		CloudAuthToken:       strings.TrimSpace(os.Getenv("CLOUD_AUTH_TOKEN")),
		HomeSiteID:           strings.TrimSpace(os.Getenv("HOME_SITE_ID")),
		ReconnectDelay:       time.Duration(getEnvInt("CLOUD_RECONNECT_DELAY_SECONDS", 3)) * time.Second,
		SessionReuseWindow:   getEnvDuration("BBCLAW_SESSION_REUSE_WINDOW", 5*time.Minute),
		SessionMaxAge:        getEnvDuration("BBCLAW_SESSION_MAX_AGE", 7*24*time.Hour),
		DefaultCwd:           strings.TrimSpace(os.Getenv("BBCLAW_DEFAULT_CWD")),
		CwdPool:              parseCwdPool(os.Getenv("BBCLAW_CWD_POOL"), strings.TrimSpace(os.Getenv("BBCLAW_DEFAULT_CWD"))),
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	switch c.normalizedMode() {
	case "auto", "local", "cloud":
	default:
		return fmt.Errorf("ADAPTER_MODE must be 'auto', 'local', or 'cloud', got: %s", c.AdapterMode)
	}

	// Common validation
	if strings.TrimSpace(c.OpenClawURL) == "" {
		return errors.New("OPENCLAW_WS_URL or OPENCLAW_RPC_URL is required")
	}
	if strings.TrimSpace(c.OpenClawNodeID) == "" {
		return errors.New("OPENCLAW_NODE_ID is required")
	}
	if c.OpenClawReplyWait <= 0 {
		return errors.New("OPENCLAW_REPLY_WAIT_SECONDS must be > 0")
	}
	if c.HTTPTimeout <= 0 {
		return errors.New("HTTP_TIMEOUT_SECONDS must be > 0")
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

	if c.EnableLocalIngress() {
		if err := c.validateLocal(); err != nil {
			return err
		}
	}
	if c.EnableCloudRelay() {
		if err := c.validateCloud(); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) validateCloud() error {
	if strings.TrimSpace(c.CloudWSURL) == "" {
		return errors.New("CLOUD_WS_URL is required when cloud relay is enabled")
	}
	if c.ReconnectDelay <= 0 {
		return errors.New("CLOUD_RECONNECT_DELAY_SECONDS must be > 0")
	}
	return nil
}

func (c Config) validateLocal() error {
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
	if c.MaxStreamSeconds <= 0 {
		return errors.New("MAX_STREAM_SECONDS must be > 0")
	}
	if c.MaxAudioBytes <= 0 {
		return errors.New("MAX_AUDIO_BYTES must be > 0")
	}
	if c.MaxConcurrentStreams <= 0 {
		return errors.New("MAX_CONCURRENT_STREAMS must be > 0")
	}
	if c.ASRTranscribeTimeout <= 0 {
		return errors.New("ASR_TRANSCRIBE_TIMEOUT_SECONDS must be > 0")
	}
	if c.ASRReadinessProbe && c.ASRReadinessTimeout <= 0 {
		return errors.New("ASR_READINESS_TIMEOUT_SECONDS must be > 0 when ASR_READINESS_PROBE is enabled")
	}
	switch strings.ToLower(strings.TrimSpace(c.TTSProvider)) {
	case "mock":
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

// parseCwdPool parses BBCLAW_CWD_POOL="name:path,name:path,..." into a slice
// of CwdEntry. Malformed entries (missing colon, empty name or path) are
// silently skipped. When the pool string is empty and defaultCwd is non-empty,
// a single synthetic entry named "default" is returned so callers always have
// at least one entry to work with. When both are empty, nil is returned.
func parseCwdPool(poolEnv, defaultCwd string) []CwdEntry {
	poolEnv = strings.TrimSpace(poolEnv)
	var entries []CwdEntry
	if poolEnv != "" {
		for _, part := range strings.Split(poolEnv, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			idx := strings.IndexByte(part, ':')
			if idx <= 0 {
				continue // no colon or empty name
			}
			name := strings.TrimSpace(part[:idx])
			path := strings.TrimSpace(part[idx+1:])
			if name == "" || path == "" {
				continue
			}
			entries = append(entries, CwdEntry{Name: name, Path: path})
		}
	}
	// Fall back to BBCLAW_DEFAULT_CWD as a single-entry pool when pool is empty.
	if len(entries) == 0 && defaultCwd != "" {
		entries = []CwdEntry{{Name: "default", Path: defaultCwd}}
	}
	return entries
}

// getEnvDuration parses a duration from an env var. Supports Go duration
// strings (e.g. "5m", "24h", "7d") with a special case for the "d" suffix
// (days) which time.ParseDuration doesn't handle natively.
func getEnvDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	// Handle "Nd" shorthand for days (e.g. "7d" → 7*24h).
	if strings.HasSuffix(raw, "d") {
		numStr := strings.TrimSuffix(raw, "d")
		if n, err := strconv.Atoi(numStr); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return d
}
