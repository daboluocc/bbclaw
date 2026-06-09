package asr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestOpenAICompatibleProviderSendsHotwordPrompt(t *testing.T) {
	var gotPrompt string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		gotPrompt = r.FormValue("prompt")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"text":"切到 bbclaw"}`))
	}))
	defer ts.Close()

	p := NewOpenAICompatibleProvider(ts.URL, "key", "model", ts.Client())
	_, err := p.Transcribe(context.Background(), []byte("abc"), Metadata{
		Hotwords: []string{"bbclaw", "bbclaw-reference"},
	})
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if gotPrompt == "" || !strings.Contains(gotPrompt, "bbclaw") {
		t.Fatalf("prompt field = %q, want it to carry the hotwords", gotPrompt)
	}
}

func TestHotwordPrompt(t *testing.T) {
	if got := hotwordPrompt(nil); got != "" {
		t.Errorf("empty hotwords should yield empty prompt, got %q", got)
	}
	got := hotwordPrompt([]string{" a ", "a", "b"}) // trims + dedupes
	if got == "" || !strings.Contains(got, "a") || !strings.Contains(got, "b") {
		t.Errorf("prompt = %q, want a and b present", got)
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
