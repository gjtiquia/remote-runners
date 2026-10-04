package runner_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gjtiquia/remote-runners/internal/runner"
	"github.com/gjtiquia/remote-runners/internal/workspace"
)

// Companion coverage for the job-pane lifecycle: independently observe the
// actual helper's stdout/stderr rather than inferring stream routing from tmux.
func TestJobExecKeepsProgressOnStderrAndOutOfStdoutPayload(t *testing.T) {
	bin := binary(t)
	base := t.TempDir()
	executable := filepath.Join(base, "sh\nFORGED_EXECUTABLE")
	if err := os.Symlink("/bin/sh", executable); err != nil {
		t.Fatal(err)
	}
	d := runner.JobDescriptor{
		Roots: workspace.Roots{Repos: filepath.Join(base, "repos"), Worktrees: filepath.Join(base, "worktrees")},
		Job: protocol.Job{ID: "helper\nFORGED_JOB", Submission: protocol.Submission{
			Source: protocol.Source{Remote: repository(t, ""), Branch: "main"},
			Args:   []string{executable, "-c", `echo STDOUT_PAYLOAD; echo STDERR_PAYLOAD >&2; : "$1"`, "command", "PRIVATE_ARGUMENT"},
		}},
		OutputPath: filepath.Join(base, "output"),
		ResultPath: filepath.Join(base, "result"),
		CancelPath: filepath.Join(base, "cancel"),
	}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := filepath.Join(base, "descriptor.json")
	if err = os.WriteFile(descriptor, data, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := exec.Command(bin, "job-exec", descriptor)
	command.Env = append(os.Environ(), "PRIVATE_ENVIRONMENT=ENVIRONMENT_SECRET")
	command.Stdout, command.Stderr = &stdout, &stderr
	if err = command.Run(); err != nil {
		t.Fatalf("helper: %v\n%s\n%s", err, &stdout, &stderr)
	}
	output, err := os.ReadFile(d.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != string(output) || !strings.Contains(stdout.String(), "STDOUT_PAYLOAD\nSTDERR_PAYLOAD") || strings.Contains(stdout.String(), "remote-runner-job ") {
		t.Fatalf("stdout must be precisely captured job payload:\nstdout=%s\ndisk=%s", &stdout, output)
	}
	assertProgress(t, stderr.String(), "remote-runner-job", `job="helper\nFORGED_JOB"`)
	assertProgress(t, stderr.String(), "remote-runner-job", `executing executable="sh\nFORGED_EXECUTABLE"`)
	assertProgress(t, stderr.String(), "remote-runner-job", `completed state="succeeded" exit=0`)
	for _, private := range []string{"STDOUT_PAYLOAD", "STDERR_PAYLOAD", "PRIVATE_ARGUMENT", "PRIVATE_ENVIRONMENT", "ENVIRONMENT_SECRET", "\nFORGED_"} {
		if strings.Contains(stderr.String(), private) {
			t.Fatalf("helper diagnostics leaked payload, environment, argv or a forged line %q:\n%s", private, &stderr)
		}
	}
}
