package main

import "testing"

func TestAdminURLFromAddr(t *testing.T) {
	cases := map[string]string{
		":18080":           "http://127.0.0.1:18080/admin",
		"0.0.0.0:18080":    "http://127.0.0.1:18080/admin",
		"127.0.0.1:9000":   "http://127.0.0.1:9000/admin",
		"192.168.1.5:8080": "http://192.168.1.5:8080/admin",
	}
	for addr, want := range cases {
		if got := adminURLFromAddr(addr); got != want {
			t.Errorf("adminURLFromAddr(%q) = %q, want %q", addr, got, want)
		}
	}
}

func TestOpenAdminEnabled(t *testing.T) {
	for _, v := range []string{"0", "false", "OFF", "no"} {
		t.Setenv("BBCLAW_OPEN_ADMIN", v)
		if openAdminEnabled() {
			t.Errorf("BBCLAW_OPEN_ADMIN=%q should disable auto-open", v)
		}
	}
	t.Setenv("BBCLAW_OPEN_ADMIN", "")
	if !openAdminEnabled() {
		t.Error("auto-open should default on when unset")
	}
}
