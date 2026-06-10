package settingsstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/config"
)

func baseSettings() Settings {
	return Settings{
		Version:  fileFormatVersion,
		Topology: TopologySettings{CloudRelayEnabled: true, LocalVoiceEnabled: false},
		AI:       AISettings{AnthropicBaseURL: "https://env.example", AnthropicAuthToken: "env-token"},
		Voice: VoiceSettings{
			ASR: ASRSettings{Provider: "local", Model: "whisper"},
			TTS: TTSSettings{Provider: "doubao_native", Cluster: "volcano_tts"},
		},
		Cloud:    CloudSettings{WSURL: "wss://env/ws"},
		OpenClaw: OpenClawSettings{WSURL: "ws://127.0.0.1:18789", NodeID: "bbclaw-adapter"},
	}
}

func TestBootstrapSeedsThenNoops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	status, err := Bootstrap(path, baseSettings())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if status != BootstrapSeeded {
		t.Fatalf("status = %q, want %q", status, BootstrapSeeded)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("seed file not written: %v", err)
	}

	// Second run with a different seed must NOT overwrite — env is spent.
	changed := baseSettings()
	changed.AI.AnthropicBaseURL = "https://SHOULD-NOT-WIN"
	status2, err := Bootstrap(path, changed)
	if err != nil {
		t.Fatalf("bootstrap2: %v", err)
	}
	if status2 != BootstrapNoop {
		t.Fatalf("status2 = %q, want %q", status2, BootstrapNoop)
	}
	s, err := Open(path, Settings{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := s.Snapshot().AI.AnthropicBaseURL; got != "https://env.example" {
		t.Fatalf("seed overwritten: got %q", got)
	}
}

func TestOpenMergesOverBaseAndClears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// A partial file: present keys (even empty) override; absent keys keep base.
	// Here local_voice is flipped on and the Anthropic token is explicitly cleared.
	partial := `{"topology":{"local_voice_enabled":true},"ai":{"anthropic_auth_token":""}}`
	if err := os.WriteFile(path, []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path, baseSettings())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got := s.Snapshot()
	if !got.Topology.LocalVoiceEnabled {
		t.Fatalf("present key not applied: local_voice_enabled")
	}
	if got.Topology.CloudRelayEnabled != true {
		t.Fatalf("absent key not preserved from base: cloud_relay_enabled")
	}
	if got.AI.AnthropicAuthToken != "" {
		t.Fatalf("explicit empty override not applied: token = %q", got.AI.AnthropicAuthToken)
	}
	if got.AI.AnthropicBaseURL != "https://env.example" {
		t.Fatalf("absent nested key not preserved: base_url = %q", got.AI.AnthropicBaseURL)
	}
	if got.Voice.ASR.Provider != "local" {
		t.Fatalf("absent voice block not preserved: asr.provider = %q", got.Voice.ASR.Provider)
	}
}

func TestReplaceRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s, err := Open(path, baseSettings())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	next := baseSettings()
	next.Topology.CloudRelayEnabled = false
	next.Voice.ASR.APIKey = "secret-key"
	if err := s.Replace(next); err != nil {
		t.Fatalf("replace: %v", err)
	}
	// Re-open from disk to prove persistence.
	reopened, err := Open(path, Settings{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.Snapshot()
	if got.Topology.CloudRelayEnabled {
		t.Fatalf("cloud_relay not persisted")
	}
	if got.Voice.ASR.APIKey != "secret-key" {
		t.Fatalf("api key not persisted: %q", got.Voice.ASR.APIKey)
	}

	// Secrets are persisted at 0600.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("settings perm = %o, want 600", perm)
	}
}

func TestApplyToOverlaysConfig(t *testing.T) {
	cfg := config.Config{
		AdapterMode:     "auto",
		ClaudeBaseURL:   "https://env-base",
		ClaudeAuthToken: "env-token",
		ASRProvider:     "doubao_native",
	}
	s := baseSettings()
	s.Topology.CloudRelayEnabled = false
	s.Topology.LocalVoiceEnabled = true
	s.AI.AnthropicBaseURL = "https://settings-base"
	s.AI.AnthropicAuthToken = "" // cleared on the page
	s.Voice.ASR.Provider = "openai_compatible"
	s.Voice.ASR.LocalArgs = "--lang zh --fast"

	s.ApplyTo(&cfg)

	if cfg.CloudRelayOverride == nil || *cfg.CloudRelayOverride != false {
		t.Fatalf("cloud relay override not applied")
	}
	if cfg.EnableCloudRelay() {
		t.Fatalf("EnableCloudRelay should honor the false override in auto mode")
	}
	if !cfg.LocalVoiceEnabled {
		t.Fatalf("local voice not applied")
	}
	if cfg.ClaudeBaseURL != "https://settings-base" {
		t.Fatalf("anthropic base not overlaid: %q", cfg.ClaudeBaseURL)
	}
	if cfg.ClaudeAuthToken != "" {
		t.Fatalf("anthropic token clear not overlaid: %q", cfg.ClaudeAuthToken)
	}
	if cfg.ASRProvider != "openai_compatible" {
		t.Fatalf("asr provider not overlaid: %q", cfg.ASRProvider)
	}
	if len(cfg.ASRLocalArgs) != 3 || cfg.ASRLocalArgs[0] != "--lang" {
		t.Fatalf("asr local args not split: %#v", cfg.ASRLocalArgs)
	}
}

func TestFromConfigRoundTrip(t *testing.T) {
	cfg := config.Config{
		AdapterMode:       "auto",
		LocalVoiceEnabled: true,
		ClaudeBaseURL:     "https://x",
		ASRProvider:       "local",
		ASRLocalArgs:      []string{"-m", "base"},
		TTSProvider:       "mock",
		OpenClawURL:       "ws://127.0.0.1:18789",
		OpenClawNodeID:    "node-1",
		CloudWSURL:        "wss://c/ws",
	}
	s := FromConfig(cfg)
	if s.Voice.ASR.LocalArgs != "-m base" {
		t.Fatalf("local args not joined: %q", s.Voice.ASR.LocalArgs)
	}
	if !s.Topology.LocalVoiceEnabled {
		t.Fatalf("topology local voice not seeded")
	}
	// Marshalable without error.
	if _, err := json.Marshal(s); err != nil {
		t.Fatalf("marshal: %v", err)
	}
}
