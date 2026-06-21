package asr

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// LocalCommandProvider runs a local ASR executable (e.g. whisper.cpp) on a temporary WAV file.
// Args may contain the placeholder {wav}, which is replaced with the absolute path of the temp WAV.
// If stdout is non-empty after the command succeeds, it is used as the transcript.
// Otherwise candidate transcript files are tried in order:
// 1) ASR_LOCAL_TEXT_PATH template (default "{wav}.txt")
// 2) <wavPath>.txt
// 3) <stem>.txt where stem is wavPath without ".wav"
type LocalCommandProvider struct {
	bin          string
	args         []string
	textTemplate string
}

func NewLocalCommandProvider(bin string, args []string, textPathTemplate string) *LocalCommandProvider {
	tpl := strings.TrimSpace(textPathTemplate)
	if tpl == "" {
		tpl = "{wav}.txt"
	}
	return &LocalCommandProvider{bin: bin, args: args, textTemplate: tpl}
}

// Ping checks that the configured binary exists and is executable (best-effort on Windows).
func (p *LocalCommandProvider) Ping(ctx context.Context) error {
	path, err := lookPathOrAbs(p.bin)
	if err != nil {
		return fmt.Errorf("asr local: %w", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("asr local: %w", err)
	}
	if fi.IsDir() {
		return fmt.Errorf("asr local: not a file: %s", path)
	}
	if runtime.GOOS != "windows" && fi.Mode()&0111 == 0 {
		return fmt.Errorf("asr local: not executable: %s", path)
	}
	return nil
}

func lookPathOrAbs(bin string) (string, error) {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		return "", fmt.Errorf("empty ASR_LOCAL_BIN")
	}
	if filepath.IsAbs(bin) || strings.ContainsRune(bin, filepath.Separator) {
		return bin, nil
	}
	return exec.LookPath(bin)
}

// LocalConfigSummary returns a compact log line for startup/diagnostics (no secrets).
func LocalConfigSummary(bin string, args []string, textPathTemplate string) string {
	tpl := strings.TrimSpace(textPathTemplate)
	if tpl == "" {
		tpl = "{wav}.txt"
	}
	bin = strings.TrimSpace(bin)
	resolved := ""
	if bin != "" {
		if p, err := lookPathOrAbs(bin); err != nil {
			resolved = fmt.Sprintf("unresolved(%v)", err)
		} else {
			resolved = p
		}
	}
	argsStr := strings.Join(args, " ")
	if argsStr == "" {
		argsStr = "(none)"
	}
	return fmt.Sprintf("local_asr bin=%q resolved=%q args=%q text_path=%q placeholder={wav}", bin, resolved, argsStr, tpl)
}

func (p *LocalCommandProvider) Transcribe(ctx context.Context, audio []byte, meta Metadata) (Result, error) {
	if len(audio) == 0 {
		return Result{}, &APIError{Code: "ASR_BAD_REQUEST", Message: "empty audio"}
	}
	sr := meta.SampleRate
	if sr <= 0 {
		sr = 16000
	}
	ch := meta.Channels
	if ch <= 0 {
		ch = 1
	}

	wavData := PCM16LEToWAV(audio, sr, ch)
	tmp, err := os.CreateTemp("", "bbclaw-asr-*.wav")
	if err != nil {
		return Result{}, fmt.Errorf("local asr temp wav: %w", err)
	}
	wavPath := tmp.Name()
	_, werr := tmp.Write(wavData)
	cerr := tmp.Close()
	if werr != nil {
		_ = os.Remove(wavPath)
		return Result{}, fmt.Errorf("local asr write wav: %w", werr)
	}
	if cerr != nil {
		_ = os.Remove(wavPath)
		return Result{}, fmt.Errorf("local asr close wav: %w", cerr)
	}
	defer func() { _ = os.Remove(wavPath) }()

	args := expandWavArgs(p.args, wavPath)
	cmd := exec.CommandContext(ctx, p.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runErr != nil {
		return Result{}, fmt.Errorf("local asr: %w: stderr=%s", runErr, truncateErrText(stderr.String()))
	}

	// Prefer transcript file (e.g. whisper -otxt / FunASR funasr_cli.py) so stdout is not polluted by
	// tools that print version/download/progress to stdout during inference.
	text := readFirstTranscript(wavPath, p.textTemplate)
	if text == "" {
		text = strings.TrimSpace(stdout.String())
	}
	if text == "" {
		return Result{}, &APIError{Code: "ASR_EMPTY_TRANSCRIPT", Message: "empty transcript"}
	}

	durMs := int64(len(audio)) / int64(2*ch*sr) * 1000
	if durMs < 0 {
		durMs = 0
	}
	return Result{Text: text, DurationMs: durMs}, nil
}

func expandWavArgs(args []string, wav string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ReplaceAll(a, "{wav}", wav)
	}
	return out
}

func readFirstTranscript(wavPath, textTemplate string) string {
	candidates := candidateTranscriptPaths(wavPath, textTemplate)
	for _, path := range candidates {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		t := strings.TrimSpace(string(b))
		if t != "" {
			return t
		}
	}
	return ""
}

func candidateTranscriptPaths(wavPath, textTemplate string) []string {
	seen := map[string]struct{}{}
	var ordered []string
	add := func(p string) {
		p = filepath.Clean(p)
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		ordered = append(ordered, p)
	}
	add(strings.ReplaceAll(textTemplate, "{wav}", wavPath))
	add(wavPath + ".txt")
	stem := strings.TrimSuffix(wavPath, ".wav")
	if stem != wavPath {
		add(stem + ".txt")
	}
	return ordered
}

func truncateErrText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 512 {
		return s
	}
	return s[:512] + "..."
}

