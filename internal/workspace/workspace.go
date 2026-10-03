// Package workspace prepares worker-owned Git worktrees and validates submission source.
package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gjtiquia/remote-runners/internal/repository"
)

type Roots struct{ Repos, Worktrees string }
type Prepared struct {
	Root    string
	Created bool
	Commit  string
}
type Overrides struct{ Repository, Remote, Branch string }

// runGit inherits the worker environment and cancels ordinary Git descendants, too.
func runGit(ctx context.Context, dir string, output io.Writer, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Preserve credentials/transport and the worker environment, but do not let
	// a caller's exported Git routing variables select some unrelated checkout.
	routing := map[string]bool{"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_COMMON_DIR": true, "GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_PREFIX": true, "GIT_NAMESPACE": true}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !routing[key] {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	var stdout, stderr bytes.Buffer
	if output != nil {
		// A single shared writer makes os/exec serialize stdout/stderr copies.
		// Stream diagnostics directly; never buffer full preparation output.
		cmd.Stdout = output
		cmd.Stderr = output
	} else {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	}
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSuffix(stdout.String(), "\n"), nil
}

// A hard reset may delete untracked paths obstructing incoming tracked paths.
// Refuse that reset rather than deleting worker artifacts (including ignored files).
func protectUntracked(ctx context.Context, root, commit string) error {
	untracked, err := runGit(ctx, root, nil, "ls-files", "--others", "-z")
	if err != nil {
		return err
	}
	incoming, err := runGit(ctx, root, nil, "ls-tree", "-r", "--name-only", "-z", commit)
	if err != nil {
		return err
	}
	files := make(map[string]bool)
	directories := make(map[string]bool)
	for _, p := range strings.Split(incoming, "\x00") {
		if p == "" {
			continue
		}
		files[p] = true
		for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
			directories[parent] = true
		}
	}
	for _, u := range strings.Split(untracked, "\x00") {
		if u == "" {
			continue
		}
		conflict := files[u] || directories[u]
		for parent := path.Dir(u); !conflict && parent != "."; parent = path.Dir(parent) {
			conflict = files[parent]
		}
		if conflict {
			return fmt.Errorf("cannot reset: untracked path %q conflicts with published files; move it aside or clean it explicitly", u)
		}
	}
	return nil
}

func validateSource(ctx context.Context, source protocol.Source) error {
	if source.Remote == "" || strings.HasPrefix(source.Remote, "-") || strings.ContainsAny(source.Remote, "\x00\r\n") {
		return fmt.Errorf("invalid remote: supply a Git URL or repository path, not an option")
	}
	if source.Branch == "" || strings.HasPrefix(source.Branch, "-") || strings.ContainsAny(source.Branch, "\x00\r\n") {
		return fmt.Errorf("invalid branch: supply a published branch name")
	}
	if _, err := runGit(ctx, "", nil, "check-ref-format", "refs/heads/"+source.Branch); err != nil {
		return fmt.Errorf("invalid branch %q: %w", source.Branch, err)
	}
	return nil
}

func escape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	if b.Len() == 0 {
		return "repository"
	}
	escaped := b.String()
	if len(escaped) > 150 {
		sum := sha256.Sum256([]byte(s))
		escaped = fmt.Sprintf("%s-%x", escaped[:80], sum)
	}
	return escaped
}

// flock is shared by independently launched helpers. Keep the lock file: unlinking
// it would allow another helper to lock a different inode for the same repository.
func lockRepository(ctx context.Context, dir, project string) (func(), error) {
	locks := filepath.Join(dir, ".locks")
	if err := safeDirectory(locks); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(locks, project+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { syscall.Flock(int(file.Fd()), syscall.LOCK_UN); file.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func safeDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		if err := os.Mkdir(dir, 0700); err == nil {
			return nil
		} else if !os.IsExist(err) {
			return err
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing non-directory or symlink workspace path %q", dir)
	}
	return nil
}

func verifyWorktree(ctx context.Context, root, clone string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing non-directory or symlink worktree %q", root)
	}
	common, err := runGit(ctx, root, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("existing path is not a worker-owned worktree: %w", err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return err
	}
	expected, err := filepath.EvalSymlinks(clone)
	if err != nil {
		return err
	}
	if common != expected {
		return fmt.Errorf("refusing unrelated checkout at %q; move it aside instead of overwriting it", root)
	}
	return nil
}

// Reserve readable project names across processes before cloning. A same-name
// repository gets an identity suffix; reservations survive failed preparations.
func projectName(ctx context.Context, repos, name, identity string) (string, error) {
	if err := os.MkdirAll(repos, 0700); err != nil {
		return "", err
	}
	unlock, err := lockRepository(ctx, repos, ".project-names")
	if err != nil {
		return "", err
	}
	defer unlock()
	identities := filepath.Join(repos, ".identities")
	if err := safeDirectory(identities); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(identity))
	base := escape(name)
	for _, candidate := range []string{base, fmt.Sprintf("%s-%x", base, sum[:16]), fmt.Sprintf("%s-%x", base, sum)} {
		marker := filepath.Join(identities, candidate)
		file, err := os.OpenFile(marker, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err == nil {
			data, readErr := io.ReadAll(file)
			file.Close()
			if readErr != nil {
				return "", readErr
			}
			if string(data) == identity {
				return candidate, nil
			}
			continue
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		// An unclaimed existing clone is not ours: never adopt/overwrite it.
		if _, err := os.Lstat(filepath.Join(repos, candidate)); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return "", err
		}
		file, err = os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return "", err
		}
		_, writeErr := io.WriteString(file, identity)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			os.Remove(marker)
			if writeErr != nil {
				return "", writeErr
			}
			return "", closeErr
		}
		return candidate, nil
	}
	return "", fmt.Errorf("repository identity collision for %q; inspect clone identities before retrying", name)
}

func Prepare(ctx context.Context, roots Roots, source protocol.Source, output io.Writer) (result Prepared, finalErr error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result = Prepared{}
			finalErr = err
		}
	}()
	if output == nil {
		output = io.Discard
	}
	if roots.Repos == "" || roots.Worktrees == "" {
		return Prepared{}, fmt.Errorf("repository and worktree roots are required")
	}
	var err error
	roots.Repos, err = filepath.Abs(roots.Repos)
	if err != nil {
		return Prepared{}, err
	}
	roots.Worktrees, err = filepath.Abs(roots.Worktrees)
	if err != nil {
		return Prepared{}, err
	}
	if err := validateSource(ctx, source); err != nil {
		return Prepared{}, err
	}
	identity, err := repository.Identity(source.Remote)
	if err != nil {
		return Prepared{}, err
	}
	name := path.Base(strings.TrimSuffix(identity, ".git"))
	if i := strings.LastIndex(name, ":"); i >= 0 {
		name = name[i+1:]
	}
	project, err := projectName(ctx, roots.Repos, name, identity)
	if err != nil {
		return Prepared{}, err
	}
	clone := filepath.Join(roots.Repos, project)
	root := filepath.Join(roots.Worktrees, project, escape(source.Branch))
	refsum := sha256.Sum256([]byte(source.Branch))
	ref := fmt.Sprintf("refs/remote-runners/%x", refsum)
	if err := os.MkdirAll(roots.Repos, 0700); err != nil {
		return Prepared{}, err
	}
	unlock, err := lockRepository(ctx, roots.Repos, project)
	if err != nil {
		return Prepared{}, err
	}
	defer unlock()
	if _, err := os.Lstat(clone); os.IsNotExist(err) {
		temporary, err := os.MkdirTemp(roots.Repos, "."+project+"-clone-")
		if err != nil {
			return Prepared{}, err
		}
		defer os.RemoveAll(temporary)
		if _, err = runGit(ctx, "", output, "clone", "--bare", "--", source.Remote, temporary); err != nil {
			return Prepared{}, err
		}
		if err := os.Rename(temporary, clone); err != nil {
			return Prepared{}, err
		}
	} else if err != nil {
		return Prepared{}, err
	}
	cloneInfo, err := os.Lstat(clone)
	if err != nil {
		return Prepared{}, err
	}
	if !cloneInfo.IsDir() || cloneInfo.Mode()&os.ModeSymlink != 0 {
		return Prepared{}, fmt.Errorf("refusing symlink or non-directory clone %q", clone)
	}
	bare, err := runGit(ctx, clone, nil, "rev-parse", "--is-bare-repository")
	if err != nil {
		return Prepared{}, err
	}
	if bare != "true" {
		return Prepared{}, fmt.Errorf("existing clone %q is not a worker bare repository", clone)
	}
	origin, err := runGit(ctx, clone, nil, "remote", "get-url", "origin")
	if err != nil {
		return Prepared{}, err
	}
	actual, err := repository.Identity(origin)
	if err != nil || actual != identity {
		return Prepared{}, fmt.Errorf("existing clone %q belongs to a different remote", clone)
	}
	if err := os.MkdirAll(roots.Worktrees, 0700); err != nil {
		return Prepared{}, err
	}
	if err := safeDirectory(filepath.Dir(root)); err != nil {
		return Prepared{}, err
	}
	if _, err := runGit(ctx, clone, output, "fetch", "--no-tags", "--", "origin", "+refs/heads/"+source.Branch+":"+ref); err != nil {
		return Prepared{}, err
	}
	commit, err := runGit(ctx, clone, nil, "rev-parse", ref)
	if err != nil {
		return Prepared{}, err
	}
	created := false
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		if _, err := runGit(ctx, clone, output, "worktree", "add", "--detach", root, commit); err != nil {
			return Prepared{}, err
		}
		created = true
	} else {
		if err := verifyWorktree(ctx, root, clone); err != nil {
			return Prepared{}, err
		}
		if err := protectUntracked(ctx, root, commit); err != nil {
			return Prepared{}, err
		}
		if _, err := runGit(ctx, root, output, "-c", "submodule.recurse=false", "reset", "--hard", commit); err != nil {
			return Prepared{}, err
		}
	}
	return Prepared{Root: root, Created: created, Commit: commit}, nil
}
