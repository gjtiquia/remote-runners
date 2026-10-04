# Remote Runners

A small, trusted Go tool for running heavy project commands on spare Linux/macOS
machines. Human-first, agent-friendly: submit a clean, pushed Git branch and one
executable with its arguments. No containers, task catalogue, or recovery platform.

## Install

Requires Go 1.24 or newer for source installation:

```sh
# All four executables (coordinator/development machine)
go install github.com/gjtiquia/remote-runners/cmd/...@latest

# Worker only
go install github.com/gjtiquia/remote-runners/cmd/remote-runner@latest

# Development branch, or from this checkout
go install github.com/gjtiquia/remote-runners/cmd/...@main
go install ./cmd/...
```

These remote install commands require the implementation to be pushed/released;
`@latest` follows Go version selection, not necessarily the newest main commit.
`go install ./cmd/...` is unchanged: Go embeds VCS revision and modified-state
metadata automatically. Clients and workers report that build metadata; only the
coordinator enforces build identity, rejecting unknown, dirty, or mismatched builds
before doing work. Rejection is logged by the server and reported to the peer.
Upgrading requires reinstalling and restarting the relevant binaries. For a dirty
checkout, commit the changes or rebuild from a clean checkout before installing.
The coordinator also refuses to start from a dirty or unknown build. For this
workflow, install from a clean Git checkout: remote module installs (`@latest`,
`@main`, or `@SHA`) may lack the required VCS metadata and be rejected.
Binaries go to `GOBIN`, or usually `~/go/bin`; add that directory to `PATH`.
Go is not required to run built binaries. Workers need Git, tmux (tested on 3.5a),
SSH/repository access, and project tools such as Bun and browsers. The dedicated
tmux session's effective `default-shell` must be Zsh (with its standard
`zsh/parameter` module) or Bash 5.1+; older macOS system Bash is not supported.

## Start the coordinator

```sh
remote-runner-server                 # default port 2461
remote-runner-server -p 3000          # optional override
```

The coordinator accepts trusted worker connections over the network, but
submission and management only from loopback socket peers. Expose its port only
on your trusted operator-managed network (e.g. Tailscale). There is **no application
authentication or sandbox**. No built-in TLS termination is provided.

## Configure and start a worker

First run creates a starter and stops:

```sh
remote-runner
```

Edit **`~/.remote-runners/config.json`** in a text editor. Set `name` to a unique,
stable machine name and `coordinator_url` to `ws://COORDINATOR:2461/runner`.
On upgrade, an existing `~/.remote-runner/config.json` is moved here automatically
if the new location does not exist. If both exist, the new location wins and the
old file is left untouched. Symlinks or cross-filesystem moves require manual
migration with an actionable message. All worker state now shares
`~/.remote-runners/`.

Starter defaults:

| Setting | Default |
| --- | --- |
| `priority` | 50 (0–100; highest eligible wins) |
| `max_jobs` | 5, total across projects and branches on this worker |
| `min_memory` | 1073741824 bytes (1 GiB admission threshold) |
| `max_windows` | 8 retained job windows, excluding worker window |
| `repos` | `~/.remote-runners/repos` (expanded absolute path in starter) |
| `worktrees` | `~/.remote-runners/worktrees` (expanded absolute path in starter) |

Start inside **one dedicated tmux session**, with no unrelated windows:

```sh
tmux new-session -s remote-runners
remote-runner
```

Keep the worker window running. Each worktree gets a real, persistent interactive
**non-login** shell and your normal prompt. Zsh loads normal `.zshenv`/`.zshrc`
(including `ZDOTDIR`); Bash loads `.bashrc`. Private startup wrappers and prompt
hooks are generated in the job directory; your dotfiles are never edited. After
preparation/execution the shell changes to the prepared worktree root and receives
the command status. Later jobs reuse that same window and shell, without respawn.
At the retention limit the least-recently-used verified idle job pane is evicted,
never an active/worker pane or an entire window. Worktrees stay on disk.
Typing `exit` at the job shell prompt closes its pane/window normally; the next
job creates a fresh shell if that worktree's window was closed.

**Environment:** launch the worker with your desired baseline exports and `PATH`.
Those keys explicitly override tmux's environment when creating the shell; normal
tmux globals may also be present (this is not strict environment isolation).
Shell startup and subsequent manual exports define the actual pane environment
inherited by preparation, hooks, and commands. `TMUX`, `TMUX_PANE`, and `TERM`
belong to the job pane.

**Manual use:** leave an untouched primary prompt and do not type while submitting.
A manual foreground command (including a builtin), or a running/stopped background
job, fails submission immediately with exit 125, `worktree window is busy`—no
waiting, requeueing, or cancellation injected into your command. Half-typed input
and continuation prompts cannot be detected externally. Copy mode, extra panes,
a replaced shell, or invalid prompt hooks fail closed. After a background job
finishes, press Enter to refresh prompt state if needed. Only a newly created
shell's startup may wait up to ten seconds for its first supported prompt.

## Submit from the coordinator machine

```sh
remote-run bun run test:e2e
remote-run -p 3000 -timeout 45m bun run test:e2e
remote-run -repository ../app -remote origin -branch main bun run test:e2e
```

Options precede the executable. Subsequent arguments belong to the job.
Invocation from a subdirectory finds the Git root, and execution uses the remote
worktree root. The checkout must be attached, clean (including non-ignored
untracked files), and synchronized with the selected remote branch. Errors explain
whether to commit/stash, push, or synchronize. Defaults: current checkout/branch,
`origin`, and 30 minutes for preparation, hooks, and execution (not queue waiting).

Submit one executable plus arguments, not shell pipes, redirections, or `&&`.
Arguments remain structured and execute directly through Go, not shell
interpolation; shell aliases/functions are not supported as job executables.
Put compound logic in a project script. Operators already interpreted by your
local shell cannot be intercepted by this CLI.

The ID is printed immediately to stderr. The CLI waits, then prints **completed
output** to stdout and exit status to stderr. There is no live output stream.
Timestamped stderr progress starts immediately before Git checks, reports queue
and assigned-runner state, and prints a waiting update every ten seconds during
long jobs. Server and worker terminals log connections, job receipt/dispatch,
window selection, completion, cancellation, and heartbeat availability transitions.
Job panes show preparation, worktree root, and hook/execution phases. These progress
logs do not contain full command arguments, environments, or remote URL credentials,
and do not change the completed command payload on stdout.

Ctrl-C requests cancellation and attempts to collect available output. Command
exit codes are mirrored; timeout is 124, infrastructure/preflight failure 125,
cancellation 130, usage error 2. Inspect job state to distinguish commands that
return those same codes themselves.

### Source contract and hooks

The worker fetches the **latest pushed branch when execution is prepared**.
Later pushes may change a queued job's source; this is deliberately not commit
pinning. It records the resolved commit. Tracked worker edits are reset; untracked
files remain (conflicting untracked paths cause an actionable failure rather than
being silently deleted). No automatic commit, push, source transfer, or Git clean.

Commit optional **`.remote-runner.json`** at the project root:

```json
{
  "AfterCreateWorktreeCommand": "bun install",
  "BeforeJobCommand": "bun install"
}
```

The first hook runs only on creation, the second every job. Both use `/bin/sh -c`;
failure prevents job execution. Jobs execute their argument vector directly.
Untracked hook files are ignored. All phases inherit the persistent job pane's
environment, including normal shell startup exports and later manual exports.

## Inspect and cancel

```sh
remote-run-utils runners
remote-run-utils jobs
remote-run-utils job JOB_ID
remote-run-utils output JOB_ID
remote-run-utils cancel JOB_ID
remote-run-utils -p 3000 jobs
```

Listings/details are JSON; output is raw completed text. Running cancellation is
a request: inspect the job afterward. No management command deletes worktrees.
Each repository + branch executes sequentially **across the fleet**; independent
branches can run concurrently. Per-worker concurrency is not a fleet limit.

## Failure and manual restart

The coordinator pings every 10 seconds. First missed response suspends dispatch;
three consecutive misses deregister the worker, fail its active coordinator jobs,
and release branch locks. Recovery before cutoff restores availability.

**Disconnection does not kill local jobs.** They may keep running; late results
from the old registration are ignored. Replacement same-branch jobs may run
elsewhere. There are no reconnects, retries, notifications, or result reconciliation.

Before restarting/re-registering a worker:

1. Stop old jobs/manual applications in job windows and confirm their processes
   have exited. A stopped job is not an exited job; resume it with `fg` and stop
   it if necessary. Leave retained shells at untouched primary prompts, outside
   copy mode and without extra panes.
2. Stop and start `remote-runner` again.

Ctrl-C on the worker alone does not stop its jobs. Verified, tagged **live idle
shells** may survive same-session restarts; running old helpers, manual activity,
and untagged/unsafe windows prevent startup. Retention is UI bookkeeping, not
job-history or orphan recovery. A suspended helper must actually exit before its
window can be reused; `fg` then completion returns to the worktree prompt. If shell
identity or completion cannot be confirmed, close that window manually.

**Upgrading from the old dead-helper windows (version 1):** stop old jobs and
manually close completed legacy job windows before starting the updated runner.
It will not adopt or force-kill them.

Coordinator restart forgets queue, registry, and job history. Stop old jobs,
restart workers, and resubmit desired work. Output is disk-backed without deliberate
truncation; coordinator output lasts for its lifetime. Private worker job directories
(0700 directories/0600 files, including shell startup files) must be kept while
the associated window exists; delete them manually **only after closing that
window**. Crash-left coordinator spools also require manual disk cleanup. Admission thresholds
are not hard memory caps, and commands may still exhaust machine resources.

## Development and validation

```sh
go test -p 1 ./...
go vet -p 1 ./...
go test -race -p 1 ./...     # requires CGO and a C compiler
GOOS=darwin GOARCH=arm64 go build ./cmd/...
GOOS=darwin GOARCH=amd64 go build ./cmd/...
```

Tests use the public coordinator/client contract, controlled scheduling/time
inputs, and real temporary Git repositories, processes, loopback networking, and
isolated test-owned tmux sockets. Race-build-tag fixtures also instrument the real
worker/helper subprocesses under `go test -race`; regular builds need neither
CGO nor a C compiler. Optional Zsh coverage uses `zsh` on `PATH`, or
`REMOTE_RUNNERS_TEST_ZSH` (executable) and `REMOTE_RUNNERS_TEST_ZSH_MODULE_PATH`
(module directory) for a privately extracted Zsh. Tests do not require a personal
repository or laptop deployment. Cross-compilation does not replace actual macOS
runtime trials.

Details: [coordinator/protocol](internal/coordinator/README.md),
[runner](internal/runner/README.md), [Git/workspaces](internal/workspace/README.md),
[submission](cmd/remote-run/README.md), [server](cmd/remote-runner-server/README.md),
[management](cmd/remote-run-utils/README.md).

Scope and specification history: [GitHub issue #1](https://github.com/gjtiquia/remote-runners/issues/1).
