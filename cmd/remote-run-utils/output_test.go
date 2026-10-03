package main_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
)

type patternVerifier struct{ n int64 }

func (v *patternVerifier) Write(p []byte) (int, error) {
	const pattern = "0123456789abcdef\n"
	for i, b := range p {
		if b != pattern[(v.n+int64(i))%17] {
			return 0, fmt.Errorf("output differs at byte %d", v.n+int64(i))
		}
	}
	v.n += int64(len(p))
	return len(p), nil
}

func TestOutputCopiesFullCompletedTextRawToStdout(t *testing.T) {
	binary := buildUtils(t)
	c, port, base := startCoordinator(t)
	conn, registration := registerRunner(t, base)
	job := submit(t, c)
	var assignment protocol.Message
	if err := conn.ReadJSON(&assignment); err != nil {
		t.Fatal(err)
	}
	if assignment.Type != "job" || assignment.Job.ID != job.ID {
		t.Fatalf("assignment: %+v", assignment)
	}
	chunk := bytes.Repeat([]byte("0123456789abcdef\n"), 2048)
	for n := 0; n < 256; n++ {
		if err := conn.WriteJSON(protocol.Message{Type: "output", RegistrationID: registration, JobID: job.ID, Data: chunk}); err != nil {
			t.Fatal(err)
		}
	}
	exit := 0
	if err := conn.WriteJSON(protocol.Message{Type: "complete", RegistrationID: registration, JobID: job.ID, State: "succeeded", ExitCode: &exit}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		j, err := c.Job(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if j.Terminal() {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "captured-output"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cmd := exec.CommandContext(ctx, binary, "-p", port, "output", job.ID)
	cmd.Stdout = file
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("output: %v: %s", err, errOut.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", errOut.String())
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	verifier := &patternVerifier{}
	if _, err := io.Copy(verifier, file); err != nil {
		t.Fatal(err)
	}
	if verifier.n != 8_912_896 {
		t.Fatalf("output truncated or decorated: %d bytes", verifier.n)
	}
}
