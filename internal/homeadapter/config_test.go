package homeadapter

import (
	"os"
	"path/filepath"
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

func TestValidateRejectsLegacyHomeMain(t *testing.T) {
	cfg := Config{
		CloudWSURL:        "https://cloud.example.com",
		CloudAuthToken:    "token",
		HomeSiteID:        "home-main",
		ReconnectDelay:    time.Second,
		HTTPTimeout:       time.Second,
		OpenClawURL:       "ws://127.0.0.1:18789",
		OpenClawNodeID:    "bbclaw-home-adapter",
		OpenClawReplyWait: time.Second,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() expected error for legacy home-main, got nil")
	}
}

func TestReadHomeSiteIDFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".bbclaw"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".bbclaw", "home_site_id")
	if err := os.WriteFile(path, []byte(" 9646c421-e179-497d-a237-384e0d226e97 \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readHomeSiteIDFile(); got != "9646c421-e179-497d-a237-384e0d226e97" {
		t.Fatalf("readHomeSiteIDFile() = %q", got)
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
