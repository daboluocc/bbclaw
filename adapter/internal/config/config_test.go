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

func TestLoadFromEnvInvalid(t *testing.T) {
	os.Clearenv()
	_, err := LoadFromEnv()
	if err == nil {
		t.Fatal("expected error for missing required env")
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

func TestLoadFromEnvLocalRequiresBin(t *testing.T) {
	t.Setenv("ASR_PROVIDER", "local")
	t.Setenv("OPENCLAW_RPC_URL", "https://gateway.example.com/rpc")
	t.Setenv("TTS_WS_URL", "wss://openspeech.bytedance.com/api/v1/tts/ws_binary")
	t.Setenv("TTS_APP_ID", "appid")
	t.Setenv("TTS_TOKEN", "token")
	t.Setenv("TTS_CLUSTER", "volcano_tts")
	t.Setenv("TTS_VOICE", "zh-CN-XiaoxiaoNeural")
	_, err := LoadFromEnv()
	if err == nil {
		t.Fatal("expected error for missing ASR_LOCAL_BIN")
	}
	if !strings.Contains(err.Error(), "ASR_LOCAL_BIN") {
		t.Fatalf("err = %v", err)
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
		{"", 5 * time.Minute, 5 * time.Minute},           // empty → fallback
		{"5m", 0, 5 * time.Minute},                       // Go duration
		{"24h", 0, 24 * time.Hour},                       // hours
		{"7d", 0, 7 * 24 * time.Hour},                    // days shorthand
		{"1d", 0, 24 * time.Hour},                        // 1 day
		{"30s", 0, 30 * time.Second},                     // seconds
		{"invalid", 3 * time.Minute, 3 * time.Minute},    // invalid → fallback
		{"0d", 5 * time.Minute, 5 * time.Minute},         // 0d → fallback (n must be > 0)
		{"-1d", 5 * time.Minute, 5 * time.Minute},        // negative day → fallback
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
