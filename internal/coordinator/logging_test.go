package coordinator_test

import (
	"context"
	"fmt"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gjtiquia/remote-runners/internal/client"
	"github.com/gjtiquia/remote-runners/internal/coordinator"
	"github.com/gjtiquia/remote-runners/internal/protocol"
)

func startLogged(t *testing.T) (*coordinator.Server, *client.Client, string, func() string) {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), "coordinator.log"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := coordinator.New(coordinator.Options{OutputDir: t.TempDir(), HeartbeatInterval: time.Hour, Logger: log.New(file, "coordinator: ", log.LstdFlags)})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() { h.Close(); s.Close(); file.Close() })
	capture := func() string {
		t.Helper()
		data, err := os.ReadFile(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	return s, client.New(h.URL), h.URL, capture
}

func requireLog(t *testing.T, capture func() string, fragments ...string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		out := capture()
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			matched := true
			for _, fragment := range fragments {
				matched = matched && strings.Contains(line, fragment)
			}
			if matched {
				if !regexp.MustCompile(`^coordinator: \d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `).MatchString(line) {
					t.Fatalf("missing component/timestamp: %q", line)
				}
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing log %q:\n%s", fragments, out)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLocalProjectLogsUseBasenameWithoutTreatingFilenameAsURL(t *testing.T) {
	_, c, _, capture := startLogged(t)
	sub := submission("main")
	privateRoot := t.TempDir()
	sub.Source.Remote = filepath.Join(privateRoot, "repo%name.git")
	job, err := c.Submit(context.Background(), sub)
	if err != nil {
		t.Fatal(err)
	}
	requireLog(t, capture, "job received", fmt.Sprintf("job_id=%q", job.ID), `project="repo%name"`)
	if out := capture(); strings.Contains(out, privateRoot) {
		t.Fatalf("project logs exposed full local path:\n%s", out)
	}
}

func TestAcceptedDeclineLogsAssignmentRollbackWithoutLoggingIgnoredDeclines(t *testing.T) {
	_, c, base, capture := startLogged(t)
	a := runner(t, base, capacity("decline-worker"))
	job, err := c.Submit(context.Background(), submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	a.read("job")
	info := capacity("decline-worker")
	a.send(protocol.Message{Type: "decline", JobID: job.ID, Runner: &info}) // Available snapshots are ignored.
	info.Slots = 0
	a.send(protocol.Message{Type: "decline", JobID: job.ID, Runner: &info})
	waitJob(t, c, job.ID, "queued")
	requireLog(t, capture, "job declined", fmt.Sprintf("job_id=%q", job.ID), `runner_id="decline-worker"`, `branch="main"`)
	out := capture()
	if strings.Count(out, "job declined") != 1 || strings.Contains(out, "job completed") {
		t.Fatalf("decline should log only the accepted rollback, not completion:\n%s", out)
	}
}

func TestCancellationLogsDistinguishQueuedCompletionFromRunningRequest(t *testing.T) {
	_, c, base, capture := startLogged(t)
	a := runner(t, base, capacity("cancel-worker"))
	ctx := context.Background()
	active, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	a.read("job")
	queued, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{queued.ID, active.ID, queued.ID, active.ID} {
		if err := c.Cancel(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	cancel := a.read("cancel")
	if cancel.JobID != active.ID {
		t.Fatalf("wrong cancellation: %+v", cancel)
	}
	waitJob(t, c, queued.ID, "cancelled")
	waitJob(t, c, active.ID, "running")
	requireLog(t, capture, "job cancellation requested", fmt.Sprintf("job_id=%q", queued.ID), `state="queued"`)
	requireLog(t, capture, "job completed", fmt.Sprintf("job_id=%q", queued.ID), `state="cancelled"`)
	requireLog(t, capture, "job cancellation requested", fmt.Sprintf("job_id=%q", active.ID), `runner_id="cancel-worker"`, `state="running"`)
	if out := capture(); strings.Contains(out, "job completed job_id="+fmt.Sprintf("%q", active.ID)) {
		t.Fatalf("running cancel logged completion prematurely:\n%s", out)
	}
	a.send(protocol.Message{Type: "complete", JobID: active.ID, State: "cancelled"})
	waitJob(t, c, active.ID, "cancelled")
	requireLog(t, capture, "job completed", fmt.Sprintf("job_id=%q", active.ID), `state="cancelled"`)
	if out := capture(); strings.Count(out, "job cancellation requested") != 2 {
		t.Fatalf("idempotent cancellations repeated logs:\n%s", out)
	}
}

func TestHeartbeatLogsOnlyFirstMissRecoveryAndCutoff(t *testing.T) {
	s, c, base, capture := startLogged(t)
	a := runner(t, base, capacity("heartbeat-worker"))
	ctx := context.Background()
	job, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	a.read("job")
	now := time.Now()
	tick := func(round int) protocol.Message {
		s.Tick(now.Add(time.Duration(round) * time.Hour))
		return a.read("heartbeat")
	}
	respond := func(m protocol.Message, priority int) {
		t.Helper()
		info := capacity("heartbeat-worker")
		info.ActiveJobs, info.Slots, info.Priority = 1, 4, priority
		a.send(protocol.Message{Type: "heartbeat_response", Sequence: m.Sequence, Runner: &info})
		deadline := time.Now().Add(3 * time.Second)
		for {
			rs, err := c.Runners(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rs) == 1 && rs[0].Priority == priority && rs[0].Misses == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("heartbeat response not observed")
			}
			time.Sleep(time.Millisecond)
		}
	}
	before := capture()
	respond(tick(1), 51)
	if out := capture(); out != before {
		t.Fatalf("successful heartbeat generated logs:\n%s", out)
	}
	tick(2)
	tick(3)
	requireLog(t, capture, "runner heartbeat missed", `runner_id="heartbeat-worker"`, "misses=1")
	respond(tick(4), 52)
	requireLog(t, capture, "runner heartbeat recovered", `runner_id="heartbeat-worker"`, "misses=2")
	tick(5)
	tick(6)
	tick(7)
	s.Tick(now.Add(8 * time.Hour))
	waitJob(t, c, job.ID, "failed")
	requireLog(t, capture, "runner deregistered", `runner_id="heartbeat-worker"`, `reason="runner missed heartbeat cutoff"`)
	out := capture()
	if strings.Count(out, "runner heartbeat missed") != 2 || strings.Count(out, "runner heartbeat recovered") != 1 || strings.Count(out, "runner deregistered") != 1 {
		t.Fatalf("heartbeat logs are not transitions only:\n%s", out)
	}
	if strings.Contains(out, "heartbeat missed runner_id=\"heartbeat-worker\" misses=2") {
		t.Fatalf("repeated heartbeat miss spam:\n%s", out)
	}
}

func TestCoordinatorLogsRegistrationDisconnectAndOwnedLifetimeOnce(t *testing.T) {
	s, c, base, capture := startLogged(t)
	requireLog(t, capture, "coordinator started")
	a := runner(t, base, capacity("worker\"quoted"))
	requireLog(t, capture, "runner registered", `runner_id="worker\"quoted"`, "priority=50", "slots=5", `peer="127.0.0.1:`)
	before := capture()
	for n := 0; n < 3; n++ {
		if _, err := c.Runners(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Jobs(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if out := capture(); out != before {
		t.Fatalf("management polling generated logs:\n%s", out)
	}
	if err := a.conn.Close(); err != nil {
		t.Fatal(err)
	}
	requireLog(t, capture, "runner transport disconnected", `runner_id="worker\"quoted"`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	requireLog(t, capture, "coordinator shutdown complete")
	out := capture()
	for _, event := range []string{"coordinator started", "runner registered", "runner transport disconnected", "coordinator shutdown started", "coordinator shutdown complete"} {
		if strings.Count(out, event) != 1 {
			t.Fatalf("expected one %q log:\n%s", event, out)
		}
	}
}

func TestJobLifecycleLogsIdentifyAcceptedDispatchedAndCompletedJobsWithoutSecrets(t *testing.T) {
	_, c, base, capture := startLogged(t)
	a := runner(t, base, capacity("worker\"quoted"))
	sub := submission("feature/quoted\"branch")
	sub.Source.Remote = "https://user:credential@example.com/private/repo.git?token=secret-token"
	sub.Args = []string{"echo", "secret-argument\nforged-log"}
	job, err := c.Submit(context.Background(), sub)
	if err != nil {
		t.Fatal(err)
	}
	a.read("job")
	requireLog(t, capture, "job received", fmt.Sprintf("job_id=%q", job.ID), `branch="feature/quoted\"branch"`, `project="repo"`, "state=queued")
	requireLog(t, capture, "job dispatched", fmt.Sprintf("job_id=%q", job.ID), `runner_id="worker\"quoted"`, "state=running")
	a.send(protocol.Message{Type: "output", JobID: job.ID, Data: []byte("secret-output")})
	exit := 0
	a.send(protocol.Message{Type: "complete", JobID: job.ID, State: "succeeded", ExitCode: &exit, Error: "secret-error", Commit: "secret-commit"})
	waitJob(t, c, job.ID, "succeeded")
	requireLog(t, capture, "job completed", fmt.Sprintf("job_id=%q", job.ID), `runner_id="worker\"quoted"`, `state="succeeded"`, "exit_code=0")

	// A later job's completion is a transport-order barrier for the ignored late result.
	a.send(protocol.Message{Type: "complete", JobID: job.ID, State: "failed"})
	next, err := c.Submit(context.Background(), submission("next"))
	if err != nil {
		t.Fatal(err)
	}
	a.read("job")
	a.send(protocol.Message{Type: "complete", JobID: next.ID, State: "succeeded"})
	waitJob(t, c, next.ID, "succeeded")
	out := capture()
	if strings.Count(out, "job completed job_id="+fmt.Sprintf("%q", job.ID)) != 1 {
		t.Fatalf("duplicate completion log:\n%s", out)
	}
	for _, secret := range []string{"credential", "example.com", "private/", "secret-token", "secret-argument", "forged-log", "secret-output", "secret-error", "secret-commit"} {
		if strings.Contains(out, secret) {
			t.Fatalf("sensitive data %q in logs:\n%s", secret, out)
		}
	}
}
