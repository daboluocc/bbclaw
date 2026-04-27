package asr

import (
	"encoding/json"
	"testing"
)

func TestGenerateHeader(t *testing.T) {
	h := generateHeader(clientAudioRequest, negSequence, 0x0)
	if len(h) != 4 {
		t.Fatalf("header len = %d", len(h))
	}
	if h[1]>>4 != clientAudioRequest {
		t.Fatalf("messageType = %d", h[1]>>4)
	}
	if h[1]&0x0f != negSequence {
		t.Fatalf("flags = %d", h[1]&0x0f)
	}
}

func TestBuildAudioPacket(t *testing.T) {
	packet, err := buildAudioPacket([]byte("abc"), true)
	if err != nil {
		t.Fatalf("buildAudioPacket() error = %v", err)
	}
	if len(packet) <= 8 {
		t.Fatalf("packet too short: %d", len(packet))
	}
}

func TestBuildInitialPacket(t *testing.T) {
	p := NewDoubaoNativeProvider(
		"wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_nostream",
		"appid",
		"token",
		"volc.bigasr.sauc.duration",
		"bigmodel",
		"zh-CN",
	)
	packet, err := p.buildInitialPacket()
	if err != nil {
		t.Fatalf("buildInitialPacket() error = %v", err)
	}
	if len(packet) <= 8 {
		t.Fatalf("packet too short: %d", len(packet))
	}
}

func TestParseDoubaoResponseInvalid(t *testing.T) {
	_, _, err := parseDoubaoResponse([]byte{0x11, 0x90})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestJSONMarshallingShapeForResult(t *testing.T) {
	result := Result{Text: "ok"}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if string(raw) == "" {
		t.Fatal("empty json")
	}
}
