package homeadapter

import (
	"testing"
	"time"
)

func TestResolveCloudDialURL(t *testing.T) {
	got, err := resolveCloudDialURL("http://daboluo.cc:38081", "home-1", "secret")
	if err != nil {
		t.Fatalf("resolveCloudDialURL() error = %v", err)
	}
	want := "ws://daboluo.cc:38081/ws?home_site_id=home-1&role=home_adapter&token=secret"
	if got != want {
		t.Fatalf("resolveCloudDialURL() = %q, want %q", got, want)
	}
}

func TestValidateRequiresFields(t *testing.T) {
	cfg := Config{}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() expected error, got nil")
	}
}

func TestValidateDoesNotRequireLocalAdapter(t *testing.T) {
	cfg := Config{
		CloudWSURL:        "https://cloud.example.com",
		CloudAuthToken:    "token",
		HomeSiteID:        "home-1",
		ReconnectDelay:    time.Second,
		HTTPTimeout:       time.Second,
		OpenClawURL:       "ws://127.0.0.1:18789",
		OpenClawNodeID:    "bbclaw-home-adapter",
		OpenClawReplyWait: time.Second,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}
