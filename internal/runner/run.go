package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gjtiquia/remote-runners/internal/repository"
	"github.com/gjtiquia/remote-runners/internal/workspace"
	"github.com/gorilla/websocket"
)

// ErrNoCapacity is only returned before preparation or execution has started.
// It returns an unstarted assignment to the coordinator, never retries a job.
var ErrNoCapacity = errors.New("no local execution capacity")

// Keep the failed admission observation: capacity may recover before the
// decline reaches the writer, but that must not invalidate the refusal.
type noCapacityError struct{ info protocol.RunnerInfo }

func (e *noCapacityError) Error() string { return ErrNoCapacity.Error() }
func (e *noCapacityError) Unwrap() error { return ErrNoCapacity }

type jobWindow struct {
	id         string
	pane       string
	shellToken string
	active     bool
	used       time.Time
}
type activeJob struct{ cancelPath string }
type worker struct {
	logger         *log.Logger
	cfg            Config
	terminal       terminal
	conn           *websocket.Conn
	registration   string
	executable     string
	environment    []string
	mu             sync.Mutex
	windows        map[protocol.Source]*jobWindow
	jobs           map[string]activeJob
	writeMu        sync.Mutex
	disconnected   chan struct{}
	disconnectOnce sync.Once
}

// Run connects once; context cancellation and transport loss disconnect the
// worker but deliberately leave jobs running. Operator teardown is mandatory
// before restarting; only completed UI windows are discovered, never orphan
// executions. There is no automatic re-registration.
func Run(ctx context.Context, cfg Config) error {
	logger := progressLogger("remote-runner")
	logger.Printf("runner=%q checking tmux/config roots repos=%q worktrees=%q", cfg.Name, cfg.Repos, cfg.Worktrees)
	if err := cfg.validate(); err != nil {
		return err
	}
	term, err := validateTerminal(ctx)
	if err != nil {
		return err
	}
	windows, err := term.retainedWindows()
	if err != nil {
		return fmt.Errorf("cannot register runner: %w", err)
	}
	if err = term.trimRetainedWindows(windows, cfg.MaxWindows); err != nil {
		return fmt.Errorf("cannot register runner: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	endpoint, err := url.Parse(cfg.CoordinatorURL)
	if err != nil {
		return err
	}
	if endpoint.Scheme == "http" {
		endpoint.Scheme = "ws"
	}
	if endpoint.Scheme == "https" {
		endpoint.Scheme = "wss"
	}
	if endpoint.Scheme != "ws" && endpoint.Scheme != "wss" {
		return fmt.Errorf("coordinator_url must use ws/wss or http/https")
	}
	if endpoint.Path == "" || endpoint.Path == "/" {
		endpoint.Path = "/runner"
	}
	logger.Printf("runner=%q connecting", cfg.Name)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint.String(), nil)
	if err != nil {
		return err
	}
	w := &worker{logger: logger, cfg: cfg, terminal: term, conn: conn, executable: executable, environment: os.Environ(), windows: windows, jobs: make(map[string]activeJob), disconnected: make(chan struct{})}
	defer w.disconnect()
	go func() {
		select {
		case <-ctx.Done():
			w.disconnect()
		case <-w.disconnected:
		}
	}()
	if err = w.sendCapacity(protocol.Message{Type: "register"}); err != nil {
		return err
	}
	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var registered protocol.Message
	if err = conn.ReadJSON(&registered); err != nil {
		return err
	}
	if registered.Type != "registered" || registered.RegistrationID == "" {
		return fmt.Errorf("coordinator did not acknowledge registration")
	}
	w.registration = registered.RegistrationID
	w.progress("registered registration=%q session=%q window=%q", w.registration, term.session, term.window)
	_ = conn.SetReadDeadline(time.Time{})
	for {
		var message protocol.Message
		if err = conn.ReadJSON(&message); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("coordinator disconnected (local jobs continue): %w", err)
		}
		switch message.Type {
		case "heartbeat":
			if err = w.sendCapacity(protocol.Message{Type: "heartbeat_response", Sequence: message.Sequence}); err != nil {
				return err
			}
		case "job":
			if message.Job == nil {
				return fmt.Errorf("assignment missing job")
			}
			w.progress("job=%q received project=%q branch=%q", message.Job.ID, projectLabel(message.Job.Source.Remote), message.Job.Source.Branch)
			if err = w.start(*message.Job); err != nil {
				var refused *noCapacityError
				if errors.As(err, &refused) {
					w.progress("job=%q capacity refused active_jobs=%d slots=%d", message.Job.ID, refused.info.ActiveJobs, refused.info.Slots)
					if e := w.send(protocol.Message{Type: "decline", JobID: message.Job.ID, Runner: &refused.info}); e != nil {
						return e
					}
				} else {
					code := 125
					w.progress("job=%q completed state=%q exit=125 (launch failed)", message.Job.ID, "failed")
					if e := w.sendCapacity(protocol.Message{Type: "complete", JobID: message.Job.ID, State: "failed", ExitCode: &code, Error: err.Error()}); e != nil {
						return e
					}
				}
			}
		case "cancel":
			w.mu.Lock()
			job, ok := w.jobs[message.JobID]
			w.mu.Unlock()
			if ok {
				w.progress("job=%q cancel requested", message.JobID)
				if err = os.WriteFile(job.cancelPath, []byte("cancel\n"), 0600); err != nil {
					return fmt.Errorf("request cancellation: %w", err)
				}
			}
		}
	}
}
func (w *worker) disconnect() {
	w.disconnectOnce.Do(func() {
		w.progress("disconnected; local jobs continue")
		close(w.disconnected)
		_ = w.conn.Close()
	})
}

// Sample capacity in wire order, not before waiting for a concurrent writer.
// Declines instead use send with the original failed admission snapshot.
func (w *worker) sendCapacity(message protocol.Message) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	info := w.info()
	message.Runner = &info
	return w.sendLocked(message)
}
func (w *worker) send(message protocol.Message) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	return w.sendLocked(message)
}
func (w *worker) sendLocked(message protocol.Message) error {
	select {
	case <-w.disconnected:
		return fmt.Errorf("worker disconnected")
	default:
	}
	message.RegistrationID = w.registration
	_ = w.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := w.conn.WriteJSON(message)
	if err != nil {
		w.disconnect()
	}
	return err
}
func (w *worker) info() protocol.RunnerInfo {
	w.mu.Lock()
	active := len(w.jobs)
	w.mu.Unlock()
	memory, err := availableMemory()
	if err != nil {
		memory = 0
	} // Measurement errors must never advertise permissive RAM.
	slots := min(w.cfg.MaxJobs, w.cfg.MaxWindows) - active
	if slots < 0 {
		slots = 0
	}
	return protocol.RunnerInfo{ID: w.cfg.Name, Name: w.cfg.Name, Priority: w.cfg.Priority, MaxJobs: w.cfg.MaxJobs, MinMemory: w.cfg.MinMemory, AvailableMemory: memory, Slots: slots, ActiveJobs: active, Available: err == nil && memory >= w.cfg.MinMemory && slots > 0}
}
func (w *worker) start(job protocol.Job) error {
	capacity := w.info()
	if !capacity.Available {
		return &noCapacityError{info: capacity}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.jobs[job.ID]; ok {
		return fmt.Errorf("duplicate job assignment")
	}
	identity, err := repository.Identity(job.Source.Remote)
	if err != nil {
		return err
	}
	key := protocol.Source{Remote: identity, Branch: job.Source.Branch}
	window := w.windows[key]
	if window != nil && window.active {
		return fmt.Errorf("worktree already active")
	}
	if window != nil {
		if err := w.terminal.verifyCompletedWindow(key, window); err != nil {
			return err
		}
	}
	if window == nil && len(w.windows) >= w.cfg.MaxWindows {
		var oldest *jobWindow
		var source protocol.Source
		var protected error
		for key, candidate := range w.windows {
			if candidate.active || candidate.id == w.terminal.window {
				continue
			}
			if err := w.terminal.verifyCompletedWindow(key, candidate); err != nil {
				protected = err
				continue
			}
			if oldest == nil || candidate.used.Before(oldest.used) {
				oldest = candidate
				source = key
			}
		}
		if oldest == nil {
			if protected != nil {
				return protected // Manual activity never waits/requeues for a window.
			}
			capacity.Available = false
			capacity.Slots = 0
			return &noCapacityError{info: capacity}
		}
		if err := w.terminal.evictCompletedWindow(source, oldest); err != nil {
			return err
		}
		delete(w.windows, source)
	}
	dirRoot := filepath.Join(w.cfg.Worktrees, ".remote-runner-jobs")
	if err := os.MkdirAll(dirRoot, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(dirRoot, "job-")
	if err != nil {
		return err
	}
	descriptor := JobDescriptor{Roots: workspace.Roots{Repos: w.cfg.Repos, Worktrees: w.cfg.Worktrees}, Job: job, OutputPath: filepath.Join(dir, "output"), ResultPath: filepath.Join(dir, "result.json"), CancelPath: filepath.Join(dir, "cancel"), ProcessPath: filepath.Join(dir, "helper.pid"), ReturnedPath: filepath.Join(dir, "shell-returned"), ShellScriptPath: filepath.Join(dir, "shell-result")}
	data, err := json.Marshal(descriptor)
	if err != nil {
		return err
	}
	descriptorPath := filepath.Join(dir, "descriptor.json")
	if err = os.WriteFile(descriptorPath, data, 0600); err != nil {
		return err
	}
	windowAction := "reused"
	if window == nil {
		windowAction = "new"
		window, err = w.terminal.newShellWindow(job.ID, dir, w.environment)
		if window == nil {
			// Unsupported configuration is a definite early failure. Only lost
			// creation replies require disconnecting to avoid uncertain admission.
			if errors.Is(err, errShellLaunchUncertain) {
				w.disconnect()
			}
			return err
		}
		w.windows[key] = window
		if err != nil {
			window.used = time.Now()
			if tagErr := w.terminal.tagWindow(key, window); tagErr != nil {
				w.disconnect()
				return tagErr
			}
			return err
		}
	}
	w.progress("job=%q %s window=%q pane=%q", job.ID, windowAction, window.id, window.pane)
	window.used = time.Now()
	// Confirm UI ownership before sending anything. A parent crash before this
	// point leaves only an untagged shell, never a delayed executable launch.
	if err = w.terminal.tagWindow(key, window); err != nil {
		w.disconnect()
		return fmt.Errorf("confirm job window ownership: %w", err)
	}
	// The helper inherits this interactive shell's environment. After it returns,
	// put the shell in the prepared worktree and preserve the command exit status.
	// A stopped helper must not clear the job marker or release its execution slot.
	command := "__rr_pending_script=" + quote(descriptor.ShellScriptPath) + "; __rr_pending_pid=" + quote(descriptor.ProcessPath) + "; " +
		quote(w.executable) + " job-exec " + quote(descriptorPath) + "; __rr_helper_status=$?; __rr_job_status=$__rr_helper_status; " +
		"if (( __rr_helper_status < 128 )) || { [[ -r \"$__rr_pending_pid\" ]] && ! builtin kill -0 \"$(<\"$__rr_pending_pid\")\" 2>/dev/null; }; then " +
		"if [[ -f \"$__rr_pending_script\" ]]; then builtin source \"$__rr_pending_script\"; fi; " +
		"command " + quote(w.terminal.binary) + " -S " + quote(w.terminal.socket) + " set-option -p -t \"$TMUX_PANE\" " + shellJobOption + " 0; " +
		"unset __rr_pending_script __rr_pending_pid; fi; " +
		"builtin printf '%s\\n' \"$__rr_helper_status\" > " + quote(descriptor.ReturnedPath) + "; (builtin exit \"$__rr_job_status\")"
	if err = w.terminal.sendShellCommand(window, command); err != nil {
		// A definite busy refusal has not started anything. A transport error may
		// have sent keys: disconnect rather than retrying an uncertain launch.
		if !errors.Is(err, ErrShellBusy) {
			w.disconnect()
		}
		return err
	}
	window.active = true
	w.jobs[job.ID] = activeJob{cancelPath: descriptor.CancelPath}
	w.progress("job=%q running helper window=%q pane=%q", job.ID, window.id, window.pane)
	go w.monitor(job, window, descriptor)
	return nil
}
func (w *worker) monitor(job protocol.Job, window *jobWindow, d JobDescriptor) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var result protocol.Message
	for range ticker.C {
		// A prompt or shell-returned marker alone is insufficient: job control can
		// return a stopped helper to the prompt. Confirm its known PID has exited.
		// The shell itself stays alive across jobs; no pane-death/respawn lifecycle.
		if window.pane == "" {
			continue
		}
		returnedData, returned := os.ReadFile(d.ReturnedPath)
		returnedCode, codeErr := strconv.Atoi(strings.TrimSpace(string(returnedData)))
		pidData, pidErr := os.ReadFile(d.ProcessPath)
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidData)))
		helperExited := pidErr == nil && parseErr == nil && pid > 0 && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		out, e := w.terminal.command(ctx, "display-message", "-p", "-t", window.pane, "#{pane_dead}").Output()
		cancel()
		paneDead := e == nil && strings.TrimSpace(string(out)) == "1"
		// A normal (<128) return without a PID means the executable failed before
		// helper initialization, not a stopped foreground process.
		neverInitialized := os.IsNotExist(pidErr) && codeErr == nil && returnedCode >= 0 && returnedCode < 128
		if (returned == nil && (helperExited || neverInitialized)) || (paneDead && (helperExited || os.IsNotExist(pidErr))) {
			// Read after process exit, so atomic result publication is complete.
			data, err := os.ReadFile(d.ResultPath)
			if err == nil {
				if err = json.Unmarshal(data, &result); err != nil {
					result = protocol.Message{State: "failed", Error: "invalid helper result: " + err.Error()}
				}
			} else {
				result = protocol.Message{State: "failed", Error: "job helper exited without a result; inspect job window and disk output"}
			}
			break
		}
	}
	// Job control may have returned the original shell wrapper before the helper
	// exited (Ctrl-Z, then fg). Only this observed-exit path may clear that lease.
	if _, err := w.terminal.output("set-option", "-p", "-t", window.pane, shellJobOption, "0"); err != nil {
		w.progress("job=%q cannot clear completed shell lease; inspect window=%q", job.ID, window.id)
	}
	w.mu.Lock()
	window.active = false
	delete(w.jobs, job.ID)
	w.mu.Unlock()
	// Output is transferred only after completion, bounded per message and never
	// accumulated in RAM. Broken transport does not affect the helper's lifetime.
	w.progress("job=%q output transfer starting", job.ID)
	file, err := os.Open(d.OutputPath)
	if err == nil {
		buffer := make([]byte, 32<<10)
		for {
			n, e := file.Read(buffer)
			if n > 0 {
				if err = w.send(protocol.Message{Type: "output", JobID: job.ID, Data: buffer[:n]}); err != nil {
					break
				}
			}
			if e != nil {
				if e != io.EOF {
					err = e
				}
				break
			}
		}
		_ = file.Close()
	}
	if err != nil {
		code := 125
		result.State = "failed"
		result.ExitCode = &code
		result.Error = "completed output transfer: " + err.Error()
		w.progress("job=%q output transfer failed", job.ID)
	} else {
		w.progress("job=%q output transfer finished", job.ID)
	}
	result.Type = "complete"
	result.JobID = job.ID
	w.progress("job=%q completed state=%q exit=%s commit=%q window=%q pane=%q", job.ID, result.State, exitLabel(result.ExitCode), result.Commit, window.id, window.pane)
	_ = w.sendCapacity(result)
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
