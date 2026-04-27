package tts

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

type LocalCommandProvider struct {
	bin          string
	args         []string
	outputFormat string
}

func NewLocalCommandProvider(bin string, args []string, outputFormat string) *LocalCommandProvider {
	return &LocalCommandProvider{
		bin:          strings.TrimSpace(bin),
		args:         append([]string(nil), args...),
		outputFormat: normalizeOutputFormat(outputFormat),
	}
}

func (p *LocalCommandProvider) OutputFormat() string {
	return p.outputFormat
}

func (p *LocalCommandProvider) Synthesize(ctx context.Context, text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("tts local: empty text")
	}

	tempDir, err := os.MkdirTemp("", "bbclaw-tts-*")
	if err != nil {
		return nil, fmt.Errorf("tts local temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	outPath := filepath.Join(tempDir, "tts-output."+p.outputFormat)
	args := expandArgs(p.args, text, outPath)

	cmd := buildCommand(ctx, p.bin, args)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tts local: %w: stderr=%s", err, truncate(stderr.String()))
	}

	if audio, err := os.ReadFile(outPath); err == nil && len(audio) > 0 {
		return audio, nil
	}
	if stdout.Len() > 0 {
		return stdout.Bytes(), nil
	}
	return nil, fmt.Errorf("tts local: empty audio")
}

func buildCommand(ctx context.Context, bin string, args []string) *exec.Cmd {
	if strings.HasSuffix(strings.ToLower(bin), ".sh") {
		all := append([]string{bin}, args...)
		return exec.CommandContext(ctx, "/bin/bash", all...)
	}
	return exec.CommandContext(ctx, bin, args...)
}

func normalizeOutputFormat(f string) string {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "", "wav":
		return "wav"
	case "pcm", "pcm16":
		return "pcm"
	default:
		return strings.ToLower(strings.TrimSpace(f))
	}
}

func expandArgs(args []string, text, outPath string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		a = strings.ReplaceAll(a, "{text}", text)
		a = strings.ReplaceAll(a, "{out}", outPath)
		out[i] = a
	}
	return out
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 512 {
		return s
	}
	return s[:512] + "..."
}

func (p *LocalCommandProvider) Ping() error {
	bin := p.bin
	if !filepath.IsAbs(bin) && !strings.ContainsRune(bin, filepath.Separator) {
		resolved, err := exec.LookPath(bin)
		if err != nil {
			return fmt.Errorf("tts local: %w", err)
		}
		bin = resolved
	}
	fi, err := os.Stat(bin)
	if err != nil {
		return fmt.Errorf("tts local: %w", err)
	}
	if fi.IsDir() {
		return fmt.Errorf("tts local: not a file: %s", bin)
	}
	if runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 && !strings.HasSuffix(strings.ToLower(bin), ".sh") {
		return fmt.Errorf("tts local: not executable: %s", bin)
	}
	return nil
}
