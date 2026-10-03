package main_test

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestServerHelpDocumentsUsageDefaultsStateLossAndExitCodes(t *testing.T) {
	binary := buildServer(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	out, code := runServerCommand(t, binary, "-h")
	if code != 0 {
		t.Fatalf("help: exit=%d output=%q", code, out)
	}
	for _, text := range []string{"usage:", "2461", filepath.Join(home, ".remote-runners", "output"), "10s", "3", "network-wide", "loopback-only", "SIGINT", "SIGTERM", "restart forgets", "Exit codes:"} {
		if !strings.Contains(out, text) {
			t.Fatalf("help missing %q: %s", text, out)
		}
	}
}
