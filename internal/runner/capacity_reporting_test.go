package runner_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
)

func TestWorkerDeclinesMemoryRefusalWithUnavailableSnapshot(t *testing.T) {
	_, read, send := workerProtocol(t, ^uint64(0))
	registration := read()
	if registration.Type != "register" || registration.Runner == nil || registration.Runner.Available || registration.Runner.Slots != 1 {
		t.Fatalf("memory-limited registration: %+v", registration)
	}
	send(protocol.Message{Type: "registered", RegistrationID: "memory-registration"})
	// Invalid source would fail preparation if the capacity gate did not run
	// first. There must be no terminal completion for this unstarted job.
	job := protocol.Job{ID: "memory-refusal", Submission: protocol.Submission{Source: protocol.Source{Remote: filepath.Join(t.TempDir(), "missing"), Branch: "main"}, Args: []string{"/bin/true"}}}
	send(protocol.Message{Type: "job", Job: &job})
	decline := read()
	if decline.Type != "decline" || decline.JobID != job.ID || decline.RegistrationID != "memory-registration" || decline.Runner == nil || decline.Runner.Available || decline.Runner.ActiveJobs != 0 || decline.Runner.Slots != 1 || decline.Runner.AvailableMemory >= decline.Runner.MinMemory {
		t.Fatalf("memory refusal must preserve unavailable admission: %+v", decline)
	}
	send(protocol.Message{Type: "heartbeat", Sequence: 1})
	heartbeat := read()
	if heartbeat.Type != "heartbeat_response" || heartbeat.Sequence != 1 || heartbeat.Runner == nil || heartbeat.Runner.Available {
		t.Fatalf("memory gate must remain effective: %+v", heartbeat)
	}
}

func TestWorkerCapacityReportsStayRecoveredAcrossCompletion(t *testing.T) {
	f, read, send := workerProtocol(t, 1)
	if registration := read(); registration.Type != "register" {
		t.Fatalf("registration: %+v", registration)
	}
	send(protocol.Message{Type: "registered", RegistrationID: "capacity-registration"})
	release := filepath.Join(t.TempDir(), "release")
	job := protocol.Job{ID: "capacity-race", Submission: protocol.Submission{Source: protocol.Source{Remote: repository(t, ""), Branch: "main"}, Args: []string{"/bin/sh", "-c", `echo CAPACITY_READY; while [ ! -f "$1" ]; do sleep 0.02; done`, "command", release}, Timeout: time.Minute}}
	send(protocol.Message{Type: "job", Job: &job})
	f.waitForTerminal(t, f.window(t, job.ID), "CAPACITY_READY")
	send(protocol.Message{Type: "heartbeat", Sequence: 1})
	busy := read()
	if busy.Type != "heartbeat_response" || busy.Runner == nil || busy.Runner.ActiveJobs != 1 || busy.Runner.Available {
		t.Fatalf("expected occupied worker: %+v", busy)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}

	// Keep heartbeats pending while the independently monitored helper exits.
	// Once any ordinary report observes recovery, a later report must not
	// overwrite it with an older busy snapshot. No assignment occurs here.
	recovered, completed := false, false
	sequence := uint64(1)
	deadline := time.Now().Add(5 * time.Second)
	for {
		alreadyCompleted := completed
		for range 128 {
			sequence++
			send(protocol.Message{Type: "heartbeat", Sequence: sequence})
		}
		for {
			message := read()
			if message.Type == "output" {
				continue
			}
			if message.Type != "heartbeat_response" && message.Type != "complete" {
				t.Fatalf("unexpected report: %+v", message)
			}
			info := message.Runner
			if info == nil || message.RegistrationID != "capacity-registration" {
				t.Fatalf("missing capacity/registration: %+v", message)
			}
			if info.ActiveJobs == 0 {
				recovered = true
			}
			if recovered && (info.ActiveJobs != 0 || info.Slots != 1 || !info.Available) {
				t.Fatalf("capacity regressed after recovery: %+v", message)
			}
			if message.Type == "complete" {
				if message.JobID != job.ID || message.State != "succeeded" || !recovered {
					t.Fatalf("completion must report recovered capacity: %+v", message)
				}
				completed = true
			}
			if message.Type == "heartbeat_response" && message.Sequence == sequence {
				break
			}
		}
		if alreadyCompleted {
			break // Also check a whole heartbeat batch after terminal completion.
		}
		if time.Now().After(deadline) {
			t.Fatal("helper never completed")
		}
	}
}
