package tts

import (
	"context"
	"encoding/base64"
)

type MockProvider struct{}

func NewMockProvider() *MockProvider {
	return &MockProvider{}
}

func (p *MockProvider) Synthesize(_ context.Context, text string) ([]byte, error) {
	// A tiny deterministic byte sequence that looks like "mock mp3 payload".
	return []byte(base64.StdEncoding.EncodeToString([]byte("mock-tts:" + text))), nil
}
