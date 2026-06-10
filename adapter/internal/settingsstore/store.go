// Package settingsstore persists the user-mutable runtime configuration that
// used to live only in .env: ASR/TTS, the Anthropic proxy endpoint, the cloud
// relay, the OpenClaw gateway, and the deployment topology toggles. The file
// <DataDir>/settings.json is the single source of truth, so these can be edited
// at runtime through the local admin page (ADR-025) without hand-editing .env.
//
// It follows the same "env-seed → file-truth" recipe as projectstore and
// driverstate:
//
//   - Bootstrap(path, seed) writes the env-derived values into a fresh file the
//     first time the adapter runs. Once the file exists, the environment is a
//     spent bootstrap and settings.json wins.
//   - Open(path, base) loads the file on top of a base Settings (built from the
//     env defaults), so keys absent from an older file keep their env/default
//     value while present keys — even empty ones — override. This makes
//     "clearing a field on the page" persist as an empty override rather than
//     silently re-inheriting the env value.
//   - Apply copies the resolved settings onto a config.Config so the rest of the
//     adapter reads one overlaid view.
//
// Writes are atomic (tmp + rename) at 0600 because the file holds plaintext API
// keys/tokens. A missing or corrupt file degrades to the base rather than
// failing startup. The applied changes take effect on the next process start
// (ADR-025 §4: save + restart-to-apply); the admin layer offers a one-click
// re-exec.
package settingsstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/daboluocc/bbclaw/adapter/internal/config"
)

// fileFormatVersion is the JSON schema version persisted at the top level. Bump
// alongside any breaking shape change and add a migration path.
const fileFormatVersion = 1

// Settings is the on-disk schema (ADR-025 §2). Leaf string/bool fields carry no
// omitempty so the page can round-trip an explicit empty/false as a deliberate
// override rather than a missing key.
type Settings struct {
	Version  int              `json:"version"`
	Topology TopologySettings `json:"topology"`
	AI       AISettings       `json:"ai"`
	Voice    VoiceSettings    `json:"voice"`
	Cloud    CloudSettings    `json:"cloud"`
	OpenClaw OpenClawSettings `json:"openclaw"`
}

// TopologySettings selects which transports run. local_voice gates the LAN
// ASR/TTS pipeline; cloud_relay gates the upstream SaaS link. Both are
// independent of "is the local admin ingress up" (the admin page is always
// served when local ingress is enabled).
type TopologySettings struct {
	CloudRelayEnabled bool `json:"cloud_relay_enabled"`
	LocalVoiceEnabled bool `json:"local_voice_enabled"`
}

// AISettings holds the third-party Claude endpoint injected into claude
// subprocesses. Driver/model live in driverstate; projects live in
// projectstore — both are surfaced on the page but stored elsewhere.
type AISettings struct {
	AnthropicBaseURL   string `json:"anthropic_base_url"`
	AnthropicAuthToken string `json:"anthropic_auth_token"`
}

// ASRSettings mirrors the ASR_* env knobs.
type ASRSettings struct {
	Provider      string `json:"provider"`
	BaseURL       string `json:"base_url"`
	WSURL         string `json:"ws_url"`
	AppID         string `json:"app_id"`
	APIKey        string `json:"api_key"`
	ResourceID    string `json:"resource_id"`
	Model         string `json:"model"`
	Language      string `json:"language"`
	LocalBin      string `json:"local_bin"`
	LocalArgs     string `json:"local_args"`
	LocalTextPath string `json:"local_text_path"`
}

// TTSSettings mirrors the TTS_* env knobs.
type TTSSettings struct {
	Provider          string `json:"provider"`
	Token             string `json:"token"`
	AppID             string `json:"app_id"`
	Cluster           string `json:"cluster"`
	Voice             string `json:"voice"`
	WSURL             string `json:"ws_url"`
	LocalBin          string `json:"local_bin"`
	LocalArgs         string `json:"local_args"`
	LocalOutputFormat string `json:"local_output_format"`
}

// VoiceSettings is the LAN voice pipeline config — only validated/constructed
// when Topology.LocalVoiceEnabled is true.
type VoiceSettings struct {
	ASR               ASRSettings `json:"asr"`
	TTS               TTSSettings `json:"tts"`
	SaveAudio         bool        `json:"save_audio"`
	SaveInputOnFinish bool        `json:"save_input_on_finish"`
}

// CloudSettings is the upstream relay config — only used when
// Topology.CloudRelayEnabled is true.
type CloudSettings struct {
	WSURL      string `json:"ws_url"`
	AuthToken  string `json:"auth_token"`
	HomeSiteID string `json:"home_site_id"`
}

// OpenClawSettings is the OpenClaw gateway config (sink + driver).
type OpenClawSettings struct {
	WSURL     string `json:"ws_url"`
	AuthToken string `json:"auth_token"`
	NodeID    string `json:"node_id"`
}

// FromConfig builds a Settings snapshot from an env-derived config.Config. Used
// both as the Bootstrap seed and as the Open base, so the page starts from the
// operator's existing .env values on first run.
func FromConfig(cfg config.Config) Settings {
	return Settings{
		Version: fileFormatVersion,
		Topology: TopologySettings{
			CloudRelayEnabled: cfg.EnableCloudRelay(),
			LocalVoiceEnabled: cfg.LocalVoiceEnabled,
		},
		AI: AISettings{
			AnthropicBaseURL:   cfg.ClaudeBaseURL,
			AnthropicAuthToken: cfg.ClaudeAuthToken,
		},
		Voice: VoiceSettings{
			ASR: ASRSettings{
				Provider:      cfg.ASRProvider,
				BaseURL:       cfg.ASRBaseURL,
				WSURL:         cfg.ASRWSURL,
				AppID:         cfg.ASRAppID,
				APIKey:        cfg.ASRAPIKey,
				ResourceID:    cfg.ASRResourceID,
				Model:         cfg.ASRModel,
				Language:      cfg.ASRLanguage,
				LocalBin:      cfg.ASRLocalBin,
				LocalArgs:     strings.Join(cfg.ASRLocalArgs, " "),
				LocalTextPath: cfg.ASRLocalTextPath,
			},
			TTS: TTSSettings{
				Provider:          cfg.TTSProvider,
				Token:             cfg.TTSToken,
				AppID:             cfg.TTSAppID,
				Cluster:           cfg.TTSCluster,
				Voice:             cfg.TTSVoice,
				WSURL:             cfg.TTSWSURL,
				LocalBin:          cfg.TTSLocalBin,
				LocalArgs:         strings.Join(cfg.TTSLocalArgs, " "),
				LocalOutputFormat: cfg.TTSLocalOutputFormat,
			},
			SaveAudio:         cfg.SaveAudio,
			SaveInputOnFinish: cfg.SaveInputOnFinish,
		},
		Cloud: CloudSettings{
			WSURL:      cfg.CloudWSURL,
			AuthToken:  cfg.CloudAuthToken,
			HomeSiteID: cfg.HomeSiteID,
		},
		OpenClaw: OpenClawSettings{
			WSURL:     cfg.OpenClawURL,
			AuthToken: cfg.OpenClawAuthToken,
			NodeID:    cfg.OpenClawNodeID,
		},
	}
}

// ApplyTo overlays the resolved settings onto cfg. Pipeline-owned fields are
// copied wholesale (settings.json is authoritative post-bootstrap); the caller
// re-runs cfg.Validate() afterwards on the effective view. Topology booleans
// drive EnableCloudRelay / voice gating via the override fields on Config.
func (s Settings) ApplyTo(cfg *config.Config) {
	cloudRelay := s.Topology.CloudRelayEnabled
	cfg.CloudRelayOverride = &cloudRelay
	cfg.LocalVoiceEnabled = s.Topology.LocalVoiceEnabled

	cfg.ClaudeBaseURL = strings.TrimSpace(s.AI.AnthropicBaseURL)
	cfg.ClaudeAuthToken = strings.TrimSpace(s.AI.AnthropicAuthToken)

	cfg.ASRProvider = s.Voice.ASR.Provider
	cfg.ASRBaseURL = strings.TrimSpace(s.Voice.ASR.BaseURL)
	cfg.ASRWSURL = strings.TrimSpace(s.Voice.ASR.WSURL)
	cfg.ASRAppID = strings.TrimSpace(s.Voice.ASR.AppID)
	cfg.ASRAPIKey = strings.TrimSpace(s.Voice.ASR.APIKey)
	cfg.ASRResourceID = strings.TrimSpace(s.Voice.ASR.ResourceID)
	cfg.ASRModel = strings.TrimSpace(s.Voice.ASR.Model)
	cfg.ASRLanguage = strings.TrimSpace(s.Voice.ASR.Language)
	cfg.ASRLocalBin = strings.TrimSpace(s.Voice.ASR.LocalBin)
	cfg.ASRLocalArgs = strings.Fields(s.Voice.ASR.LocalArgs)
	cfg.ASRLocalTextPath = strings.TrimSpace(s.Voice.ASR.LocalTextPath)

	cfg.TTSProvider = s.Voice.TTS.Provider
	cfg.TTSToken = strings.TrimSpace(s.Voice.TTS.Token)
	cfg.TTSAppID = strings.TrimSpace(s.Voice.TTS.AppID)
	cfg.TTSCluster = strings.TrimSpace(s.Voice.TTS.Cluster)
	cfg.TTSVoice = strings.TrimSpace(s.Voice.TTS.Voice)
	cfg.TTSWSURL = strings.TrimSpace(s.Voice.TTS.WSURL)
	cfg.TTSLocalBin = strings.TrimSpace(s.Voice.TTS.LocalBin)
	cfg.TTSLocalArgs = strings.Fields(s.Voice.TTS.LocalArgs)
	cfg.TTSLocalOutputFormat = strings.TrimSpace(s.Voice.TTS.LocalOutputFormat)

	cfg.SaveAudio = s.Voice.SaveAudio
	cfg.SaveInputOnFinish = s.Voice.SaveInputOnFinish

	cfg.CloudWSURL = strings.TrimSpace(s.Cloud.WSURL)
	cfg.CloudAuthToken = strings.TrimSpace(s.Cloud.AuthToken)
	cfg.HomeSiteID = strings.TrimSpace(s.Cloud.HomeSiteID)

	cfg.OpenClawURL = strings.TrimSpace(s.OpenClaw.WSURL)
	cfg.OpenClawAuthToken = strings.TrimSpace(s.OpenClaw.AuthToken)
	cfg.OpenClawNodeID = strings.TrimSpace(s.OpenClaw.NodeID)
}

// Bootstrap status values, for the caller to log.
const (
	BootstrapNoop   = "noop"   // file already present → env ignored
	BootstrapSeeded = "seeded" // fresh file written from the env seed
)

// Bootstrap writes the env-derived seed into a fresh settings.json the first
// time the adapter runs. If the file already exists it is left untouched and
// the environment is ignored from then on. Safe and idempotent across processes
// (atomic write; an existing file is never rewritten).
func Bootstrap(path string, seed Settings) (string, error) {
	if _, err := os.Stat(path); err == nil {
		return BootstrapNoop, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("settingsstore: stat %s: %w", path, err)
	}
	seed.Version = fileFormatVersion
	if err := writeFile(path, seed); err != nil {
		return "", err
	}
	return BootstrapSeeded, nil
}

// Store is the file-backed settings cache. Safe for concurrent use.
type Store struct {
	path string
	mu   sync.RWMutex
	data Settings
}

// Open loads settings.json on top of base (built from env defaults via
// FromConfig). A missing file leaves base as-is; a corrupt file is surfaced via
// the returned error but the Store stays usable (holding base), so a bad file
// never blocks startup.
func Open(path string, base Settings) (*Store, error) {
	s := &Store{path: path, data: base}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("settingsstore: read %s: %w", path, err)
	}
	// Unmarshal over the base: keys present in the file (even empty) override;
	// keys absent keep the base value (forward-compatible across schema growth).
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return s, fmt.Errorf("settingsstore: parse %s: %w", path, err)
	}
	s.data.Version = fileFormatVersion
	return s, nil
}

// Snapshot returns a copy of the current settings.
func (s *Store) Snapshot() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

// Replace persists next as the full settings document (the page always PUTs the
// whole doc). The caller is expected to have validated the effective config
// first. Writes atomically and updates the in-memory snapshot.
func (s *Store) Replace(next Settings) error {
	next.Version = fileFormatVersion
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeFile(s.path, next); err != nil {
		return err
	}
	s.data = next
	return nil
}

// writeFile atomically serialises settings to path at 0600 (plaintext secrets).
func writeFile(path string, s Settings) error {
	s.Version = fileFormatVersion
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("settingsstore: marshal: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("settingsstore: mkdir %s: %w", dir, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("settingsstore: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("settingsstore: rename: %w", err)
	}
	return nil
}
