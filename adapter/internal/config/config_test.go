package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadFromEnvDefaultsAndRequireds(t *testing.T) {
	t.Setenv("ASR_LOCAL_BIN", "/bin/echo")
	t.Setenv("OPENCLAW_RPC_URL", "https://gateway.example.com/rpc")
	t.Setenv("TTS_WS_URL", "wss://openspeech.bytedance.com/api/v1/tts/ws_binary")
	t.Setenv("TTS_APP_ID", "appid")
	t.Setenv("TTS_TOKEN", "token")
	t.Setenv("TTS_CLUSTER", "volcano_tts")
	t.Setenv("TTS_VOICE", "zh-CN-XiaoxiaoNeural")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.Addr != ":18080" {
		t.Fatalf("Addr = %q", cfg.Addr)
	}
	if cfg.MaxStreamSeconds != 90 {
		t.Fatalf("MaxStreamSeconds = %d", cfg.MaxStreamSeconds)
	}
	if cfg.ASRProvider != "local" {
		t.Fatalf("ASRProvider = %q", cfg.ASRProvider)
	}
	if cfg.AdapterMode != "auto" {
		t.Fatalf("AdapterMode = %q", cfg.AdapterMode)
	}
	if !cfg.EnableLocalIngress() {
		t.Fatal("expected local ingress enabled by default")
	}
	// CLOUD_WS_URL has a built-in production default now, so cloud relay is on
	// out-of-the-box. The unauthenticated home_adapter goes through the cloud's
	// claim_required pairing flow before any traffic flows.
	if !cfg.EnableCloudRelay() {
		t.Fatal("expected cloud relay enabled by default (CLOUD_WS_URL has a baked-in default)")
	}
	if cfg.CloudWSURL == "" {
		t.Fatal("expected CloudWSURL default to be populated, got empty")
	}
}

// TestLoadFromEnvEmptyIsCloudDefault: under ADR-025 a bare env is a valid
// zero-config cloud-default deployment — local ingress (admin page) is up, the
// cloud relay is on (CLOUD_WS_URL has a baked-in default), and the LAN voice
// pipeline is off so no ASR/TTS config is required. (Previously this errored
// because voice validation was unconditional.)
func TestLoadFromEnvEmptyIsCloudDefault(t *testing.T) {
	os.Clearenv()
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("empty env should load as cloud-default, got error: %v", err)
	}
	if cfg.LocalVoiceEnabled {
		t.Fatal("expected local voice disabled with no ASR/TTS config")
	}
	if !cfg.EnableLocalIngress() {
		t.Fatal("expected local ingress (admin page) enabled by default")
	}
	if !cfg.EnableCloudRelay() {
		t.Fatal("expected cloud relay enabled by default (CLOUD_WS_URL has a baked-in default)")
	}
}

// TestLoadFromEnvLocalVoiceAutoOn: a complete env voice config auto-enables the
// LAN pipeline (no BBCLAW_LOCAL_VOICE needed), preserving existing local_home setups.
func TestLoadFromEnvLocalVoiceAutoOn(t *testing.T) {
	t.Setenv("ASR_LOCAL_BIN", "/bin/echo")
	t.Setenv("OPENCLAW_RPC_URL", "https://gateway.example.com/rpc")
	t.Setenv("TTS_WS_URL", "wss://openspeech.bytedance.com/api/v1/tts/ws_binary")
	t.Setenv("TTS_APP_ID", "appid")
	t.Setenv("TTS_TOKEN", "token")
	t.Setenv("TTS_CLUSTER", "volcano_tts")
	t.Setenv("TTS_VOICE", "zh-CN-XiaoxiaoNeural")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if !cfg.LocalVoiceEnabled {
		t.Fatal("expected local voice auto-enabled when env voice config is complete")
	}
}

func TestLoadFromEnvInvalidNumericFallback(t *testing.T) {
	t.Setenv("ASR_LOCAL_BIN", "/bin/echo")
	t.Setenv("OPENCLAW_RPC_URL", "https://gateway.example.com/rpc")
	t.Setenv("MAX_STREAM_SECONDS", "x")
	t.Setenv("TTS_WS_URL", "wss://openspeech.bytedance.com/api/v1/tts/ws_binary")
	t.Setenv("TTS_APP_ID", "appid")
	t.Setenv("TTS_TOKEN", "token")
	t.Setenv("TTS_CLUSTER", "volcano_tts")
	t.Setenv("TTS_VOICE", "zh-CN-XiaoxiaoNeural")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.MaxStreamSeconds != 90 {
		t.Fatalf("expected fallback, got %d", cfg.MaxStreamSeconds)
	}
}

func TestLoadFromEnvOpenAICompatible(t *testing.T) {
	t.Setenv("ASR_PROVIDER", "openai_compatible")
	t.Setenv("ASR_BASE_URL", "https://asr.example.com")
	t.Setenv("ASR_API_KEY", "k")
	t.Setenv("OPENCLAW_RPC_URL", "https://gateway.example.com/rpc")
	t.Setenv("TTS_WS_URL", "wss://openspeech.bytedance.com/api/v1/tts/ws_binary")
	t.Setenv("TTS_APP_ID", "appid")
	t.Setenv("TTS_TOKEN", "token")
	t.Setenv("TTS_CLUSTER", "volcano_tts")
	t.Setenv("TTS_VOICE", "zh-CN-XiaoxiaoNeural")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.ASRProvider != "openai_compatible" {
		t.Fatalf("ASRProvider = %q", cfg.ASRProvider)
	}
}

// TestLoadFromEnvLocalVoiceIncompleteDegrades: with the LAN pipeline opted in
// (BBCLAW_LOCAL_VOICE=1) but ASR incomplete, the adapter still LOADS (incomplete
// voice degrades, never a hard boot failure — ADR-025 §3). VoiceReady is false
// and VoiceConfigError points at the missing knob.
func TestLoadFromEnvLocalVoiceIncompleteDegrades(t *testing.T) {
	t.Setenv("BBCLAW_LOCAL_VOICE", "1")
	t.Setenv("ASR_PROVIDER", "local")
	t.Setenv("OPENCLAW_RPC_URL", "https://gateway.example.com/rpc")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("incomplete voice should not fail boot, got: %v", err)
	}
	if !cfg.LocalVoiceEnabled {
		t.Fatal("BBCLAW_LOCAL_VOICE=1 should set LocalVoiceEnabled")
	}
	if cfg.VoiceReady() {
		t.Fatal("VoiceReady should be false with ASR_LOCAL_BIN missing")
	}
	if err := cfg.VoiceConfigError(); err == nil || !strings.Contains(err.Error(), "ASR_LOCAL_BIN") {
		t.Fatalf("VoiceConfigError = %v, want mention of ASR_LOCAL_BIN", err)
	}
}

func TestLoadFromEnvDoubaoNative(t *testing.T) {
	t.Setenv("ASR_PROVIDER", "doubao_native")
	t.Setenv("ASR_WS_URL", "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_nostream")
	t.Setenv("ASR_APP_ID", "6213721583")
	t.Setenv("ASR_API_KEY", "k")
	t.Setenv("ASR_RESOURCE_ID", "volc.bigasr.sauc.duration")
	t.Setenv("OPENCLAW_RPC_URL", "https://gateway.example.com/rpc")
	t.Setenv("TTS_WS_URL", "wss://openspeech.bytedance.com/api/v1/tts/ws_binary")
	t.Setenv("TTS_APP_ID", "appid")
	t.Setenv("TTS_TOKEN", "token")
	t.Setenv("TTS_CLUSTER", "volcano_tts")
	t.Setenv("TTS_VOICE", "zh-CN-XiaoxiaoNeural")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.ASRProvider != "doubao_native" {
		t.Fatalf("ASRProvider = %q", cfg.ASRProvider)
	}
}

func TestLoadFromEnvAutoEnablesCloudRelayWhenConfigured(t *testing.T) {
	t.Setenv("ASR_LOCAL_BIN", "/bin/echo")
	t.Setenv("OPENCLAW_RPC_URL", "https://gateway.example.com/rpc")
	t.Setenv("TTS_WS_URL", "wss://openspeech.bytedance.com/api/v1/tts/ws_binary")
	t.Setenv("TTS_APP_ID", "appid")
	t.Setenv("TTS_TOKEN", "token")
	t.Setenv("TTS_CLUSTER", "volcano_tts")
	t.Setenv("TTS_VOICE", "zh-CN-XiaoxiaoNeural")
	t.Setenv("CLOUD_WS_URL", "wss://cloud.example.com/ws")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if !cfg.EnableLocalIngress() {
		t.Fatal("expected local ingress enabled in auto mode")
	}
	if !cfg.EnableCloudRelay() {
		t.Fatal("expected cloud relay enabled when CLOUD_WS_URL is set")
	}
}

func TestLoadFromEnvCloudModeDisablesLocalIngress(t *testing.T) {
	t.Setenv("ADAPTER_MODE", "cloud")
	t.Setenv("OPENCLAW_RPC_URL", "https://gateway.example.com/rpc")
	t.Setenv("CLOUD_WS_URL", "wss://cloud.example.com/ws")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.EnableLocalIngress() {
		t.Fatal("expected local ingress disabled in cloud mode")
	}
	if !cfg.EnableCloudRelay() {
		t.Fatal("expected cloud relay enabled in cloud mode")
	}
}

func TestParseCwdPool(t *testing.T) {
	tests := []struct {
		name       string
		poolEnv    string
		defaultCwd string
		want       []CwdEntry
	}{
		{
			name:       "empty pool and no default → nil",
			poolEnv:    "",
			defaultCwd: "",
			want:       nil,
		},
		{
			name:       "empty pool with default → single default entry",
			poolEnv:    "",
			defaultCwd: "/home/user/code",
			want:       []CwdEntry{{Name: "default", Path: "/home/user/code"}},
		},
		{
			name:       "single entry",
			poolEnv:    "myproject:/Users/mikas/code/myproject",
			defaultCwd: "",
			want:       []CwdEntry{{Name: "myproject", Path: "/Users/mikas/code/myproject"}},
		},
		{
			name:       "multiple entries",
			poolEnv:    "myproject:/Users/mikas/code/myproject,side:/Users/mikas/code/side",
			defaultCwd: "",
			want: []CwdEntry{
				{Name: "myproject", Path: "/Users/mikas/code/myproject"},
				{Name: "side", Path: "/Users/mikas/code/side"},
			},
		},
		{
			name:       "pool present overrides default",
			poolEnv:    "a:/path/a",
			defaultCwd: "/fallback",
			want:       []CwdEntry{{Name: "a", Path: "/path/a"}},
		},
		{
			name:       "malformed entry (no colon) is skipped",
			poolEnv:    "nocolon,good:/path/good",
			defaultCwd: "",
			want:       []CwdEntry{{Name: "good", Path: "/path/good"}},
		},
		{
			name:       "empty name is skipped",
			poolEnv:    ":/path/bad,ok:/path/ok",
			defaultCwd: "",
			want:       []CwdEntry{{Name: "ok", Path: "/path/ok"}},
		},
		{
			name:       "empty path is skipped",
			poolEnv:    "bad:,ok:/path/ok",
			defaultCwd: "",
			want:       []CwdEntry{{Name: "ok", Path: "/path/ok"}},
		},
		{
			name:       "whitespace trimmed",
			poolEnv:    "  proj : /path/proj , other : /path/other ",
			defaultCwd: "",
			want: []CwdEntry{
				{Name: "proj", Path: "/path/proj"},
				{Name: "other", Path: "/path/other"},
			},
		},
		{
			name:       "all malformed falls back to default",
			poolEnv:    "nocolon,alsono",
			defaultCwd: "/fallback",
			want:       []CwdEntry{{Name: "default", Path: "/fallback"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCwdPool(tt.poolEnv, tt.defaultCwd)
			if len(got) != len(tt.want) {
				t.Fatalf("parseCwdPool() len=%d, want %d; got=%v", len(got), len(tt.want), got)
			}
			for i, e := range got {
				if e.Name != tt.want[i].Name || e.Path != tt.want[i].Path {
					t.Errorf("entry[%d] = {%q,%q}, want {%q,%q}", i, e.Name, e.Path, tt.want[i].Name, tt.want[i].Path)
				}
			}
		})
	}
}

func TestLoadFromEnvCwdPool(t *testing.T) {
	t.Setenv("ASR_LOCAL_BIN", "/bin/echo")
	t.Setenv("OPENCLAW_RPC_URL", "https://gateway.example.com/rpc")
	t.Setenv("TTS_WS_URL", "wss://openspeech.bytedance.com/api/v1/tts/ws_binary")
	t.Setenv("TTS_APP_ID", "appid")
	t.Setenv("TTS_TOKEN", "token")
	t.Setenv("TTS_CLUSTER", "volcano_tts")
	t.Setenv("TTS_VOICE", "zh-CN-XiaoxiaoNeural")

	t.Run("pool parsed from env", func(t *testing.T) {
		t.Setenv("BBCLAW_CWD_POOL", "a:/path/a,b:/path/b")
		cfg, err := LoadFromEnv()
		if err != nil {
			t.Fatalf("LoadFromEnv() error = %v", err)
		}
		if len(cfg.CwdPool) != 2 {
			t.Fatalf("CwdPool len=%d, want 2", len(cfg.CwdPool))
		}
		if cfg.CwdPool[0].Name != "a" || cfg.CwdPool[0].Path != "/path/a" {
			t.Errorf("CwdPool[0] = %+v", cfg.CwdPool[0])
		}
		if cfg.CwdPool[1].Name != "b" || cfg.CwdPool[1].Path != "/path/b" {
			t.Errorf("CwdPool[1] = %+v", cfg.CwdPool[1])
		}
	})

	t.Run("default cwd becomes single pool entry when pool empty", func(t *testing.T) {
		t.Setenv("BBCLAW_DEFAULT_CWD", "/home/user/work")
		cfg, err := LoadFromEnv()
		if err != nil {
			t.Fatalf("LoadFromEnv() error = %v", err)
		}
		if len(cfg.CwdPool) != 1 {
			t.Fatalf("CwdPool len=%d, want 1", len(cfg.CwdPool))
		}
		if cfg.CwdPool[0].Name != "default" || cfg.CwdPool[0].Path != "/home/user/work" {
			t.Errorf("CwdPool[0] = %+v", cfg.CwdPool[0])
		}
	})
}

func TestGetEnvDuration(t *testing.T) {
	tests := []struct {
		envVal   string
		fallback time.Duration
		want     time.Duration
	}{
		{"", 5 * time.Minute, 5 * time.Minute},        // empty → fallback
		{"5m", 0, 5 * time.Minute},                    // Go duration
		{"24h", 0, 24 * time.Hour},                    // hours
		{"7d", 0, 7 * 24 * time.Hour},                 // days shorthand
		{"1d", 0, 24 * time.Hour},                     // 1 day
		{"30s", 0, 30 * time.Second},                  // seconds
		{"invalid", 3 * time.Minute, 3 * time.Minute}, // invalid → fallback
		{"0d", 5 * time.Minute, 5 * time.Minute},      // 0d → fallback (n must be > 0)
		{"-1d", 5 * time.Minute, 5 * time.Minute},     // negative day → fallback
	}
	for _, tt := range tests {
		t.Run(tt.envVal, func(t *testing.T) {
			const envKey = "TEST_DURATION_PARSE"
			if tt.envVal != "" {
				t.Setenv(envKey, tt.envVal)
			} else {
				os.Unsetenv(envKey)
			}
			got := getEnvDuration(envKey, tt.fallback)
			if got != tt.want {
				t.Errorf("getEnvDuration(%q, %v) = %v, want %v", tt.envVal, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestOpencodeServeEnabled(t *testing.T) {
	t.Setenv("AGENT_OPENCODE_SERVE", "")
	// env truthy values
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv("AGENT_OPENCODE_SERVE", v)
		if !(Config{}).OpencodeServeEnabled() {
			t.Errorf("env %q should enable", v)
		}
	}
	for _, v := range []string{"", "0", "false", "off", "nope"} {
		t.Setenv("AGENT_OPENCODE_SERVE", v)
		if (Config{}).OpencodeServeEnabled() {
			t.Errorf("env %q should NOT enable", v)
		}
	}
	// web override wins over env
	t.Setenv("AGENT_OPENCODE_SERVE", "1")
	off := false
	if (Config{OpencodeServeOverride: &off}).OpencodeServeEnabled() {
		t.Errorf("override=false should win over env=1")
	}
	t.Setenv("AGENT_OPENCODE_SERVE", "0")
	on := true
	if !(Config{OpencodeServeOverride: &on}).OpencodeServeEnabled() {
		t.Errorf("override=true should win over env=0")
	}
}
