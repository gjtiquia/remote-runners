package coordinator_test

import (
	"bytes"
	"context"
	"strings"

	"fmt"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gjtiquia/remote-runners/internal/client"
	"github.com/gjtiquia/remote-runners/internal/coordinator"
	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gjtiquia/remote-runners/internal/testutil"
)

func start(t *testing.T) (*coordinator.Server, *client.Client, string) {
	t.Helper()
	s, err := coordinator.New(coordinator.Options{OutputDir: t.TempDir(), HeartbeatInterval: time.Hour, Build: testutil.PtrBuild()})
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
	return s, testutil.Client(h.URL), h.URL
}
func submission(branch string) protocol.Submission {
	return protocol.Submission{Source: protocol.Source{Remote: "git@example.com:team/repo.git", Branch: branch}, Args: []string{"echo", "hello"}, Timeout: time.Minute}
}

type adapter struct {
	t            *testing.T
	conn         *websocket.Conn
	registration string
}

func runner(t *testing.T, base string, info protocol.RunnerInfo) *adapter {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/runner", testutil.Headers())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	a := &adapter{t: t, conn: conn}
	a.send(protocol.Message{Type: "register", Runner: &info})
	m := a.read("registered")
	if m.RegistrationID == "" {
		t.Fatal("missing registration identity")
	}
	a.registration = m.RegistrationID
	return a
}
func (a *adapter) send(m protocol.Message) {
	a.t.Helper()
	if m.RegistrationID == "" {
		m.RegistrationID = a.registration
	}
	if err := a.conn.WriteJSON(m); err != nil {
		a.t.Fatal(err)
	}
}
func (a *adapter) read(kind string) protocol.Message {
	a.t.Helper()
	a.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var m protocol.Message
	if err := a.conn.ReadJSON(&m); err != nil {
		a.t.Fatal(err)
	}
	if m.Type != kind {
		a.t.Fatalf("expected %s, got %+v", kind, m)
	}
	return m
}
func capacity(id string) protocol.RunnerInfo {
	return protocol.RunnerInfo{ID: id, Name: id, Priority: 50, MaxJobs: 5, Slots: 5, AvailableMemory: 2 << 30, MinMemory: 1 << 30}
}
func waitJob(t *testing.T, c *client.Client, id, state string) protocol.Job {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		j, err := c.Job(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State == state {
			return j
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job %s did not reach %s", id, state)
	return protocol.Job{}
}
func TestRunnerExecutesJobAndCompletedOutputIsRetrievable(t *testing.T) {
	_, c, base := start(t)
	a := runner(t, base, capacity("laptop"))
	ctx := context.Background()
	j, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	assignment := a.read("job")
	if assignment.Job == nil || assignment.Job.ID != j.ID {
		t.Fatalf("assignment: %+v", assignment)
	}
	if err := c.Output(ctx, j.ID, &bytes.Buffer{}); err == nil {
		t.Fatal("running output must not be exposed")
	}
	a.send(protocol.Message{Type: "output", JobID: j.ID, Data: []byte("hello\n")})
	exit := 0
	a.send(protocol.Message{Type: "complete", JobID: j.ID, State: "succeeded", ExitCode: &exit, Commit: "abc123"})
	done := waitJob(t, c, j.ID, "succeeded")
	if done.ExitCode == nil || *done.ExitCode != 0 || done.Commit != "abc123" {
		t.Fatalf("result: %+v", done)
	}
	var output bytes.Buffer
	if err := c.Output(ctx, j.ID, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "hello\n" {
		t.Fatalf("output %q", output.String())
	}
	rs, err := c.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].ActiveJobs != 0 || !rs[0].Available {
		t.Fatalf("registry: %+v", rs)
	}
}

func TestSchedulingRanksEligibleRunnersAndSkipsGloballyLockedBranches(t *testing.T) {
	_, c, base := start(t)
	ctx := context.Background()
	low := capacity("fallback")
	low.Priority = 10
	low.MaxJobs = 1
	z := capacity("z")
	z.MaxJobs = 1
	a := capacity("a")
	a.MaxJobs = 1
	pressured := capacity("pressured")
	pressured.Priority = 100
	pressured.AvailableMemory = 512 << 20
	runner(t, base, low)
	runner(t, base, z)
	first := runner(t, base, a)
	runner(t, base, pressured)
	submit := func(branch string) protocol.Job {
		t.Helper()
		j, err := c.Submit(ctx, submission(branch))
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	main := submit("main")
	if main.RunnerID != "a" {
		t.Fatalf("deterministic priority tie: %+v", main)
	}
	assignment := first.read("job")
	if assignment.Job.ID != main.ID {
		t.Fatal("wrong assignment")
	}
	blocked := submit("main")
	if blocked.State != "queued" {
		t.Fatalf("global branch lock: %+v", blocked)
	}
	dev := submit("dev")
	if dev.RunnerID != "z" {
		t.Fatalf("independent branch capacity: %+v", dev)
	}
	feature := submit("feature")
	if feature.RunnerID != "fallback" {
		t.Fatalf("lower priority fallback: %+v", feature)
	}
	later := submit("later")
	if later.State != "queued" {
		t.Fatalf("all slots full or pressured: %+v", later)
	}
	rs, err := c.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.ID != "pressured" && (r.Slots != 0 || r.Available) {
			t.Fatalf("full runner advertised usable capacity: %+v", r)
		}
		if r.ID == "pressured" && r.Available {
			t.Fatalf("memory pressured runner advertised availability: %+v", r)
		}
	}
	first.send(protocol.Message{Type: "complete", JobID: main.ID, State: "succeeded"})
	next := first.read("job")
	if next.Job.ID != blocked.ID {
		t.Fatalf("oldest eligible job should win: %+v", next)
	}
	still, err := c.Job(ctx, later.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.State != "queued" {
		t.Fatalf("capacity exceeded: %+v", still)
	}
}

func TestCancellationKeepsActiveCapacityAndBranchLockUntilCompletion(t *testing.T) {
	_, c, base := start(t)
	info := capacity("one")
	info.MaxJobs = 1
	a := runner(t, base, info)
	ctx := context.Background()
	submit := func(branch string) protocol.Job {
		j, err := c.Submit(ctx, submission(branch))
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	active := submit("main")
	a.read("job")
	queued := submit("main")
	if err := c.Cancel(ctx, queued.ID); err != nil {
		t.Fatal(err)
	}
	waitJob(t, c, queued.ID, "cancelled")
	if err := c.Cancel(ctx, active.ID); err != nil {
		t.Fatal(err)
	}
	cancel := a.read("cancel")
	if cancel.JobID != active.ID {
		t.Fatalf("cancel: %+v", cancel)
	}
	replacement := submit("main")
	other := submit("other")
	if replacement.State != "queued" || other.State != "queued" {
		t.Fatalf("cancel must keep capacity: %+v %+v", replacement, other)
	}
	a.send(protocol.Message{Type: "complete", JobID: active.ID, State: "cancelled"})
	waitJob(t, c, active.ID, "cancelled")
	next := a.read("job")
	if next.Job.ID != replacement.ID {
		t.Fatalf("replacement after cancellation: %+v", next)
	}
	if err := c.Cancel(ctx, active.ID); err != nil {
		t.Fatalf("terminal cancellation should be idempotent: %v", err)
	}
}

func TestHeartbeatMissSuspendsDispatchAndResponseBeforeCutoffRestoresIt(t *testing.T) {
	s, c, base := start(t)
	a := runner(t, base, capacity("a"))
	ctx := context.Background()
	active, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	a.read("job")
	now := time.Now()
	s.Tick(now.Add(time.Hour))
	a.read("heartbeat")
	s.Tick(now.Add(2 * time.Hour))
	heartbeat := a.read("heartbeat")
	rs, err := c.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Misses != 1 || rs[0].Available {
		t.Fatalf("miss policy: %+v", rs)
	}
	queued, err := c.Submit(ctx, submission("dev"))
	if err != nil {
		t.Fatal(err)
	}
	if queued.State != "queued" {
		t.Fatalf("missed runner received new work: %+v", queued)
	}
	info := capacity("a")
	info.ActiveJobs = 1
	info.Slots = 4
	a.send(protocol.Message{Type: "heartbeat_response", Sequence: heartbeat.Sequence, Runner: &info})
	next := a.read("job")
	if next.Job.ID != queued.ID {
		t.Fatalf("recovery did not dispatch: %+v", next)
	}
	still, err := c.Job(ctx, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.State != "running" {
		t.Fatalf("first miss failed active work: %+v", still)
	}
	rs, err = c.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Misses != 0 || !rs[0].Available {
		t.Fatalf("recovered registry: %+v", rs)
	}
}

func TestHeartbeatCutoffFailsActiveJobsReleasesLocksAndRejectsOldIdentity(t *testing.T) {
	s, c, base := start(t)
	old := runner(t, base, capacity("a"))
	ctx := context.Background()
	original, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	old.read("job")
	old.send(protocol.Message{Type: "output", JobID: original.ID, Data: []byte("partial diagnostics\n")})
	replacement, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for round := 1; round <= 3; round++ {
		s.Tick(now.Add(time.Duration(round) * time.Hour))
		old.read("heartbeat")
	}
	s.Tick(now.Add(4 * time.Hour))
	failed := waitJob(t, c, original.ID, "failed")
	if failed.Error == "" {
		t.Fatal("cutoff needs actionable failure")
	}
	rs, err := c.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 0 {
		t.Fatalf("cutoff retained registration: %+v", rs)
	}
	fresh := runner(t, base, capacity("a"))
	if fresh.registration == old.registration {
		t.Fatal("registration identity was reused")
	}
	assigned := fresh.read("job")
	if assigned.Job.ID != replacement.ID {
		t.Fatalf("branch lock not released: %+v", assigned)
	}
	fresh.send(protocol.Message{Type: "output", RegistrationID: old.registration, JobID: replacement.ID, Data: []byte("orphan")})
	fresh.send(protocol.Message{Type: "complete", RegistrationID: old.registration, JobID: replacement.ID, State: "succeeded"})
	fresh.send(protocol.Message{Type: "output", JobID: replacement.ID, Data: []byte("fresh")})
	fresh.send(protocol.Message{Type: "complete", JobID: replacement.ID, State: "succeeded"})
	waitJob(t, c, replacement.ID, "succeeded")
	var output bytes.Buffer
	if err := c.Output(ctx, replacement.ID, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "fresh" {
		t.Fatalf("old registration accepted: %q", output.String())
	}
	output.Reset()
	if err := c.Output(ctx, original.ID, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "partial diagnostics\n" {
		t.Fatalf("lost partial output: %q", output.String())
	}
}

func TestManagementUsesSocketPeerLoopbackButRunnerRegistrationIsNetworkFacing(t *testing.T) {
	s, _, _ := start(t)
	for _, peer := range []string{"203.0.113.4:1234", "[2001:db8::1]:1234", "localhost:1234", "malformed"} {
		req := httptest.NewRequest("GET", "http://coordinator/runners", nil)
		req.RemoteAddr = peer
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
		req.Header.Set("Forwarded", "for=127.0.0.1")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("peer %q allowed: %d", peer, rec.Code)
		}
	}
	for _, peer := range []string{"127.0.0.1:1234", "[::1]:1234", "[::ffff:127.0.0.1]:1234"} {
		req := httptest.NewRequest("GET", "http://coordinator/runners", nil)
		req.RemoteAddr = peer
		req.Header = testutil.Headers()
		req.Header.Set("X-Forwarded-For", "203.0.113.4")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("loopback %q rejected: %d", peer, rec.Code)
		}
	}
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "203.0.113.4:5678"
		s.Handler().ServeHTTP(w, r)
	}))
	defer public.Close()
	runner(t, public.URL, capacity("network-runner"))
}

func TestHTTPRejectsInvalidMethodsBodiesAndPathsWithoutAcceptingJobs(t *testing.T) {
	_, c, base := start(t)
	ctx := context.Background()
	j, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		method, path, body string
		status             int
	}{
		{"DELETE", "/jobs", "", 405}, {"POST", "/runners", "", 405}, {"POST", "/jobs/" + j.ID, "", 405},
		{"GET", "/jobs/" + j.ID + "/cancel", "", 405},
		{"POST", "/jobs/" + j.ID + "/cancel", "{}", 400}, {"POST", "/jobs/" + j.ID + "/output", "", 405}, {"POST", "/runner", "", 405},
		{"GET", "/unrelated", "", 404}, {"GET", "/jobs/" + j.ID + "/extra", "", 404},
		{"POST", "/jobs", `{}`, 400},
		{"POST", "/jobs", `{"source":{"remote":"repo","branch":"main"},"args":[]}`, 400},
		{"POST", "/jobs", `{"source":{"remote":"repo","branch":"main"},"args":["echo"],"timeout":-1}`, 400},
		{"POST", "/jobs", `{"source":{"remote":"repo","branch":"main"},"args":["echo"],"unknown":true}`, 400},
		{"POST", "/jobs", `{"source":{"remote":"repo","branch":"main"},"args":["echo"]} {}`, 400},
		{"POST", "/jobs", `{"source":{"remote":"repo","branch":"main"},"args":["echo","` + strings.Repeat("x", 128*1024) + `"]}`, 413},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path+tc.body[:min(len(tc.body), 20)], func(t *testing.T) {
			req, err := http.NewRequest(tc.method, base+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header = testutil.Headers()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("got %d want %d", resp.StatusCode, tc.status)
			}
		})
	}
	jobs, err := c.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("invalid submissions accepted: %+v", jobs)
	}
	defaulted := submission("dev")
	defaulted.Timeout = 0
	accepted, err := c.Submit(ctx, defaulted)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Timeout != 30*time.Minute {
		t.Fatalf("default timeout: %v", accepted.Timeout)
	}
}

func TestManualRegistrationReplacesIdentityWithoutRestoringOldOccupancy(t *testing.T) {
	_, c, base := start(t)
	old := runner(t, base, capacity("a"))
	ctx := context.Background()
	active, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	old.read("job")
	queued, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	fresh := runner(t, base, capacity("a"))
	if fresh.registration == old.registration {
		t.Fatal("manual registration identity reused")
	}
	waitJob(t, c, active.ID, "failed")
	assigned := fresh.read("job")
	if assigned.Job.ID != queued.ID {
		t.Fatalf("old occupancy restored: %+v", assigned)
	}
}

func TestRunnerRegistrationRejectsInvalidCapacityAndPriority(t *testing.T) {
	_, c, base := start(t)
	for _, mutate := range []func(*protocol.RunnerInfo){
		func(r *protocol.RunnerInfo) { r.ID = "" }, func(r *protocol.RunnerInfo) { r.ID = strings.Repeat("x", 129) }, func(r *protocol.RunnerInfo) { r.Name = strings.Repeat("x", 257) }, func(r *protocol.RunnerInfo) { r.Priority = -1 }, func(r *protocol.RunnerInfo) { r.Priority = 101 },
		func(r *protocol.RunnerInfo) { r.MaxJobs = 0 }, func(r *protocol.RunnerInfo) { r.Slots = -1 }, func(r *protocol.RunnerInfo) { r.ActiveJobs = -1 },
	} {
		info := capacity("invalid")
		mutate(&info)
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/runner", testutil.Headers())
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.WriteJSON(protocol.Message{Type: "register", Runner: &info}); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(time.Second))
		var m protocol.Message
		err = conn.ReadJSON(&m)
		conn.Close()
		if err == nil {
			t.Fatalf("invalid runner accepted: %+v", info)
		}
	}
	rs, err := c.Runners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 0 {
		t.Fatalf("invalid registrations retained: %+v", rs)
	}
}

func TestCleanCloseOwnsOnlyItsSpoolAndRestartForgetsHistoryAndRegistrations(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "unrelated"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	open := func() (*coordinator.Server, *client.Client, string) {
		t.Helper()
		s, err := coordinator.New(coordinator.Options{OutputDir: root, HeartbeatInterval: time.Hour, Now: func() time.Time { return now }, Build: testutil.PtrBuild()})
		if err != nil {
			t.Fatal(err)
		}
		h := httptest.NewServer(s.Handler())
		t.Cleanup(func() { h.Close(); s.Close() })
		return s, testutil.Client(h.URL), h.URL
	}
	first, c, base := open()
	old := runner(t, base, capacity("a"))
	ctx := context.Background()
	j, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	if !j.CreatedAt.Equal(now) {
		t.Fatalf("clock option ignored: %v", j.CreatedAt)
	}
	old.read("job")
	old.send(protocol.Message{Type: "output", JobID: j.ID, Data: []byte("history")})
	old.send(protocol.Message{Type: "complete", JobID: j.ID, State: "succeeded"})
	waitJob(t, c, j.ID, "succeeded")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "unrelated" {
		t.Fatalf("clean close removed unrelated files or retained its spool: %+v", entries)
	}
	_, fresh, base := open()
	jobs, err := fresh.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := fresh.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 || len(rs) != 0 {
		t.Fatalf("state recovered after restart: %+v %+v", jobs, rs)
	}
	if _, err := fresh.Job(ctx, j.ID); err == nil {
		t.Fatal("old job recovered")
	}
	if err := fresh.Output(ctx, j.ID, &bytes.Buffer{}); err == nil {
		t.Fatal("old output recovered")
	}
	newRunner := runner(t, base, capacity("a"))
	if newRunner.registration == old.registration {
		t.Fatal("registration identity reused across coordinator lifetimes")
	}
}

func TestEquivalentRepositorySpellingsShareGlobalBranchLock(t *testing.T) {
	_, c, base := start(t)
	runner(t, base, capacity("a"))
	ctx := context.Background()
	first := submission("main")
	first.Source.Remote = "git@EXAMPLE.com:/team/repo.git"
	active, err := c.Submit(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if active.State != "running" {
		t.Fatalf("first job: %+v", active)
	}
	alias := first
	alias.Source.Remote = "ssh://git@example.com:22/team/repo.git"
	blocked, err := c.Submit(ctx, alias)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.State != "queued" {
		t.Fatalf("equivalent repository bypassed branch lock: %+v", blocked)
	}
	alias.Source.Branch = "dev"
	independent, err := c.Submit(ctx, alias)
	if err != nil {
		t.Fatal(err)
	}
	if independent.State != "running" {
		t.Fatalf("branch independence lost: %+v", independent)
	}
}

type patternSink struct{ n int64 }

func (w *patternSink) Write(p []byte) (int, error) {
	const pattern = "0123456789abcdef\n"
	for i, b := range p {
		if b != pattern[(int(w.n)+i)%len(pattern)] {
			return 0, fmt.Errorf("corrupt output at %d", w.n+int64(i))
		}
	}
	w.n += int64(len(p))
	return len(p), nil
}
func TestLargeCompletedOutputTransfersWithoutTruncation(t *testing.T) {
	_, c, base := start(t)
	a := runner(t, base, capacity("a"))
	ctx := context.Background()
	j, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	a.read("job")
	chunk := bytes.Repeat([]byte("0123456789abcdef\n"), 1024)
	for n := 0; n < 512; n++ {
		a.send(protocol.Message{Type: "output", JobID: j.ID, Data: chunk})
	}
	a.send(protocol.Message{Type: "complete", JobID: j.ID, State: "timed_out", Error: "deadline exceeded"})
	waitJob(t, c, j.ID, "timed_out")
	sink := &patternSink{}
	if err := c.Output(ctx, j.ID, sink); err != nil {
		t.Fatal(err)
	}
	if sink.n != 8_912_896 {
		t.Fatalf("truncated output: %d bytes", sink.n)
	}
}

func TestReportedSlotsAndMemoryLimitDispatchUntilHeartbeatRefresh(t *testing.T) {
	s, c, base := start(t)
	info := capacity("a")
	info.Slots = 1
	info.MaxJobs = 5
	a := runner(t, base, info)
	ctx := context.Background()
	first, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	a.read("job")
	second, err := c.Submit(ctx, submission("dev"))
	if err != nil {
		t.Fatal(err)
	}
	if second.State != "queued" {
		t.Fatalf("unreported assignment double-spent slot: %+v", second)
	}
	now := time.Now()
	s.Tick(now.Add(time.Hour))
	hb := a.read("heartbeat")
	info.ActiveJobs = 1
	info.Slots = 4
	info.AvailableMemory = 1
	a.send(protocol.Message{Type: "heartbeat_response", Sequence: hb.Sequence, Runner: &info})
	// Wait for the public capacity report, rather than relying on socket timing.
	until := time.Now().Add(3 * time.Second)
	observed := false
	for time.Now().Before(until) {
		rs, err := c.Runners(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if rs[0].AvailableMemory == 1 {
			observed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !observed {
		t.Fatal("heartbeat capacity not recorded")
	}
	queued, err := c.Job(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.State != "queued" {
		t.Fatalf("memory admission bypassed: %+v", queued)
	}
	s.Tick(now.Add(2 * time.Hour))
	hb = a.read("heartbeat")
	info.AvailableMemory = info.MinMemory
	a.send(protocol.Message{Type: "heartbeat_response", Sequence: hb.Sequence, Runner: &info})
	assigned := a.read("job")
	if assigned.Job.ID != second.ID {
		t.Fatalf("capacity refresh failed: %+v", assigned)
	}
	active, err := c.Job(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.State != "running" {
		t.Fatalf("capacity refresh preempted job: %+v", active)
	}
}

func TestSubmissionRejectsInvalidGitBranchNames(t *testing.T) {
	_, c, _ := start(t)
	for _, branch := range []string{"main~1", "../main", "main..other", "main.lock", "main/.hidden", "main//dev", "main/", "main@{1}", "main dev"} {
		sub := submission(branch)
		if _, err := c.Submit(context.Background(), sub); err == nil {
			t.Fatalf("invalid branch %q accepted", branch)
		}
	}
	jobs, err := c.Jobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatal("invalid branches entered queue")
	}
}

func TestSubmissionAcceptsPublishedAtBranchName(t *testing.T) {
	_, c, _ := start(t)
	job, err := c.Submit(context.Background(), submission("@"))
	if err != nil {
		t.Fatalf("Git permits refs/heads/@: %v", err)
	}
	if job.Source.Branch != "@" || job.State != "queued" {
		t.Fatalf("branch was changed: %+v", job)
	}
}

func TestDelayedFreshHeartbeatResponseRecoversButReplayDoesNot(t *testing.T) {
	s, c, base := start(t)
	a := runner(t, base, capacity("a"))
	ctx := context.Background()
	now := time.Now()
	s.Tick(now.Add(time.Hour))
	first := a.read("heartbeat")
	s.Tick(now.Add(2 * time.Hour))
	a.read("heartbeat")
	queued, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	if queued.State != "queued" {
		t.Fatal("miss did not suspend dispatch")
	}
	info := capacity("a")
	a.send(protocol.Message{Type: "heartbeat_response", Sequence: first.Sequence, Runner: &info})
	assigned := a.read("job")
	if assigned.Job.ID != queued.ID {
		t.Fatal("delayed response did not recover")
	}
	s.Tick(now.Add(3 * time.Hour))
	a.read("heartbeat")
	s.Tick(now.Add(4 * time.Hour))
	a.read("heartbeat")
	a.send(protocol.Message{Type: "heartbeat_response", Sequence: first.Sequence, Runner: &info})
	a.send(protocol.Message{Type: "complete", JobID: queued.ID, State: "succeeded"})
	waitJob(t, c, queued.ID, "succeeded")
	rs, err := c.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Misses != 1 || rs[0].Available {
		t.Fatalf("heartbeat replay restored availability: %+v", rs)
	}
}

func TestBackgroundTickerAppliesCustomHeartbeatCutoff(t *testing.T) {
	s, err := coordinator.New(coordinator.Options{OutputDir: t.TempDir(), HeartbeatInterval: 100 * time.Millisecond, MissLimit: 2, Build: testutil.PtrBuild()})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() { h.Close(); s.Close() })
	c := testutil.Client(h.URL)
	a := runner(t, h.URL, capacity("a"))
	first := a.read("heartbeat")
	if first.Sequence != 1 {
		t.Fatalf("initial heartbeat: %+v", first)
	}
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		rs, err := c.Runners(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(rs) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("background ticker did not deregister at configured cutoff")
}

func TestSubmissionRemainsQueuedAndCanBeInspectedWithoutRunners(t *testing.T) {
	_, c, _ := start(t)
	ctx := context.Background()
	j, err := c.Submit(ctx, submission("main"))
	if err != nil {
		t.Fatal(err)
	}
	if j.ID == "" || j.State != "queued" {
		t.Fatalf("accepted job: %+v", j)
	}
	got, err := c.Job(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != j.ID || got.State != "queued" {
		t.Fatalf("lookup: %+v", got)
	}
	jobs, err := c.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != j.ID {
		t.Fatalf("jobs: %+v", jobs)
	}
	runners, err := c.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 0 {
		t.Fatalf("runners: %+v", runners)
	}
}
