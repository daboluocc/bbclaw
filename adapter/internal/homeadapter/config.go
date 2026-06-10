package homeadapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Config struct {
	CloudWSURL           string
	CloudAuthToken       string
	HomeSiteID           string
	ReconnectDelay       time.Duration
	HTTPTimeout          time.Duration
	OpenClawURL          string
	OpenClawAuthToken    string
	OpenClawNodeID       string
	OpenClawReplyWait    time.Duration
	OpenClawIdentityPath string

	// HeartbeatInterval controls how often a voice.reply.heartbeat envelope is
	// sent to the cloud during silent stretches of a long agent turn (tool
	// execution, long thinking, etc.). This keeps the cloud hub's idle-window
	// timer from expiring before the turn completes. Only applied on the cloud
	// WebSocket path; LAN-direct (local_home) never constructs CloudEnvelopes.
	// Set via BBCLAW_HOMEADAPTER_HEARTBEAT_INTERVAL (e.g. "10s"). <= 0 disables.
	HeartbeatInterval time.Duration
}

type identityFile struct {
	HomeSiteID string `json:"homeSiteId"`
}

// identityPath returns ~/.bbclaw-adapter/identity.json, using $HOME when set
// (allows tests to redirect via t.Setenv("HOME", dir)).
func identityPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return filepath.Join(home, ".bbclaw-adapter", "identity.json"), nil
}

// EnsureHomeSiteID returns HOME_SITE_ID env var if set; otherwise loads the
// persisted ID from ~/.bbclaw-adapter/identity.json, creating it on first run.
// This guarantees the same ID across restarts regardless of hostname changes.
func EnsureHomeSiteID() (string, error) {
	if id := strings.TrimSpace(os.Getenv("HOME_SITE_ID")); id != "" {
		return id, nil
	}
	path, err := identityPath()
	if err != nil {
		return "", err
	}
	// Try to read existing identity.
	if raw, err := os.ReadFile(path); err == nil {
		var f identityFile
		if json.Unmarshal(raw, &f) == nil && strings.TrimSpace(f.HomeSiteID) != "" {
			return strings.TrimSpace(f.HomeSiteID), nil
		}
	}
	// First run: generate and persist a new random UUID.
	id := uuid.New().String()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create identity dir: %w", err)
	}
	data, _ := json.Marshal(identityFile{HomeSiteID: id})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write identity file: %w", err)
	}
	return id, nil
}

func LoadFromEnv() (Config, error) {
	openclawURL := strings.TrimSpace(os.Getenv("OPENCLAW_WS_URL"))
	if openclawURL == "" {
		openclawURL = "ws://127.0.0.1:18789"
	}

	homeSiteID, err := EnsureHomeSiteID()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		// Default to production SaaS so adapter dials cloud out-of-the-box.
		// CLOUD_ALLOW_ANON_HOME_ADAPTER on the cloud side accepts the upgrade
		// without a token; the peer is held in claim_required until claimed.
		// Override via env (or set ADAPTER_MODE=local in the parent config) to
		// disable cloud relay.
		CloudWSURL:           getEnvOrDefault("CLOUD_WS_URL", "wss://bbclaw.daboluo.cc/ws"),
		CloudAuthToken:       strings.TrimSpace(os.Getenv("CLOUD_AUTH_TOKEN")),
		HomeSiteID:           homeSiteID,
		ReconnectDelay:       time.Duration(getEnvInt("CLOUD_RECONNECT_DELAY_SECONDS", 3)) * time.Second,
		HTTPTimeout:          time.Duration(getEnvInt("HTTP_TIMEOUT_SECONDS", 30)) * time.Second,
		OpenClawURL:          openclawURL,
		OpenClawAuthToken:    strings.TrimSpace(os.Getenv("OPENCLAW_AUTH_TOKEN")),
		OpenClawNodeID:       getEnvOrDefault("OPENCLAW_NODE_ID", "bbclaw-home-adapter"),
		OpenClawReplyWait:    time.Duration(getEnvInt("OPENCLAW_REPLY_WAIT_SECONDS", 25)) * time.Second,
		OpenClawIdentityPath: strings.TrimSpace(os.Getenv("OPENCLAW_DEVICE_IDENTITY_PATH")),
		HeartbeatInterval:    getEnvDuration("BBCLAW_HOMEADAPTER_HEARTBEAT_INTERVAL", 10*time.Second),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	// CloudWSURL has a built-in default (production SaaS); only validate the URL
	// shape here. resolveCloudDialURL below catches malformed input.
	if strings.TrimSpace(c.HomeSiteID) == "" {
		return errors.New("HOME_SITE_ID is empty (unexpected after EnsureHomeSiteID)")
	}
	if strings.EqualFold(strings.TrimSpace(c.HomeSiteID), "home-main") {
		return errors.New("HOME_SITE_ID must be your portal home-site UUID, not the legacy placeholder home-main")
	}
	if c.ReconnectDelay <= 0 {
		return errors.New("CLOUD_RECONNECT_DELAY_SECONDS must be > 0")
	}
	if c.HTTPTimeout <= 0 {
		return errors.New("HTTP_TIMEOUT_SECONDS must be > 0")
	}
	if strings.TrimSpace(c.OpenClawURL) == "" {
		return errors.New("OPENCLAW_WS_URL is required")
	}
	if c.OpenClawReplyWait <= 0 {
		return errors.New("OPENCLAW_REPLY_WAIT_SECONDS must be > 0")
	}
	if strings.TrimSpace(c.OpenClawNodeID) == "" {
		return errors.New("OPENCLAW_NODE_ID is required")
	}
	if _, err := resolveCloudDialURL(c.CloudWSURL, c.HomeSiteID, c.CloudAuthToken); err != nil {
		return err
	}
	if _, err := url.ParseRequestURI(c.OpenClawURL); err != nil {
		return fmt.Errorf("OPENCLAW_WS_URL is invalid: %w", err)
	}
	return nil
}

func resolveCloudDialURL(baseURL, homeSiteID, token string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("CLOUD_WS_URL is invalid: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("CLOUD_WS_URL scheme must be one of http/https/ws/wss, got: %s", parsed.Scheme)
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = "/ws"
	}
	query := parsed.Query()
	query.Set("role", "home_adapter")
	query.Set("home_site_id", homeSiteID)
	if strings.TrimSpace(token) != "" {
		query.Set("token", token)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
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

func getEnvDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return d
}
