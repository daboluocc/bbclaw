package audio

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

var bbclawPCMEnvelope = []byte("BBPCM16\n")

func DecodeToPCM16LE(ctx context.Context, codec string, sampleRate int, channels int, payload []byte) ([]byte, error) {
	switch normalizeCodec(codec) {
	case "pcm16", "pcm_s16le":
		return payload, nil
	case "opus":
		if bytes.HasPrefix(payload, bbclawPCMEnvelope) {
			return payload[len(bbclawPCMEnvelope):], nil
		}
		return decodeOpusWithFFmpeg(ctx, sampleRate, channels, payload)
	default:
		return nil, fmt.Errorf("unsupported codec: %s", codec)
	}
}

func normalizeCodec(codec string) string {
	return strings.ToLower(strings.TrimSpace(codec))
}

func decodeOpusWithFFmpeg(ctx context.Context, sampleRate int, channels int, payload []byte) ([]byte, error) {
	if sampleRate <= 0 || channels <= 0 {
		return nil, fmt.Errorf("invalid sampleRate/channels")
	}
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-f", "opus",
		"-ar", fmt.Sprintf("%d", sampleRate),
		"-ac", fmt.Sprintf("%d", channels),
		"-i", "pipe:0",
		"-f", "s16le",
		"-ar", fmt.Sprintf("%d", sampleRate),
		"-ac", fmt.Sprintf("%d", channels),
		"pipe:1",
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdin = bytes.NewReader(payload)

	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ffmpeg opus decode failed: %s", msg)
	}
	return out.Bytes(), nil
}

func DecodeMediaToPCM16LE(ctx context.Context, inputFormat string, sampleRate int, channels int, payload []byte) ([]byte, error) {
	if sampleRate <= 0 || channels <= 0 {
		return nil, fmt.Errorf("invalid sampleRate/channels")
	}
	fmtName := strings.TrimSpace(strings.ToLower(inputFormat))
	if fmtName == "" {
		fmtName = "mp3"
	}
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-f", fmtName,
		"-i", "pipe:0",
		"-f", "s16le",
		"-ar", fmt.Sprintf("%d", sampleRate),
		"-ac", fmt.Sprintf("%d", channels),
		"pipe:1",
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdin = bytes.NewReader(payload)

	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ffmpeg media decode failed: %s", msg)
	}
	return out.Bytes(), nil
}
