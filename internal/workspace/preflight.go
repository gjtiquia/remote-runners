package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gjtiquia/remote-runners/internal/protocol"
)

// Preflight validates a clean checkout against the selected published branch.
// Repository overrides the checkout directory; Branch selects a local branch
// (default current branch). Remote accepts a configured name or an explicit Git
// URL/path (default origin). Fetch updates only a private remote-runners ref.
// It never commits, pushes, resets, or checks out local source.
func Preflight(ctx context.Context, dir string, overrides Overrides) (result protocol.Source, finalErr error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result = protocol.Source{}
			finalErr = err
		}
	}()
	if overrides.Repository != "" {
		dir = overrides.Repository
	}
	root, err := runGit(ctx, dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return protocol.Source{}, fmt.Errorf("not a Git checkout: run from a repository or select --repository: %w", err)
	}
	current, err := runGit(ctx, root, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return protocol.Source{}, fmt.Errorf("detached HEAD: check out a local branch before submitting")
	}
	status, err := runGit(ctx, root, nil, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return protocol.Source{}, err
	}
	if status != "" {
		return protocol.Source{}, fmt.Errorf("dirty checkout: commit and push intended changes, or stash them (git stash -u for untracked files), before submitting")
	}
	branch := overrides.Branch
	if branch == "" {
		branch = current
	}
	selected := overrides.Remote
	if selected == "" {
		selected = "origin"
	}
	remote, err := runGit(ctx, root, nil, "remote", "get-url", "--", selected)
	if err != nil {
		if !strings.ContainsAny(selected, "/:") {
			return protocol.Source{}, fmt.Errorf("remote %q is not configured; add origin or supply --remote with a remote name or URL", selected)
		}
		remote = selected
	}
	// Git resolves local remote paths relative to the checkout. Workers require
	// an absolute path (use SSH/HTTPS remotes to reach a different machine).
	if !strings.Contains(remote, ":") && !filepath.IsAbs(remote) {
		remote = filepath.Join(root, remote)
	}
	source := protocol.Source{Remote: remote, Branch: branch}
	if err := validateSource(ctx, source); err != nil {
		return protocol.Source{}, err
	}
	localRef := "refs/heads/" + branch
	if _, err := runGit(ctx, root, nil, "rev-parse", "--verify", localRef+"^{commit}"); err != nil {
		return protocol.Source{}, fmt.Errorf("local branch %q does not exist; check it out or select an existing --branch", branch)
	}
	sum := sha256.Sum256([]byte(remote + "\x00" + branch))
	ref := fmt.Sprintf("refs/remote-runners/preflight/%x", sum)
	if _, err := runGit(ctx, root, nil, "fetch", "--no-tags", "--", remote, "+refs/heads/"+branch+":"+ref); err != nil {
		if ctx.Err() != nil {
			return protocol.Source{}, ctx.Err()
		}
		_, lookupErr := runGit(ctx, root, nil, "ls-remote", "--exit-code", "--", remote, "refs/heads/"+branch)
		var exit *exec.ExitError
		if errors.As(lookupErr, &exit) && exit.ExitCode() == 2 {
			return protocol.Source{}, fmt.Errorf("unpublished branch %q: publish it with git push -u %s %s before submitting", branch, selected, branch)
		}
		return protocol.Source{}, fmt.Errorf("cannot fetch branch %q; verify remote access/credentials and retry: %w", branch, err)
	}
	counts, err := runGit(ctx, root, nil, "rev-list", "--left-right", "--count", localRef+"..."+ref)
	if err != nil {
		return protocol.Source{}, err
	}
	fields := strings.Fields(counts)
	if len(fields) != 2 {
		return protocol.Source{}, fmt.Errorf("cannot determine branch synchronization: %q", counts)
	}
	ahead, err := strconv.Atoi(fields[0])
	if err != nil {
		return protocol.Source{}, err
	}
	behind, err := strconv.Atoi(fields[1])
	if err != nil {
		return protocol.Source{}, err
	}
	switch {
	case ahead > 0 && behind > 0:
		return protocol.Source{}, fmt.Errorf("branch %q has diverged (%d ahead, %d behind); synchronize with the remote using merge or rebase, resolve conflicts, then push", branch, ahead, behind)
	case ahead > 0:
		return protocol.Source{}, fmt.Errorf("branch %q has %d unpushed commits; git push %s %s before submitting", branch, ahead, selected, branch)
	case behind > 0:
		return protocol.Source{}, fmt.Errorf("branch %q is behind by %d commits; synchronize with the remote (git pull --ff-only for the current branch) before submitting", branch, behind)
	}
	return source, nil
}
