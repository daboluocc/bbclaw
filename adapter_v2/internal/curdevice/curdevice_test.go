package curdevice

import (
	"os"
	"path/filepath"
	"testing"
)

// useTempDataDir points DataDir() at a fresh temp dir for the test via the
// BBCLAW_ADAPTER_V2_DATA_DIR override settingsstore honors.
func useTempDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BBCLAW_ADAPTER_V2_DATA_DIR", dir)
	return dir
}

func TestRecordAndGet(t *testing.T) {
	dir := useTempDataDir(t)

	if got := Get(); got != "" {
		t.Fatalf("Get() on empty dir = %q, want \"\"", got)
	}
	if err := Record("BBClaw-0.4.1-C7EB89"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := Get(); got != "BBClaw-0.4.1-C7EB89" {
		t.Fatalf("Get() = %q, want the recorded id", got)
	}
	// The marker file lives in the data dir under the documented name.
	if _, err := os.Stat(filepath.Join(dir, fileName)); err != nil {
		t.Fatalf("marker file not written: %v", err)
	}
}

func TestRecordIgnoresEmpty(t *testing.T) {
	useTempDataDir(t)
	if err := Record("   "); err != nil {
		t.Fatalf("Record(blank): %v", err)
	}
	if got := Get(); got != "" {
		t.Fatalf("blank id was recorded: %q", got)
	}
}

func TestRecordOverwritesAndTrims(t *testing.T) {
	useTempDataDir(t)
	_ = Record("dev-A")
	if err := Record("  dev-B \n"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := Get(); got != "dev-B" {
		t.Fatalf("Get() = %q, want trimmed latest 'dev-B'", got)
	}
}
