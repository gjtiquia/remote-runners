package runner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gjtiquia/remote-runners/internal/client"
	"github.com/gjtiquia/remote-runners/internal/coordinator"
	"github.com/gjtiquia/remote-runners/internal/protocol"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func binary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "remote-runner")
	args := []string{"build", "-p", "1"}
	if raceEnabled {
		args = append(args, "-race")
	}
	args = append(args, "-o", path, "../../cmd/remote-runner")
	c := exec.Command("go", args...)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return path
}

func repository(t *testing.T, hooks string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.invalid")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	if hooks != "" {
		if err := os.WriteFile(filepath.Join(dir, ".remote-runner.json"), []byte(hooks), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-m", "initial")
	return dir
}

func helper(t *testing.T, bin, source, base string, args []string, env []string) (map[string]any, string) {
	t.Helper()
	descriptor := map[string]any{"roots": map[string]string{"Repos": filepath.Join(base, "repos"), "Worktrees": filepath.Join(base, "worktrees")}, "job": map[string]any{"id": "test", "source": map[string]string{"remote": source, "branch": "main"}, "args": args, "timeout": int64(10000000000)}, "output_path": filepath.Join(base, "output"), "result_path": filepath.Join(base, "result"), "cancel_path": filepath.Join(base, "cancel")}
	data, _ := json.Marshal(descriptor)
	path := filepath.Join(base, "descriptor.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(bin, "job-exec", path)
	c.Env = env
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()
	if err != nil {
		t.Fatalf("helper infrastructure: %v stdout=%s stderr=%s", err, &stdout, &stderr)
	}
	out := stdout.Bytes()
	data, err = os.ReadFile(filepath.Join(base, "result"))
	if err != nil {
		t.Fatalf("result: %v %s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(filepath.Join(base, "output"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, output) {
		t.Fatalf("stdout must match disk payload without stderr diagnostics: %s / %s (stderr=%s)", output, out, &stderr)
	}
	return result, string(output)
}

func TestJobExecPreservesArgumentsEnvironmentAndWorktreeRoot(t *testing.T) {
	bin := binary(t)
	source := repository(t, "")
	result, out := helper(t, bin, source, t.TempDir(), []string{"/bin/sh", "-c", `printf '%s|%s|%s' "$MARKER" "$1" "$(cat tracked)"; test -f .git`, "command", "space ' quote ; $literal"}, append(os.Environ(), "MARKER=runner-environment"))
	if result["state"] != "succeeded" || result["exit_code"] != float64(0) || !strings.Contains(out, "runner-environment|space ' quote ; $literal|initial") || len(fmt.Sprint(result["commit"])) != 40 {
		t.Fatalf("result=%v output=%s", result, out)
	}
}

func TestProjectHooksRunOnCreationAndBeforeEveryJob(t *testing.T) {
	bin := binary(t)
	source := repository(t, `{"AfterCreateWorktreeCommand":"echo CREATED","BeforeJobCommand":"echo BEFORE"}`)
	base := t.TempDir()
	_, first := helper(t, bin, source, base, []string{"/bin/echo", "JOB"}, os.Environ())
	_, second := helper(t, bin, source, base, []string{"/bin/echo", "JOB"}, os.Environ())
	if !strings.Contains(first, "CREATED\nBEFORE\nJOB") || strings.Contains(second, "CREATED") || !strings.Contains(second, "BEFORE\nJOB") {
		t.Fatalf("incorrect hook lifecycle: first=%s second=%s", first, second)
	}
}

type integration struct {
	client          *client.Client
	socket          string
	source          string
	session         string
	stopCoordinator func() error
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func startRunner(t *testing.T) integration {
	return startRunnerWithWindows(t, 2)
}
func startRunnerWithWindows(t *testing.T, maxWindows int) integration {
	return startRunnerWithEnvironment(t, maxWindows, nil)
}
func startRunnerWithEnvironment(t *testing.T, maxWindows int, environment []string) integration {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	bin := binary(t)
	dir, err := os.MkdirTemp("", "rr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	server, err := coordinator.New(coordinator.Options{OutputDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	home := filepath.Join(dir, "home")
	cfgdir := filepath.Join(home, ".remote-runner")
	if err = os.MkdirAll(cfgdir, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{"coordinator_url":%q,"name":"test","max_windows":%d,"max_jobs":2,"min_memory":1,"repos":%q,"worktrees":%q}`, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/runner", maxWindows, filepath.Join(dir, "repos"), filepath.Join(dir, "worktrees"))
	if err = os.WriteFile(filepath.Join(cfgdir, "config.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "sock")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
	command := "exec env HOME=" + shellQuote(home) + " MARKER=from-runner "
	for _, entry := range environment {
		command += shellQuote(entry) + " "
	}
	command += shellQuote(bin)
	out, err := exec.Command("tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-s", "worker", "-P", "-F", "#{session_id}", command).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux session: %v %s", err, out)
	}
	api := client.New(httpServer.URL)
	deadline := time.Now().Add(5 * time.Second)
	for {
		runners, err := api.Runners(context.Background())
		if err == nil && len(runners) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner did not register: %v %v", runners, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return integration{api, socket, repository(t, ""), strings.TrimSpace(string(out)), server.Close}
}
func (f integration) submit(t *testing.T, args ...string) protocol.Job {
	t.Helper()
	j, err := f.client.Submit(context.Background(), protocol.Submission{Source: protocol.Source{Remote: f.source, Branch: "main"}, Args: args, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func (f integration) wait(t *testing.T, id string) (protocol.Job, string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		j, err := f.client.Job(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Terminal() {
			var out strings.Builder
			if err = f.client.Output(context.Background(), id, &out); err != nil {
				t.Fatal(err)
			}
			return j, out.String()
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %+v", j)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func TestCoordinatorRunsJobInRunnerTmuxSession(t *testing.T) {
	f := startRunner(t)
	j := f.submit(t, "/bin/sh", "-c", `printf '%s|%s|%s' "$MARKER" "$TMUX" "$(cat tracked)"`)
	result, out := f.wait(t, j.ID)
	if result.State != "succeeded" || !strings.Contains(out, "from-runner|"+f.socket+",") || !strings.Contains(out, "|initial") {
		t.Fatalf("real tmux job failed: %+v %s", result, out)
	}
	windows, err := exec.Command("tmux", "-S", f.socket, "list-windows", "-t", f.session, "-F", "#{window_id}").Output()
	if err != nil || len(strings.Fields(string(windows))) != 2 {
		t.Fatalf("expected runner + job window: %v %s", err, windows)
	}
}

func (f integration) window(t *testing.T, name string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := exec.Command("tmux", "-S", f.socket, "list-windows", "-t", f.session, "-F", "#{window_id} #{window_name}").Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == name {
				return fields[0]
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no job window named %s: %s", name, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func (f integration) waitForTerminal(t *testing.T, window, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := exec.Command("tmux", "-S", f.socket, "capture-pane", "-p", "-J", "-S", "-", "-t", window).Output()
		if err == nil && strings.Contains(string(out), text) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal missing %q: %v %s", text, err, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func TestHumanCtrlCCancelsCommandAndOrdinaryDescendants(t *testing.T) {
	f := startRunner(t)
	marker := filepath.Join(t.TempDir(), "leaked")
	j := f.submit(t, "/bin/sh", "-c", `(sleep 2; touch "$1") & echo COMMAND_READY; wait`, "command", marker)
	window := f.window(t, j.ID)
	f.waitForTerminal(t, window, "COMMAND_READY")
	if out, err := exec.Command("tmux", "-S", f.socket, "send-keys", "-t", window, "C-c").CombinedOutput(); err != nil {
		t.Fatalf("Ctrl-C: %v %s", err, out)
	}
	result, out := f.wait(t, j.ID)
	if result.State != "cancelled" || result.ExitCode == nil || *result.ExitCode != 130 || !strings.Contains(out, "COMMAND_READY") {
		t.Fatalf("Ctrl-C result: %+v %s", result, out)
	}
	time.Sleep(2200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("descendant survived cancellation: %v", err)
	}
}

func TestJobTerminalMetadataIdentifiesItsOwnPane(t *testing.T) {
	f := startRunner(t)
	j := f.submit(t, "/bin/sh", "-c", `printf 'JOB_PANE=%s\n' "$TMUX_PANE"`)
	result, out := f.wait(t, j.ID)
	window := f.window(t, j.ID)
	pane, err := exec.Command("tmux", "-S", f.socket, "display-message", "-p", "-t", window, "#{pane_id}").Output()
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "succeeded" || !strings.Contains(out, "JOB_PANE="+strings.TrimSpace(string(pane))) {
		t.Fatalf("job inherited runner pane metadata: %+v %s (actual %s)", result, out, pane)
	}
}

func TestCompletedWorktreeWindowsReuseAndEvictLeastRecentlyUsed(t *testing.T) {
	f := startRunner(t)
	first := f.submit(t, "/bin/echo", "first")
	f.wait(t, first.ID)
	firstWindow := f.window(t, first.ID)
	original := f.source
	f.source = repository(t, "")
	second := f.submit(t, "/bin/echo", "second")
	f.wait(t, second.ID)
	secondWindow := f.window(t, second.ID)
	f.source = original
	reused := f.submit(t, "/bin/echo", "reused")
	f.wait(t, reused.ID)
	f.waitForTerminal(t, firstWindow, "reused")
	f.source = repository(t, "")
	third := f.submit(t, "/bin/echo", "third")
	f.wait(t, third.ID)
	windows, err := exec.Command("tmux", "-S", f.socket, "list-windows", "-t", f.session, "-F", "#{window_id}").Output()
	if err != nil {
		t.Fatal(err)
	}
	ids := strings.Fields(string(windows))
	present := func(id string) bool {
		for _, candidate := range ids {
			if candidate == id {
				return true
			}
		}
		return false
	}
	if len(ids) != 3 || !present(firstWindow) || present(secondWindow) {
		t.Fatalf("retention must keep runner and two newest completed worktrees: %s (first %s second %s)", windows, firstWindow, secondWindow)
	}
}

func TestInspectingAnotherPaneDoesNotMarkActiveJobCompleted(t *testing.T) {
	f := startRunner(t)
	j := f.submit(t, "/bin/sh", "-c", "echo ACTIVE_PANE_READY; sleep 30")
	window := f.window(t, j.ID)
	f.waitForTerminal(t, window, "ACTIVE_PANE_READY")
	pane, err := exec.Command("tmux", "-S", f.socket, "split-window", "-d", "-P", "-F", "#{pane_id}", "-t", window, "/bin/sh", "-c", "exit 0").CombinedOutput()
	if err != nil {
		t.Fatalf("owned inspection pane: %v %s", err, pane)
	}
	if out, err := exec.Command("tmux", "-S", f.socket, "select-pane", "-t", strings.TrimSpace(string(pane))).CombinedOutput(); err != nil {
		t.Fatalf("select owned pane: %v %s", err, out)
	}
	time.Sleep(350 * time.Millisecond)
	state, err := f.client.Job(context.Background(), j.ID)
	if err != nil || state.Terminal() {
		t.Fatalf("pane selection incorrectly completed active job: %+v %v", state, err)
	}
	if err = f.client.Cancel(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	f.wait(t, j.ID)
}

func TestRetentionNeverEvictsAnActiveWindow(t *testing.T) {
	f := startRunner(t)
	active := f.submit(t, "/bin/sh", "-c", "echo ACTIVE_WINDOW_READY; sleep 30")
	activeWindow := f.window(t, active.ID)
	f.waitForTerminal(t, activeWindow, "ACTIVE_WINDOW_READY")
	f.source = repository(t, "")
	completed := f.submit(t, "/bin/echo", "completed")
	f.wait(t, completed.ID)
	completedWindow := f.window(t, completed.ID)
	f.source = repository(t, "")
	newest := f.submit(t, "/bin/echo", "newest")
	f.wait(t, newest.ID)
	windows, err := exec.Command("tmux", "-S", f.socket, "list-windows", "-t", f.session, "-F", "#{window_id}").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(windows), activeWindow+"\n") || strings.Contains(string(windows), completedWindow+"\n") {
		t.Fatalf("active window was evicted instead of completed: %s", windows)
	}
	state, err := f.client.Job(context.Background(), active.ID)
	if err != nil || state.Terminal() {
		t.Fatalf("active job disrupted: %+v %v", state, err)
	}
	if err = f.client.Cancel(context.Background(), active.ID); err != nil {
		t.Fatal(err)
	}
	f.wait(t, active.ID)
}

func TestTransportLossDoesNotTerminateLocalJob(t *testing.T) {
	f := startRunner(t)
	j := f.submit(t, "/bin/sh", "-c", `echo READY_FOR_DISCONNECT; sleep 1; echo FINISHED_AFTER_DISCONNECT`)
	window := f.window(t, j.ID)
	f.waitForTerminal(t, window, "READY_FOR_DISCONNECT")
	if err := f.stopCoordinator(); err != nil {
		t.Fatal(err)
	}
	f.waitForTerminal(t, window, "FINISHED_AFTER_DISCONNECT")
	f.waitForTerminal(t, window, "succeeded, exit 0")
}

func TestManagementCancellationStopsTheJob(t *testing.T) {
	f := startRunner(t)
	j := f.submit(t, "/bin/sh", "-c", `echo READY_FOR_CANCEL; sleep 30; echo SHOULD_NOT_RUN`)
	f.waitForTerminal(t, f.window(t, j.ID), "READY_FOR_CANCEL")
	if err := f.client.Cancel(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	result, out := f.wait(t, j.ID)
	if result.State != "cancelled" || result.ExitCode == nil || *result.ExitCode != 130 || strings.Contains(out, "SHOULD_NOT_RUN") {
		t.Fatalf("cancel result: %+v %s", result, out)
	}
}

func TestUntrackedHookConfigurationIsNotExecuted(t *testing.T) {
	f := startRunner(t)
	first := f.submit(t, "/bin/sh", "-c", `printf '%s' "$1" > .remote-runner.json`, "command", `{"BeforeJobCommand":"echo UNTRACKED_HOOK"}`)
	result, _ := f.wait(t, first.ID)
	if result.State != "succeeded" {
		t.Fatalf("create untracked config: %+v", result)
	}
	second := f.submit(t, "/bin/echo", "JOB")
	result, out := f.wait(t, second.ID)
	if result.State != "succeeded" || strings.Contains(out, "UNTRACKED_HOOK") {
		t.Fatalf("only committed project hooks should execute: %+v %s", result, out)
	}
}

func TestHookFailurePreventsTheSubmittedCommand(t *testing.T) {
	f := startRunner(t)
	f.source = repository(t, `{"BeforeJobCommand":"echo HOOK_FAILURE; exit 17"}`)
	j := f.submit(t, "/bin/echo", "SHOULD_NOT_RUN")
	result, out := f.wait(t, j.ID)
	if result.State != "failed" || !strings.Contains(result.Error, "project hook") || !strings.Contains(out, "HOOK_FAILURE") || strings.Contains(out, "SHOULD_NOT_RUN") {
		t.Fatalf("hook failure did not stop execution: %+v %s", result, out)
	}
}

func TestTimeoutIncludesBeforeJobHook(t *testing.T) {
	f := startRunner(t)
	f.source = repository(t, `{"BeforeJobCommand":"echo HOOK_STARTED; sleep 30"}`)
	j, err := f.client.Submit(context.Background(), protocol.Submission{Source: protocol.Source{Remote: f.source, Branch: "main"}, Args: []string{"/bin/echo", "SHOULD_NOT_RUN"}, Timeout: 400 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	result, out := f.wait(t, j.ID)
	if result.State != "timed_out" || result.ExitCode == nil || *result.ExitCode != 124 || !strings.Contains(out, "HOOK_STARTED") || strings.Contains(out, "SHOULD_NOT_RUN") {
		t.Fatalf("hook timeout: %+v %s", result, out)
	}
	logs := f.tmux(t, "capture-pane", "-p", "-J", "-S", "-", "-t", f.window(t, j.ID))
	assertProgress(t, logs, "remote-runner-job", `completed state="timed_out" exit=124`)
	if strings.Contains(out, "remote-runner-job ") || strings.Contains(logs, `executing executable="echo"`) {
		t.Fatalf("timed-out hook leaked progress into payload or started the command: output=%s pane=%s", out, logs)
	}
}

func TestCompletedOutputIsTransferredWithoutTruncation(t *testing.T) {
	f := startRunner(t)
	j := f.submit(t, "/bin/sh", "-c", `head -c 262144 /dev/zero | tr '\000' Z; exit 7`)
	result, out := f.wait(t, j.ID)
	if result.State != "failed" || result.ExitCode == nil || *result.ExitCode != 7 || strings.Count(out, "Z") != 262144 {
		t.Fatalf("output/result lost: state=%s exit=%v Z count=%d", result.State, result.ExitCode, strings.Count(out, "Z"))
	}
}

func TestWhitespaceConfigurationCreatesStarter(t *testing.T) {
	bin := binary(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".remote-runner")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(" \n\t"), 0600); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(bin)
	c.Env = append(os.Environ(), "HOME="+home, "TMUX=", "TMUX_PANE=")
	out, err := c.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "starter configuration created") {
		t.Fatalf("empty config needs starter: %v %s", err, out)
	}
}

func TestConfigurationReportsInvalidCoordinatorBeforeTmux(t *testing.T) {
	bin := binary(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".remote-runner")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"coordinator_url":"not-a-url","name":"test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(bin)
	c.Env = append(os.Environ(), "HOME="+home, "TMUX=", "TMUX_PANE=")
	out, err := c.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "coordinator_url") {
		t.Fatalf("invalid config must be actionable before tmux: %v %s", err, out)
	}
}

func TestConfiguredRunnerRejectsInvalidTmux(t *testing.T) {
	bin := binary(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".remote-runner")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"coordinator_url":"ws://127.0.0.1:2461/runner","name":"test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(bin)
	c.Env = append(os.Environ(), "HOME="+home, "TMUX="+filepath.Join(home, "missing")+",1,0", "TMUX_PANE=%0")
	out, err := c.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "validate tmux") {
		t.Fatalf("invalid tmux must be verified, not trusted: %v %s", err, out)
	}
}

func TestFirstRunCreatesStarterOutsideTmux(t *testing.T) {
	bin := binary(t)
	home := t.TempDir()
	c := exec.Command(bin)
	c.Env = append(os.Environ(), "HOME="+home, "TMUX=", "TMUX_PANE=")
	out, err := c.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "config.json") {
		t.Fatalf("expected configure instruction: %v %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(home, ".remote-runner", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["coordinator_url"] != "" || cfg["priority"] != float64(50) || cfg["max_jobs"] != float64(5) || cfg["max_windows"] != float64(8) || cfg["min_memory"] != float64(1073741824) {
		t.Fatalf("incorrect starter: %s", data)
	}
}
