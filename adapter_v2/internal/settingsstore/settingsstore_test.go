package settingsstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// settingsPath returns a settings.json path inside a fresh temp HOME, with the
// data-dir override cleared so DataDir() resolves under that HOME.
func settingsPath(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BBCLAW_ADAPTER_V2_DATA_DIR", "")
	return filepath.Join(DataDir(), "settings.json")
}

// TestBootstrapWritesOnce verifies Bootstrap creates the file on first boot and
// is a no-op (does not overwrite) if the file already exists.
func TestBootstrapWritesOnce(t *testing.T) {
	path := settingsPath(t)

	seed := Settings{}
	seed.Voice.ASR.Provider = "doubao_native"
	if err := Bootstrap(path, seed); err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("settings file missing after Bootstrap: %v", err)
	}
	// File must be 0600 (plaintext secrets).
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("settings perm = %o, want 600", perm)
	}

	// A second Bootstrap with a DIFFERENT seed must NOT overwrite the file.
	other := Settings{}
	other.Voice.ASR.Provider = "openai_compatible"
	if err := Bootstrap(path, other); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var got Settings
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal persisted: %v", err)
	}
	if got.Voice.ASR.Provider != "doubao_native" {
		t.Fatalf("Bootstrap overwrote existing file: provider=%q, want doubao_native", got.Voice.ASR.Provider)
	}
	if got.Version != currentVersion {
		t.Fatalf("seed version = %d, want %d", got.Version, currentVersion)
	}
}

// TestOpenOverlaysFileOverBase verifies the file overlays base: present keys
// (including an explicit empty) win, absent keys keep base.
func TestOpenOverlaysFileOverBase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	base := Settings{Version: currentVersion}
	base.Voice.ASR.Provider = "BASE_PROVIDER"
	base.Voice.ASR.Language = "BASE_LANG" // absent from file ⇒ kept
	base.Voice.TTS.Voice = "BASE_VOICE"
	base.Device.StreamDelta = true
	base.CLI.Cli = "claude"

	// File sets provider to a new value, clears the TTS voice explicitly (""),
	// flips StreamDelta to false, and omits language entirely.
	fileDoc := `{
	  "voice": {
	    "asr": {"provider": "FILE_PROVIDER"},
	    "tts": {"voice": ""}
	  },
	  "device": {"streamDelta": false}
	}`
	if err := os.WriteFile(path, []byte(fileDoc), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	st, err := Open(path, base)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got := st.Snapshot()

	if got.Voice.ASR.Provider != "FILE_PROVIDER" {
		t.Errorf("present key not applied: provider=%q, want FILE_PROVIDER", got.Voice.ASR.Provider)
	}
	if got.Voice.ASR.Language != "BASE_LANG" {
		t.Errorf("absent key not kept from base: language=%q, want BASE_LANG", got.Voice.ASR.Language)
	}
	if got.Voice.TTS.Voice != "" {
		t.Errorf("present-empty key did not override base: voice=%q, want empty", got.Voice.TTS.Voice)
	}
	if got.Device.StreamDelta != false {
		t.Errorf("present false bool did not override base true: streamDelta=%v, want false", got.Device.StreamDelta)
	}
	if got.CLI.Cli != "claude" {
		t.Errorf("absent CLI key not kept: cli=%q, want claude", got.CLI.Cli)
	}
}

// TestOpenMissingFileKeepsBase verifies a missing file yields a usable Store
// holding base with no error.
func TestOpenMissingFileKeepsBase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	base := Settings{Version: currentVersion}
	base.Cloud.WsURL = "wss://example/ws"
	st, err := Open(path, base)
	if err != nil {
		t.Fatalf("Open missing file should not error: %v", err)
	}
	if got := st.Snapshot(); got.Cloud.WsURL != "wss://example/ws" {
		t.Fatalf("base not retained: wsUrl=%q", got.Cloud.WsURL)
	}
}

// TestOpenCorruptFileReturnsErrorButUsableStore verifies a corrupt file returns
// the parse error AND a usable Store holding base (never blocks startup).
func TestOpenCorruptFileReturnsErrorButUsableStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	base := Settings{Version: currentVersion}
	base.CLI.Addr = ":18090"
	st, err := Open(path, base)
	if err == nil {
		t.Fatalf("Open corrupt file should return an error")
	}
	if st == nil {
		t.Fatalf("Open corrupt file must still return a usable Store")
	}
	if got := st.Snapshot(); got.CLI.Addr != ":18090" {
		t.Fatalf("corrupt-file Store should hold base: addr=%q", got.CLI.Addr)
	}
}

// TestExportEnv verifies non-empty strings + both bools are exported, empty
// strings are skipped (so a blank HOME_SITE_ID stays unset).
func TestExportEnv(t *testing.T) {
	// Pre-set the env vars we expect to be either overwritten or left alone.
	t.Setenv("ASR_PROVIDER", "stale")
	t.Setenv("HOME_SITE_ID", "")
	if err := os.Unsetenv("HOME_SITE_ID"); err != nil {
		t.Fatalf("unset HOME_SITE_ID: %v", err)
	}
	t.Setenv("ADAPTER_V2_STREAM_DELTA", "1")
	t.Setenv("ADAPTER_V2_SKIP_PERMISSIONS", "1")
	// A dev shell may export this (they run claude); force-unset so the
	// "blank token stays unset" assertion is deterministic. t.Setenv restores it.
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	if err := os.Unsetenv("ANTHROPIC_AUTH_TOKEN"); err != nil {
		t.Fatalf("unset ANTHROPIC_AUTH_TOKEN: %v", err)
	}

	s := Settings{Version: currentVersion}
	s.Voice.ASR.Provider = "doubao_native"
	s.Voice.ASR.APIKey = "sekret"
	s.Voice.ASR.Language = "" // empty ⇒ must NOT be exported
	s.Device.StreamDelta = false
	s.Device.SegmentTTS = true
	s.CLI.SkipPermissions = false
	s.AI.AnthropicBaseURL = "https://proxy.example.com"
	s.AI.AnthropicAuthToken = "" // empty ⇒ must NOT be exported
	s.Cloud.HomeSiteID = ""      // empty ⇒ must NOT shadow identity.json

	st := &Store{path: filepath.Join(t.TempDir(), "s.json"), s: s}
	st.ExportEnv()

	if got := os.Getenv("ASR_PROVIDER"); got != "doubao_native" {
		t.Errorf("ASR_PROVIDER = %q, want doubao_native", got)
	}
	if got := os.Getenv("ASR_API_KEY"); got != "sekret" {
		t.Errorf("ASR_API_KEY = %q, want sekret", got)
	}
	if _, ok := os.LookupEnv("ASR_LANGUAGE"); ok {
		t.Errorf("empty ASR_LANGUAGE should not be exported, but it is set")
	}
	if _, ok := os.LookupEnv("HOME_SITE_ID"); ok {
		t.Errorf("empty HOME_SITE_ID must stay unset (identity.json wins), but it is set")
	}
	// Both bools always exported, including page-set false overriding env "1".
	if got := os.Getenv("ADAPTER_V2_STREAM_DELTA"); got != "0" {
		t.Errorf("ADAPTER_V2_STREAM_DELTA = %q, want 0 (false must override env)", got)
	}
	if got := os.Getenv("ADAPTER_V2_SEGMENT_TTS"); got != "1" {
		t.Errorf("ADAPTER_V2_SEGMENT_TTS = %q, want 1", got)
	}
	if got := os.Getenv("ADAPTER_V2_SKIP_PERMISSIONS"); got != "0" {
		t.Errorf("ADAPTER_V2_SKIP_PERMISSIONS = %q, want 0 (false must override env)", got)
	}
	// Third-party Claude endpoint: base URL exported, blank token left unset so
	// `claude` falls back to its own login rather than an empty auth token.
	if got := os.Getenv("ANTHROPIC_BASE_URL"); got != "https://proxy.example.com" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want https://proxy.example.com", got)
	}
	if _, ok := os.LookupEnv("ANTHROPIC_AUTH_TOKEN"); ok {
		t.Errorf("empty ANTHROPIC_AUTH_TOKEN should not be exported, but it is set")
	}
}

// TestReplaceRoundTrips verifies Replace persists to disk and updates memory,
// and that a fresh Open reads back the replaced document.
func TestReplaceRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	base := Settings{Version: currentVersion}
	st, err := Open(path, base)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	next := base
	next.Voice.TTS.Provider = "doubao_native"
	next.Voice.TTS.Token = "tok"
	next.Cloud.WsURL = "wss://cloud/ws"
	next.Device.SegmentTTS = true
	if err := st.Replace(next); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	// In-memory updated.
	if got := st.Snapshot(); got.Voice.TTS.Token != "tok" || !got.Device.SegmentTTS {
		t.Fatalf("in-memory not updated after Replace: %+v", got.Voice.TTS)
	}
	// Persisted: a fresh Open over empty base reads the replaced doc.
	st2, err := Open(path, Settings{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := st2.Snapshot()
	if got.Voice.TTS.Provider != "doubao_native" || got.Cloud.WsURL != "wss://cloud/ws" {
		t.Fatalf("Replace did not round-trip on disk: %+v", got)
	}
	// File still 0600 after the atomic rewrite.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after replace: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("settings perm after Replace = %o, want 600", perm)
	}
}

// TestFromEnvDefaults verifies the documented defaults are applied when the env
// is empty, and the bool truthiness matches the rest of v2.
func TestFromEnvDefaults(t *testing.T) {
	for _, k := range []string{
		"ASR_PROVIDER", "ASR_LANGUAGE", "TTS_LOCAL_OUTPUT_FORMAT",
		"ADAPTER_V2_CLI", "ADAPTER_V2_ADDR", "ADAPTER_V2_STREAM_DELTA",
		"ADAPTER_V2_SEGMENT_TTS", "ADAPTER_V2_SKIP_PERMISSIONS", "HOME_SITE_ID",
	} {
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
	}
	s := FromEnv()
	if s.Voice.ASR.Language != "zh-CN" {
		t.Errorf("ASR language default = %q, want zh-CN", s.Voice.ASR.Language)
	}
	if s.Voice.TTS.LocalOutputFormat != "wav" {
		t.Errorf("TTS local output format default = %q, want wav", s.Voice.TTS.LocalOutputFormat)
	}
	if s.CLI.Cli != "claude" {
		t.Errorf("CLI default = %q, want claude", s.CLI.Cli)
	}
	if s.CLI.Addr != ":18090" {
		t.Errorf("Addr default = %q, want :18090", s.CLI.Addr)
	}
	if !s.Device.StreamDelta {
		t.Errorf("StreamDelta default = false, want true")
	}
	if s.Device.SegmentTTS {
		t.Errorf("SegmentTTS default = true, want false")
	}
	if !s.CLI.SkipPermissions {
		t.Errorf("SkipPermissions default = false, want true")
	}
}
