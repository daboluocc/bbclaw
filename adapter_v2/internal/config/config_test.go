package config

import (
	"reflect"
	"testing"
)

// TestLoadFromEnv covers the three knobs v2 exposes: listen addr, default CLI
// argv and cwd — both their env overrides and their built-in defaults.
func TestLoadFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantAddr string
		wantArgv []string
		wantCwd  string
	}{
		{
			name:     "all defaults when unset",
			env:      nil,
			wantAddr: DefaultAddr,
			wantArgv: []string{"claude"},
			wantCwd:  "",
		},
		{
			name:     "addr override",
			env:      map[string]string{"ADAPTER_V2_ADDR": ":9000"},
			wantAddr: ":9000",
			wantArgv: []string{"claude"},
			wantCwd:  "",
		},
		{
			name:     "blank addr falls back to default",
			env:      map[string]string{"ADAPTER_V2_ADDR": "   "},
			wantAddr: DefaultAddr,
			wantArgv: []string{"claude"},
			wantCwd:  "",
		},
		{
			name:     "multi-word cli is split into argv",
			env:      map[string]string{"ADAPTER_V2_CLI": "codex --model gpt"},
			wantAddr: DefaultAddr,
			wantArgv: []string{"codex", "--model", "gpt"},
			wantCwd:  "",
		},
		{
			name:     "blank cli falls back to default argv",
			env:      map[string]string{"ADAPTER_V2_CLI": "   "},
			wantAddr: DefaultAddr,
			wantArgv: []string{"claude"},
			wantCwd:  "",
		},
		{
			name:     "cwd is trimmed",
			env:      map[string]string{"ADAPTER_V2_CWD": "  /tmp/proj  "},
			wantAddr: DefaultAddr,
			wantArgv: []string{"claude"},
			wantCwd:  "/tmp/proj",
		},
		{
			name: "all three overridden together",
			env: map[string]string{
				"ADAPTER_V2_ADDR": "127.0.0.1:1234",
				"ADAPTER_V2_CLI":  "opencode",
				"ADAPTER_V2_CWD":  "/home/me/code",
			},
			wantAddr: "127.0.0.1:1234",
			wantArgv: []string{"opencode"},
			wantCwd:  "/home/me/code",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv isolates and auto-restores each var per subtest.
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			got := LoadFromEnv()
			if got.Addr != tt.wantAddr {
				t.Errorf("Addr = %q, want %q", got.Addr, tt.wantAddr)
			}
			if !reflect.DeepEqual(got.Argv, tt.wantArgv) {
				t.Errorf("Argv = %v, want %v", got.Argv, tt.wantArgv)
			}
			if got.Cwd != tt.wantCwd {
				t.Errorf("Cwd = %q, want %q", got.Cwd, tt.wantCwd)
			}
		})
	}
}

// TestParseArgvReturnsFreshCopy guards the package-default argv against mutation
// by a caller: each call must hand back an independent slice.
func TestParseArgvReturnsFreshCopy(t *testing.T) {
	a := parseArgv("")
	b := parseArgv("")
	a[0] = "tampered"
	if b[0] != "claude" {
		t.Fatalf("parseArgv shares backing array: b[0] = %q, want %q", b[0], "claude")
	}
}
