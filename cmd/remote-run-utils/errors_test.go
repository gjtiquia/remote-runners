package main_test

import (
	"net"
	"strings"
	"testing"
)

func TestCoordinatorFailuresIdentifyTheOperationAndJobWithoutPanicking(t *testing.T) {
	binary := buildUtils(t)
	c, port, _ := startCoordinator(t)
	queued := submit(t, c)
	for _, tc := range []struct{ command, id, status string }{
		{"job", "missing-job", "404"}, {"output", "missing-job", "404"},
		{"cancel", "missing-job", "404"}, {"output", queued.ID, "409"},
	} {
		out, errOut, code := runUtils(t, binary, "-p", port, tc.command, tc.id)
		if code != 1 || out != "" || !strings.Contains(errOut, tc.command+" "+tc.id) || !strings.Contains(errOut, tc.status) || strings.Contains(errOut, "panic") {
			t.Fatalf("failure: exit=%d stdout=%q stderr=%q", code, out, errOut)
		}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, unusedPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := runUtils(t, binary, "-p", unusedPort, "jobs")
	if code != 1 || out != "" || !strings.Contains(errOut, "connect") || strings.Contains(errOut, "panic") {
		t.Fatalf("unavailable coordinator: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
}
