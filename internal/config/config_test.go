package config

import (
	"os"
	"strings"
	"testing"
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
