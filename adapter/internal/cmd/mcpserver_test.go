package cmd

import (
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/config"
)

func TestCwdPoolToProjects(t *testing.T) {
	if got := cwdPoolToProjects(nil); got != nil {
		t.Fatalf("empty pool should yield nil, got %v", got)
	}
	pool := []config.CwdEntry{
		{Name: "alpha", Path: "/p/alpha"},
		{Name: "beta", Path: "/p/beta"},
	}
	got := cwdPoolToProjects(pool)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0].Name != "alpha" || got[0].Cwd != "/p/alpha" {
		t.Errorf("entry 0 = %+v", got[0])
	}
	if got[1].Name != "beta" || got[1].Cwd != "/p/beta" {
		t.Errorf("entry 1 = %+v", got[1])
	}
}

func TestParseArgList(t *testing.T) {
	if got := parseArgList(""); got != nil {
		t.Errorf("empty => %v", got)
	}
	if got := parseArgList("  "); got != nil {
		t.Errorf("blank => %v", got)
	}
	got := parseArgList("--model, claude-sonnet-4-6 ,")
	if len(got) != 2 || got[0] != "--model" || got[1] != "claude-sonnet-4-6" {
		t.Errorf("got %v", got)
	}
}

func TestMcpServerCmdWiring(t *testing.T) {
	root := NewRootCmd()
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "mcp-server" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("mcp-server subcommand not registered on root")
	}
}
