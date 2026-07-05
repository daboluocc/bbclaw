// Package settingsstore is adapter_v2's web-mutable runtime configuration store
// (ADR-025 "web-first config", env-overlay variant). It is fully self-contained:
// it imports nothing from the rest of adapter_v2 and nothing from v1.
//
// The design keeps voicekit/cloudrelay/butler/config UNCHANGED. They each read
// their settings from os.Getenv at boot (FromEnv / LoadFromEnv / LoadConfig).
// This store sits in front of them:
//
//  1. FromEnv() takes a snapshot of the SAME env vars the readers use — the seed.
//  2. Bootstrap() writes that seed to settings.json on first boot only.
//  3. Open() loads settings.json OVER the seed (file keys win, absent keep seed).
//  4. ExportEnv() pushes every value back into os.Setenv BEFORE the existing
//     readers run, so they transparently see the file-sourced configuration.
//
// The file is plaintext (it holds secrets such as API keys/tokens): it is the
// loopback-only admin surface that mutates it, mirroring v1's choice. The file
// is mode 0600 inside a 0700 dir.
package settingsstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// currentVersion stamps the schema so a future migration can detect old files.
const currentVersion = 1

// Settings is the full web-editable configuration document, grouped to mirror
// the admin page's sections. Every string field is serialized WITHOUT omitempty
// so an explicitly-cleared field round-trips as "" (and ExportEnv can decide,
// per leaf, whether an empty value should shadow the environment). Bools always
// serialize (Go's default), so a page-set false persists and overrides a code
// default of true.
type Settings struct {
	Version int            `json:"version"`
	Voice   VoiceSettings  `json:"voice"`
	Device  DeviceSettings `json:"device"`
	CLI     CLISettings    `json:"cli"`
	AI      AISettings     `json:"ai"`
	Cloud   CloudSettings  `json:"cloud"`
}

// AISettings holds the third-party / proxy Claude endpoint, injected into the
// spawned claude CLI through the standard ANTHROPIC_* env vars. v2 has no
// separate "driver" layer like v1 — it just runs the `claude` TUI under a PTY,
// and ptyhost.buildEnv seeds the child from os.Environ(), so exporting these two
// is all it takes for `claude` to talk to a compatible endpoint. Leave both
// blank to use claude's own login state (the official Anthropic endpoint). This
// restores v1's "配置第三方 claude" capability on the v2 admin page.
type AISettings struct {
	AnthropicBaseURL   string `json:"anthropicBaseUrl"`   // ANTHROPIC_BASE_URL
	AnthropicAuthToken string `json:"anthropicAuthToken"` // ANTHROPIC_AUTH_TOKEN
	Model              string `json:"model"`              // ANTHROPIC_MODEL (default model preference)
}

// VoiceSettings groups the ASR and TTS provider configuration. The env-var names
// mirror the ones voicekit.FromEnv reads (ASR_* / TTS_*).
type VoiceSettings struct {
	ASR ASRSettings `json:"asr"`
	TTS TTSSettings `json:"tts"`
}

// ASRSettings mirrors voicekit's ASR_* reads (see internal/voicekit/voicekit.go).
type ASRSettings struct {
	Provider      string `json:"provider"`      // ASR_PROVIDER
	Hotwords      string `json:"hotwords"`      // ASR_HOTWORDS
	BaseURL       string `json:"baseUrl"`       // ASR_BASE_URL
	APIKey        string `json:"apiKey"`        // ASR_API_KEY
	Model         string `json:"model"`         // ASR_MODEL
	AppID         string `json:"appId"`         // ASR_APP_ID
	ResourceID    string `json:"resourceId"`    // ASR_RESOURCE_ID
	Language      string `json:"language"`      // ASR_LANGUAGE
	LocalBin      string `json:"localBin"`      // ASR_LOCAL_BIN
	LocalArgs     string `json:"localArgs"`     // ASR_LOCAL_ARGS
	LocalTextPath string `json:"localTextPath"` // ASR_LOCAL_TEXT_PATH
}

// TTSSettings mirrors voicekit's TTS_* reads (see internal/voicekit/voicekit.go).
type TTSSettings struct {
	Provider          string `json:"provider"`          // TTS_PROVIDER
	BaseURL           string `json:"baseUrl"`           // TTS_BASE_URL
	AppID             string `json:"appId"`             // TTS_APP_ID
	Token             string `json:"token"`             // TTS_TOKEN
	Cluster           string `json:"cluster"`           // TTS_CLUSTER
	Voice             string `json:"voice"`             // TTS_VOICE
	LocalBin          string `json:"localBin"`          // TTS_LOCAL_BIN
	LocalArgs         string `json:"localArgs"`         // TTS_LOCAL_ARGS
	LocalOutputFormat string `json:"localOutputFormat"` // TTS_LOCAL_OUTPUT_FORMAT
}

// DeviceSettings groups the bbwire/2 device-line streaming knobs (main.go).
type DeviceSettings struct {
	StreamDelta bool `json:"streamDelta"` // ADAPTER_V2_STREAM_DELTA (default true)
	SegmentTTS  bool `json:"segmentTts"`  // ADAPTER_V2_SEGMENT_TTS (default false)
}

// CLISettings groups the PTY/CLI and butler knobs (config.go + butler).
type CLISettings struct {
	Cli               string `json:"cli"`               // ADAPTER_V2_CLI (space-split argv; default "claude")
	Cwd               string `json:"cwd"`               // ADAPTER_V2_CWD
	SkipPermissions   bool   `json:"skipPermissions"`   // ADAPTER_V2_SKIP_PERMISSIONS (default true)
	VoiceSystemPrompt string `json:"voiceSystemPrompt"` // ADAPTER_V2_VOICE_SYSTEM_PROMPT
	Addr              string `json:"addr"`              // ADAPTER_V2_ADDR (default ":18090")
	// ClaudeAutoEnter auto-sends a few Enters after a claude session spawns to
	// dismiss its first-run "Try the new fullscreen renderer?" upsell, which would
	// otherwise block the shared TUI (the voice/device path can't answer it).
	ClaudeAutoEnter bool `json:"claudeAutoEnter"` // ADAPTER_V2_CLAUDE_AUTO_ENTER (default true)

	// ConfirmOnDevice turns OFF the permission bypass and instead forwards claude's
	// tool/permission menus to the device for confirmation (ADR-033). When set the
	// butler spawns claude WITHOUT --dangerously-skip-permissions and WITH
	// --permission-mode default (else the persisted auto-accept mode swallows the
	// prompts — ADR-033 spike), and the device Bridge forwards them (auto-DENY on
	// timeout / no-device). Default false = today's bypass behaviour, untouched.
	ConfirmOnDevice bool `json:"confirmOnDevice"` // ADAPTER_V2_CONFIRM_ON_DEVICE (default false)
}

// CloudSettings groups the cloud-relay knobs (cloudrelay.go).
type CloudSettings struct {
	WsURL      string `json:"wsUrl"`      // CLOUD_WS_URL
	AuthToken  string `json:"authToken"`  // CLOUD_AUTH_TOKEN
	HomeSiteID string `json:"homeSiteId"` // HOME_SITE_ID (blank ⇒ persisted identity.json wins)
}

// envBool parses a boolean env var with the same 1/true/yes/on truthiness the
// rest of adapter_v2 uses (config/main/butler all share this), returning def
// when unset or unrecognised.
func envBool(name string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// envOr returns the trimmed env var value, or def when unset/blank.
func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// ConfirmOnDeviceEnabled reports whether forward-to-device permission
// confirmation is on (ADR-033), read from the ExportEnv-populated environment.
// The device transports consult it to turn on Bridge prompt forwarding without
// threading the whole settings doc through their constructors — the same
// "readers read os.Getenv" pattern the rest of adapter_v2 uses.
func ConfirmOnDeviceEnabled() bool { return envBool("ADAPTER_V2_CONFIRM_ON_DEVICE", false) }

// FromEnv reads the SAME env vars the rest of adapter_v2 reads, producing the
// seed/base snapshot. Defaults match the code defaults exactly so a zero-config
// boot persists what the readers would have computed anyway.
func FromEnv() Settings {
	return Settings{
		Version: currentVersion,
		Voice: VoiceSettings{
			ASR: ASRSettings{
				Provider:      strings.TrimSpace(os.Getenv("ASR_PROVIDER")),
				Hotwords:      strings.TrimSpace(os.Getenv("ASR_HOTWORDS")),
				BaseURL:       strings.TrimSpace(os.Getenv("ASR_BASE_URL")),
				APIKey:        strings.TrimSpace(os.Getenv("ASR_API_KEY")),
				Model:         strings.TrimSpace(os.Getenv("ASR_MODEL")),
				AppID:         strings.TrimSpace(os.Getenv("ASR_APP_ID")),
				ResourceID:    strings.TrimSpace(os.Getenv("ASR_RESOURCE_ID")),
				Language:      envOr("ASR_LANGUAGE", "zh-CN"),
				LocalBin:      strings.TrimSpace(os.Getenv("ASR_LOCAL_BIN")),
				LocalArgs:     strings.TrimSpace(os.Getenv("ASR_LOCAL_ARGS")),
				LocalTextPath: strings.TrimSpace(os.Getenv("ASR_LOCAL_TEXT_PATH")),
			},
			TTS: TTSSettings{
				Provider:          strings.TrimSpace(os.Getenv("TTS_PROVIDER")),
				BaseURL:           strings.TrimSpace(os.Getenv("TTS_BASE_URL")),
				AppID:             strings.TrimSpace(os.Getenv("TTS_APP_ID")),
				Token:             strings.TrimSpace(os.Getenv("TTS_TOKEN")),
				Cluster:           strings.TrimSpace(os.Getenv("TTS_CLUSTER")),
				Voice:             strings.TrimSpace(os.Getenv("TTS_VOICE")),
				LocalBin:          strings.TrimSpace(os.Getenv("TTS_LOCAL_BIN")),
				LocalArgs:         strings.TrimSpace(os.Getenv("TTS_LOCAL_ARGS")),
				LocalOutputFormat: envOr("TTS_LOCAL_OUTPUT_FORMAT", "wav"),
			},
		},
		Device: DeviceSettings{
			StreamDelta: envBool("ADAPTER_V2_STREAM_DELTA", true),
			SegmentTTS:  envBool("ADAPTER_V2_SEGMENT_TTS", false),
		},
		CLI: CLISettings{
			Cli:               envOr("ADAPTER_V2_CLI", "claude"),
			Cwd:               strings.TrimSpace(os.Getenv("ADAPTER_V2_CWD")),
			SkipPermissions:   envBool("ADAPTER_V2_SKIP_PERMISSIONS", true),
			VoiceSystemPrompt: os.Getenv("ADAPTER_V2_VOICE_SYSTEM_PROMPT"), // may be set-empty on purpose
			Addr:              envOr("ADAPTER_V2_ADDR", ":18090"),
			ClaudeAutoEnter:   envBool("ADAPTER_V2_CLAUDE_AUTO_ENTER", true),
			ConfirmOnDevice:   envBool("ADAPTER_V2_CONFIRM_ON_DEVICE", false),
		},
		AI: AISettings{
			AnthropicBaseURL:   strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
			AnthropicAuthToken: strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")),
			Model:              strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL")),
		},
		Cloud: CloudSettings{
			WsURL:      strings.TrimSpace(os.Getenv("CLOUD_WS_URL")),
			AuthToken:  strings.TrimSpace(os.Getenv("CLOUD_AUTH_TOKEN")),
			HomeSiteID: strings.TrimSpace(os.Getenv("HOME_SITE_ID")),
		},
	}
}

// DataDir returns the directory holding settings.json: ~/.bbclaw-adapter-v2
// (the same v2-specific dir cloudrelay uses for identity.json), overridable with
// BBCLAW_ADAPTER_V2_DATA_DIR for tests/relocation.
func DataDir() string {
	if d := strings.TrimSpace(os.Getenv("BBCLAW_ADAPTER_V2_DATA_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Degrade to the current dir rather than failing startup; settings persist
		// somewhere writable and the rest of the boot continues on env defaults.
		return ".bbclaw-adapter-v2"
	}
	return filepath.Join(home, ".bbclaw-adapter-v2")
}

// writeAtomic writes data to path via a temp file + rename so a crash mid-write
// never leaves a half-written (corrupt) settings.json. The dir is created 0700
// and the file 0600 because it holds plaintext secrets.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create settings dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp settings: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if anything below fails before the rename.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp settings: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp settings: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp settings: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename settings: %w", err)
	}
	return nil
}

func marshal(s Settings) ([]byte, error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Bootstrap writes seed to path on first boot only. If the file already exists it
// is a no-op (the persisted file is the source of truth from then on). Idempotent.
func Bootstrap(path string, seed Settings) error {
	if _, err := os.Stat(path); err == nil {
		return nil // already initialised; leave the operator's file untouched
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat settings: %w", err)
	}
	if seed.Version == 0 {
		seed.Version = currentVersion
	}
	data, err := marshal(seed)
	if err != nil {
		return fmt.Errorf("marshal seed: %w", err)
	}
	return writeAtomic(path, data)
}

// Store holds the live settings document behind an RWMutex.
type Store struct {
	mu   sync.RWMutex
	path string
	s    Settings
}

// Open loads the settings file at path OVER base: keys present in the file
// override base, absent keys keep base. A missing file yields a Store holding
// base. A CORRUPT file returns the parse error BUT also a usable Store holding
// base — settings must never block startup.
func Open(path string, base Settings) (*Store, error) {
	st := &Store{path: path, s: base}
	if st.s.Version == 0 {
		st.s.Version = currentVersion
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil // first boot before Bootstrap, or no file: base is fine
		}
		return st, fmt.Errorf("read settings: %w", err) // usable Store + error
	}
	// Unmarshal OVER base: json.Unmarshal only sets the keys present in the file,
	// so absent keys keep their base value and present keys (incl. explicit "") win.
	merged := base
	if err := json.Unmarshal(raw, &merged); err != nil {
		// Corrupt file: keep base (already in st.s), surface the error so the caller
		// can log it, but return a usable Store so the boot continues.
		return st, fmt.Errorf("parse settings: %w", err)
	}
	if merged.Version == 0 {
		merged.Version = currentVersion
	}
	st.s = merged
	return st, nil
}

// Path returns the settings file path this store reads/writes.
func (s *Store) Path() string { return s.path }

// Snapshot returns a copy of the current settings under a read lock. Settings is
// a value type with no reference fields, so the returned copy is independent.
func (s *Store) Snapshot() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.s
}

// Replace atomically persists next (temp + rename) and updates the in-memory
// copy under a write lock. The file write happens while holding the lock so a
// concurrent Snapshot never observes a state that did not reach disk.
func (s *Store) Replace(next Settings) error {
	if next.Version == 0 {
		next.Version = currentVersion
	}
	data, err := marshal(next)
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeAtomic(s.path, data); err != nil {
		return err
	}
	s.s = next
	return nil
}

// ExportEnv pushes the current settings into the process environment so the
// existing FromEnv/LoadFromEnv/LoadConfig readers transparently see file-sourced
// values. It MUST run before those readers.
//
// Rules (load-bearing):
//   - Every bool leaf is ALWAYS exported as "1"/"0" — a page-set false must
//     override a code default of true, so we can never skip a bool.
//   - A NON-EMPTY string leaf is exported as-is.
//   - An EMPTY string leaf is SKIPPED (os.Setenv is NOT called): a blank
//     HOME_SITE_ID must not shadow the persisted identity.json UUID, and a blank
//     field must not clobber a value the operator exported in the shell.
//   - CLI.Cli is exported verbatim to ADAPTER_V2_CLI (main splits it on spaces).
func (s *Store) ExportEnv() {
	snap := s.Snapshot()

	setStr := func(name, val string) {
		if strings.TrimSpace(val) == "" {
			return // skip empty: don't shadow identity.json / shell-exported values
		}
		_ = os.Setenv(name, val)
	}
	setBool := func(name string, val bool) {
		if val {
			_ = os.Setenv(name, "1")
		} else {
			_ = os.Setenv(name, "0")
		}
	}

	a := snap.Voice.ASR
	setStr("ASR_PROVIDER", a.Provider)
	setStr("ASR_HOTWORDS", a.Hotwords)
	setStr("ASR_BASE_URL", a.BaseURL)
	setStr("ASR_API_KEY", a.APIKey)
	setStr("ASR_MODEL", a.Model)
	setStr("ASR_APP_ID", a.AppID)
	setStr("ASR_RESOURCE_ID", a.ResourceID)
	setStr("ASR_LANGUAGE", a.Language)
	setStr("ASR_LOCAL_BIN", a.LocalBin)
	setStr("ASR_LOCAL_ARGS", a.LocalArgs)
	setStr("ASR_LOCAL_TEXT_PATH", a.LocalTextPath)

	t := snap.Voice.TTS
	setStr("TTS_PROVIDER", t.Provider)
	setStr("TTS_BASE_URL", t.BaseURL)
	setStr("TTS_APP_ID", t.AppID)
	setStr("TTS_TOKEN", t.Token)
	setStr("TTS_CLUSTER", t.Cluster)
	setStr("TTS_VOICE", t.Voice)
	setStr("TTS_LOCAL_BIN", t.LocalBin)
	setStr("TTS_LOCAL_ARGS", t.LocalArgs)
	setStr("TTS_LOCAL_OUTPUT_FORMAT", t.LocalOutputFormat)

	setBool("ADAPTER_V2_STREAM_DELTA", snap.Device.StreamDelta)
	setBool("ADAPTER_V2_SEGMENT_TTS", snap.Device.SegmentTTS)

	c := snap.CLI
	setStr("ADAPTER_V2_CLI", c.Cli) // verbatim; main splits on spaces
	setStr("ADAPTER_V2_CWD", c.Cwd)
	setBool("ADAPTER_V2_SKIP_PERMISSIONS", c.SkipPermissions)
	setStr("ADAPTER_V2_VOICE_SYSTEM_PROMPT", c.VoiceSystemPrompt)
	setStr("ADAPTER_V2_ADDR", c.Addr)
	setBool("ADAPTER_V2_CLAUDE_AUTO_ENTER", c.ClaudeAutoEnter)
	setBool("ADAPTER_V2_CONFIRM_ON_DEVICE", c.ConfirmOnDevice)

	ai := snap.AI
	// Blank ⇒ skipped, so an unconfigured endpoint never shadows a token the
	// operator exported in the shell, and `claude` falls back to its own login.
	setStr("ANTHROPIC_BASE_URL", ai.AnthropicBaseURL)
	setStr("ANTHROPIC_AUTH_TOKEN", ai.AnthropicAuthToken)
	setStr("ANTHROPIC_MODEL", ai.Model)

	cl := snap.Cloud
	setStr("CLOUD_WS_URL", cl.WsURL)
	setStr("CLOUD_AUTH_TOKEN", cl.AuthToken)
	setStr("HOME_SITE_ID", cl.HomeSiteID) // blank skipped ⇒ identity.json UUID wins
}
