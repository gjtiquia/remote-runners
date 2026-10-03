package runner_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommittedHookRunsWithInheritedGitRoutingEnvironment(t *testing.T) {
	source := repository(t, `{"BeforeJobCommand":"printf 'HOOK|%s|%s|%s\\n' \"$GIT_DIR\" \"$GIT_WORK_TREE\" \"$MARKER\""}`)
	// This repository deliberately has no committed hook configuration. Exported
	// routing variables must not make the runner check its index instead.
	unrelated := repository(t, "")
	gitDir := filepath.Join(unrelated, ".git")
	f := startRunnerWithEnvironment(t, 2, []string{"GIT_DIR=" + gitDir, "GIT_WORK_TREE=" + unrelated, "MARKER=inherited-by-hook-and-job"})
	f.source = source
	job := f.submit(t, "/bin/sh", "-c", `printf 'JOB|%s|%s|%s\n' "$GIT_DIR" "$GIT_WORK_TREE" "$MARKER"`)
	result, out := f.wait(t, job.ID)
	want := fmt.Sprintf("HOOK|%s|%s|inherited-by-hook-and-job\nJOB|%s|%s|inherited-by-hook-and-job\n", gitDir, unrelated, gitDir, unrelated)
	if result.State != "succeeded" || result.ExitCode == nil || *result.ExitCode != 0 || !strings.Contains(out, want) {
		t.Fatalf("committed hook must precede the job, both inheriting the runner environment: result=%v output=%s", result, out)
	}
}
