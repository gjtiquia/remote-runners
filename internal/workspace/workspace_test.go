package workspace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gjtiquia/remote-runners/internal/workspace"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "project.git")
	local := filepath.Join(root, "checkout")
	git(t, root, "init", "--bare", remote)
	git(t, root, "init", "-b", "main", local)
	if err := os.WriteFile(filepath.Join(local, "tracked"), []byte("initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, local, "add", "tracked")
	git(t, local, "commit", "-m", "initial")
	git(t, local, "remote", "add", "origin", remote)
	git(t, local, "push", "-u", "origin", "main")
	return remote, local
}
func TestPrepareReusesWorktreeAndResetsTrackedButKeepsUntracked(t *testing.T) {
	remote, local := fixture(t)
	root := t.TempDir()
	roots := workspace.Roots{Repos: filepath.Join(root, "repos"), Worktrees: filepath.Join(root, "worktrees")}
	source := protocol.Source{Remote: remote, Branch: "main"}
	p, err := workspace.Prepare(context.Background(), roots, source, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p.Root, "tracked"), []byte("worker edit\n"), 0600)
	os.WriteFile(filepath.Join(p.Root, "untracked"), []byte("keep me\n"), 0600)
	os.WriteFile(filepath.Join(local, "tracked"), []byte("latest\n"), 0600)
	os.WriteFile(filepath.Join(local, "untracked"), []byte("published\n"), 0600)
	git(t, local, "add", ".")
	git(t, local, "commit", "-m", "latest")
	git(t, local, "push")
	if _, err := workspace.Prepare(context.Background(), roots, source, io.Discard); err == nil {
		t.Fatal("must reject reset that would overwrite untracked files")
	}
	data, err := os.ReadFile(filepath.Join(p.Root, "untracked"))
	if err != nil || string(data) != "keep me\n" {
		t.Fatalf("untracked destroyed: %q %v", data, err)
	}
	os.Rename(filepath.Join(p.Root, "untracked"), filepath.Join(p.Root, "preserved"))
	next, err := workspace.Prepare(context.Background(), roots, source, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if next.Root != p.Root || next.Created || next.Commit+"\n" != git(t, local, "rev-parse", "HEAD") {
		t.Fatalf("bad reused preparation: %+v", next)
	}
	for path, want := range map[string]string{"tracked": "latest\n", "preserved": "keep me\n", "untracked": "published\n"} {
		data, err := os.ReadFile(filepath.Join(next.Root, path))
		if err != nil || string(data) != want {
			t.Fatalf("%s: %q %v", path, data, err)
		}
	}
}

func TestPrepareKeepsBranchPathsIndependentAndNormalizesRemoteIdentity(t *testing.T) {
	remote, local := fixture(t)
	root := t.TempDir()
	roots := workspace.Roots{Repos: filepath.Join(root, "repos"), Worktrees: filepath.Join(root, "worktrees")}
	// Branch names may share filesystem prefixes without sharing a worktree.
	for _, branch := range []string{"feature/sub", "feature%2Fsub"} {
		git(t, local, "push", "origin", "HEAD:refs/heads/"+branch)
	}
	first, err := workspace.Prepare(context.Background(), roots, protocol.Source{Remote: remote, Branch: "feature/sub"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	alias, err := workspace.Prepare(context.Background(), roots, protocol.Source{Remote: "file://" + remote, Branch: "feature/sub"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if alias.Root != first.Root || alias.Created {
		t.Fatalf("equivalent remote must reuse: %+v %+v", first, alias)
	}
	seen := map[string]bool{first.Root: true}
	for _, branch := range []string{"feature%2Fsub"} {
		p, err := workspace.Prepare(context.Background(), roots, protocol.Source{Remote: remote, Branch: branch}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if seen[p.Root] || filepath.Dir(p.Root) != filepath.Dir(first.Root) {
			t.Fatalf("branch worktree not independent: %s", p.Root)
		}
		seen[p.Root] = true
	}
}

func TestPreflightDiscoversRootAndPublishedSource(t *testing.T) {
	remote, local := fixture(t)
	sub := filepath.Join(local, "subdirectory")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := workspace.Preflight(context.Background(), sub, workspace.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if source != (protocol.Source{Remote: remote, Branch: "main"}) {
		t.Fatalf("inferred source: %+v", source)
	}
	source, err = workspace.Preflight(context.Background(), t.TempDir(), workspace.Overrides{Repository: local, Remote: remote, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if source != (protocol.Source{Remote: remote, Branch: "main"}) {
		t.Fatalf("override source: %+v", source)
	}
}

func TestPreflightRejectsDirtyCheckoutWithRemedy(t *testing.T) {
	_, local := fixture(t)
	for _, state := range []string{"untracked", "unstaged", "staged"} {
		t.Run(state, func(t *testing.T) {
			path := filepath.Join(local, "tracked")
			if state == "untracked" {
				path = filepath.Join(local, "untracked")
			}
			if err := os.WriteFile(path, []byte("local edit\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if state == "staged" {
				git(t, local, "add", "tracked")
			}
			_, err := workspace.Preflight(context.Background(), local, workspace.Overrides{})
			if err == nil || !strings.Contains(err.Error(), "stash") {
				t.Fatalf("want actionable dirty error, got %v", err)
			}
			if state == "untracked" {
				os.Remove(path)
			} else {
				git(t, local, "reset", "--hard", "HEAD")
			}
		})
	}
}

func TestPreflightRequiresSynchronizedBranchTips(t *testing.T) {
	remote, local := fixture(t)
	original := strings.TrimSpace(git(t, local, "rev-parse", "HEAD"))
	os.WriteFile(filepath.Join(local, "tracked"), []byte("local commit\n"), 0600)
	git(t, local, "commit", "-am", "ahead")
	assertError := func(word string) {
		t.Helper()
		_, err := workspace.Preflight(context.Background(), local, workspace.Overrides{})
		if err == nil || !strings.Contains(err.Error(), word) {
			t.Fatalf("want %q error, got %v", word, err)
		}
	}
	assertError("push")
	git(t, local, "push")
	git(t, local, "reset", "--hard", original)
	assertError("behind")
	os.WriteFile(filepath.Join(local, "tracked"), []byte("divergent commit\n"), 0600)
	git(t, local, "commit", "-am", "diverged")
	assertError("diverged")
	// Checking cannot publish or move local branch tips.
	if got := strings.TrimSpace(git(t, local, "rev-parse", "HEAD")); got == original {
		t.Fatal("preflight changed local branch")
	}
	published := strings.TrimSpace(git(t, filepath.Dir(remote), "--git-dir="+remote, "rev-parse", "refs/heads/main"))
	if published == strings.TrimSpace(git(t, local, "rev-parse", "HEAD")) {
		t.Fatal("preflight published a commit")
	}
}

func TestWorkspaceHelper(t *testing.T) {
	if os.Getenv("WORKSPACE_HELPER") != "1" {
		return
	}
	root := os.Getenv("WORKSPACE_ROOT")
	id := os.Getenv("WORKSPACE_ID")
	if err := os.WriteFile(filepath.Join(root, "ready"+id), nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "go")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("start barrier timed out")
		}
		time.Sleep(time.Millisecond)
	}
	p, err := workspace.Prepare(context.Background(), workspace.Roots{Repos: filepath.Join(root, "repos"), Worktrees: filepath.Join(root, "worktrees")}, protocol.Source{Remote: os.Getenv("WORKSPACE_REMOTE"), Branch: "main"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	json.NewEncoder(os.Stdout).Encode(p)
	os.Exit(0)
}

func TestPrepareSerializesAcrossHelperProcesses(t *testing.T) {
	remote, _ := fixture(t)
	root := t.TempDir()
	const n = 6
	cmds := make([]*exec.Cmd, n)
	outputs := make([]bytes.Buffer, n)
	for i := range cmds {
		cmds[i] = exec.Command(os.Args[0], "-test.run=^TestWorkspaceHelper$")
		cmds[i].Env = append(os.Environ(), "WORKSPACE_HELPER=1", "WORKSPACE_ROOT="+root, "WORKSPACE_REMOTE="+remote, fmt.Sprintf("WORKSPACE_ID=%d", i))
		cmds[i].Stdout = &outputs[i]
		cmds[i].Stderr = &outputs[i]
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, cmd := range cmds {
			if cmd != nil && cmd.Process != nil {
				cmd.Process.Kill()
			}
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		ready := true
		for i := range cmds {
			if _, err := os.Stat(filepath.Join(root, fmt.Sprintf("ready%d", i))); err != nil {
				ready = false
			}
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helpers never ready")
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(root, "go"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	created := 0
	path := ""
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper %d: %v\n%s", i, err, outputs[i].String())
		}
		var p workspace.Prepared
		if err := json.Unmarshal(outputs[i].Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if p.Created {
			created++
		}
		if path == "" {
			path = p.Root
		}
		if p.Root != path {
			t.Fatal("helpers did not reuse one worktree")
		}
	}
	if created != 1 {
		t.Fatalf("created worktree %d times", created)
	}
}

func TestPrepareCancellationStopsGitDescendantsAndKeepsDiagnostics(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "ssh")
	pidfile := filepath.Join(root, "pids")
	body := "#!/bin/sh\nprintf 'transport stderr diagnostic\\n' >&2\nsleep 300 &\nprintf '%s %s' \"$$\" \"$!\" > \"$PIDFILE\"\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", "'"+script+"'")
	t.Setenv("GIT_SSH_VARIANT", "ssh")
	t.Setenv("PIDFILE", pidfile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := workspace.Prepare(ctx, workspace.Roots{Repos: filepath.Join(root, "repos"), Worktrees: filepath.Join(root, "worktrees")}, protocol.Source{Remote: "git@example.invalid:project.git", Branch: "main"}, &output)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	var pids []byte
	for {
		var err error
		pids, err = os.ReadFile(pidfile)
		if err == nil && len(strings.Fields(string(pids))) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("transport did not start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Git cancellation hung")
	}
	for _, field := range strings.Fields(string(pids)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}
		// A reparented zombie is dead, though kill(pid,0) can still see it on Linux.
		deadline := time.Now().Add(2 * time.Second)
		for {
			state, _ := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
			if syscall.Kill(pid, 0) == syscall.ESRCH || strings.Contains(string(state), "Z") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("ordinary descendant %d still running", pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !strings.Contains(output.String(), "transport stderr diagnostic") {
		t.Fatalf("missing diagnostics: %s", output.String())
	}
}

func TestPrepareCapturesGitStdoutDiagnostics(t *testing.T) {
	remote, _ := fixture(t)
	root := t.TempDir()
	roots := workspace.Roots{Repos: filepath.Join(root, "repos"), Worktrees: filepath.Join(root, "worktrees")}
	source := protocol.Source{Remote: remote, Branch: "main"}
	if _, err := workspace.Prepare(context.Background(), roots, source, io.Discard); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := workspace.Prepare(context.Background(), roots, source, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "HEAD is now at") {
		t.Fatalf("reset diagnostics missing: %q", output.String())
	}
}

func TestPrepareNeverResetsUnrelatedCheckoutAtOwnedPath(t *testing.T) {
	remote, local := fixture(t)
	root := t.TempDir()
	roots := workspace.Roots{Repos: filepath.Join(root, "repos"), Worktrees: filepath.Join(root, "worktrees")}
	source := protocol.Source{Remote: remote, Branch: "main"}
	p, err := workspace.Prepare(context.Background(), roots, source, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	git(t, p.Root, "worktree", "remove", p.Root)
	if err := os.Symlink(local, p.Root); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(local, "tracked"), []byte("unrelated local change\n"), 0600)
	if _, err := workspace.Prepare(context.Background(), roots, source, io.Discard); err == nil {
		t.Fatal("must reject unrelated symlink checkout")
	}
	data, err := os.ReadFile(filepath.Join(local, "tracked"))
	if err != nil || string(data) != "unrelated local change\n" {
		t.Fatalf("unrelated source modified: %q %v", data, err)
	}
}

func TestPreflightRejectsUnpublishedBranch(t *testing.T) {
	_, local := fixture(t)
	git(t, local, "switch", "-c", "draft")
	_, err := workspace.Preflight(context.Background(), local, workspace.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "unpublished") || !strings.Contains(err.Error(), "push") {
		t.Fatalf("want unpublished branch remedy: %v", err)
	}
}

func TestPrepareSupportsLongAndCaseDistinctBranchNames(t *testing.T) {
	remote, local := fixture(t)
	root := t.TempDir()
	roots := workspace.Roots{Repos: filepath.Join(root, "repos"), Worktrees: filepath.Join(root, "worktrees")}
	seen := map[string]bool{}
	branches := []string{"Feature", "feature", strings.Repeat("long", 20) + "/" + strings.Repeat("branch", 20) + "/" + strings.Repeat("name", 20)}
	for _, branch := range branches {
		git(t, local, "push", "origin", "HEAD:refs/heads/"+branch)
		p, err := workspace.Prepare(context.Background(), roots, protocol.Source{Remote: remote, Branch: branch}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		key := strings.ToLower(p.Root)
		if seen[key] {
			t.Fatal("worktree names collide on case-insensitive filesystems")
		}
		seen[key] = true
	}
}

func TestPreflightRejectsOutsideAndDetachedEvenWithSourceOverrides(t *testing.T) {
	remote, local := fixture(t)
	// Shells can export Git's repository-routing variables; they must not redirect
	// validation away from the directory the caller explicitly selected.
	t.Setenv("GIT_DIR", filepath.Join(local, ".git"))
	t.Setenv("GIT_WORK_TREE", local)
	_, err := workspace.Preflight(context.Background(), t.TempDir(), workspace.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "not a Git checkout") {
		t.Fatalf("outside checkout error: %v", err)
	}
	t.Setenv("GIT_DIR", "")
	t.Setenv("GIT_WORK_TREE", "")
	// The fixture command helper strips routing variables as well.
	cmd := exec.Command("git", "-C", local, "checkout", "--detach")
	cmd.Env = append(os.Environ(), "GIT_DIR="+filepath.Join(local, ".git"), "GIT_WORK_TREE="+local)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("detach: %v %s", err, output)
	}
	_, err = workspace.Preflight(context.Background(), local, workspace.Overrides{Remote: remote, Branch: "main"})
	if err == nil || !strings.Contains(err.Error(), "detached HEAD") {
		t.Fatalf("detached error: %v", err)
	}
}

func TestPreparePreservesProjectBranchLayoutAndDisambiguatesProjects(t *testing.T) {
	firstRemote, _ := fixture(t)
	secondRemote, _ := fixture(t)
	root := t.TempDir()
	roots := workspace.Roots{Repos: filepath.Join(root, "repos"), Worktrees: filepath.Join(root, "worktrees")}
	first, err := workspace.Prepare(context.Background(), roots, protocol.Source{Remote: firstRemote, Branch: "main"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if first.Root != filepath.Join(roots.Worktrees, "project", "main") {
		t.Fatalf("ordinary project/branch layout: %s", first.Root)
	}
	second, err := workspace.Prepare(context.Background(), roots, protocol.Source{Remote: secondRemote, Branch: "main"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if second.Root == first.Root || !strings.HasPrefix(filepath.Base(filepath.Dir(second.Root)), "project-") {
		t.Fatalf("colliding project not disambiguated: %s", second.Root)
	}
	again, err := workspace.Prepare(context.Background(), roots, protocol.Source{Remote: secondRemote, Branch: "main"}, io.Discard)
	if err != nil || again.Root != second.Root || again.Created {
		t.Fatalf("collision name not stable: %+v %v", again, err)
	}
}

func TestPrepareCreatesPublishedBranchWorktree(t *testing.T) {
	remote, local := fixture(t)
	root := t.TempDir()
	p, err := workspace.Prepare(context.Background(), workspace.Roots{Repos: filepath.Join(root, "repos"), Worktrees: filepath.Join(root, "worktrees")}, protocol.Source{Remote: remote, Branch: "main"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Created {
		t.Fatal("first preparation must report creation")
	}
	if p.Commit+"\n" != git(t, local, "rev-parse", "HEAD") {
		t.Fatalf("wrong provenance: %s", p.Commit)
	}
	data, err := os.ReadFile(filepath.Join(p.Root, "tracked"))
	if err != nil || string(data) != "initial\n" {
		t.Fatalf("published content: %q %v", data, err)
	}
}
