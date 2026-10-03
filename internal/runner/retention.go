package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
)

const windowMetadataOption = "@remote-runner-window"

// Retention metadata identifies UI resources only. It deliberately contains no
// job IDs, result paths, execution state, or recoverable execution descriptors.
type windowMetadata struct {
	Version int    `json:"version"`
	Key     string `json:"key"`
	Window  string `json:"window"`
	Pane    string `json:"pane"`
	Used    int64  `json:"used"`
}

func (t terminal) output(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return t.command(ctx, args...).CombinedOutput()
}

// The parent writes and confirms metadata before releasing the launch gate.
// The waiting shell never tags itself or releases itself after a parent crash.
func (t terminal) tagWindow(key protocol.Source, window *jobWindow) error {
	data, _ := json.Marshal(key)
	metadata, _ := json.Marshal(windowMetadata{Version: 1, Key: base64.RawURLEncoding.EncodeToString(data), Window: window.id, Pane: window.pane, Used: window.used.UnixNano()})
	out, err := t.output("set-option", "-w", "-t", window.pane, "remain-on-exit", "on", ";", "set-option", "-w", "-t", window.pane, windowMetadataOption, string(metadata))
	if err != nil {
		return fmt.Errorf("tag job window: %w: %s", err, out)
	}
	out, err = t.output("display-message", "-p", "-t", window.pane, "#{window_id}\t#{pane_id}\t#{"+windowMetadataOption+"}")
	if err != nil || strings.TrimSpace(string(out)) != window.id+"\t"+window.pane+"\t"+string(metadata) {
		return fmt.Errorf("could not confirm job window metadata: %v: %s", err, out)
	}
	return nil
}

func parseWindowMetadata(raw, id string) (protocol.Source, *jobWindow, error) {
	var metadata windowMetadata
	var key protocol.Source
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return key, nil, fmt.Errorf("invalid retention metadata in window %s: %w", id, err)
	}
	data, err := base64.RawURLEncoding.DecodeString(metadata.Key)
	if err != nil {
		return key, nil, fmt.Errorf("invalid retention key in window %s", id)
	}
	if err = json.Unmarshal(data, &key); err != nil || metadata.Version != 1 || metadata.Window != id || len(metadata.Pane) < 2 || metadata.Pane[0] != '%' || strings.Trim(metadata.Pane[1:], "0123456789") != "" || key.Remote == "" || key.Branch == "" || metadata.Used <= 0 {
		return key, nil, fmt.Errorf("invalid retention identity in window %s; inspect its metadata manually", id)
	}
	return key, &jobWindow{id: id, pane: metadata.Pane, used: time.Unix(0, metadata.Used)}, nil
}

// Only a tagged, exact, dead helper pane can be retained. Extra inspection panes
// are never killed by respawn/eviction; the operator must close them first.
func (t terminal) completedWindow(window *jobWindow) error {
	out, err := t.output("list-panes", "-t", window.id, "-F", "#{pane_id}\t#{pane_dead}")
	if err != nil {
		return fmt.Errorf("inspect retained window %s: %w: %s", window.id, err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	found := false
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) == 2 && fields[0] == window.pane {
			found = true
			if fields[1] != "1" {
				return fmt.Errorf("job pane %s in window %s is still live: Ctrl-C the old job and wait for it to exit before restarting the runner", window.pane, window.id)
			}
		}
	}
	if !found {
		return fmt.Errorf("retained helper pane %s missing from window %s; inspect manually", window.pane, window.id)
	}
	if len(lines) != 1 {
		return fmt.Errorf("retained window %s contains extra inspection/user panes; close those panes manually before reuse or restart", window.id)
	}
	return nil
}

func (t terminal) retainedWindows() (map[protocol.Source]*jobWindow, error) {
	out, err := t.output("list-windows", "-t", t.session, "-F", "#{window_id}\t#{"+windowMetadataOption+"}")
	if err != nil {
		return nil, fmt.Errorf("inspect retained job windows: %w: %s", err, out)
	}
	windows := make(map[protocol.Source]*jobWindow)
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		id, raw, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("invalid retained window listing")
		}
		if id == t.window {
			continue
		}
		if raw == "" {
			return nil, fmt.Errorf("untagged window %s in dedicated runner session: stop any pending launch and close this window manually before restarting", id)
		}
		key, window, err := parseWindowMetadata(raw, id)
		if err != nil {
			return nil, err
		}
		if err = t.completedWindow(window); err != nil {
			return nil, err
		}
		if windows[key] != nil {
			return nil, fmt.Errorf("duplicate retained worktree windows; inspect manually before restarting")
		}
		windows[key] = window
	}
	return windows, nil
}

// Apply the current configuration before registration, including a lowered
// budget after restart. Delete accounting only for positively confirmed eviction.
func (t terminal) trimRetainedWindows(windows map[protocol.Source]*jobWindow, limit int) error {
	for len(windows) > limit {
		var source protocol.Source
		var oldest *jobWindow
		for key, candidate := range windows {
			if !candidate.active && candidate.id != t.window && (oldest == nil || candidate.used.Before(oldest.used)) {
				source, oldest = key, candidate
			}
		}
		if oldest == nil {
			return fmt.Errorf("cannot enforce max_windows=%d; stop old jobs and clean up retained windows manually", limit)
		}
		if err := t.evictCompletedWindow(source, oldest); err != nil {
			return fmt.Errorf("cannot enforce max_windows=%d; clean up retained windows manually: %w", limit, err)
		}
		delete(windows, source)
	}
	return nil
}

// Evaluate the destructive preconditions inside tmux's command queue, not in a
// stale client-side snapshot. Never target a whole window: the stable dead helper
// pane is the only resource we own. A refused/uncertain eviction keeps accounting.
func (t terminal) evictCompletedWindow(key protocol.Source, window *jobWindow) error {
	if window.active || window.id == t.window {
		return fmt.Errorf("refusing to evict active or runner window %s", window.id)
	}
	if err := t.verifyCompletedWindow(key, window); err != nil {
		return err
	}
	guard := "#{&&:#{pane_dead},#{&&:#{==:#{window_id}," + window.id + "},#{&&:#{==:#{session_id}," + t.session + "},#{==:#{window_panes},1}}}}"
	out, err := t.output("if-shell", "-F", "-t", window.pane, guard,
		"kill-pane -t "+window.pane+" ; display-message -p remote-runner-evicted",
		"display-message -p remote-runner-retained")
	if err != nil || strings.TrimSpace(string(out)) != "remote-runner-evicted" {
		return fmt.Errorf("cannot safely evict retained window %s; inspect helper and inspection/user panes manually: %v: %s", window.id, err, out)
	}
	return nil
}

func (t terminal) verifyCompletedWindow(key protocol.Source, window *jobWindow) error {
	out, err := t.output("display-message", "-p", "-t", window.id, "#{"+windowMetadataOption+"}")
	if err != nil {
		return fmt.Errorf("verify retained window: %w: %s", err, out)
	}
	actual, tagged, err := parseWindowMetadata(strings.TrimSpace(string(out)), window.id)
	if err != nil {
		return err
	}
	if actual != key || tagged.pane != window.pane || window.id == t.window {
		return fmt.Errorf("retained window identity changed; inspect manually")
	}
	return t.completedWindow(window)
}
