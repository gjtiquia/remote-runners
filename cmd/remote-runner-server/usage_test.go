package main_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func runServerCommand(t *testing.T, binary string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	}
	t.Fatal(err)
	return "", -1
}

func TestInvalidServerOptionsStopClearlyBeforeCreatingOutputOrListening(t *testing.T) {
	binary := buildServer(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, args := range [][]string{
		{"-p", "0", "unexpected"}, {"-p", "-1"}, {"-p", "65536"}, {"-p", "no"},
		{"-p", "0", "-heartbeat-interval", "0s"}, {"-heartbeat-interval", "-1s"},
		{"-p", "0", "-heartbeat-misses", "0"}, {"-heartbeat-misses", "-1"}, {"-output-dir", ""},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			out, code := runServerCommand(t, binary, args...)
			if code != 2 || out == "" || strings.Contains(out, "panic") || strings.Contains(out, "listening on") {
				t.Fatalf("bad startup: exit=%d output=%q", code, out)
			}
		})
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid options created output: %+v", entries)
	}
}
