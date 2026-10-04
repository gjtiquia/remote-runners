package runner_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExistingPluralConfigWinsWithoutChangingLegacyConfig(t *testing.T) {
	bin := binary(t)
	home := t.TempDir()
	old := filepath.Join(home, ".remote-runner", "config.json")
	current := filepath.Join(home, ".remote-runners", "config.json")
	legacy := []byte(`{"coordinator_url":"legacy-invalid-url","name":"legacy"}`)
	selected := []byte(`{"name":"current"}`)
	for path, data := range map[string][]byte{old: legacy, current: selected} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := exec.Command(bin)
	c.Env = append(os.Environ(), "HOME="+home, "TMUX=", "TMUX_PANE=")
	out, err := c.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "coordinator_url and name are required") {
		t.Fatalf("plural config did not win: %v %s", err, out)
	}
	for path, want := range map[string][]byte{old: legacy, current: selected} {
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, want) {
			t.Fatalf("existing config changed: %s %v %q", path, err, data)
		}
	}
}

func TestLegacySymlinkRequiresManualMoveWithoutCreatingStarter(t *testing.T) {
	bin := binary(t)
	home := t.TempDir()
	old := filepath.Join(home, ".remote-runner", "config.json")
	current := filepath.Join(home, ".remote-runners", "config.json")
	target := filepath.Join(home, "actual.json")
	data := []byte(`{"name":"legacy"}`)
	if err := os.WriteFile(target, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(old), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../actual.json", old); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(bin)
	c.Env = append(os.Environ(), "HOME="+home, "TMUX=", "TMUX_PANE=")
	out, err := c.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "move its contents") {
		t.Fatalf("symlink migration must be explicit: %v %s", err, out)
	}
	if _, err := os.Lstat(current); !os.IsNotExist(err) {
		t.Fatalf("unexpected new config: %v", err)
	}
	preserved, err := os.ReadFile(old)
	if err != nil || !bytes.Equal(preserved, data) {
		t.Fatalf("legacy symlink/target changed: %v %q", err, preserved)
	}
}

func TestRunnerMovesLegacyConfigWithoutChangingItsContents(t *testing.T) {
	bin := binary(t)
	home := t.TempDir()
	old := filepath.Join(home, ".remote-runner", "config.json")
	current := filepath.Join(home, ".remote-runners", "config.json")
	data := []byte("{\n  \"coordinator_url\": \"legacy-invalid-url\", \"name\": \"legacy\"\n}\n")
	if err := os.MkdirAll(filepath.Dir(old), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, data, 0600); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(bin)
	c.Env = append(os.Environ(), "HOME="+home, "TMUX=", "TMUX_PANE=")
	out, err := c.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "coordinator_url must be an absolute") {
		t.Fatalf("legacy config was not loaded: %v %s", err, out)
	}
	moved, err := os.ReadFile(current)
	if err != nil || !bytes.Equal(moved, data) {
		t.Fatalf("config contents not preserved: %v %q", err, moved)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("legacy file was not moved: %v", err)
	}
}
