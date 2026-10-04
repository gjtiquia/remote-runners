package main_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gjtiquia/remote-runners/internal/client"
	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gjtiquia/remote-runners/internal/testutil"
	"github.com/gorilla/websocket"
)

func buildServer(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "remote-runner-server")
	if out, err := exec.Command("go", "build", "-p", "1", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, out)
	}
	return binary
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

type serverProcess struct {
	logPath string
	command *exec.Cmd
	done    chan struct{}
	err     error // Read only after done closes.
}

func startServer(t *testing.T, binary string, port int, args ...string) (*serverProcess, *client.Client) {
	t.Helper()
	args = append([]string{"-p", fmt.Sprint(port)}, args...)
	command := exec.Command(binary, args...)
	log, err := os.Create(filepath.Join(t.TempDir(), "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &serverProcess{command: command, done: make(chan struct{}), logPath: log.Name()}
	go func() { process.err = command.Wait(); close(process.done) }()
	t.Cleanup(func() {
		select {
		case <-process.done:
		default:
			command.Process.Kill()
			select {
			case <-process.done:
			case <-time.After(5 * time.Second):
				t.Error("server did not stop after kill")
			}
		}
	})
	c := testutil.Client(fmt.Sprintf("http://127.0.0.1:%d", port))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := c.Runners(ctx)
		cancel()
		if err == nil {
			return process, c
		}
		select {
		case <-process.done:
			out, _ := os.ReadFile(log.Name())
			t.Fatalf("server exited before ready: %v\n%s", process.err, out)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	out, _ := os.ReadFile(log.Name())
	t.Fatalf("server not ready: %s", out)
	return nil, nil
}

func TestServerLogsTimestampedListeningAndGracefulShutdown(t *testing.T) {
	process, _ := startServer(t, buildServer(t), freePort(t), "-output-dir", t.TempDir())
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.done:
		if process.err != nil {
			t.Fatal(process.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
	data, err := os.ReadFile(process.logPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, event := range []string{"remote-runner-server listening on", "shutdown requested", "shutdown complete"} {
		matched := false
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, event) && regexp.MustCompile(`^remote-runner-server: \d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `).MatchString(line) {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("missing timestamped server event %q:\n%s", event, out)
		}
	}
}

func TestServerServesCoordinatorAndGracefullyClosesOnSIGTERM(t *testing.T) {
	binary := buildServer(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".remote-runners", "output")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unrelated"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	process, c := startServer(t, binary, port)
	conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d/runner", port), testutil.Headers())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	info := protocol.RunnerInfo{ID: "test-runner", Priority: 50, MaxJobs: 1, Slots: 1, AvailableMemory: 2 << 30, MinMemory: 1 << 30}
	if err := conn.WriteJSON(protocol.Message{Type: "register", Runner: &info}); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var registered protocol.Message
	if err := conn.ReadJSON(&registered); err != nil {
		t.Fatal(err)
	}
	if registered.Type != "registered" {
		t.Fatalf("registration: %+v", registered)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	job, err := c.Submit(ctx, protocol.Submission{Source: protocol.Source{Remote: "git@example.com:team/repo.git", Branch: "main"}, Args: []string{"echo", "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "running" {
		t.Fatalf("job not dispatched: %+v", job)
	}
	spools, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(spools) != 2 {
		t.Fatalf("default output directory not used: %+v", spools)
	}
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.done:
		if process.err != nil {
			t.Fatalf("graceful stop: %v", process.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not stop server")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "unrelated" {
		t.Fatalf("spool cleanup touched unrelated files or retained output: %v", entries)
	}
	if _, err := c.Runners(ctx); err == nil {
		t.Fatal("HTTP listener survived shutdown")
	}
	for {
		var message protocol.Message
		if err := conn.ReadJSON(&message); err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("runner socket did not close on shutdown")
			}
			break
		}
		if strings.EqualFold(message.Type, "registered") {
			t.Fatal("unexpected duplicate registration")
		}
	}
	fresh, newClient := startServer(t, binary, port)
	jobs, err := newClient.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runners, err := newClient.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 || len(runners) != 0 {
		t.Fatalf("restart retained state: jobs=%+v runners=%+v", jobs, runners)
	}
	if _, err := newClient.Job(ctx, job.ID); err == nil {
		t.Fatal("restart recovered old job")
	}
	if err := fresh.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fresh.done:
		if fresh.err != nil {
			t.Fatalf("restart stop: %v", fresh.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restart did not stop")
	}
}
