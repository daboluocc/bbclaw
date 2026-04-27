package asr

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func lookOrSkip(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not in PATH: %v", name, err)
	}
	return p
}

func TestLocalCommandProviderStdout(t *testing.T) {
	p := NewLocalCommandProvider(lookOrSkip(t, "echo"), []string{"hello"}, "{wav}.txt")
	res, err := p.Transcribe(context.Background(), []byte{0x00, 0x00}, Metadata{SampleRate: 16000, Channels: 1})
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if res.Text != "hello" {
		t.Fatalf("Text = %q", res.Text)
	}
}

func TestLocalCommandProviderTranscriptFile(t *testing.T) {
	sh := lookOrSkip(t, "sh")
	dir := t.TempDir()
	script := filepath.Join(dir, "fake_asr.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'from file' > \"$1.txt\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := NewLocalCommandProvider(sh, []string{script, "{wav}"}, "{wav}.txt")
	res, err := p.Transcribe(context.Background(), []byte{0x00, 0x00}, Metadata{SampleRate: 16000, Channels: 1})
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if res.Text != "from file" {
		t.Fatalf("Text = %q", res.Text)
	}
}

func TestLocalConfigSummary(t *testing.T) {
	s := LocalConfigSummary("echo", []string{"-n", "hi"}, "{wav}.txt")
	if !strings.Contains(s, `bin="echo"`) || !strings.Contains(s, "text_path") {
		t.Fatalf("unexpected summary: %s", s)
	}
}

func TestLocalCommandProviderPing(t *testing.T) {
	p := NewLocalCommandProvider(lookOrSkip(t, "true"), nil, "")
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() = %v", err)
	}
}
