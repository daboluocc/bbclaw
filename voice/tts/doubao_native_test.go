package tts

import "testing"

func TestBuildRequestPacket(t *testing.T) {
	packet, err := buildRequestPacket(map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("buildRequestPacket() error = %v", err)
	}
	if len(packet) <= 8 {
		t.Fatalf("packet too short: %d", len(packet))
	}
}

func TestParseTTSResponseInvalid(t *testing.T) {
	_, _, err := parseTTSResponse([]byte{0x11, 0xb1, 0x11})
	if err == nil {
		t.Fatal("expected error")
	}
}
