package audio

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrStreamExists    = errors.New("stream already exists")
	ErrUnknownStream   = errors.New("stream not found")
	ErrInvalidSequence = errors.New("invalid chunk sequence")
	ErrAudioTooLarge   = errors.New("audio exceeds max bytes")
	ErrDurationTooLong = errors.New("audio exceeds max stream duration")
	ErrBusy            = errors.New("too many concurrent streams")
)

type StartRequest struct {
	DeviceID   string
	SessionKey string
	StreamID   string
	Codec      string
	SampleRate int
	Channels   int
}

type Chunk struct {
	Seq         int
	TimestampMs int64
	Payload     []byte
}

type FinishedStream struct {
	StartRequest
	Audio      []byte
	DurationMs int64
}

type Manager struct {
	mu                 sync.Mutex
	streams            map[string]*streamState
	maxAudioBytes      int
	maxStreamMs        int64
	maxConcurrent      int
	currentConcurrency int
}

type streamState struct {
	start          StartRequest
	buf            []byte
	nextSeq        int
	firstTimestamp int64
	lastTimestamp  int64
}

func NewManager(maxAudioBytes int, maxStreamSeconds int, maxConcurrent int) *Manager {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return &Manager{
		streams:       make(map[string]*streamState),
		maxAudioBytes: maxAudioBytes,
		maxStreamMs:   int64(maxStreamSeconds) * 1000,
		maxConcurrent: maxConcurrent,
	}
}

func (m *Manager) Start(req StartRequest) error {
	if req.StreamID == "" || req.DeviceID == "" || req.SessionKey == "" {
		return fmt.Errorf("deviceId/sessionKey/streamId are required")
	}
	if req.SampleRate <= 0 || req.Channels <= 0 {
		return fmt.Errorf("sampleRate/channels must be > 0")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.streams[req.StreamID]; ok {
		return ErrStreamExists
	}
	if m.currentConcurrency >= m.maxConcurrent {
		return ErrBusy
	}

	m.streams[req.StreamID] = &streamState{
		start:   req,
		nextSeq: 1,
	}
	m.currentConcurrency++
	return nil
}

func (m *Manager) AppendChunk(streamID string, chunk Chunk) error {
	if len(chunk.Payload) == 0 {
		return fmt.Errorf("audioBase64 must not be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.streams[streamID]
	if !ok {
		return ErrUnknownStream
	}
	if chunk.Seq != st.nextSeq {
		return ErrInvalidSequence
	}

	newSize := len(st.buf) + len(chunk.Payload)
	if newSize > m.maxAudioBytes {
		return ErrAudioTooLarge
	}

	if st.firstTimestamp == 0 {
		st.firstTimestamp = chunk.TimestampMs
	}
	st.lastTimestamp = chunk.TimestampMs
	if st.lastTimestamp-st.firstTimestamp > m.maxStreamMs {
		return ErrDurationTooLong
	}

	st.buf = append(st.buf, chunk.Payload...)
	st.nextSeq++
	return nil
}

func (m *Manager) Finish(streamID string) (FinishedStream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.streams[streamID]
	if !ok {
		return FinishedStream{}, ErrUnknownStream
	}
	delete(m.streams, streamID)
	if m.currentConcurrency > 0 {
		m.currentConcurrency--
	}

	out := FinishedStream{
		StartRequest: st.start,
		Audio:        append([]byte(nil), st.buf...),
		DurationMs:   st.lastTimestamp - st.firstTimestamp,
	}
	return out, nil
}
