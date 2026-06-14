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
		"",
		DoubaoOptions{EnableDDC: true, Hotwords: []string{"bbclaw"}},
	)
	packet, err := p.buildInitialPacket(Metadata{Hotwords: []string{"Anthropic"}})
	if err != nil {
		t.Fatalf("buildInitialPacket() error = %v", err)
	}
	if len(packet) <= 8 {
		t.Fatalf("packet too short: %d", len(packet))
	}
}

func TestBuildCorpusInlineHotwords(t *testing.T) {
	p := NewDoubaoNativeProvider("", "", "", "", "bigmodel", "",
		DoubaoOptions{Hotwords: []string{"bbclaw", " bbclaw ", ""}, BoostingTable: "tbl"})

	// Static + per-request hotwords merge and de-duplicate.
	corpus := p.buildCorpus(Metadata{Hotwords: []string{"Anthropic", "bbclaw"}})
	if corpus == nil {
		t.Fatal("expected non-nil corpus")
	}
	if corpus["boosting_table_name"] != "tbl" {
		t.Fatalf("boosting_table_name = %v", corpus["boosting_table_name"])
	}
	ctxStr, ok := corpus["context"].(string)
	if !ok {
		t.Fatalf("context not a string: %T", corpus["context"])
	}
	var decoded struct {
		Hotwords []struct {
			Word string `json:"word"`
		} `json:"hotwords"`
	}
	if err := json.Unmarshal([]byte(ctxStr), &decoded); err != nil {
		t.Fatalf("context is not valid JSON: %v", err)
	}
	if len(decoded.Hotwords) != 2 {
		t.Fatalf("expected 2 deduped hotwords, got %d (%s)", len(decoded.Hotwords), ctxStr)
	}

	// No hotwords and no tables → nil (don't send an empty corpus).
	bare := NewDoubaoNativeProvider("", "", "", "", "bigmodel", "", DoubaoOptions{})
	if got := bare.buildCorpus(Metadata{}); got != nil {
		t.Fatalf("expected nil corpus, got %v", got)
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
