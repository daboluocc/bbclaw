package asr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenAICompatibleProviderSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key" {
			t.Fatalf("authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"text":"hello"}`))
	}))
	defer ts.Close()

	p := NewOpenAICompatibleProvider(ts.URL, "key", "model", ts.Client())
	res, err := p.Transcribe(context.Background(), []byte("abc"), Metadata{})
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if res.Text != "hello" {
		t.Fatalf("Text = %q", res.Text)
	}
}

func TestOpenAICompatibleProviderRateLimited(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer ts.Close()

	p := NewOpenAICompatibleProvider(ts.URL, "key", "model", ts.Client())
	_, err := p.Transcribe(context.Background(), []byte("abc"), Metadata{})
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if apiErr.Code != "ASR_RATE_LIMITED" {
		t.Fatalf("Code = %q", apiErr.Code)
	}
}

func TestOpenAICompatibleProviderPing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key" {
			t.Fatalf("authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	p := NewOpenAICompatibleProvider(ts.URL, "key", "model", ts.Client())
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() = %v", err)
	}
}

func TestOpenAICompatibleProviderTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"text":"late"}`))
	}))
	defer ts.Close()

	httpClient := ts.Client()
	httpClient.Timeout = 50 * time.Millisecond
	p := NewOpenAICompatibleProvider(ts.URL, "key", "model", httpClient)
	_, err := p.Transcribe(context.Background(), []byte("abc"), Metadata{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
