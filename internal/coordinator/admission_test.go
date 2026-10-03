package coordinator_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gjtiquia/remote-runners/internal/client"
	"github.com/gjtiquia/remote-runners/internal/protocol"
)

func submitJob(t *testing.T, c *client.Client, branch string) protocol.Job {
	t.Helper()
	j, err := c.Submit(context.Background(), submission(branch))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestDeclinedUnstartedAssignmentKeepsIdentityAndAcceptanceOrder(t *testing.T) {
	_, c, base := start(t)
	info := capacity("preferred")
	info.MaxJobs, info.Slots = 2, 2
	a := runner(t, base, info)
	executing := submitJob(t, c, "already-executing")
	a.read("job")
	original := submitJob(t, c, "main")
	a.read("job")
	later := submitJob(t, c, "main")
	unrelated := submitJob(t, c, "other")
	info.ActiveJobs, info.Slots = 1, 0
	a.send(protocol.Message{Type: "decline", JobID: original.ID, Runner: &info})
	queued := waitJob(t, c, original.ID, "queued")
	if queued.RunnerID != "" || !queued.StartedAt.IsZero() || queued.Source != original.Source || !queued.CreatedAt.Equal(original.CreatedAt) {
		t.Fatalf("decline changed accepted source/identity or retained assignment: %+v", queued)
	}
	rs, err := c.Runners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Available || rs[0].ActiveJobs != 1 || rs[0].Slots != 0 {
		t.Fatalf("declining capacity: %+v", rs)
	}
	if j, err := c.Job(context.Background(), executing.ID); err != nil || j.State != "running" {
		t.Fatalf("decline disturbed executing job: %+v %v", j, err)
	}
	low := capacity("fallback")
	low.Priority, low.MaxJobs, low.Slots = 10, 1, 1
	b := runner(t, base, low)
	assigned := b.read("job").Job
	if assigned.ID != original.ID || assigned.Source != original.Source {
		t.Fatalf("oldest unstarted job not preserved: %+v", assigned)
	}
	for _, id := range []string{later.ID, unrelated.ID} {
		if j, err := c.Job(context.Background(), id); err != nil || j.State != "queued" {
			t.Fatalf("queue drained: %+v %v", j, err)
		}
	}
	b.send(protocol.Message{Type: "complete", JobID: original.ID, State: "succeeded"})
	if next := b.read("job").Job; next.ID != later.ID {
		t.Fatalf("acceptance order changed: %+v", next)
	}
	jobs, err := c.Jobs(context.Background())
	if err != nil || len(jobs) != 4 || jobs[1].ID != original.ID || jobs[2].ID != later.ID {
		t.Fatalf("history changed: %+v %v", jobs, err)
	}
}

func TestDeclineAfterOutputCannotRetryExecutedWork(t *testing.T) {
	s, c, base := start(t)
	info := capacity("one")
	a := runner(t, base, info)
	first := submitJob(t, c, "main")
	a.read("job")
	a.send(protocol.Message{Type: "output", JobID: first.ID, Data: []byte("execution began")})
	info.Slots = 0
	a.send(protocol.Message{Type: "decline", JobID: first.ID, Runner: &info})
	// Ordered heartbeat response is a processing barrier for the preceding messages.
	s.Tick(time.Now().Add(time.Hour))
	hb := a.read("heartbeat")
	info.Name = "barrier"
	a.send(protocol.Message{Type: "heartbeat_response", Sequence: hb.Sequence, Runner: &info})
	waitRunnerInfo(t, c, info.ID, func(r protocol.RunnerInfo) bool { return r.Name == "barrier" })
	j, err := c.Job(context.Background(), first.ID)
	if err != nil || j.State != "running" {
		t.Fatalf("executed work returned to queue: %+v %v", j, err)
	}
}

func TestCompletionRefreshesPreferredRunnerCapacityBeforeDispatch(t *testing.T) {
	s, c, base := start(t)
	info := capacity("preferred")
	info.MaxJobs, info.Slots = 1, 1
	a := runner(t, base, info)
	first := submitJob(t, c, "main")
	a.read("job")
	next := submitJob(t, c, "main")
	low := capacity("fallback")
	low.Priority = 10
	runner(t, base, low)
	s.Tick(time.Now().Add(time.Hour))
	hb := a.read("heartbeat")
	info.ActiveJobs, info.Slots, info.AvailableMemory = 1, 0, 1
	a.send(protocol.Message{Type: "heartbeat_response", Sequence: hb.Sequence, Runner: &info})
	waitRunnerInfo(t, c, info.ID, func(r protocol.RunnerInfo) bool { return r.AvailableMemory == 1 })
	info.ActiveJobs, info.Slots, info.AvailableMemory = 0, 1, 2<<30
	a.send(protocol.Message{Type: "complete", JobID: first.ID, State: "succeeded", Runner: &info})
	waitJob(t, c, first.ID, "succeeded")
	assigned := waitJob(t, c, next.ID, "running")
	if assigned.RunnerID != info.ID {
		t.Fatalf("completion used stale capacity and fell back: %+v", assigned)
	}
	if m := a.read("job"); m.Job.ID != next.ID {
		t.Fatalf("wrong dispatch: %+v", m)
	}
}

func TestCompletionWithoutReportReleasesKnownReportedSlotWithoutInventingMemory(t *testing.T) {
	for _, pressured := range []bool{false, true} {
		t.Run(map[bool]string{false: "capacity", true: "memory remains pressured"}[pressured], func(t *testing.T) {
			s, c, base := start(t)
			info := capacity("one")
			info.MaxJobs, info.Slots = 1, 1
			a := runner(t, base, info)
			first := submitJob(t, c, "main")
			a.read("job")
			next := submitJob(t, c, "main")
			s.Tick(time.Now().Add(time.Hour))
			hb := a.read("heartbeat")
			info.ActiveJobs, info.Slots = 1, 0
			if pressured {
				info.AvailableMemory = 1
			}
			// Name is a harmless telemetry marker proving the full report was applied.
			info.Name = "reported"
			a.send(protocol.Message{Type: "heartbeat_response", Sequence: hb.Sequence, Runner: &info})
			waitRunnerInfo(t, c, info.ID, func(r protocol.RunnerInfo) bool { return r.Name == "reported" })
			a.send(protocol.Message{Type: "complete", JobID: first.ID, State: "succeeded"})
			waitJob(t, c, first.ID, "succeeded")
			if pressured {
				j, err := c.Job(context.Background(), next.ID)
				if err != nil || j.State != "queued" {
					t.Fatalf("completion invented memory capacity: %+v %v", j, err)
				}
			} else {
				if m := a.read("job"); m.Job.ID != next.ID {
					t.Fatalf("reported slot not released: %+v", m)
				}
			}
		})
	}
}

func TestHeartbeatCutoffNeverDispatchesReleasedQueueToAnotherOverdueRunner(t *testing.T) {
	// Repetition covers map iteration permutations through the public interface.
	for attempt := 0; attempt < 16; attempt++ {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			s, c, base := start(t)
			info := capacity("original")
			info.Priority = 100
			a := runner(t, base, info)
			active := submitJob(t, c, "main")
			a.read("job")
			queued := submitJob(t, c, "main")
			now := time.Now()
			for round := 1; round <= 2; round++ {
				s.Tick(now.Add(time.Duration(round) * time.Hour))
				a.read("heartbeat")
			}
			var others []*adapter
			for i := 0; i < 8; i++ {
				others = append(others, runner(t, base, capacity(fmt.Sprintf("other-%d", i))))
			}
			s.Tick(now.Add(3 * time.Hour))
			a.read("heartbeat")
			for _, other := range others {
				other.read("heartbeat")
			}
			// Original reaches cutoff while every potential target has its first miss.
			s.Tick(now.Add(4 * time.Hour))
			waitJob(t, c, active.ID, "failed")
			j, err := c.Job(context.Background(), queued.ID)
			if err != nil || j.State != "queued" || j.RunnerID != "" {
				t.Fatalf("dispatch during partial heartbeat update: %+v %v", j, err)
			}
			for _, other := range others {
				other.read("heartbeat")
			}
		})
	}
}

// Wait for a report at the public management seam, not WebSocket write timing.
func waitRunnerInfo(t *testing.T, c *client.Client, id string, matches func(protocol.RunnerInfo) bool) protocol.RunnerInfo {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		rs, err := c.Runners(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if r.ID == id && matches(r) {
				return r
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("runner %s did not publish expected capacity", id)
	return protocol.RunnerInfo{}
}
