package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/detect"
	"github.com/daboluocc/bbclaw/adapter/internal/envfile"
	"github.com/spf13/cobra"
)

var (
	doctorFix        bool
	doctorReset      bool
	doctorDoubaoEnv  string
	doctorDryRun     bool
	doctorAutoInstall bool // skip confirmation prompt for auto-install
)

// NewDoctorCmd creates the doctor command
func NewDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose environment and optionally fix configuration",
		Long: `Scan local environment for AI tools and voice components.

Without --fix, performs read-only diagnostics and reports status.
With --fix, writes detected configuration to .env file.

Examples:
  bbclaw-adapter doctor                    # Read-only diagnostics
  bbclaw-adapter doctor --fix              # Detect and write .env
  bbclaw-adapter doctor --fix --reset      # Discard existing .env
  bbclaw-adapter doctor --doubao-env PATH  # Import Doubao keys`,
		RunE: runDoctor,
	}

	cmd.Flags().BoolVar(&doctorFix, "fix", false, "Write detected configuration to .env")
	cmd.Flags().BoolVar(&doctorReset, "reset", false, "Discard existing .env and write fresh (backup first)")
	cmd.Flags().StringVar(&doctorDoubaoEnv, "doubao-env", os.Getenv("DOUBAO_ENV_FILE"), "Path to external .env with Doubao/Volcano keys")
	cmd.Flags().BoolVar(&doctorDryRun, "dry-run", false, "Print .env to stdout without writing")
	cmd.Flags().BoolVar(&doctorAutoInstall, "yes", false, "Auto-install missing tools without prompting")

	return cmd
}

func runDoctor(cmd *cobra.Command, args []string) error {
	// Detect all components
	env := detect.DetectAll(doctorDoubaoEnv)

	// Auto-install FunASR if missing and --fix is set
	if !env.FunASR.Present && doctorFix {
		if err := installFunASR(); err != nil {
			fmt.Printf("\nWarning: FunASR auto-install failed: %v\n", err)
			fmt.Println("You can install manually:")
			fmt.Println("  python3 tools/asr/run_funasr_server.sh")
		} else {
			// Re-detect after installation
			env = detect.DetectAll(doctorDoubaoEnv)
		}
	}

	// Print detection results
	fmt.Println("Driver scan:")
	printDetectionResult("openclaw", env.OpenClaw, func(r detect.Result) string {
		if !r.Present {
			return ""
		}
		port := r.Data["port"].(int)
		token := r.Data["token"].(string)
		tokenPreview := "(missing)"
		if len(token) >= 8 {
			tokenPreview = token[:8] + "…"
		}
		return fmt.Sprintf("  port=%d  token=%s", port, tokenPreview)
	})

	printDetectionResult("claude-code", env.ClaudeCode, func(r detect.Result) string {
		if !r.Present {
			return ""
		}
		if bin, ok := r.Data["bin"].(string); ok {
			return fmt.Sprintf("  %s", bin)
		}
		return ""
	})

	printDetectionResult("opencode", env.OpenCode, func(r detect.Result) string {
		if !r.Present {
			return ""
		}
		if bin, ok := r.Data["bin"].(string); ok {
			return fmt.Sprintf("  %s", bin)
		}
		return ""
	})

	printDetectionResult("aider", env.Aider, func(r detect.Result) string {
		if !r.Present {
			return ""
		}
		if bin, ok := r.Data["bin"].(string); ok {
			return fmt.Sprintf("  %s", bin)
		}
		return ""
	})

	printDetectionResult("ollama", env.Ollama, func(r detect.Result) string {
		if !r.Present {
			return ""
		}
		if models, ok := r.Data["models"].([]string); ok {
			modelList := "(none installed)"
			if len(models) > 0 {
				modelList = strings.Join(models[:min(3, len(models))], ",")
			}
			return fmt.Sprintf("  models=%s", modelList)
		}
		return ""
	})

	if doctorDoubaoEnv != "" {
		printDetectionResult("doubao", env.Doubao, func(r detect.Result) string {
			if !r.Present {
				return ""
			}
			source := r.Data["source"].(string)
			mapped := r.Data["mapped"].(map[string]string)
			var keys []string
			for k := range mapped {
				keys = append(keys, k)
			}
			keyList := "(none)"
			if len(keys) > 0 {
				keyList = strings.Join(keys, ",")
			}
			return fmt.Sprintf("  from %s  keys=%s", source, keyList)
		})
	}

	// ASR/TTS detection
	printDetectionResult("funasr", env.FunASR, func(r detect.Result) string {
		if !r.Present {
			return ""
		}
		if path, ok := r.Data["path"].(string); ok {
			return fmt.Sprintf("  %s", path)
		}
		return ""
	})

	if runtime.GOOS == "darwin" {
		printDetectionResult("say-tts", env.SayTTS, func(r detect.Result) string {
			if !r.Present {
				return ""
			}
			if bin, ok := r.Data["bin"].(string); ok {
				return fmt.Sprintf("  %s", bin)
			}
			return ""
		})
	}

	if runtime.GOOS == "linux" {
		printDetectionResult("espeak-tts", env.EspeakTTS, func(r detect.Result) string {
			if !r.Present {
				return ""
			}
			if bin, ok := r.Data["bin"].(string); ok {
				variant := r.Data["variant"].(string)
				return fmt.Sprintf("  %s (%s)", bin, variant)
			}
			return ""
		})
	}

	// If not fixing, we're done
	if !doctorFix && !doctorDryRun {
		fmt.Println("\nRun with --fix to write configuration to .env")
		return nil
	}

	// Build .env values
	envPath := ".env"
	existing, err := envfile.Parse(envPath)
	if err != nil {
		return fmt.Errorf("parse existing .env: %w", err)
	}

	values := envfile.BuildValues(existing, env, doctorReset)
	rendered := envfile.Render(values, env)

	// Dry run: print to stdout
	if doctorDryRun {
		fmt.Printf("\n--- would write %s ---\n", envPath)
		fmt.Print(rendered)
		return nil
	}

	// Backup existing .env if resetting
	if doctorReset {
		if _, err := os.Stat(envPath); err == nil {
			backup := fmt.Sprintf(".env.bak.%d", time.Now().Unix())
			data, _ := os.ReadFile(envPath)
			if err := os.WriteFile(backup, data, 0644); err == nil {
				fmt.Printf("\nBacked up existing .env → %s\n", backup)
			}
		}
	}

	// Write .env
	if err := os.WriteFile(envPath, []byte(rendered), 0644); err != nil {
		return fmt.Errorf("write .env: %w", err)
	}

	lineCount := len(strings.Split(rendered, "\n"))
	fmt.Printf("\nWrote %s (%d lines)\n", envPath, lineCount)

	// Print follow-up hints
	var todo []string
	if values["ASR_API_KEY"] == "" {
		todo = append(todo, "ASR_API_KEY (for voice path)")
	}
	if values["TTS_TOKEN"] == "" && values["TTS_LOCAL_BIN"] == "" {
		todo = append(todo, "TTS_TOKEN (for voice path)")
	}

	if len(todo) > 0 {
		fmt.Println("\nManual follow-up needed for these:")
		for _, t := range todo {
			fmt.Printf("  - %s\n", t)
		}
	}

	// Doubao cluster warning
	if env.Doubao.Present {
		if cluster, ok := env.Doubao.Data["cluster_hint"].(string); ok && cluster != "" {
			if cluster != "volc.bigasr.sauc.duration" {
				fmt.Printf("\nNote: imported ASR_CLUSTER=%q differs from bbclaw default\n", cluster)
				fmt.Println("(volc.bigasr.sauc.duration). If you hit auth errors, your Volcengine")
				fmt.Println("app may not have access to the bigmodel cluster.")
			}
		}
	}

	// ASR/TTS installation hints
	if !env.FunASR.Present {
		fmt.Println("\nFunASR not detected. To install:")
		fmt.Println("  python3 tools/asr/run_funasr_server.sh")
	}

	if runtime.GOOS == "linux" && !env.EspeakTTS.Present {
		fmt.Println("\nespeak-ng not detected. To install on Linux:")
		fmt.Println("  sudo apt-get install espeak-ng  # Debian/Ubuntu")
		fmt.Println("  sudo yum install espeak-ng      # RHEL/CentOS")
	}

	if runtime.GOOS == "windows" {
		fmt.Println("\nNote: Local TTS is not supported on Windows. Use cloud TTS providers.")
	}

	return nil
}

func printDetectionResult(name string, result detect.Result, extraInfo func(detect.Result) string) {
	var tag string
	var extra string

	if result.Present {
		tag = "✓"
		extra = extraInfo(result)
	} else {
		tag = "✗"
		extra = "  " + result.Reason
	}

	fmt.Printf("  %s %-12s%s\n", tag, name, extra)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// installFunASR clones the bbclaw repo and copies tools/ to ~/.bbclaw/tools/
func installFunASR() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot get home dir: %w", err)
	}

	bbclawToolsDir := filepath.Join(home, ".bbclaw", "tools")
	bbclawDir := filepath.Join(home, ".bbclaw")

	// Check if already installed
	if _, err := os.Stat(filepath.Join(bbclawToolsDir, "asr")); err == nil {
		return nil // already installed
	}

	fmt.Println("\nDownloading FunASR tools from GitHub...")

	// Clone to a temp directory
	tmpDir, err := os.MkdirTemp("", "bbclaw-tools-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Clone only the tools directory using sparse-checkout
	cloneCmd := exec.Command("git", "clone", "--depth=1", "https://github.com/daboluocc/bbclaw", tmpDir)
	cloneCmd.Stdout = os.Stdout
	cloneCmd.Stderr = os.Stderr
	if err := cloneCmd.Run(); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}

	// Ensure ~/.bbclaw exists
	if err := os.MkdirAll(bbclawDir, 0755); err != nil {
		return fmt.Errorf("create ~/.bbclaw: %w", err)
	}

	// Copy tools directory
	srcTools := filepath.Join(tmpDir, "tools")
	if _, err := os.Stat(srcTools); err != nil {
		return fmt.Errorf("tools/ not found in clone: %w", err)
	}

	// Remove existing tools dir if exists
	if _, err := os.Stat(bbclawToolsDir); err == nil {
		if err := os.RemoveAll(bbclawToolsDir); err != nil {
			return fmt.Errorf("remove old tools: %w", err)
		}
	}

	if err := copyDir(srcTools, bbclawToolsDir); err != nil {
		return fmt.Errorf("copy tools: %w", err)
	}

	fmt.Printf("FunASR installed to %s\n", bbclawToolsDir)
	return nil
}

// copyDir recursively copies src to dst
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, _ := filepath.Rel(src, path)
		dstPath := filepath.Join(dst, relPath)

		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}
		return copyFile(path, dstPath, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}
