package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestProgressShowsRunnerAndOutputLifecycleWithoutLeakingArguments(t *testing.T) {
	local, _ := checkout(t)
	f := startWorker(t, startCoordinator(t))
	c := command(t, local, "-p", f.port, "/bin/sh", "-c", "sleep 0.6; printf PRIVATE_ARG_PAYLOAD; exit 7")
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if code := exitCode(c.Run()); code != 7 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	text := stderr.String()
	for _, event := range []string{"checking Git source", "Git source ready", "submitting job", "job ID:", "running on runner=\"cli-test\"", "failed", "fetching completed output", "exit code: 7"} {
		if !strings.Contains(text, event) {
			t.Fatalf("missing progress %q in %s", event, text)
		}
	}
	if strings.Contains(text, "PRIVATE_ARG_PAYLOAD") || !strings.Contains(stdout.String(), "PRIVATE_ARG_PAYLOAD") || strings.Contains(stdout.String(), "checking Git source") {
		t.Fatalf("logs/payload were mixed or command arguments logged: stderr=%s stdout=%s", text, stdout.String())
	}
}

func TestQueuedJobReportsWaitingProgressWithoutPollingSpam(t *testing.T) {
	local, _ := checkout(t)
	f := startCoordinator(t)
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	c := command(t, local, "-p", f.port, "/bin/echo", "payload")
	var stdout bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = stderr
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Process.Kill() }()
	deadline := time.Now().Add(18 * time.Second)
	var diagnostics []byte
	for {
		diagnostics, err = os.ReadFile(stderr.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(diagnostics), "still queued") {
			break
		}
		if time.Now().After(deadline) {
			_ = c.Process.Signal(os.Interrupt)
			_ = c.Wait()
			t.Fatalf("no periodic waiting progress: %s", diagnostics)
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = c.Process.Signal(os.Interrupt)
	result := c.Wait()
	if exitCode(result) != 130 || stdout.Len() != 0 {
		t.Fatalf("queued output/exit changed: exit=%d stdout=%s", exitCode(result), stdout.String())
	}
	if strings.Count(string(diagnostics), "still queued") != 1 || !strings.Contains(string(diagnostics), "output appears on completion") || !regexp.MustCompile(`\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}`).Match(diagnostics) {
		t.Fatalf("untimestamped or noisy waiting progress: %s", diagnostics)
	}
}

func TestProgressAppearsBeforeSlowGitPreflightCompletes(t *testing.T) {
	local, _ := checkout(t)
	git(t, local, "remote", "set-url", "origin", "ssh://test@example.invalid/project.git")
	dir := t.TempDir()
	started := filepath.Join(dir, "ssh-started")
	ssh := filepath.Join(dir, "slow-ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\ntouch '"+started+"'\nexec sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	c := command(t, local, "/bin/echo", "payload")
	c.Env = append(c.Env, "GIT_SSH_COMMAND="+ssh)
	var stdout bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = stderr
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Process.Kill() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = c.Process.Signal(os.Interrupt)
			_ = c.Wait()
			t.Fatal("Git preflight did not reach SSH")
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Process.Signal(os.Interrupt)
	result := c.Wait()
	if !strings.Contains(string(data), "checking Git source") || !regexp.MustCompile(`\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}`).Match(data) {
		t.Fatalf("no timestamped immediate progress while Git blocked: %q", data)
	}
	if strings.Contains(string(data), "job ID:") || stdout.Len() != 0 || exitCode(result) != 130 {
		t.Fatalf("submission/output occurred during preflight: stderr=%s stdout=%s exit=%d", data, stdout.String(), exitCode(result))
	}
}
