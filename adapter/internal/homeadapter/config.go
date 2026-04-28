package homeadapter

import (
	"errors"
	"fmt"
	"net/url"
	"os"
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
}

// homeSiteFingerprint collects stable per-machine inputs so the derived ID is the same on every
// process start without persisting state. Linux machine-id disambiguates hosts that share hostname.
func homeSiteFingerprint() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("hostname: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	user := strings.TrimSpace(os.Getenv("USER"))
	if user == "" {
		user = strings.TrimSpace(os.Getenv("USERNAME"))
	}
	var machineID string
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		machineID = strings.TrimSpace(string(raw))
		break
	}
	// NUL-separated single string for deterministic UUID v5 name bytes.
	return fmt.Sprintf("bbclaw-home-site/v1\000%s\000%s\000%s\000%s", hostname, home, user, machineID), nil
}

// derivedHomeSiteID returns a UUID v5 from homeSiteFingerprint (same machine → same ID every run).
func derivedHomeSiteID() (string, error) {
	fp, err := homeSiteFingerprint()
	if err != nil {
		return "", err
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fp)).String(), nil
}

// EnsureHomeSiteID returns HOME_SITE_ID if set; otherwise a deterministic UUID derived from this host.
func EnsureHomeSiteID() (string, error) {
	if id := strings.TrimSpace(os.Getenv("HOME_SITE_ID")); id != "" {
		return id, nil
	}
	return derivedHomeSiteID()
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
