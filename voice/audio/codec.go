package audio

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func DecodeToPCM16LE(ctx context.Context, codec string, sampleRate int, channels int, payload []byte) ([]byte, error) {
	switch normalizeCodec(codec) {
	case "pcm16", "pcm_s16le":
		return payload, nil
	case "opus":
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
	var lastErr error
	for _, inputFormat := range []string{"opus", "ogg", ""} {
		out, err := decodeWithFFmpeg(ctx, inputFormat, sampleRate, channels, payload)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("ffmpeg opus decode failed: %w", lastErr)
}

func DecodeMediaToPCM16LE(ctx context.Context, inputFormat string, sampleRate int, channels int, payload []byte) ([]byte, error) {
	if sampleRate <= 0 || channels <= 0 {
		return nil, fmt.Errorf("invalid sampleRate/channels")
	}
	fmtName := strings.TrimSpace(strings.ToLower(inputFormat))
	if fmtName == "" {
		fmtName = "mp3"
	}
	out, err := decodeWithFFmpeg(ctx, fmtName, sampleRate, channels, payload)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg media decode failed: %w", err)
	}
	return out, nil
}

func decodeWithFFmpeg(ctx context.Context, inputFormat string, sampleRate int, channels int, payload []byte) ([]byte, error) {
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
	}
	if strings.TrimSpace(inputFormat) != "" {
		args = append(args, "-f", strings.TrimSpace(inputFormat))
	}
	args = append(args,
		"-i", "pipe:0",
		"-f", "s16le",
		"-ar", fmt.Sprintf("%d", sampleRate),
		"-ac", fmt.Sprintf("%d", channels),
		"pipe:1",
	)
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
		return nil, fmt.Errorf("%s", msg)
	}
	return out.Bytes(), nil
}
