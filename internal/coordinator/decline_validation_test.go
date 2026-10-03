package coordinator_test

import (
	"context"
	"testing"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
)

func TestDeclineRequiresCurrentAssignmentIdentityAndUnavailableValidReport(t *testing.T) {
	for _, kind := range []string{"missing report", "wrong runner", "wrong registration", "wrong job", "negative slots", "negative active", "invalid priority", "still available"} {
		t.Run(kind, func(t *testing.T) {
			s, c, base := start(t)
			info := capacity("one")
			a := runner(t, base, info)
			first := submitJob(t, c, "main")
			a.read("job")
			next := submitJob(t, c, "main")
			report := info
			report.Slots = 0
			m := protocol.Message{Type: "decline", JobID: first.ID, Runner: &report}
			switch kind {
			case "missing report":
				m.Runner = nil
			case "wrong runner":
				report.ID = "someone-else"
			case "wrong registration":
				m.RegistrationID = "old-identity"
			case "wrong job":
				m.JobID = next.ID
			case "negative slots":
				report.Slots = -1
			case "negative active":
				report.ActiveJobs = -1
			case "invalid priority":
				report.Priority = 101
			case "still available":
				report.Slots = 1
			}
			a.send(m)
			s.Tick(time.Now().Add(time.Hour))
			hb := a.read("heartbeat")
			info.Slots, info.Name = 0, "barrier"
			a.send(protocol.Message{Type: "heartbeat_response", Sequence: hb.Sequence, Runner: &info})
			waitRunnerInfo(t, c, info.ID, func(r protocol.RunnerInfo) bool { return r.Name == "barrier" })
			j, err := c.Job(context.Background(), first.ID)
			if err != nil || j.State != "running" || j.RunnerID != first.RunnerID || !j.StartedAt.Equal(first.StartedAt) {
				t.Fatalf("invalid decline released assignment: %+v %v", j, err)
			}
			j, err = c.Job(context.Background(), next.ID)
			if err != nil || j.State != "queued" {
				t.Fatalf("invalid decline released branch: %+v %v", j, err)
			}
		})
	}
}

func TestUnavailableDeclineDoesNotRedispatchUntilFreshCapacity(t *testing.T) {
	for _, kind := range []string{"slots", "memory", "concurrency"} {
		t.Run(kind, func(t *testing.T) {
			s, c, base := start(t)
			info := capacity("one")
			a := runner(t, base, info)
			first := submitJob(t, c, "main")
			a.read("job")
			switch kind {
			case "slots":
				info.Slots = 0
			case "memory":
				info.AvailableMemory = 1
			case "concurrency":
				info.ActiveJobs = info.MaxJobs
			}
			a.send(protocol.Message{Type: "decline", JobID: first.ID, Runner: &info})
			waitJob(t, c, first.ID, "queued")
			rs, err := c.Runners(context.Background())
			if err != nil || rs[0].Available {
				t.Fatalf("declining runner remains eligible: %+v %v", rs, err)
			}
			s.Tick(time.Now().Add(time.Hour))
			hb := a.read("heartbeat")
			info = capacity("one")
			a.send(protocol.Message{Type: "heartbeat_response", Sequence: hb.Sequence, Runner: &info})
			if m := a.read("job"); m.Job.ID != first.ID {
				t.Fatalf("fresh capacity did not dispatch original: %+v", m)
			}
		})
	}
}

func TestDeclinedCancelledAssignmentIsNotRequeued(t *testing.T) {
	_, c, base := start(t)
	info := capacity("one")
	a := runner(t, base, info)
	first := submitJob(t, c, "main")
	a.read("job")
	if err := c.Cancel(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	a.read("cancel")
	info.Slots = 0
	a.send(protocol.Message{Type: "decline", JobID: first.ID, Runner: &info})
	waitJob(t, c, first.ID, "cancelled")
	b := runner(t, base, capacity("other"))
	next := submitJob(t, c, "main")
	if m := b.read("job"); m.Job.ID != next.ID {
		t.Fatalf("cancelled work was dispatched again: %+v", m)
	}
}
