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

func TestEnsureHomeSiteIDDeterministic(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("HOME_SITE_ID", "")
	a, err := EnsureHomeSiteID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := EnsureHomeSiteID()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("two calls: %q vs %q", a, b)
	}
	if len(a) != 36 {
		t.Fatalf("expected UUID, got %q", a)
	}
}

func TestEnsureHomeSiteIDDifferentHOME(t *testing.T) {
	t.Setenv("HOME_SITE_ID", "")
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	t.Setenv("HOME", dir1)
	id1, err := EnsureHomeSiteID()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir2)
	id2, err := EnsureHomeSiteID()
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatalf("different HOME should yield different home_site_id: %q", id1)
	}
}

func TestEnsureHomeSiteIDPrefersEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("HOME_SITE_ID", "from-env")
	got, err := EnsureHomeSiteID()
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-env" {
		t.Fatalf("EnsureHomeSiteID() = %q", got)
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
