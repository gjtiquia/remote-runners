# Workspace contract

- `Roots{Repos, Worktrees string}` supplies worker storage roots.
- `Prepare(ctx, roots, protocol.Source{Remote, Branch}, output)` returns
  `Prepared{Root, Created, Commit}`. `Root` is absolute; `Created` is true only
  for a newly added worktree; `Commit` is the fetched branch tip used for this
  preparation. Jobs are branch-based, not pinned to the submission-time commit.
- `Preflight(ctx, dir, Overrides{Repository, Remote, Branch})` returns a source
  suitable for submission, or an actionable error. It never commits/pushes or
  changes checkout/index contents.

## Preflight choices

`Repository` selects a checkout directory, replacing `dir`; discovery works
from subdirectories. `Remote` accepts a configured remote name or explicit Git
URL/path, defaulting to `origin`. Plain explicit paths should contain `/` (e.g.
`./other.git`); a bare unknown name is diagnosed as an unknown named remote.
Local relative remotes are resolved relative to the checkout root. Local paths
are useful for tests/on-host execution; use reachable SSH/HTTPS URLs across
machines.

`Branch` selects an existing **local** branch, defaulting to the current branch.
It is compared with the same-named branch on the selected remote. Even with
branch overrides, the checkout must have an attached HEAD and be clean. Staged,
unstaged, and non-ignored untracked files are rejected. Ignored build artifacts
follow ordinary Git cleanliness semantics. Fetch writes a private
`refs/remote-runners/preflight/*` ref; origin's normal tracking refs and local
branch tips are not moved. Missing publication, ahead, behind, and divergence
have distinct remedies. Remote-access failures are not called unpublished
branches.

## Worker storage and safety

Ordinary paths are `Repos/project` (a bare clone) and
`Worktrees/project/branch` (detached worktrees). Separate remotes with the same
project basename get a stable identity-hash suffix. Reservations live under
`Repos/.identities`; do not delete those while retaining clones/worktrees.
Repository identity is shared with coordinator scheduling and runner window reuse.
Naming preserves the project/branch levels, encoding slash, percent, uppercase,
and other unsafe bytes in a single branch component. This avoids traversal,
branch-prefix nesting, and case-insensitive filesystem collisions. Very long
components use a shortened prefix plus hash. Repository identities normalize
local file URLs/symlinks, hostname case and SSH's default port, while retaining
transport, user, repository-path case, and relative-vs-absolute SSH semantics.

Preparation uses cross-process `flock` files under `Repos/.locks`, with
context-cancellable acquisition. A short allocation lock reserves names, and a
per-repository lock covers cloning, fetching, and worktree mutations. Normal
job commands are **not** covered by this lock: the coordinator must serialize
jobs targeting the same logical repository/branch. Storage needs a filesystem
supporting ordinary advisory locks (intended: local Linux/macOS storage).

Clone is staged in an owned temporary directory and uses `git clone --`.
Remote-option-like strings and invalid Git branch refs are rejected before
storage mutation. Existing clone origins/worktree ownership are verified;
worker child paths may not be symlinks. A reusable worktree is hard-reset to the
fetched tip, without `git clean` and without recursive submodule reset. Worker
untracked/ignored files are retained. If incoming tracked paths would overwrite
untracked files, preparation **fails**, asking the operator/hook to move them
aside, rather than destroying them.

Git commands inherit credentials/transport and the worker environment, except
repository-routing variables such as `GIT_DIR`, `GIT_WORK_TREE`, and
`GIT_INDEX_FILE`. Commands use their own process groups; context cancellation
kills ordinary descendants on Linux/macOS, not deliberate daemon/group escape.
Mutation diagnostics stream both stdout and stderr directly to `output` without
whole-log buffering. A nil writer discards diagnostics.

## Validation

Public-boundary tests use real temporary Git remotes/checkouts, six simultaneous
helper processes, and a real Git SSH-transport subprocess with an ordinary child.
They cover published creation/provenance, latest-state reset/reuse, untracked
conflict safety, normalized aliases, project collisions, long/case-distinct
branches, unrelated-path protection, output, process-group cancellation, root
inference/overrides, dirty states, unpublished/ahead/behind/diverged branches,
and detached/outside invocations (including exported Git routing variables).

Linux tests and macOS cross-compilation are automated; actual macOS runtime and
real network/credential behavior still require operator trials. Advisory locks
are not a sandbox against another process intentionally mutating the storage.
