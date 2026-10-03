package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gjtiquia/remote-runners/internal/client"
	"github.com/gjtiquia/remote-runners/internal/coordinator"
	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gorilla/websocket"
)

func buildUtils(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "remote-run-utils")
	if out, err := exec.Command("go", "build", "-p", "1", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build utils: %v\n%s", err, out)
	}
	return binary
}

func startCoordinator(t *testing.T) (*client.Client, string, string) {
	t.Helper()
	s, err := coordinator.New(coordinator.Options{OutputDir: t.TempDir(), HeartbeatInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		h.Close()
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(h.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	return client.New(h.URL), port, h.URL
}

func runUtils(t *testing.T, binary string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("CLI hung: %v", ctx.Err())
	}
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return out.String(), errOut.String(), code
}

func submit(t *testing.T, c *client.Client) protocol.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	j, err := c.Submit(ctx, protocol.Submission{Source: protocol.Source{Remote: "git@example.com:team/repo.git", Branch: "main"}, Args: []string{"printf", "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func registerRunner(t *testing.T, base string) (*websocket.Conn, string) {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/runner", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	info := protocol.RunnerInfo{ID: "test-runner", Name: "Test laptop", Priority: 75, MaxJobs: 5, Slots: 5, MinMemory: 1 << 30, AvailableMemory: 2 << 30}
	if err := conn.WriteJSON(protocol.Message{Type: "register", Runner: &info}); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var m protocol.Message
	if err := conn.ReadJSON(&m); err != nil {
		t.Fatal(err)
	}
	if m.Type != "registered" || m.RegistrationID == "" {
		t.Fatalf("registration: %+v", m)
	}
	return conn, m.RegistrationID
}

func TestRunnersListsIdentityPriorityCapacityAndAvailabilityAsJSON(t *testing.T) {
	binary := buildUtils(t)
	_, port, base := startCoordinator(t)
	registerRunner(t, base)
	out, errOut, code := runUtils(t, binary, "-p", port, "runners")
	if code != 0 || errOut != "" {
		t.Fatalf("runners exited %d: %s", code, errOut)
	}
	var runners []protocol.RunnerInfo
	if err := json.Unmarshal([]byte(out), &runners); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(runners) != 1 {
		t.Fatalf("runners: %+v", runners)
	}
	r := runners[0]
	if r.ID != "test-runner" || r.Name != "Test laptop" || r.Priority != 75 || r.MaxJobs != 5 || r.Slots != 5 || r.AvailableMemory != 2<<30 || !r.Available || r.Misses != 0 {
		t.Fatalf("runner details: %+v", r)
	}
}

func TestJobsListsQueuedJobsAsJSONWithPortBeforeSubcommand(t *testing.T) {
	binary := buildUtils(t)
	c, port, _ := startCoordinator(t)
	job := submit(t, c)
	out, errOut, code := runUtils(t, binary, "-p", port, "jobs")
	if code != 0 || errOut != "" {
		t.Fatalf("jobs exited %d: %s", code, errOut)
	}
	var jobs []protocol.Job
	if err := json.Unmarshal([]byte(out), &jobs); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].State != "queued" {
		t.Fatalf("jobs: %+v", jobs)
	}
}
