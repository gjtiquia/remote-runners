package runner_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gorilla/websocket"
)

// The peer exercises the real worker's public wire protocol and tmux execution,
// including stale assignments that an ordinary coordinator avoids sending.
func workerProtocol(t *testing.T, minMemory uint64) (integration, func() protocol.Message, func(protocol.Message)) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	connections := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connections <- conn
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	bin := binary(t)
	dir, err := os.MkdirTemp("", "rr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	home := filepath.Join(dir, "home")
	cfgdir := filepath.Join(home, ".remote-runners")
	if err = os.MkdirAll(cfgdir, 0700); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"coordinator_url": server.URL, "name": "admission", "max_jobs": 1, "max_windows": 1, "min_memory": minMemory, "repos": filepath.Join(dir, "repos"), "worktrees": filepath.Join(dir, "worktrees")}
	data, _ := json.Marshal(config)
	if err = os.WriteFile(filepath.Join(cfgdir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "sock")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
	command := "exec env HOME=" + shellQuote(home) + " " + shellQuote(bin)
	out, err := exec.Command("tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-s", "worker", "-P", "-F", "#{session_id}", command).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux: %v %s", err, out)
	}
	f := integration{socket: socket, session: strings.TrimSpace(string(out))}
	var conn *websocket.Conn
	select {
	case conn = <-connections:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never connected")
	}
	read := func() protocol.Message {
		t.Helper()
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var m protocol.Message
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	send := func(m protocol.Message) {
		t.Helper()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := conn.WriteJSON(m); err != nil {
			t.Fatal(err)
		}
	}
	return f, read, send
}

func TestWorkerDeclinesUnstartedAssignmentAndReportsFreshCompletionCapacity(t *testing.T) {
	f, read, send := workerProtocol(t, 1)
	registration := read()
	if registration.Type != "register" || registration.Runner == nil || !registration.Runner.Available {
		t.Fatalf("registration: %+v", registration)
	}
	send(protocol.Message{Type: "registered", RegistrationID: "test-registration"})
	source := repository(t, "")
	first := protocol.Job{ID: "first", Submission: protocol.Submission{Source: protocol.Source{Remote: source, Branch: "main"}, Args: []string{"/bin/sh", "-c", "echo ADMITTED; sleep 30"}, Timeout: time.Minute}}
	send(protocol.Message{Type: "job", Job: &first})
	f.waitForTerminal(t, f.window(t, first.ID), "ADMITTED")
	second := first
	second.ID = "stale-assignment"
	second.Args = []string{"/bin/echo", "MUST_NOT_EXECUTE"}
	send(protocol.Message{Type: "job", Job: &second})
	declined := read()
	if declined.Type != "decline" || declined.JobID != second.ID || declined.RegistrationID != "test-registration" || declined.Runner == nil || declined.Runner.ActiveJobs != 1 || declined.Runner.Slots != 0 || declined.Runner.Available {
		t.Fatalf("unstarted work must decline with current occupancy, not terminal failure: %+v", declined)
	}
	send(protocol.Message{Type: "cancel", JobID: first.ID})
	for {
		complete := read()
		if complete.Type != "complete" {
			continue
		}
		if complete.JobID != first.ID || complete.State != "cancelled" || complete.Runner == nil || complete.Runner.ActiveJobs != 0 || complete.Runner.Slots != 1 || !complete.Runner.Available {
			t.Fatalf("completion must refresh local capacity: %+v", complete)
		}
		break
	}
	// Actual preparation failure is terminal, never a capacity decline/retry.
	second.Source.Remote = filepath.Join(t.TempDir(), "does-not-exist")
	send(protocol.Message{Type: "job", Job: &second})
	for {
		complete := read()
		if complete.Type == "output" {
			continue
		}
		if complete.Type != "complete" || complete.JobID != second.ID || complete.State != "failed" {
			t.Fatalf("preparation must fail, not decline: %+v", complete)
		}
		break
	}
	// A heartbeat is handled without a new registration after the failure.
	send(protocol.Message{Type: "heartbeat", Sequence: 1})
	heartbeat := read()
	if heartbeat.Type != "heartbeat_response" || heartbeat.RegistrationID != "test-registration" || heartbeat.Runner == nil || !heartbeat.Runner.Available || heartbeat.Runner.ActiveJobs != 0 || heartbeat.Runner.Slots != 1 {
		t.Fatalf("worker changed registration: %+v", heartbeat)
	}
}
