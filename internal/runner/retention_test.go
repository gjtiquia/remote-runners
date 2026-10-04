package runner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func (f integration) tmux(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", append([]string{"-S", f.socket}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f integration) runnerPane(t *testing.T) string {
	t.Helper()
	return f.tmux(t, "display-message", "-p", "-t", f.session+":0", "#{pane_id}")
}

func (f integration) stopRunner(t *testing.T) {
	t.Helper()
	pane := f.runnerPane(t)
	f.tmux(t, "set-option", "-w", "-t", pane, "remain-on-exit", "on")
	f.tmux(t, "send-keys", "-t", pane, "C-c")
	f.waitDead(t, pane)
}

func (f integration) waitDead(t *testing.T, pane string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.tmux(t, "display-message", "-p", "-t", pane, "#{pane_dead}") != "1" {
		if time.Now().After(deadline) {
			t.Fatalf("pane %s did not exit", pane)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f integration) restartRunner(t *testing.T) {
	t.Helper()
	f.tmux(t, "respawn-pane", "-t", f.runnerPane(t))
	deadline := time.Now().Add(5 * time.Second)
	for {
		runners, err := f.client.Runners(context.Background())
		if err == nil && len(runners) == 1 && runners[0].Available {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner did not restart: %+v %v", runners, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSameSessionRestartRetainsEightWindowsAndReusesExistingWorktree(t *testing.T) {
	f := startRunnerWithWindows(t, 8)
	original := f.source
	var firstWindow string
	for i := 0; i < 8; i++ {
		if i > 0 {
			f.source = repository(t, "")
		}
		job := f.submit(t, "/bin/echo", "COMPLETED")
		result, out := f.wait(t, job.ID)
		if result.State != "succeeded" {
			t.Fatalf("job: %+v %s", result, out)
		}
		if i == 0 {
			firstWindow = f.window(t, job.ID)
		}
	}
	for i := 0; i < 2; i++ {
		f.stopRunner(t)
		// Wait for the coordinator to observe the intentional disconnect before restart.
		time.Sleep(400 * time.Millisecond)
		f.restartRunner(t)
		f.source = "file://" + original
		job := f.submit(t, "/bin/echo", "REUSED_AFTER_RESTART")
		result, out := f.wait(t, job.ID)
		if result.State != "succeeded" {
			t.Fatalf("reused job: %+v %s", result, out)
		}
		windows := f.tmux(t, "list-windows", "-t", f.session, "-F", "#{window_id}")
		if len(strings.Fields(windows)) != 9 {
			t.Fatalf("restart grew beyond eight retained windows: %s", windows)
		}
		f.waitForTerminal(t, firstWindow, "REUSED_AFTER_RESTART")
		f.source = repository(t, "")
		next := f.submit(t, "/bin/echo", "NEW_WORKTREE")
		f.wait(t, next.ID)
		windows = f.tmux(t, "list-windows", "-t", f.session, "-F", "#{window_id}")
		if len(strings.Fields(windows)) != 9 || !strings.Contains(windows, firstWindow+"\n") {
			t.Fatalf("retention/recent reuse lost after restart: %s", windows)
		}
	}
}

func (f integration) setMaxWindows(t *testing.T, limit int) {
	t.Helper()
	path := filepath.Join(filepath.Dir(f.socket), "home", ".remote-runners", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	config["max_windows"] = limit
	data, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRestartEnforcesReducedWindowBudgetBeforeRegistration(t *testing.T) {
	f := startRunnerWithWindows(t, 8)
	var retained []string
	for i := 0; i < 8; i++ {
		if i > 0 {
			f.source = repository(t, "")
		}
		job := f.submit(t, "/bin/echo", "COMPLETED")
		result, _ := f.wait(t, job.ID)
		if result.State != "succeeded" {
			t.Fatalf("prepare retained windows: %+v", result)
		}
		retained = append(retained, f.window(t, job.ID))
	}
	f.stopRunner(t)
	f.setMaxWindows(t, 2)
	time.Sleep(400 * time.Millisecond)
	f.restartRunner(t)
	ids := strings.Fields(f.tmux(t, "list-windows", "-t", f.session, "-F", "#{window_id}"))
	if len(ids) != 3 || ids[1] != retained[6] || ids[2] != retained[7] {
		t.Fatalf("restart must trim to runner plus two newest windows before registration: %v (retained %v)", ids, retained)
	}
	f.source = repository(t, "")
	job := f.submit(t, "/bin/echo", "NEW_BUDGET")
	result, _ := f.wait(t, job.ID)
	ids = strings.Fields(f.tmux(t, "list-windows", "-t", f.session, "-F", "#{window_id}"))
	if result.State != "succeeded" || len(ids) != 3 {
		t.Fatalf("new work exceeded reduced budget: %+v %v", result, ids)
	}
}

func TestReducedWindowBudgetRejectsProtectedInspectionPanes(t *testing.T) {
	f := startRunnerWithWindows(t, 3)
	var oldest string
	for i := 0; i < 3; i++ {
		if i > 0 {
			f.source = repository(t, "")
		}
		job := f.submit(t, "/bin/echo", "COMPLETED")
		f.wait(t, job.ID)
		if i == 0 {
			oldest = f.window(t, job.ID)
		}
	}
	inspection := f.tmux(t, "split-window", "-d", "-P", "-F", "#{pane_id}", "-t", oldest, "/bin/sh", "-c", "sleep 30")
	f.stopRunner(t)
	f.setMaxWindows(t, 1)
	time.Sleep(400 * time.Millisecond)
	f.tmux(t, "respawn-pane", "-t", f.runnerPane(t))
	f.waitDead(t, f.runnerPane(t))
	f.waitForTerminal(t, f.runnerPane(t), "close those panes manually")
	ids := strings.Fields(f.tmux(t, "list-windows", "-t", f.session, "-F", "#{window_id}"))
	if len(ids) != 4 || f.tmux(t, "display-message", "-p", "-t", inspection, "#{pane_dead}") != "0" {
		t.Fatalf("reduced budget destroyed protected windows: %v", ids)
	}
	runners, err := f.client.Runners(context.Background())
	if err != nil || (len(runners) > 0 && runners[0].Available) {
		t.Fatalf("over-budget startup registered: %+v %v", runners, err)
	}
	f.tmux(t, "kill-pane", "-t", inspection)
	f.restartRunner(t)
	ids = strings.Fields(f.tmux(t, "list-windows", "-t", f.session, "-F", "#{window_id}"))
	if len(ids) != 2 {
		t.Fatalf("operator cleanup did not enable reduced budget: %v", ids)
	}
}

func TestRestartRequiresHumanToStopLiveTaggedJobPane(t *testing.T) {
	f := startRunner(t)
	home := filepath.Join(filepath.Dir(f.socket), "home")
	for _, name := range []string{".bashrc", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("PS1='OLD_JOB_PROMPT> '\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	job := f.submit(t, "/bin/sh", "-c", "echo OLD_JOB_STILL_LIVE; sleep 30")
	window := f.window(t, job.ID)
	f.waitForTerminal(t, window, "OLD_JOB_STILL_LIVE")
	helperPane := f.tmux(t, "display-message", "-p", "-t", window, "#{pane_id}")
	f.stopRunner(t)
	f.tmux(t, "respawn-pane", "-t", f.runnerPane(t))
	f.waitDead(t, f.runnerPane(t))
	f.waitForTerminal(t, f.runnerPane(t), "Ctrl-C the old job")
	if dead := f.tmux(t, "display-message", "-p", "-t", helperPane, "#{pane_dead}"); dead != "0" {
		t.Fatal("restart killed or adopted old helper")
	}
	f.tmux(t, "send-keys", "-t", helperPane, "C-c")
	f.waitForTerminal(t, window, "cancelled, exit 130")
	// A footer/result can precede actual process exit (notably race-runtime
	// teardown). The real shell prompt confirms the foreground helper returned.
	f.waitForShellPrompt(t, window, "OLD_JOB_PROMPT>")
	if dead := f.tmux(t, "display-message", "-p", "-t", helperPane, "#{pane_dead}"); dead != "0" {
		t.Fatal("stopping the old job must return to its live shell")
	}
	time.Sleep(400 * time.Millisecond)
	f.restartRunner(t)
	next := f.submit(t, "/bin/echo", "HUMAN_STOP_CONFIRMED")
	result, out := f.wait(t, next.ID)
	if result.State != "succeeded" {
		t.Fatalf("restart after human stop: %+v %s", result, out)
	}
	f.waitForTerminal(t, window, "HUMAN_STOP_CONFIRMED")
	if ids := f.tmux(t, "list-windows", "-t", f.session, "-F", "#{window_id}"); len(strings.Fields(ids)) != 2 {
		t.Fatalf("completed old worktree was not reused: %s", ids)
	}
}

func TestRestartRejectsUntaggedPendingLaunchWithoutExecutingIt(t *testing.T) {
	f := startRunner(t)
	first := f.submit(t, "/bin/echo", "BEFORE_RESTART")
	f.wait(t, first.ID)
	f.stopRunner(t)
	marker := filepath.Join(t.TempDir(), "must-not-execute")
	command := "tmux -S " + shellQuote(f.socket) + " wait-for pending-launch && touch " + shellQuote(marker)
	pending := f.tmux(t, "new-window", "-d", "-t", f.session+":", "-n", "pending", "-P", "-F", "#{pane_id}", "/bin/sh", "-c", command)
	time.Sleep(400 * time.Millisecond)
	f.tmux(t, "respawn-pane", "-t", f.runnerPane(t))
	f.waitDead(t, f.runnerPane(t))
	f.waitForTerminal(t, f.runnerPane(t), "untagged window")
	if dead := f.tmux(t, "display-message", "-p", "-t", pending, "#{pane_dead}"); dead != "0" {
		t.Fatal("startup killed pending/unrelated pane")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("pending launch was released: %v", err)
	}
	runners, err := f.client.Runners(context.Background())
	if err != nil || (len(runners) > 0 && runners[0].Available) {
		t.Fatalf("unsafe startup registered: %+v %v", runners, err)
	}
}

func TestCompletedWindowWithInspectionPaneIsNeverReclaimed(t *testing.T) {
	f := startRunner(t)
	job := f.submit(t, "/bin/echo", "COMPLETED")
	f.wait(t, job.ID)
	window := f.window(t, job.ID)
	inspection := f.tmux(t, "split-window", "-d", "-P", "-F", "#{pane_id}", "-t", window, "/bin/sh", "-c", "echo HUMAN_INSPECTION; sleep 30")
	next := f.submit(t, "/bin/echo", "MUST_NOT_RECLAIM")
	result, _ := f.wait(t, next.ID)
	if result.State != "failed" || !strings.Contains(result.Error, "inspection/user panes") {
		t.Fatalf("unsafe reuse was not rejected: %+v", result)
	}
	if dead := f.tmux(t, "display-message", "-p", "-t", inspection, "#{pane_dead}"); dead != "0" {
		t.Fatal("inspection pane killed")
	}
	f.stopRunner(t)
	f.tmux(t, "respawn-pane", "-t", f.runnerPane(t))
	f.waitDead(t, f.runnerPane(t))
	f.waitForTerminal(t, f.runnerPane(t), "inspection/user panes")
	if dead := f.tmux(t, "display-message", "-p", "-t", inspection, "#{pane_dead}"); dead != "0" {
		t.Fatal("startup killed inspection pane")
	}
}

// Delay only the real tmux client response, not the server or job pane. This
// models a parent dying after pane creation but before it can confirm metadata.
func TestInterruptedWindowCreationCannotExecuteAnUntaggedJob(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	dir := t.TempDir()
	created := filepath.Join(dir, "created")
	release := filepath.Join(dir, "release")
	marker := filepath.Join(dir, "executed")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$3\" = new-window ]; then\n %s \"$@\" > %s || exit $?\n touch %s\n while [ ! -f %s ]; do sleep 0.02; done\n cat %s\nelse\n exec %s \"$@\"\nfi\n", shellQuote(realTmux), shellQuote(filepath.Join(dir, "ids")), shellQuote(created), shellQuote(release), shellQuote(filepath.Join(dir, "ids")), shellQuote(realTmux))
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	f := startRunnerWithEnvironment(t, 2, []string{"PATH=" + dir + ":" + os.Getenv("PATH")})
	f.submit(t, "/usr/bin/touch", marker)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(created); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("window creation was not intercepted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	pane := f.runnerPane(t)
	f.tmux(t, "set-option", "-w", "-t", pane, "remain-on-exit", "on")
	var pid int
	fmt.Sscan(f.tmux(t, "display-message", "-p", "-t", pane, "#{pane_pid}"), &pid)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	f.waitDead(t, pane)
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("job executed before parent confirmed its window metadata: %v", err)
	}
	f.tmux(t, "respawn-pane", "-t", pane)
	f.waitDead(t, pane)
	f.waitForTerminal(t, pane, "untagged window")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("replacement released pending job: %v", err)
	}
}

func TestInspectionPaneAddedDuringEvictionSurvivesAndRetainsBudget(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	dir := t.TempDir()
	trigger := filepath.Join(dir, "inspect-next")
	inspectionFile := filepath.Join(dir, "inspection")
	// Return the genuine pre-inspection snapshot, then add a real inspection
	// pane before the runner can act on that now-stale snapshot.
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$3\" = list-panes ] && [ -f %s ]; then\n %s \"$@\" > %s || exit $?\n rm %s\n %s -S \"$2\" split-window -d -P -F '#{pane_id}' -t \"$5\" /bin/sh -c 'sleep 30' > %s || exit $?\n cat %s\nelse\n exec %s \"$@\"\nfi\n", shellQuote(trigger), shellQuote(realTmux), shellQuote(filepath.Join(dir, "snapshot")), shellQuote(trigger), shellQuote(realTmux), shellQuote(inspectionFile), shellQuote(filepath.Join(dir, "snapshot")), shellQuote(realTmux))
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f := startRunnerWithEnvironment(t, 1, []string{"PATH=" + dir + ":" + os.Getenv("PATH")})
	first := f.submit(t, "/bin/echo", "COMPLETED")
	f.wait(t, first.ID)
	window := f.window(t, first.ID)
	if err := os.WriteFile(trigger, nil, 0600); err != nil {
		t.Fatal(err)
	}
	f.source = repository(t, "")
	next := f.submit(t, "/bin/echo", "MUST_NOT_RUN")
	result, _ := f.wait(t, next.ID)
	if result.State != "failed" {
		t.Fatalf("stale inspection authorized eviction: %+v", result)
	}
	data, err := os.ReadFile(inspectionFile)
	if err != nil {
		t.Fatal(err)
	}
	inspection := strings.TrimSpace(string(data))
	if dead := f.tmux(t, "display-message", "-p", "-t", inspection, "#{pane_dead}"); dead != "0" {
		t.Fatal("inspection pane killed by eviction")
	}
	// A failed eviction must not forget the retained window and create another.
	next = f.submit(t, "/bin/echo", "STILL_MUST_NOT_RUN")
	result, _ = f.wait(t, next.ID)
	ids := f.tmux(t, "list-windows", "-t", f.session, "-F", "#{window_id}")
	if result.State != "failed" || len(strings.Fields(ids)) != 2 || !strings.Contains(ids, window) {
		t.Fatalf("failed eviction released the retention budget: %+v windows=%s", result, ids)
	}
	f.tmux(t, "kill-pane", "-t", inspection)
	next = f.submit(t, "/bin/echo", "OPERATOR_CLEANED_UP")
	result, _ = f.wait(t, next.ID)
	if result.State != "succeeded" {
		t.Fatalf("safe eviction after cleanup: %+v", result)
	}
}

func TestRepositoryAliasesReuseTheSameJobWindow(t *testing.T) {
	f := startRunner(t)
	first := f.submit(t, "/bin/echo", "FIRST_ALIAS")
	result, out := f.wait(t, first.ID)
	if result.State != "succeeded" {
		t.Fatalf("first: %+v %s", result, out)
	}
	window := f.window(t, first.ID)
	f.source = "file://" + f.source
	second := f.submit(t, "/bin/echo", "SECOND_ALIAS")
	result, out = f.wait(t, second.ID)
	if result.State != "succeeded" {
		t.Fatalf("second: %+v %s", result, out)
	}
	windows, err := exec.Command("tmux", "-S", f.socket, "list-windows", "-t", f.session, "-F", "#{window_id}").Output()
	if err != nil || len(strings.Fields(string(windows))) != 2 {
		t.Fatalf("aliases must share runner + one worktree window: %v %s", err, windows)
	}
	f.waitForTerminal(t, window, "SECOND_ALIAS")
}
