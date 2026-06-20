package audio

import "testing"

func TestManagerHappyPath(t *testing.T) {
	m := NewManager(1024, 10, 1)
	err := m.Start(StartRequest{
		DeviceID:   "d1",
		SessionKey: "s1",
		StreamID:   "st1",
		Codec:      "pcm16",
		SampleRate: 16000,
		Channels:   1,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := m.AppendChunk("st1", Chunk{Seq: 1, TimestampMs: 1000, Payload: []byte("ab")}); err != nil {
		t.Fatalf("AppendChunk 1 error = %v", err)
	}
	if err := m.AppendChunk("st1", Chunk{Seq: 2, TimestampMs: 1200, Payload: []byte("cd")}); err != nil {
		t.Fatalf("AppendChunk 2 error = %v", err)
	}
	fin, err := m.Finish("st1")
	if err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	if string(fin.Audio) != "abcd" {
		t.Fatalf("audio = %q", string(fin.Audio))
	}
	if fin.DurationMs != 200 {
		t.Fatalf("duration = %d", fin.DurationMs)
	}
}

func TestManagerInvalidSeq(t *testing.T) {
	m := NewManager(1024, 10, 1)
	_ = m.Start(StartRequest{DeviceID: "d1", SessionKey: "s1", StreamID: "st1", Codec: "pcm16", SampleRate: 16000, Channels: 1})
	err := m.AppendChunk("st1", Chunk{Seq: 2, TimestampMs: 1000, Payload: []byte("x")})
	if err != ErrInvalidSequence {
		t.Fatalf("AppendChunk() error = %v", err)
	}
}

func TestManagerLimits(t *testing.T) {
	m := NewManager(3, 1, 1)
	_ = m.Start(StartRequest{DeviceID: "d1", SessionKey: "s1", StreamID: "st1", Codec: "pcm16", SampleRate: 16000, Channels: 1})
	if err := m.AppendChunk("st1", Chunk{Seq: 1, TimestampMs: 1000, Payload: []byte("ab")}); err != nil {
		t.Fatalf("AppendChunk() error = %v", err)
	}
	if err := m.AppendChunk("st1", Chunk{Seq: 2, TimestampMs: 1200, Payload: []byte("cd")}); err != ErrAudioTooLarge {
		t.Fatalf("expected ErrAudioTooLarge, got %v", err)
	}
}

func TestManagerConcurrencyLimit(t *testing.T) {
	m := NewManager(1024, 10, 1)
	if err := m.Start(StartRequest{DeviceID: "d1", SessionKey: "s1", StreamID: "st1", Codec: "pcm16", SampleRate: 16000, Channels: 1}); err != nil {
		t.Fatalf("Start st1 error = %v", err)
	}
	if err := m.Start(StartRequest{DeviceID: "d2", SessionKey: "s2", StreamID: "st2", Codec: "pcm16", SampleRate: 16000, Channels: 1}); err != ErrBusy {
		t.Fatalf("expected ErrBusy, got %v", err)
	}
}
