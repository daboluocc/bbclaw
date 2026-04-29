package detect

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Result holds detection results for a single component
type Result struct {
	Present bool
	Reason  string
	Data    map[string]interface{}
}

// Environment holds all detected environment information
type Environment struct {
	OpenClaw   Result
	ClaudeCode Result
	OpenCode   Result
	Aider      Result
	Ollama     Result
	SayTTS     Result
	Doubao     Result
	FunASR     Result
	EspeakTTS  Result
}

const (
	defaultOpenClawPort = 18789
	ollamaDefaultHost   = "127.0.0.1:11434"
	ollamaProbeTimeout  = 500 * time.Millisecond
)

// DetectAll runs all detection checks
func DetectAll(doubaoEnvPath string) *Environment {
	env := &Environment{
		OpenClaw:   detectOpenClaw(),
		ClaudeCode: detectClaudeCode(),
		OpenCode:   detectOpenCode(),
		Aider:      detectAider(),
		Ollama:     detectOllama(),
		SayTTS:     detectSayTTS(),
		FunASR:     detectFunASR(),
		EspeakTTS:  detectEspeakTTS(),
	}

	if doubaoEnvPath != "" {
		env.Doubao = detectDoubao(doubaoEnvPath)
	} else {
		env.Doubao = Result{Present: false, Reason: "no source path provided"}
	}

	return env
}

// detectOpenClaw reads ~/.openclaw/openclaw.json
func detectOpenClaw() Result {
	home, err := os.UserHomeDir()
	if err != nil {
		return Result{Present: false, Reason: fmt.Sprintf("cannot get home dir: %v", err)}
	}

	configPath := filepath.Join(home, ".openclaw", "openclaw.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{Present: false, Reason: "config file not found"}
		}
		return Result{Present: false, Reason: fmt.Sprintf("read error: %v", err)}
	}

	var cfg struct {
		Gateway struct {
			Port int `json:"port"`
			Auth struct {
				Token string `json:"token"`
			} `json:"auth"`
		} `json:"gateway"`
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return Result{Present: false, Reason: fmt.Sprintf("parse error: %v", err)}
	}

	port := cfg.Gateway.Port
	if port == 0 {
		port = defaultOpenClawPort
	}

	token := cfg.Gateway.Auth.Token
	if token == "" {
		return Result{Present: false, Reason: "token missing in config"}
	}

	return Result{
		Present: true,
		Data: map[string]interface{}{
			"config": configPath,
			"port":   port,
			"token":  token,
		},
	}
}

// detectClaudeCode checks for claude binary on PATH
func detectClaudeCode() Result {
	binPath, err := exec.LookPath("claude")
	if err != nil {
		return Result{Present: false, Reason: "claude not on PATH"}
	}
	return Result{
		Present: true,
		Data:    map[string]interface{}{"bin": binPath},
	}
}

// detectOpenCode checks for opencode binary on PATH
func detectOpenCode() Result {
	binPath, err := exec.LookPath("opencode")
	if err != nil {
		return Result{Present: false, Reason: "opencode not on PATH"}
	}
	return Result{
		Present: true,
		Data:    map[string]interface{}{"bin": binPath},
	}
}

// detectAider checks for aider binary on PATH
func detectAider() Result {
	binPath, err := exec.LookPath("aider")
	if err != nil {
		return Result{Present: false, Reason: "aider not on PATH"}
	}
	return Result{
		Present: true,
		Data:    map[string]interface{}{"bin": binPath},
	}
}

// detectOllama checks if ollama is running and lists models
func detectOllama() Result {
	// TCP probe
	conn, err := net.DialTimeout("tcp", ollamaDefaultHost, ollamaProbeTimeout)
	if err != nil {
		return Result{Present: false, Reason: fmt.Sprintf("%s not listening", ollamaDefaultHost)}
	}
	conn.Close()

	// Try to list models
	binPath, err := exec.LookPath("ollama")
	var models []string
	if err == nil {
		cmd := exec.Command(binPath, "list")
		output, err := cmd.Output()
		if err == nil {
			lines := strings.Split(string(output), "\n")
			for i, line := range lines {
				if i == 0 { // skip header
					continue
				}
				cols := strings.Fields(line)
				if len(cols) > 0 {
					models = append(models, cols[0])
				}
			}
		}
	}

	return Result{
		Present: true,
		Data: map[string]interface{}{
			"bin":    binPath,
			"models": models,
		},
	}
}

// detectSayTTS checks for macOS say command
func detectSayTTS() Result {
	if runtime.GOOS != "darwin" {
		return Result{Present: false, Reason: "say is macOS-only"}
	}

	binPath, err := exec.LookPath("say")
	if err != nil {
		return Result{Present: false, Reason: "say not on PATH"}
	}

	return Result{
		Present: true,
		Data:    map[string]interface{}{"bin": binPath},
	}
}

// detectFunASR checks if FunASR is installed in tools/asr/
func detectFunASR() Result {
	// Check if tools/asr/ directory exists with FunASR components
	asrPath := "tools/asr"
	if _, err := os.Stat(asrPath); os.IsNotExist(err) {
		return Result{Present: false, Reason: "tools/asr/ directory not found"}
	}

	// Check for key FunASR files
	requiredFiles := []string{
		filepath.Join(asrPath, "funasr_server.py"),
		filepath.Join(asrPath, "funasr_wrapper.sh"),
	}

	for _, file := range requiredFiles {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			return Result{Present: false, Reason: fmt.Sprintf("%s not found", file)}
		}
	}

	return Result{
		Present: true,
		Data:    map[string]interface{}{"path": asrPath},
	}
}

// detectEspeakTTS checks for espeak-ng or espeak on Linux
func detectEspeakTTS() Result {
	if runtime.GOOS != "linux" {
		return Result{Present: false, Reason: "espeak is Linux-only"}
	}

	// Try espeak-ng first, then espeak
	for _, bin := range []string{"espeak-ng", "espeak"} {
		if binPath, err := exec.LookPath(bin); err == nil {
			return Result{
				Present: true,
				Data:    map[string]interface{}{"bin": binPath, "variant": bin},
			}
		}
	}

	return Result{Present: false, Reason: "neither espeak-ng nor espeak found on PATH"}
}

// detectDoubao imports Doubao/Volcano keys from external .env file
func detectDoubao(sourcePath string) Result {
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return Result{Present: false, Reason: fmt.Sprintf("cannot read %s: %v", sourcePath, err)}
	}

	raw := parseEnvFile(string(data))
	mapped := make(map[string]string)

	// Map sales-apis style → bbclaw adapter style
	if v, ok := raw["ASR_APP_ID"]; ok && v != "" {
		mapped["ASR_APP_ID"] = v
	}
	if v, ok := raw["ASR_TOKEN"]; ok && v != "" {
		mapped["ASR_API_KEY"] = v
	} else if v, ok := raw["ASR_API_KEY"]; ok && v != "" {
		mapped["ASR_API_KEY"] = v
	}
	if v, ok := raw["TTS_APP_ID"]; ok && v != "" {
		mapped["TTS_APP_ID"] = v
	}
	if v, ok := raw["TTS_TOKEN"]; ok && v != "" {
		mapped["TTS_TOKEN"] = v
	}

	if len(mapped) == 0 {
		return Result{Present: false, Reason: "no valid keys found in source file"}
	}

	return Result{
		Present: true,
		Data: map[string]interface{}{
			"source":       sourcePath,
			"mapped":       mapped,
			"cluster_hint": raw["ASR_CLUSTER"],
		},
	}
}

// parseEnvFile parses a .env file into a map
func parseEnvFile(content string) map[string]string {
	result := make(map[string]string)
	lines := strings.Split(content, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// Strip quotes
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		result[key] = value
	}

	return result
}

// PickDefaultDriver selects the default driver based on detection results
func (e *Environment) PickDefaultDriver() string {
	// Preference order
	prefs := []struct {
		name    string
		present bool
	}{
		{"claude-code", e.ClaudeCode.Present},
		{"opencode", e.OpenCode.Present},
		{"aider", e.Aider.Present},
		{"openclaw", e.OpenClaw.Present},
		{"ollama", e.Ollama.Present},
	}

	for _, p := range prefs {
		if p.present {
			return p.name
		}
	}

	return ""
}
