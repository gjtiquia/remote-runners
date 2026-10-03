package runner_test

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gjtiquia/remote-runners/internal/protocol"
)

func assertProgress(t *testing.T, logs, component, text string) {
	t.Helper()
	stamp := regexp.MustCompile(`^` + regexp.QuoteMeta(component) + ` \d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, text) && stamp.MatchString(line) {
			return
		}
	}
	t.Fatalf("missing timestamped %s progress %q:\n%s", component, text, logs)
}

func TestRunnerReportsDisconnectWhileLocalJobContinues(t *testing.T) {
	f := startRunner(t)
	f.tmux(t, "set-option", "-w", "-t", f.session+":0", "remain-on-exit", "on")
	job := f.submit(t, "/bin/sh", "-c", "echo DISCONNECT_READY; sleep 1; echo LOCAL_JOB_FINISHED")
	window := f.window(t, job.ID)
	f.waitForTerminal(t, window, "DISCONNECT_READY")
	if err := f.stopCoordinator(); err != nil {
		t.Fatal(err)
	}
	f.waitForTerminal(t, f.session+":0", "disconnected; local jobs continue")
	logs := f.tmux(t, "capture-pane", "-p", "-J", "-S", "-", "-t", f.session+":0")
	assertProgress(t, logs, "remote-runner", `runner="test" disconnected; local jobs continue`)
	f.waitForTerminal(t, window, "LOCAL_JOB_FINISHED")
	f.waitForTerminal(t, window, "[remote-runner: succeeded, exit 0]")
}

func TestRunnerConsoleReportsCancellationAndJobPaneOutcome(t *testing.T) {
	f := startRunner(t)
	job := f.submit(t, "/bin/sh", "-c", "echo CANCEL_READY; sleep 30")
	window := f.window(t, job.ID)
	f.waitForTerminal(t, window, "CANCEL_READY")
	if err := f.client.Cancel(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	result, output := f.wait(t, job.ID)
	if result.State != "cancelled" || result.ExitCode == nil || *result.ExitCode != 130 || strings.Contains(output, "remote-runner-job ") {
		t.Fatalf("cancellation/output changed: %+v %s", result, output)
	}
	logs := f.tmux(t, "capture-pane", "-p", "-J", "-S", "-", "-t", f.session+":0")
	assertProgress(t, logs, "remote-runner", fmt.Sprintf("job=%q cancel requested", job.ID))
	logs = f.tmux(t, "capture-pane", "-p", "-J", "-S", "-", "-t", window)
	assertProgress(t, logs, "remote-runner-job", `completed state="cancelled" exit=130`)
}

func TestJobPaneShowsPreparationHooksAndCompletionWithoutContaminatingOutput(t *testing.T) {
	f := startRunner(t)
	f.source = repository(t, `{"AfterCreateWorktreeCommand":"echo CREATE_PAYLOAD","BeforeJobCommand":"echo BEFORE_PAYLOAD >&2"}`)
	job := f.submit(t, "/bin/sh", "-c", `printf 'WORKTREE_ROOT=%s\n' "$PWD"; echo STDOUT_PAYLOAD; echo STDERR_PAYLOAD >&2; : "$1"`, "command", "PRIVATE_ARGUMENT")
	result, output := f.wait(t, job.ID)
	if result.State != "succeeded" || !strings.Contains(output, "CREATE_PAYLOAD\nBEFORE_PAYLOAD") || !strings.Contains(output, "STDOUT_PAYLOAD\nSTDERR_PAYLOAD") || !strings.Contains(output, "[remote-runner: succeeded, exit 0]") {
		t.Fatalf("hook/command payload or footer changed: %+v %s", result, output)
	}
	_, rootLine, ok := strings.Cut(output, "WORKTREE_ROOT=")
	if !ok {
		t.Fatalf("missing command root: %s", output)
	}
	root, _, _ := strings.Cut(rootLine, "\n")
	window := f.window(t, job.ID)
	pane := f.tmux(t, "display-message", "-p", "-t", window, "#{pane_id}")
	logs := f.tmux(t, "capture-pane", "-p", "-J", "-S", "-", "-t", window)
	for _, text := range []string{
		fmt.Sprintf("job=%q pane=%q checking Git/preparing worktree", job.ID, pane),
		fmt.Sprintf("worktree ready root=%q commit=%q", root, result.Commit),
		fmt.Sprintf("running creation hook root=%q", root),
		fmt.Sprintf("running before-job hook root=%q", root),
		fmt.Sprintf("executing executable=\"sh\" root=%q", root),
		fmt.Sprintf("completed state=\"succeeded\" exit=0 commit=%q", result.Commit),
	} {
		assertProgress(t, logs, "remote-runner-job", text)
	}
	if strings.Contains(output, "remote-runner-job ") || strings.Contains(logs, "PRIVATE_ARGUMENT") || strings.Contains(logs, "from-runner") {
		t.Fatalf("diagnostics contaminated job output or exposed argv/environment:\noutput=%s\npane=%s", output, logs)
	}
}

func TestRunnerLogsCapacityRefusalWithQuotedLabelsWithoutRemoteCredentials(t *testing.T) {
	f, read, send := workerProtocol(t, ^uint64(0))
	read()
	send(protocol.Message{Type: "registered", RegistrationID: "test-registration"})
	job := protocol.Job{ID: "unstarted\nFORGED_ID", Submission: protocol.Submission{
		Source: protocol.Source{Remote: "https://USER_SECRET:PASSWORD_SECRET@example.invalid/team/project%0AFORGED_PROJECT.git?token=QUERY_SECRET", Branch: "main\nFORGED_BRANCH"},
		Args:   []string{"/bin/echo", "PRIVATE_ARGUMENT"},
	}}
	send(protocol.Message{Type: "job", Job: &job})
	if refused := read(); refused.Type != "decline" || refused.JobID != job.ID {
		t.Fatalf("expected unstarted refusal: %+v", refused)
	}
	logs := f.tmux(t, "capture-pane", "-p", "-J", "-S", "-", "-t", f.session+":0")
	assertProgress(t, logs, "remote-runner", fmt.Sprintf("job=%q received project=%q branch=%q", job.ID, "project\nFORGED_PROJECT", job.Source.Branch))
	assertProgress(t, logs, "remote-runner", fmt.Sprintf("job=%q capacity refused", job.ID))
	for _, secret := range []string{"USER_SECRET", "PASSWORD_SECRET", "QUERY_SECRET", "example.invalid", "PRIVATE_ARGUMENT", "\nFORGED_"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("unsafe diagnostics leaked %q:\n%s", secret, logs)
		}
	}
}

func TestRunnerConsoleReportsJobLifecycleWithoutPayloadOrHeartbeatSpam(t *testing.T) {
	f := startRunner(t)
	job := f.submit(t, "/bin/sh", "-c", `echo STDOUT_PAYLOAD; echo STDERR_PAYLOAD >&2; : "$1"`, "command", "PRIVATE_ARGUMENT")
	result, output := f.wait(t, job.ID)
	if result.State != "succeeded" || !strings.Contains(output, "STDOUT_PAYLOAD\nSTDERR_PAYLOAD") {
		t.Fatalf("job output changed: %+v %s", result, output)
	}
	window := f.window(t, job.ID)
	pane := f.tmux(t, "display-message", "-p", "-t", window, "#{pane_id}")
	logs := f.tmux(t, "capture-pane", "-p", "-J", "-S", "-", "-t", f.session+":0")
	for _, text := range []string{
		`runner="test" checking tmux`,
		`runner="test" connecting`,
		`runner="test" registered registration=`,
		fmt.Sprintf("job=%q received project=%q branch=\"main\"", job.ID, filepath.Base(f.source)),
		fmt.Sprintf("job=%q new window=%q pane=%q", job.ID, window, pane),
		fmt.Sprintf("job=%q running helper window=%q pane=%q", job.ID, window, pane),
		fmt.Sprintf("job=%q completed state=\"succeeded\" exit=0 commit=%q", job.ID, result.Commit),
		fmt.Sprintf("job=%q output transfer starting", job.ID),
		fmt.Sprintf("job=%q output transfer finished", job.ID),
	} {
		assertProgress(t, logs, "remote-runner", text)
	}
	for _, private := range []string{"STDOUT_PAYLOAD", "STDERR_PAYLOAD", "PRIVATE_ARGUMENT", "from-runner", "heartbeat"} {
		if strings.Contains(logs, private) {
			t.Fatalf("runner diagnostics leaked payload/environment or heartbeat spam %q:\n%s", private, logs)
		}
	}
	reused := f.submit(t, "/bin/echo", "SECOND_PAYLOAD")
	f.wait(t, reused.ID)
	logs = f.tmux(t, "capture-pane", "-p", "-J", "-S", "-", "-t", f.session+":0")
	assertProgress(t, logs, "remote-runner", fmt.Sprintf("job=%q reused window=%q pane=%q", reused.ID, window, pane))
}
