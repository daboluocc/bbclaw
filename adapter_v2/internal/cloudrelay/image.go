package cloudrelay

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/butler"
)

var imageIDSanitize = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// handleImageCapture ingests a device photo (ADR-049): decode the base64 JPEG,
// write it into the claude workspace, then run a normal agent turn whose prompt
// points claude at the image file. claude reads the image natively over its PTY;
// the reply streams back as voice.reply (same path as a voice turn) so the device
// can speak claude's description. The board sensor (GC0308) has no hardware JPEG,
// so the device software-encodes the frame before sending — adapter is format-blind.
func (r *Relay) handleImageCapture(ctx context.Context, write func(Envelope) error, env Envelope) error {
	deviceID := strings.TrimSpace(env.DeviceID)
	if deviceID == "" {
		deviceID = "cloud-anon"
	}
	dataB64, _ := env.Payload["dataBase64"].(string)
	note, _ := env.Payload["note"].(string)
	note = strings.TrimSpace(note)

	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(dataB64))
	if err != nil || len(raw) == 0 {
		r.replyImageError(write, env, "IMAGE_DECODE_FAILED", "base64 decode failed")
		return fmt.Errorf("image.capture decode device=%s: %w", deviceID, err)
	}
	// Optional declared-bytes sanity check (non-fatal — log a mismatch and proceed).
	if v, ok := env.Payload["bytes"].(float64); ok && int(v) != len(raw) {
		r.log("cloudrelay: image device=%s declared bytes=%d != decoded=%d", deviceID, int(v), len(raw))
	}

	path, err := saveInboxImage(deviceID, raw)
	if err != nil {
		r.replyImageError(write, env, "INTERNAL", err.Error())
		return fmt.Errorf("image.capture save device=%s: %w", deviceID, err)
	}
	r.log("cloudrelay: 📷 image device=%s saved=%s bytes=%d note=%q", deviceID, path, len(raw), note)

	if note == "" {
		note = "描述你在这张照片里看到的画面，简洁、口语化，一两句话。"
	}
	// Absolute path so claude (cwd = workspace) reads the exact file. The reply of
	// this turn (voice.reply) is the single reply to the image.capture messageId —
	// no separate ack, so the cloud/device sees the same voice-turn shape.
	prompt := fmt.Sprintf("我用摄像头拍了一张照片，保存在 %s 。请查看这张图片，然后回答：%s", path, note)
	committed := "📷 " + note
	return r.runTurn(ctx, write, env, prompt, committed)
}

// saveInboxImage writes the JPEG to <workspace>/inbox/<sanitized-device>.jpg,
// OVERWRITING any prior image for this device. Each photo triggers an immediate
// turn that reads it, so one rolling file per device is enough and never
// accumulates. Returns the absolute path claude should open.
func saveInboxImage(deviceID string, jpeg []byte) (string, error) {
	ws, err := butler.DefaultWorkspaceDir()
	if err != nil {
		return "", err
	}
	inbox := filepath.Join(ws, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		return "", err
	}
	name := imageIDSanitize.ReplaceAllString(deviceID, "_")
	if name == "" {
		name = "device"
	}
	path := filepath.Join(inbox, name+".jpg")
	if err := os.WriteFile(path, jpeg, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// replyImageError sends a typed error reply for a failed image.capture so the
// cloud's request completes (mirrors ADR-027's error-reply shape).
func (r *Relay) replyImageError(write func(Envelope) error, env Envelope, code, detail string) {
	_ = write(Envelope{
		Type: "error", MessageID: env.MessageID, DeviceID: env.DeviceID,
		HomeSiteID: r.cfg.HomeSiteID, Kind: "image.capture.ack",
		Error:   code,
		Payload: map[string]any{"error": map[string]any{"code": code, "detail": detail}},
	})
}
