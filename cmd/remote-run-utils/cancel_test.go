package main_test

import (
	"context"
	"testing"
	"time"
)

func TestCancelRemovesQueuedJobEligibilityByID(t *testing.T) {
	binary := buildUtils(t)
	c, port, _ := startCoordinator(t)
	job := submit(t, c)
	out, errOut, code := runUtils(t, binary, "-p", port, "cancel", job.ID)
	if code != 0 || errOut != "" || out != "" {
		t.Fatalf("cancel exited %d: stdout=%q stderr=%q", code, out, errOut)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	detail, err := c.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "cancelled" {
		t.Fatalf("cancelled job: %+v", detail)
	}
}
