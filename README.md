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
Binaries go to `GOBIN`, or usually `~/go/bin`; add that directory to `PATH`.
Go is not required to run built binaries. Workers need Git, tmux (tested on 3.5a),
SSH/repository access, and project tools such as Bun and browsers.

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

Edit **`~/.remote-runner/config.json`** in a text editor. Set `name` to a unique,
stable machine name and `coordinator_url` to `ws://COORDINATOR:2461/runner`.
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

Keep the worker window running. Jobs appear in separate windows; repeated jobs
reuse a worktree's idle window. At the retention limit the least-recently-used
completed window is evicted, never an active/worker window. Worktrees stay on disk.
Prefer launching from an environment with your desired project tools on `PATH`;
job helpers inherit that environment rather than the tmux server's stale one.

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
Untracked hook files are ignored. All phases share the launching worker's
environment; personal interactive shell startup files are not sourced implicitly.

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

1. Ctrl-C **each old job window** and confirm its processes have exited.
2. Stop and start `remote-runner` again.

Ctrl-C on the worker alone does not stop its jobs. Human teardown is a prerequisite;
there is no orphan discovery or occupancy restoration. Tagged completed job
windows are retained/reused across same-session restarts; startup rejects a still-live
old helper and asks you to stop it. This retention metadata does not recover jobs
or results.

Coordinator restart forgets queue, registry, and job history. Stop old jobs,
restart workers, and resubmit desired work. Output is disk-backed without deliberate
truncation; coordinator output lasts for its lifetime. Worker job directories and
crash-left coordinator spools require manual disk cleanup. Admission thresholds
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
isolated test-owned tmux sockets. They do not require a personal repository or
laptop deployment. Cross-compilation does not replace actual macOS runtime trials.

Details: [coordinator/protocol](internal/coordinator/README.md),
[runner](internal/runner/README.md), [Git/workspaces](internal/workspace/README.md),
[submission](cmd/remote-run/README.md), [server](cmd/remote-runner-server/README.md),
[management](cmd/remote-run-utils/README.md).

Scope and specification history: [GitHub issue #1](https://github.com/gjtiquia/remote-runners/issues/1).
