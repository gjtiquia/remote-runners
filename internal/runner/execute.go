package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gjtiquia/remote-runners/internal/workspace"
)

// JobDescriptor is the private job-exec CLI input. Paths must be runner-owned.
// Environment is captured from the runner, not inherited from the tmux server.
type JobDescriptor struct {
	Roots       workspace.Roots `json:"roots"`
	Job         protocol.Job    `json:"job"`
	Environment []string        `json:"environment"`
	OutputPath  string          `json:"output_path"`
	ResultPath  string          `json:"result_path"`
	CancelPath  string          `json:"cancel_path"`
}

// JobExec runs the descriptor in the foreground of its terminal. Completion is
// atomic at ResultPath; command failure is a result, not helper infrastructure failure.
func JobExec(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var d JobDescriptor
	if err = json.Unmarshal(data, &d); err != nil {
		return err
	}
	// The descriptor contains the inherited environment and may contain secrets.
	if err = os.Remove(path); err != nil {
		return err
	}
	// Only terminal identity belongs to the new job pane. All tool/user variables
	// come from the launching runner, never the tmux server's stale environment.
	tmux, pane := os.Getenv("TMUX"), os.Getenv("TMUX_PANE")
	os.Clearenv()
	for _, entry := range d.Environment {
		if k, v, ok := strings.Cut(entry, "="); ok {
			_ = os.Setenv(k, v)
		}
	}
	if tmux != "" {
		_ = os.Setenv("TMUX", tmux)
	}
	if pane != "" {
		_ = os.Setenv("TMUX_PANE", pane)
	}
	output, err := os.OpenFile(d.OutputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	writer := io.MultiWriter(output, os.Stdout)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	timeout := d.Job.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, e := os.Stat(d.CancelPath); e == nil {
					cancel()
					return
				}
			}
		}
	}()
	result := protocol.Message{Type: "complete", JobID: d.Job.ID, State: "failed"}
	prepared, err := workspace.Prepare(ctx, d.Roots, d.Job.Source, writer)
	result.Commit = prepared.Commit
	code := 1
	if err == nil {
		err = runHooks(ctx, prepared, writer)
	}
	if err == nil {
		code, err = execute(ctx, prepared.Root, d.Job.Args, writer)
	}
	if err == nil {
		result.State = "succeeded"
	} else {
		result.Error = err.Error()
		fmt.Fprintln(writer, err)
	}
	if ctx.Err() == context.DeadlineExceeded {
		result.State = "timed_out"
		code = 124
		result.Error = ctx.Err().Error()
	} else if ctx.Err() != nil {
		result.State = "cancelled"
		code = 130
		result.Error = ctx.Err().Error()
	}
	result.ExitCode = &code
	if _, e := fmt.Fprintf(writer, "\n[remote-runner: %s, exit %d]\n", result.State, code); e != nil {
		code = 125
		result.State = "failed"
		result.Error = "output: " + e.Error()
	}
	if e := output.Close(); e != nil {
		code = 125
		result.State = "failed"
		result.Error = "output close: " + e.Error()
	}
	data, err = json.Marshal(result)
	if err != nil {
		return err
	}
	if err = os.WriteFile(d.ResultPath+".tmp", data, 0600); err != nil {
		return err
	}
	return os.Rename(d.ResultPath+".tmp", d.ResultPath)
}

// Hook configuration is committed at the worktree root as .remote-runner.json.
type projectHooks struct {
	AfterCreateWorktreeCommand string
	BeforeJobCommand           string
}

func runHooks(ctx context.Context, prepared workspace.Prepared, output io.Writer) error {
	path := filepath.Join(prepared.Root, ".remote-runner.json")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	// Untracked files intentionally survive workspace reset; they must not turn
	// into project hook configuration merely because a previous job wrote one.
	// Match workspace.runGit's routing exclusions for runner-owned Git queries.
	// User hooks and commands still receive the complete inherited environment.
	routing := map[string]bool{"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_COMMON_DIR": true, "GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_PREFIX": true, "GIT_NAMESPACE": true}
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !routing[key] {
			env = append(env, entry)
		}
	}
	code, err := executeWithEnvironment(ctx, prepared.Root, []string{"git", "ls-files", "--error-unmatch", "--", ".remote-runner.json"}, io.Discard, env)
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && code == 1 && exit.ExitCode() == 1 && ctx.Err() == nil {
			return nil
		}
		return fmt.Errorf("check committed hook configuration: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var hooks projectHooks
	if err = json.Unmarshal(data, &hooks); err != nil {
		return fmt.Errorf(".remote-runner.json: %w", err)
	}
	commands := []string{}
	if prepared.Created {
		commands = append(commands, hooks.AfterCreateWorktreeCommand)
	}
	commands = append(commands, hooks.BeforeJobCommand)
	for _, command := range commands {
		if command != "" {
			if _, err = execute(ctx, prepared.Root, []string{"/bin/sh", "-c", command}, output); err != nil {
				return fmt.Errorf("project hook: %w", err)
			}
		}
	}
	return nil
}

func execute(ctx context.Context, root string, args []string, output io.Writer) (int, error) {
	return executeWithEnvironment(ctx, root, args, output, nil)
}

func executeWithEnvironment(ctx context.Context, root string, args []string, output io.Writer, env []string) (int, error) {
	if len(args) == 0 {
		return 1, fmt.Errorf("job args must contain an executable")
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = root
	cmd.Env = env
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if e, ok := err.(*exec.ExitError); ok {
		if s, ok := e.Sys().(syscall.WaitStatus); ok && s.Signaled() {
			return 128 + int(s.Signal()), err
		}
		return e.ExitCode(), err
	}
	return 1, err
}
