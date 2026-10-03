package main_test

import (
	"encoding/json"
	"testing"

	"github.com/gjtiquia/remote-runners/internal/protocol"
)

func TestJobInspectsSubmissionAndStatusAsJSON(t *testing.T) {
	binary := buildUtils(t)
	c, port, _ := startCoordinator(t)
	job := submit(t, c)
	out, errOut, code := runUtils(t, binary, "-p", port, "job", job.ID)
	if code != 0 || errOut != "" {
		t.Fatalf("job exited %d: %s", code, errOut)
	}
	var detail protocol.Job
	if err := json.Unmarshal([]byte(out), &detail); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if detail.ID != job.ID || detail.State != "queued" || detail.Source.Branch != "main" || detail.Source.Remote != "git@example.com:team/repo.git" || len(detail.Args) != 2 || detail.Args[0] != "printf" || detail.Args[1] != "hello" || detail.CreatedAt.IsZero() {
		t.Fatalf("detail: %+v", detail)
	}
}
