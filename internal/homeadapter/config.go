package homeadapter

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
}

func readHomeSiteIDFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".bbclaw", "home_site_id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func LoadFromEnv() (Config, error) {
	openclawURL := strings.TrimSpace(os.Getenv("OPENCLAW_WS_URL"))
	if openclawURL == "" {
		openclawURL = "ws://127.0.0.1:18789"
	}

	homeSiteID := strings.TrimSpace(os.Getenv("HOME_SITE_ID"))
	if homeSiteID == "" {
		homeSiteID = readHomeSiteIDFile()
	}

	cfg := Config{
		CloudWSURL:           strings.TrimSpace(os.Getenv("CLOUD_WS_URL")),
		CloudAuthToken:       strings.TrimSpace(os.Getenv("CLOUD_AUTH_TOKEN")),
		HomeSiteID:           homeSiteID,
		ReconnectDelay:       time.Duration(getEnvInt("CLOUD_RECONNECT_DELAY_SECONDS", 3)) * time.Second,
		HTTPTimeout:          time.Duration(getEnvInt("HTTP_TIMEOUT_SECONDS", 30)) * time.Second,
		OpenClawURL:          openclawURL,
		OpenClawAuthToken:    strings.TrimSpace(os.Getenv("OPENCLAW_AUTH_TOKEN")),
		OpenClawNodeID:       getEnvOrDefault("OPENCLAW_NODE_ID", "bbclaw-home-adapter"),
		OpenClawReplyWait:    time.Duration(getEnvInt("OPENCLAW_REPLY_WAIT_SECONDS", 25)) * time.Second,
		OpenClawIdentityPath: strings.TrimSpace(os.Getenv("OPENCLAW_DEVICE_IDENTITY_PATH")),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.CloudWSURL) == "" {
		return errors.New("CLOUD_WS_URL is required")
	}
	if strings.TrimSpace(c.HomeSiteID) == "" {
		return errors.New("set HOME_SITE_ID or write the home-site UUID to ~/.bbclaw/home_site_id")
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
