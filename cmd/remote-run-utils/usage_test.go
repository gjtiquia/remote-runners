package main_test

import (
	"strings"
	"testing"
)

func TestMalformedInvocationsFailClearlyWithoutQueryingOrMutatingJobs(t *testing.T) {
	binary := buildUtils(t)
	c, port, _ := startCoordinator(t)
	job := submit(t, c)
	cases := [][]string{
		{}, {"unknown"}, {"jobs", "extra"}, {"runners", "-p", port},
		{"job"}, {"job", job.ID, "extra"}, {"output"}, {"cancel"},
		{"cancel", job.ID, "extra"}, {"cancel", ""},
		{"job", "-h"}, {"output", "-p"}, {"cancel", "-h"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			args = append([]string{"-p", port}, args...)
			out, errOut, code := runUtils(t, binary, args...)
			if code != 2 || out != "" || !strings.Contains(errOut, "usage:") || strings.Contains(errOut, "panic") {
				t.Fatalf("bad usage: exit=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}
	for _, args := range [][]string{{"-p", "0", "jobs"}, {"-p", "65536", "jobs"}, {"-p", "no", "jobs"}, {"-unknown"}} {
		out, errOut, code := runUtils(t, binary, args...)
		if code != 2 || out != "" || errOut == "" || strings.Contains(errOut, "panic") {
			t.Fatalf("bad flag: exit=%d stdout=%q stderr=%q", code, out, errOut)
		}
	}
	out, errOut, code := runUtils(t, binary, "-p", port, "job", job.ID)
	if code != 0 || !strings.Contains(out, `"state":"queued"`) {
		t.Fatalf("invalid invocation mutated job: %s %s", out, errOut)
	}
}
