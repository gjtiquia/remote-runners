package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	id     string
	pane   string
	active bool
	used   time.Time
}
type activeJob struct{ cancelPath string }
type worker struct {
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
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint.String(), nil)
	if err != nil {
		return err
	}
	w := &worker{cfg: cfg, terminal: term, conn: conn, executable: executable, environment: os.Environ(), windows: windows, jobs: make(map[string]activeJob), disconnected: make(chan struct{})}
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
			if err = w.start(*message.Job); err != nil {
				var refused *noCapacityError
				if errors.As(err, &refused) {
					if e := w.send(protocol.Message{Type: "decline", JobID: message.Job.ID, Runner: &refused.info}); e != nil {
						return e
					}
				} else {
					code := 125
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
				if err = os.WriteFile(job.cancelPath, []byte("cancel\n"), 0600); err != nil {
					return fmt.Errorf("request cancellation: %w", err)
				}
			}
		}
	}
}
func (w *worker) disconnect() {
	w.disconnectOnce.Do(func() { close(w.disconnected); _ = w.conn.Close() })
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
		for key, candidate := range w.windows {
			if !candidate.active && candidate.id != w.terminal.window && (oldest == nil || candidate.used.Before(oldest.used)) {
				oldest = candidate
				source = key
			}
		}
		if oldest == nil {
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
	descriptor := JobDescriptor{Roots: workspace.Roots{Repos: w.cfg.Repos, Worktrees: w.cfg.Worktrees}, Job: job, Environment: w.environment, OutputPath: filepath.Join(dir, "output"), ResultPath: filepath.Join(dir, "result.json"), CancelPath: filepath.Join(dir, "cancel")}
	data, err := json.Marshal(descriptor)
	if err != nil {
		return err
	}
	descriptorPath := filepath.Join(dir, "descriptor.json")
	if err = os.WriteFile(descriptorPath, data, 0600); err != nil {
		return err
	}
	// The helper is the actual foreground program in the tmux pane. It receives
	// terminal SIGINT, then cancels preparation/hooks/command process groups.
	used := time.Now()
	// Only this parent releases the private gate, after confirming the tag.
	// A crash before tagging leaves a visibly untagged, non-executing pane;
	// startup rejects it rather than overlooking a delayed self-tagging shell.
	gate := filepath.Join(dir, "launch-ready")
	command := "while [ ! -f " + quote(gate) + " ]; do sleep 0.05; done; exec " + quote(w.executable) + " job-exec " + quote(descriptorPath)
	var out []byte
	if window == nil {
		out, err = w.terminal.output("new-window", "-d", "-t", w.terminal.session+":", "-n", job.ID, "-P", "-F", "#{window_id}\t#{pane_id}", "/bin/sh", "-c", command)
		if err == nil {
			ids := strings.Fields(string(out))
			if len(ids) != 2 || !strings.HasPrefix(ids[0], "@") || !strings.HasPrefix(ids[1], "%") || ids[0] == w.terminal.window {
				w.disconnect() // An unconfirmed pane may exist; do not admit more work.
				return fmt.Errorf("invalid job window/pane IDs: %q", out)
			}
			window = &jobWindow{id: ids[0], pane: ids[1]}
			w.windows[key] = window
		}
	} else {
		// Without -k tmux itself refuses to replace a live pane, even if it
		// changed after our inspection. Never destroy another inspection pane.
		out, err = w.terminal.output("respawn-pane", "-t", window.pane, "/bin/sh", "-c", command)
	}
	if err != nil {
		w.disconnect() // Creation/respawn may have succeeded despite a lost reply.
		return fmt.Errorf("launch job window: %w: %s", err, out)
	}
	window.active = true
	window.used = used
	w.jobs[job.ID] = activeJob{cancelPath: descriptor.CancelPath}
	if err = w.terminal.tagWindow(key, window); err == nil {
		err = os.WriteFile(gate, nil, 0600)
	}
	if err != nil {
		w.disconnect() // Keep the pending pane occupied; only an operator may stop it.
		return fmt.Errorf("confirm job window launch (stop pending pane manually): %w", err)
	}
	go w.monitor(job, window, descriptor)
	return nil
}
func (w *worker) monitor(job protocol.Job, window *jobWindow, d JobDescriptor) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var result protocol.Message
	for range ticker.C {
		// A published result alone is not proof that the foreground helper has
		// exited. Keep the window active until its exact pane is dead.
		// Inaccessible results or unqueryable tmux are not proof that the
		// helper stopped. Keep the slot/window active until completion is known.
		if window.pane == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		out, e := w.terminal.command(ctx, "display-message", "-p", "-t", window.pane, "#{pane_dead}").Output()
		cancel()
		if e == nil && strings.TrimSpace(string(out)) == "1" {
			// A dead pane proves no more result writes can occur. Read only
			// after that observation to avoid racing atomic result publication.
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
	w.mu.Lock()
	window.active = false
	delete(w.jobs, job.ID)
	w.mu.Unlock()
	// Output is transferred only after completion, bounded per message and never
	// accumulated in RAM. Broken transport does not affect the helper's lifetime.
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
	}
	result.Type = "complete"
	result.JobID = job.ID
	_ = w.sendCapacity(result)
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
