package memory

import "testing"

func TestEnabledDefaultsOn(t *testing.T) {
	// Since 63e80fc the long-term-memory pipeline defaults ON when env unset so
	// users get it out of the box; only an explicit falsy value disables it.
	t.Setenv(envEnable, "")
	if !Enabled() {
		t.Error("memory pipeline must default ON when env unset")
	}
}

func TestEnabledParsesTruthyValues(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv(envEnable, v)
		if !Enabled() {
			t.Errorf("Enabled() = false for %q, want true", v)
		}
	}
	for _, v := range []string{"0", "false", "no", "off", "garbage"} {
		t.Setenv(envEnable, v)
		if Enabled() {
			t.Errorf("Enabled() = true for %q, want false", v)
		}
	}
}

func TestNewFromEnvReturnsNilWhenDisabled(t *testing.T) {
	t.Setenv(envEnable, "0") // explicit disable (default is ON since 63e80fc)
	w, on := NewFromEnv("/tmp/CLAUDE.md", "claude", nil)
	if on || w != nil {
		t.Errorf("NewFromEnv when disabled = (%v, %v), want (nil, false)", w, on)
	}
}

func TestParseItemsExtractsArray(t *testing.T) {
	raw := "Sure, here you go:\n```json\n[{\"category\":\"preference\",\"text\":\"简短\"},{\"category\":\"\",\"text\":\"\"}]\n```\nDone."
	items, err := parseItems(raw)
	if err != nil {
		t.Fatalf("parseItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 (empty text dropped): %+v", len(items), items)
	}
	if items[0].Category != "preference" || items[0].Text != "简短" {
		t.Errorf("bad item: %+v", items[0])
	}
}

func TestParseItemsNoArrayIsEmpty(t *testing.T) {
	items, err := parseItems("no json here")
	if err != nil {
		t.Fatalf("parseItems: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("got %d items, want 0", len(items))
	}
}
