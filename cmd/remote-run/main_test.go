package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gjtiquia/remote-runners/internal/client"
	"github.com/gjtiquia/remote-runners/internal/coordinator"
	"github.com/gjtiquia/remote-runners/internal/runner"
)

// Re-execute the actual command entrypoint, not an internal argument parser.
func TestCLIEntrypoint(t *testing.T) {
	if os.Getenv("REMOTE_RUN_CLI_HELPER") != "1" {
		return
	}
	for i, a := range os.Args {
		if a == "--" {
			os.Args = append([]string{"remote-run"}, os.Args[i+1:]...)
			main()
			return
		}
	}
	os.Exit(99)
}

func command(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(bin, append([]string{"-test.run=^TestCLIEntrypoint$", "--"}, args...)...)
	c.Env = append(os.Environ(), "REMOTE_RUN_CLI_HELPER=1")
	c.Dir = dir
	return c
}
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := err.(*exec.ExitError); ok {
		return e.ExitCode()
	}
	return -1
}
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func checkout(t *testing.T) (string, string) {
	t.Helper()
	remote := t.TempDir()
	git(t, remote, "init", "-b", "main")
	git(t, remote, "config", "user.name", "Test")
	git(t, remote, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(remote, "job.sh"), []byte("printf 'ARG=<%s>\\n' \"$@\"\nprintf 'ROOT='; cat marker\nexit 7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "marker"), []byte("published\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, remote, "add", ".")
	git(t, remote, "commit", "-m", "test fixture")
	local := filepath.Join(t.TempDir(), "checkout")
	git(t, "", "clone", "--", remote, local)
	sub := filepath.Join(local, "subdir")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	return local, sub
}

type fixture struct {
	api    *client.Client
	port   string
	socket string
}

func startCoordinator(t *testing.T) fixture {
	t.Helper()
	// These exercise CLI behaviour, not heartbeat cutoff. Use production timing
	// so instrumented large-output transfers don't cause artificial runner loss.
	s, err := coordinator.New(coordinator.Options{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h := httptest.NewServer(s.Handler())
	t.Cleanup(h.Close)
	return fixture{api: client.New(h.URL), port: strings.TrimPrefix(h.URL, "http://127.0.0.1:")}
}
func startWorker(t *testing.T, f fixture) fixture {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	base, err := os.MkdirTemp("", "rrcli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	bin := filepath.Join(base, "remote-runner")
	c := exec.Command("go", "build", "-p", "1", "-o", bin, "../remote-runner")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("build worker: %v\n%s", err, out)
	}
	home := filepath.Join(base, "home")
	cfgdir := filepath.Join(home, ".remote-runner")
	if err = os.MkdirAll(cfgdir, 0700); err != nil {
		t.Fatal(err)
	}
	config := runner.Config{CoordinatorURL: "ws://127.0.0.1:" + f.port + "/runner", Name: "cli-test", Priority: 50, MaxJobs: 1, MinMemory: 1, MaxWindows: 2, Repos: filepath.Join(base, "repos"), Worktrees: filepath.Join(base, "worktrees")}
	data, _ := json.Marshal(config)
	if err = os.WriteFile(filepath.Join(cfgdir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	f.socket = filepath.Join(base, "sock")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", f.socket, "kill-server").Run() })
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	launch := "exec env HOME=" + quote(home) + " " + quote(bin)
	if out, err := exec.Command("tmux", "-S", f.socket, "-f", "/dev/null", "new-session", "-d", "-s", "worker", launch).CombinedOutput(); err != nil {
		t.Fatalf("tmux: %v %s", err, out)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		rs, err := f.api.Runners(context.Background())
		if err == nil && len(rs) == 1 {
			return f
		}
		if time.Now().After(deadline) {
			t.Fatalf("registration failed: %v %+v", err, rs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPrefixPreservesCommandArgumentsAndReturnsCompletedOutputAndExit(t *testing.T) {
	_, sub := checkout(t)
	f := startWorker(t, startCoordinator(t))
	c := command(t, sub, "-p", f.port, "/bin/sh", "job.sh", "space ' ; $literal", "-p", "987")
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	if exitCode(err) != 7 || !strings.Contains(stdout.String(), "ARG=<space ' ; $literal>\nARG=<-p>\nARG=<987>\nROOT=published") || !strings.Contains(stderr.String(), "job ID: job-") || !strings.Contains(stderr.String(), "exit code: 7") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exitCode(err), stdout.String(), stderr.String())
	}
	jobs, err := f.api.Jobs(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].Timeout != 30*time.Minute {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
}

func TestCtrlCCancelsQueuedJobAndReturnsCancellationExit(t *testing.T) {
	local, _ := checkout(t)
	f := startCoordinator(t)
	c := command(t, local, "-p", f.port, "/bin/echo", "never-run")
	var stdout bytes.Buffer
	c.Stdout = &stdout
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	c.Stderr = stderr
	if err = c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.Process != nil {
			_ = c.Process.Kill()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	var id string
	for {
		jobs, e := f.api.Jobs(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		if len(jobs) == 1 {
			id = jobs[0].ID
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no accepted job")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		data, e := os.ReadFile(stderr.Name())
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(data), "job ID: "+id) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CLI did not report ID")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = c.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		_ = c.Process.Kill()
		<-done
		t.Fatal("Ctrl-C did not finish")
	}
	data, e := os.ReadFile(stderr.Name())
	if e != nil {
		t.Fatal(e)
	}
	job, e := f.api.Job(context.Background(), id)
	if exitCode(err) != 130 || e != nil || job.State != "cancelled" || stdout.Len() != 0 || !strings.Contains(string(data), "exit code: 130") {
		t.Fatalf("exit=%d job=%+v err=%v stdout=%s stderr=%s", exitCode(err), job, e, stdout.String(), data)
	}
}

func TestRunningCtrlCReturnsAvailableOutputOnlyAfterCompletion(t *testing.T) {
	local, _ := checkout(t)
	f := startWorker(t, startCoordinator(t))
	c := command(t, local, "-p", f.port, "/bin/sh", "-c", "echo COMMAND_READY; sleep 20")
	stdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	c.Stdout = stdout
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	c.Stderr = stderr
	if err = c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.Process != nil {
			_ = c.Process.Kill()
		}
	})
	deadline := time.Now().Add(8 * time.Second)
	var id string
	for {
		jobs, e := f.api.Jobs(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		if len(jobs) == 1 {
			id = jobs[0].ID
			pane, e := exec.Command("tmux", "-S", f.socket, "capture-pane", "-p", "-t", "worker:"+id).CombinedOutput()
			if e == nil && strings.Contains(string(pane), "COMMAND_READY") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("command never reached ready state")
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("live output unexpectedly exposed: %s", data)
	}
	if err = c.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err = <-done:
	case <-time.After(12 * time.Second):
		_ = c.Process.Kill()
		<-done
		t.Fatal("running cancellation hung")
	}
	data, e := os.ReadFile(stdout.Name())
	if e != nil {
		t.Fatal(e)
	}
	diagnostics, e := os.ReadFile(stderr.Name())
	if e != nil {
		t.Fatal(e)
	}
	job, e := f.api.Job(context.Background(), id)
	if exitCode(err) != 130 || e != nil || job.State != "cancelled" || !strings.Contains(string(data), "COMMAND_READY") || !strings.Contains(string(diagnostics), "exit code: 130") {
		t.Fatalf("exit=%d job=%+v err=%v output=%s stderr=%s", exitCode(err), job, e, data, diagnostics)
	}
}

func TestTimeoutReturns124AndCapturedDiagnostics(t *testing.T) {
	local, _ := checkout(t)
	f := startWorker(t, startCoordinator(t))
	c := command(t, local, "-p", f.port, "-timeout", "2s", "/bin/sh", "-c", "echo BEFORE_TIMEOUT; sleep 20")
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	if exitCode(err) != 124 || !strings.Contains(stdout.String(), "BEFORE_TIMEOUT") || !strings.Contains(stderr.String(), "exit code: 124") {
		t.Fatalf("exit=%d output=%s stderr=%s", exitCode(err), stdout.String(), stderr.String())
	}
}

func TestCompletedLargeOutputIsNotTruncated(t *testing.T) {
	local, _ := checkout(t)
	f := startWorker(t, startCoordinator(t))
	c := command(t, local, "-p", f.port, "/bin/sh", "-c", `printf 'PAYLOAD_BEGIN\n'; head -c 8388608 /dev/zero | tr '\0' Z; printf '\nPAYLOAD_END\n'`)
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	c.Stdout = output
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err = c.Run(); err != nil {
		t.Fatalf("exit=%d stderr=%s", exitCode(err), stderr.String())
	}
	if _, err = output.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(output)
	for {
		line, e := reader.ReadString('\n')
		if e != nil {
			t.Fatalf("missing payload: %v", e)
		}
		if line == "PAYLOAD_BEGIN\n" {
			break
		}
	}
	remaining := 8388608
	var chunk [4096]byte
	for remaining > 0 {
		n := len(chunk)
		if remaining < n {
			n = remaining
		}
		if _, err = io.ReadFull(reader, chunk[:n]); err != nil {
			t.Fatalf("payload truncated: %v", err)
		}
		if bytes.Count(chunk[:n], []byte("Z")) != n {
			t.Fatal("payload corrupted")
		}
		remaining -= n
	}
	line, err := reader.ReadString('\n')
	if err != nil || line != "\n" {
		t.Fatalf("payload length changed: %q %v", line, err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || line != "PAYLOAD_END\n" {
		t.Fatalf("missing trailer: %q %v", line, err)
	}
}

func TestGitPreflightErrorsAreActionableAndDoNotSubmit(t *testing.T) {
	local, _ := checkout(t)
	f := startCoordinator(t)
	if err := os.WriteFile(filepath.Join(local, "untracked"), []byte("must not transfer"), 0600); err != nil {
		t.Fatal(err)
	}
	c := command(t, local, "-p", f.port, "/bin/echo", "never")
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	jobs, e := f.api.Jobs(context.Background())
	if exitCode(err) != 125 || e != nil || len(jobs) != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "dirty checkout") || !strings.Contains(stderr.String(), "git stash -u") || strings.Contains(stderr.String(), "job ID:") {
		t.Fatalf("exit=%d jobs=%+v err=%v stdout=%s stderr=%s", exitCode(err), jobs, e, stdout.String(), stderr.String())
	}
}

func TestFailedSubmissionReportsInfrastructureAndAvoidsBlindRetry(t *testing.T) {
	local, _ := checkout(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strings.TrimPrefix(listener.Addr().String(), "127.0.0.1:")
	_ = listener.Close()
	c := command(t, local, "-p", port, "/bin/echo", "not-submitted")
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err = c.Run()
	if exitCode(err) != 125 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "before resubmitting") || !strings.Contains(stderr.String(), "jobs") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exitCode(err), stdout.String(), stderr.String())
	}
}

func TestHelpDocumentsPrefixAndExitPoliciesWithoutGit(t *testing.T) {
	c := command(t, t.TempDir(), "-h")
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	if exitCode(err) != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "executable [args...]") || !strings.Contains(stderr.String(), "125") || !strings.Contains(stderr.String(), "130") || !strings.Contains(stderr.String(), "124") || !strings.Contains(stderr.String(), "Options precede") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exitCode(err), stdout.String(), stderr.String())
	}
}

func TestInvalidPortAndTimeoutFailBeforeGitOrSubmission(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"-p", "0", "echo"}, "port must be"},
		{[]string{"-p", "65536", "echo"}, "port must be"},
		{[]string{"-timeout", "0", "echo"}, "timeout must be"},
		{[]string{"-timeout", "-1s", "echo"}, "timeout must be"},
	} {
		c := command(t, t.TempDir(), tc.args...)
		var stdout, stderr bytes.Buffer
		c.Stdout = &stdout
		c.Stderr = &stderr
		err := c.Run()
		if exitCode(err) != 2 || !strings.Contains(stderr.String(), tc.message) || stdout.Len() != 0 {
			t.Fatalf("args=%v exit=%d stdout=%s stderr=%s", tc.args, exitCode(err), stdout.String(), stderr.String())
		}
	}
}

func TestLiteralShellCompositionIsRejectedBeforeSubmission(t *testing.T) {
	c := command(t, t.TempDir(), "/bin/echo", "hello", "&&", "/bin/echo", "world")
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	if exitCode(err) != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "shell operator") || !strings.Contains(stderr.String(), "project script") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exitCode(err), stdout.String(), stderr.String())
	}
}

func TestExplicitSourceAndTimeoutCanBeSelectedOutsideCheckout(t *testing.T) {
	local, _ := checkout(t)
	git(t, local, "remote", "rename", "origin", "published")
	f := startWorker(t, startCoordinator(t))
	c := command(t, t.TempDir(), "-p", f.port, "-repository", local, "-remote", "published", "-branch", "main", "-timeout", "2m", "/bin/echo", "selected")
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exitCode(err), stdout.String(), stderr.String())
	}
	jobs, err := f.api.Jobs(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].Source.Branch != "main" || jobs[0].Timeout != 2*time.Minute || !strings.Contains(stdout.String(), "selected\n") {
		t.Fatalf("jobs=%+v err=%v stdout=%s", jobs, err, stdout.String())
	}
}
