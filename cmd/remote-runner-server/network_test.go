package main_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gjtiquia/remote-runners/internal/testutil"
	"github.com/gorilla/websocket"
)

func networkIP(t *testing.T) string {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && !ip.IsLoopback() {
				return ip.String()
			}
		}
	}
	t.Skip("no non-loopback IPv4 interface available")
	return ""
}

func TestServerAcceptsNetworkRunnerButRejectsRemoteManagementAndAppliesHeartbeatFlags(t *testing.T) {
	ip := networkIP(t)
	binary := buildServer(t)
	port := freePort(t)
	process, local := startServer(t, binary, port, "-output-dir", t.TempDir(), "-heartbeat-interval", "100ms", "-heartbeat-misses", "2")
	base := fmt.Sprintf("http://%s:%d", ip, port)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	remote := &http.Client{Transport: transport, Timeout: time.Second}
	for _, route := range []struct{ method, path string }{
		{"GET", "/runners"}, {"GET", "/jobs"}, {"POST", "/jobs"},
		{"GET", "/jobs/missing"}, {"GET", "/jobs/missing/output"}, {"POST", "/jobs/missing/cancel"},
	} {
		req, err := http.NewRequest(route.method, base+route.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
		resp, err := remote.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "loopback") {
			t.Fatalf("remote %s %s: %d %s", route.method, route.path, resp.StatusCode, body)
		}
	}
	dialer := websocket.Dialer{HandshakeTimeout: time.Second}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/runner", testutil.Headers())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	info := protocol.RunnerInfo{ID: "network-runner", Priority: 50, MaxJobs: 1, Slots: 1}
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
	job, err := local.Submit(ctx, protocol.Submission{Source: protocol.Source{Remote: "git@example.com:team/repo.git", Branch: "main"}, Args: []string{"echo"}})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "running" {
		t.Fatalf("network runner did not dispatch: %+v", job)
	}
	heartbeats := 0
	for {
		var message protocol.Message
		if err := conn.ReadJSON(&message); err != nil {
			break
		}
		if message.Type == "heartbeat" {
			heartbeats++
		}
	}
	if heartbeats != 2 {
		t.Fatalf("configured cutoff: got %d heartbeat requests, want 2", heartbeats)
	}
	detail, err := local.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "failed" || !strings.Contains(detail.Error, "heartbeat") {
		t.Fatalf("cutoff result: %+v", detail)
	}
	runners, err := local.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 0 {
		t.Fatalf("runner survived cutoff: %+v", runners)
	}
	if err := process.command.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.done:
		if process.err != nil {
			t.Fatalf("SIGINT stop: %v", process.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT did not stop server")
	}
}
