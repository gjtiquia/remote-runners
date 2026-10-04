package runner_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func modernBash(t *testing.T) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash unavailable")
	}
	check := exec.Command(bash, "--noprofile", "--norc", "-c", `(( BASH_VERSINFO[0] > 5 || (BASH_VERSINFO[0] == 5 && BASH_VERSINFO[1] >= 1) ))`)
	check.Env = []string{}
	if err := check.Run(); err != nil {
		t.Skip("shell-specific test requires Bash 5.1+")
	}
	return bash
}

func writeOperatorPrompt(t *testing.T, home, prompt string) {
	t.Helper()
	for _, name := range []string{".bashrc", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("PS1="+shellQuote(prompt)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// Observe the operator-visible prompt, not a private runner completion marker.
func (f integration) waitForShellPrompt(t *testing.T, window, prompt string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := f.tmux(t, "capture-pane", "-p", "-J", "-t", window)
		if strings.HasSuffix(strings.TrimSpace(out), prompt) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("shell did not return to %q: %s", prompt, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExitClosesCompletedJobWindow(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		restart, differentWorktree bool
	}{
		{"new/same-worktree", false, false},
		{"new/different-worktree", false, true},
		{"retained/same-worktree", true, false},
		{"retained/different-worktree", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startRunnerWithWindows(t, 1)
			job := f.submit(t, "/bin/echo", "FIRST")
			if result, output := f.wait(t, job.ID); result.State != "succeeded" {
				t.Fatalf("initial job: %+v %s", result, output)
			}
			window := f.window(t, job.ID)
			if tc.restart {
				// Simulate a retained interactive window from the previous version.
				f.tmux(t, "set-option", "-w", "-t", window, "remain-on-exit", "on")
				f.stopRunner(t)
				time.Sleep(400 * time.Millisecond)
				f.restartRunner(t)
			}
			f.tmux(t, "send-keys", "-l", "-t", window, "exit")
			f.tmux(t, "send-keys", "-t", window, "Enter")
			deadline := time.Now().Add(2 * time.Second)
			for {
				windows := strings.Fields(f.tmux(t, "list-windows", "-t", f.session, "-F", "#{window_id}"))
				if len(windows) == 1 && windows[0] != window {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("exit retained the job window: %v", windows)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if tc.differentWorktree {
				f.source = repository(t, "")
			}
			next := f.submit(t, "/bin/echo", "AFTER_EXIT")
			result, output := f.wait(t, next.ID)
			if result.State != "succeeded" || !strings.Contains(output, "AFTER_EXIT") {
				t.Fatalf("next job did not recreate its shell/reclaim the closed window's budget: %+v %s", result, output)
			}
			if nextWindow := f.window(t, next.ID); nextWindow == window {
				t.Fatalf("next job reused the closed window %s", window)
			}
		})
	}
}

func TestManualCommandsMakeWindowFailEarlyWithoutQueuedKeystrokes(t *testing.T) {
	for _, manual := range []string{"sleep 60", "while :; do :; done"} {
		t.Run(manual, func(t *testing.T) {
			f := startRunner(t)
			first := f.submit(t, "/bin/echo", "FIRST")
			if result, output := f.wait(t, first.ID); result.State != "succeeded" {
				t.Fatalf("initial shell job failed: %+v %s", result, output)
			}
			window := f.window(t, first.ID)
			started := filepath.Join(t.TempDir(), "manual-started")
			f.tmux(t, "send-keys", "-l", "-t", window, "printf started > "+shellQuote(started)+"; "+manual)
			f.tmux(t, "send-keys", "-t", window, "Enter")
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(started); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("manual command did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			before := time.Now()
			job := f.submit(t, "/bin/echo", "MUST_NOT_BE_QUEUED")
			result, output := f.wait(t, job.ID)
			if result.State != "failed" || result.ExitCode == nil || *result.ExitCode != 125 || !strings.Contains(result.Error, "worktree window is busy") || strings.Contains(output, "MUST_NOT_BE_QUEUED") || time.Since(before) > 2*time.Second {
				t.Fatalf("busy shell did not fail early: %+v output=%s elapsed=%s", result, output, time.Since(before))
			}
			f.tmux(t, "send-keys", "-t", window, "C-c")
			time.Sleep(100 * time.Millisecond)
			logs := f.tmux(t, "capture-pane", "-p", "-S", "-", "-t", window)
			if strings.Contains(logs, "MUST_NOT_BE_QUEUED") {
				t.Fatalf("runner queued keys behind manual command: %s", logs)
			}
		})
	}
}

func TestRetentionSkipsManualBusyShellAndEvictsTheOldestIdleShell(t *testing.T) {
	f := startRunner(t)
	first := f.submit(t, "/bin/echo", "FIRST")
	if result, output := f.wait(t, first.ID); result.State != "succeeded" {
		t.Fatalf("initial job: %+v %s", result, output)
	}
	busyWindow := f.window(t, first.ID)
	f.source = repository(t, "")
	second := f.submit(t, "/bin/echo", "SECOND")
	if result, output := f.wait(t, second.ID); result.State != "succeeded" {
		t.Fatalf("second job: %+v %s", result, output)
	}
	idleWindow := f.window(t, second.ID)
	started := filepath.Join(t.TempDir(), "manual-started")
	f.tmux(t, "send-keys", "-l", "-t", busyWindow, "printf started > "+shellQuote(started)+"; sleep 60")
	f.tmux(t, "send-keys", "-t", busyWindow, "Enter")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("manual work did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.source = repository(t, "")
	third := f.submit(t, "/bin/echo", "THIRD")
	result, output := f.wait(t, third.ID)
	windows := f.tmux(t, "list-windows", "-t", f.session, "-F", "#{window_id}")
	if result.State != "succeeded" || !strings.Contains(windows, busyWindow+"\n") || strings.Contains(windows, idleWindow+"\n") {
		t.Fatalf("retention must protect manual work and use another idle window: %+v output=%s windows=%s", result, output, windows)
	}
}

func TestManualBackgroundJobMakesWindowFailEarly(t *testing.T) {
	f := startRunner(t)
	home := filepath.Join(filepath.Dir(f.socket), "home")
	writeOperatorPrompt(t, home, "BG_PROMPT> ")
	first := f.submit(t, "/bin/echo", "FIRST")
	if result, output := f.wait(t, first.ID); result.State != "succeeded" {
		t.Fatalf("initial shell job failed: %+v %s", result, output)
	}
	window := f.window(t, first.ID)
	f.waitForShellPrompt(t, window, "BG_PROMPT>")
	f.tmux(t, "send-keys", "-l", "-t", window, "sleep 60 &")
	f.tmux(t, "send-keys", "-t", window, "Enter")
	f.waitForTerminal(t, window, "sleep 60 &")
	f.waitForShellPrompt(t, window, "BG_PROMPT>")
	job := f.submit(t, "/bin/echo", "MUST_NOT_RUN_WITH_BACKGROUND_JOB")
	result, output := f.wait(t, job.ID)
	if result.State != "failed" || !strings.Contains(result.Error, "worktree window is busy") || strings.Contains(output, "MUST_NOT_RUN_WITH_BACKGROUND_JOB") {
		t.Fatalf("manual background job was treated as idle: %+v output=%s", result, output)
	}
}

func TestStoppedHelperDoesNotReleaseJobBeforeItExits(t *testing.T) {
	bash := modernBash(t)
	f := startRunner(t)
	f.tmux(t, "set-option", "-t", f.session, "default-shell", bash)
	home := filepath.Join(filepath.Dir(f.socket), "home")
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("PS1='STOP_PROMPT> '\n"), 0600); err != nil {
		t.Fatal(err)
	}
	job := f.submit(t, "/bin/sh", "-c", `echo READY_TO_STOP; printf 'JOB_ROOT=%s\n' "$PWD"; sleep 60`)
	window := f.window(t, job.ID)
	f.waitForTerminal(t, window, "READY_TO_STOP")
	f.tmux(t, "send-keys", "-t", window, "C-z")
	f.waitForTerminal(t, window, "Stopped")
	f.waitForShellPrompt(t, window, "STOP_PROMPT>")
	time.Sleep(300 * time.Millisecond)
	state, err := f.client.Job(context.Background(), job.ID)
	if err != nil || state.Terminal() {
		t.Fatalf("stopped helper was treated as exited: %+v %v", state, err)
	}
	f.tmux(t, "send-keys", "-l", "-t", window, "fg")
	f.tmux(t, "send-keys", "-t", window, "Enter")
	deadline := time.Now().Add(5 * time.Second)
	for f.tmux(t, "display-message", "-p", "-t", window, "#{pane_current_command}") != "remote-runner" {
		if time.Now().After(deadline) {
			t.Fatal("fg did not resume the helper")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.tmux(t, "send-keys", "-t", window, "C-c")
	result, output := f.wait(t, job.ID)
	if result.State != "cancelled" {
		t.Fatalf("resumed helper did not cancel: %+v %s", result, output)
	}
	f.waitForShellPrompt(t, window, "STOP_PROMPT>")
	var root string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "JOB_ROOT=") {
			root = strings.TrimPrefix(line, "JOB_ROOT=")
		}
	}
	if root == "" || f.tmux(t, "display-message", "-p", "-t", window, "#{pane_current_path}") != root {
		t.Fatalf("resumed helper did not return shell to its worktree: root=%q output=%s", root, output)
	}
	f.tmux(t, "send-keys", "-l", "-t", window, `builtin printf 'RETURN_STATUS=%s\n' "$?"`)
	f.tmux(t, "send-keys", "-t", window, "Enter")
	f.waitForTerminal(t, window, "RETURN_STATUS=130")
	f.waitForShellPrompt(t, window, "STOP_PROMPT>")
	next := f.submit(t, "/bin/echo", "REUSED_AFTER_STOPPED_HELPER_EXITED")
	result, output = f.wait(t, next.ID)
	if result.State != "succeeded" {
		t.Fatalf("known exited helper left shell permanently busy: %+v %s", result, output)
	}
}

func TestJobUsesInteractiveStartupEnvironmentAndPreservesPrompt(t *testing.T) {
	bash := modernBash(t)
	f := startRunnerWithEnvironment(t, 2, []string{"MARKER=runner-baseline"})
	f.tmux(t, "set-option", "-g", "default-shell", bash)
	home := filepath.Join(filepath.Dir(f.socket), "home")
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("PS1='operator@hostname:worktree % '\nexport MARKER=from-bashrc\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.source = repository(t, `{"BeforeJobCommand":"printf 'HOOK_MARKER=%s\\n' \"$MARKER\""}`)
	job := f.submit(t, "/bin/sh", "-c", `printf 'JOB_MARKER=%s\n' "$MARKER"`)
	result, output := f.wait(t, job.ID)
	window := f.window(t, job.ID)
	if result.State != "succeeded" || !strings.Contains(output, "HOOK_MARKER=from-bashrc") || !strings.Contains(output, "JOB_MARKER=from-bashrc") {
		t.Fatalf("shell startup environment not used by hooks/job: %+v %s", result, output)
	}
	f.waitForTerminal(t, window, "operator@hostname:worktree % ")
}

func TestZshShellPreservesStartupNamedHooksAndManualExports(t *testing.T) {
	zsh := os.Getenv("REMOTE_RUNNERS_TEST_ZSH")
	if zsh == "" {
		var err error
		zsh, err = exec.LookPath("zsh")
		if err != nil {
			t.Skip("zsh unavailable; optionally set REMOTE_RUNNERS_TEST_ZSH")
		}
	}
	f := startRunner(t)
	f.tmux(t, "set-option", "-t", f.session, "default-shell", zsh)
	home := filepath.Join(filepath.Dir(f.socket), "home")
	preexec := filepath.Join(t.TempDir(), "preexec")
	rc := "PS1='operator-zsh> '\nexport MARKER=from-zshrc\npreexec() { print -r -- named >> " + shellQuote(preexec) + "; }\n__user_preexec() { print -r -- array >> " + shellQuote(preexec) + "; }\npreexec_functions+=(__user_preexec)\n"
	if modules := os.Getenv("REMOTE_RUNNERS_TEST_ZSH_MODULE_PATH"); modules != "" {
		rc = "module_path=(" + shellQuote(modules) + " $module_path)\n" + rc
	}
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte(rc), 0600); err != nil {
		t.Fatal(err)
	}
	first := f.submit(t, "/bin/sh", "-c", `printf 'ZSH_MARKER=%s\n' "$MARKER"`)
	result, output := f.wait(t, first.ID)
	window := f.window(t, first.ID)
	if result.State != "succeeded" || !strings.Contains(output, "ZSH_MARKER=from-zshrc") {
		t.Fatalf("zsh startup did not reach job: %+v %s", result, output)
	}
	f.waitForShellPrompt(t, window, "operator-zsh>")
	f.tmux(t, "send-keys", "-l", "-t", window, "export MARKER=manual-zsh")
	f.tmux(t, "send-keys", "-t", window, "Enter")
	f.waitForTerminal(t, window, "export MARKER=manual-zsh")
	f.waitForShellPrompt(t, window, "operator-zsh>")
	second := f.submit(t, "/bin/sh", "-c", `printf 'ZSH_MARKER=%s\n' "$MARKER"`)
	result, output = f.wait(t, second.ID)
	data, err := os.ReadFile(preexec)
	if err != nil || result.State != "succeeded" || !strings.Contains(output, "ZSH_MARKER=manual-zsh") || strings.Count(string(data), "named\n") != 3 || strings.Count(string(data), "array\n") != 3 {
		t.Fatalf("zsh hooks/exports/reuse changed: %+v output=%s hooks=%s err=%v", result, output, data, err)
	}
	f.waitForShellPrompt(t, window, "operator-zsh>")
	f.tmux(t, "send-keys", "-l", "-t", window, "sleep 60 &")
	f.tmux(t, "send-keys", "-t", window, "Enter")
	f.waitForTerminal(t, window, "sleep 60 &")
	f.waitForShellPrompt(t, window, "operator-zsh>")
	blocked := f.submit(t, "/bin/echo", "MUST_NOT_RUN_WITH_ZSH_BACKGROUND_JOB")
	result, output = f.wait(t, blocked.ID)
	if result.State != "failed" || !strings.Contains(result.Error, "worktree window is busy") || strings.Contains(output, "MUST_NOT_RUN_WITH_ZSH_BACKGROUND_JOB") {
		t.Fatalf("Zsh background job did not block admission: %+v %s", result, output)
	}
}

func TestShellAliasesDoNotBreakCompletionOrWorktreePrompt(t *testing.T) {
	bash := modernBash(t)
	f := startRunner(t)
	f.tmux(t, "set-option", "-t", f.session, "default-shell", bash)
	home := filepath.Join(filepath.Dir(f.socket), "home")
	rc := "PS1='ALIAS_PROMPT> '\nalias printf='false'\nalias cat='false'\nalias cd='false'\nkill() { return 0; }\n"
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(rc), 0600); err != nil {
		t.Fatal(err)
	}
	job := f.submit(t, "/bin/pwd")
	result, output := f.wait(t, job.ID)
	window := f.window(t, job.ID)
	f.waitForShellPrompt(t, window, "ALIAS_PROMPT>")
	cwd := f.tmux(t, "display-message", "-p", "-t", window, "#{pane_current_path}")
	if result.State != "succeeded" || !strings.Contains(cwd, "/worktrees/") || !strings.Contains(output, cwd+"\n") {
		t.Fatalf("shell aliases broke tracking or returning to worktree: %+v cwd=%s output=%s", result, cwd, output)
	}
}

func TestBashPromptHookSeesRemoteCommandStatusAndExportedFunctionsSurvive(t *testing.T) {
	bash := modernBash(t)
	f := startRunnerWithEnvironment(t, 2, []string{"BASH_FUNC_rr_exported%%=() { printf EXPORTED_FUNCTION_OK; }"})
	f.tmux(t, "set-option", "-g", "default-shell", bash)
	status := filepath.Join(t.TempDir(), "prompt-status")
	rc := "PROMPT_COMMAND=" + shellQuote("printf '%s\\n' \"$?\" >> "+shellQuote(status)) + "\nPS1='status-prompt % '\n"
	home := filepath.Join(filepath.Dir(f.socket), "home")
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(rc), 0600); err != nil {
		t.Fatal(err)
	}
	job := f.submit(t, bash, "-c", "rr_exported; exit 7")
	result, output := f.wait(t, job.ID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(status)
		if err == nil && strings.Contains(string(data), "7\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("existing prompt hook did not see remote exit 7: %q err=%v", data, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if result.ExitCode == nil || *result.ExitCode != 7 || !strings.Contains(output, "EXPORTED_FUNCTION_OK") {
		t.Fatalf("exported functions or remote status changed: %+v %s", result, output)
	}
}

func TestCompletedJobReturnsToAnInteractiveShell(t *testing.T) {
	f := startRunner(t)
	home := filepath.Join(filepath.Dir(f.socket), "home")
	writeOperatorPrompt(t, home, "RR_OPERATOR_PROMPT> ")
	first := f.submit(t, "/bin/echo", "FIRST_JOB")
	result, _ := f.wait(t, first.ID)
	window := f.window(t, first.ID)
	pane := f.tmux(t, "display-message", "-p", "-t", window, "#{pane_id}")
	if result.State != "succeeded" || f.tmux(t, "display-message", "-p", "-t", pane, "#{pane_dead}") != "0" {
		t.Fatalf("job must return to a live shell, not a dead helper pane: %+v", result)
	}
	f.tmux(t, "send-keys", "-l", "-t", pane, "export MARKER=from-manual-shell; printf 'MANUAL_PROMPT_READY\\n'")
	f.tmux(t, "send-keys", "-t", pane, "Enter")
	f.waitForTerminal(t, window, "MANUAL_PROMPT_READY")
	f.waitForShellPrompt(t, window, "RR_OPERATOR_PROMPT>")
	second := f.submit(t, "/bin/sh", "-c", `printf 'PANE_MARKER=%s\n' "$MARKER"; pwd`)
	result, out := f.wait(t, second.ID)
	if result.State != "succeeded" || !strings.Contains(out, "PANE_MARKER=from-manual-shell") || f.tmux(t, "display-message", "-p", "-t", window, "#{pane_id}") != pane {
		t.Fatalf("same live shell and manual exports must survive reuse: %+v output=%s", result, out)
	}
}
