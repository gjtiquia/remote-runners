# Runner contract

`cmd/remote-runner` delegates to `runner.Main(args) int`. `runner.LoadConfig(path)`
initializes missing/empty/whitespace-only JSON, returns an instruction to edit it,
and validates configured values before requiring tmux. `runner.Run(ctx, Config)`
connects once and returns on disconnection; cancellation of its context disconnects
only. It deliberately does **not** stop local jobs.

## Machine setup

First run creates `~/.remote-runner/config.json` (singular):

```json
{
  "coordinator_url": "",
  "name": "",
  "priority": 50,
  "max_jobs": 5,
  "min_memory": 1073741824,
  "max_windows": 8,
  "repos": "/home/you/.remote-runners/repos",
  "worktrees": "/home/you/.remote-runners/worktrees"
}
```

Set `name` to a unique, stable worker name and `coordinator_url` to e.g.
`ws://coordinator:2461/runner`. HTTP(S) URLs are converted to WS(S); an omitted
URL path defaults to `/runner`. Memory is in bytes; priority is 0–100. Storage
uses `.remote-runners` (plural). Defaults use the actual home directory.
Unspecified optional values retain defaults; explicitly invalid values fail.
Paths should be absolute. There is no application authentication or sandbox.

Run in one **dedicated** tmux session, with no unrelated windows. Startup verifies
`TMUX`, `TMUX_PANE`, session ID, and runner window ID through `display-message`.
Use a current tmux supporting argv-style `new-window`/`respawn-pane` and
synchronous `if-shell -F` guards (tested with tmux 3.5a). Commands launch through explicit
`/bin/sh`, not a login shell. Terminal inspection/mutation calls have deadlines.

## Job execution

A worker-owned window is reused per shared `repository.Identity(Remote)` plus
exact branch; remote aliases for the same worktree share a window. Only completed
windows may be respawned/evicted. At the limit, the least-recently-used idle window
is removed. Active windows and the runner window are protected; worktrees are
not deleted. Advertised slots are `min(max_jobs, max_windows) - active`, with
preparation and hooks counted active. An assignment without local capacity
returns `ErrNoCapacity` **before execution** and sends `decline` with job ID,
registration ID, and the unavailable snapshot that caused admission refusal.
Heartbeat and completion capacity sampling is serialized with wire emission so
older snapshots cannot overtake newer completion reports. Preparation, hook,
command, and infrastructure failures remain terminal, never retries.

Each owned window carries `@remote-runner-window` versioned retention metadata:
a base64-serialized normalized source/branch key, exact window and helper pane
IDs, and last-use timestamp. A newly created/respawned shell waits on a private
launch gate. Only its parent can release that gate, after setting and confirming
the exact window/pane metadata; the shell never tags or releases itself. A parent
crash before tagging cannot start a hidden job. Startup rejects **all untagged
non-runner windows**, including pending launches, with a manual stop/close
instruction. It never adopts or automatically releases a pending launch.

Startup loads only tagged, positively completed helper panes. Before connecting,
it evicts least-recently-used completed windows until the current `max_windows`
budget is met, including when that limit was reduced since the previous run.
Unsafe or uncertain cleanup rejects registration instead of exceeding the limit.
Eviction uses tmux's synchronous `if-shell -F` guard for the exact dead helper,
its window/session, and a one-pane window, followed by `kill-pane`, never
`kill-window`. A failed/refused eviction does not release window accounting.
Malformed tags, missing helper panes, or extra inspection/user panes in an owned
window require manual attention; no user-owned pane is destroyed to reclaim a
window. Inspection panes may still be used during execution, but must be closed
manually before reuse/restart.

The same executable's private `job-exec DESCRIPTOR_PATH` command runs as the
actual foreground program in each job pane. `JobDescriptor` is the JSON contract
for this subprocess (roots, protocol job, environment, output/result/cancel
paths). `JobExec(path)` is also exported for integration use; ordinary applications
should use `Run`. The helper removes its descriptor after reading it because it
contains the launching runner's environment and potentially credentials.

Preparation uses `workspace.Prepare(ctx, Roots, Source, writer)`; workspace owns
cross-process repository locking and cancellable Git process groups. The helper
uses the returned worktree root, creation flag, and resolved commit. Optional
project-root `.remote-runner.json` hook fields use these literal names:

```json
{
  "AfterCreateWorktreeCommand": "npm ci",
  "BeforeJobCommand": "npm run prepare-tests"
}
```

Commit this configuration; untracked hook files left by previous jobs are ignored.
The internal committed-file Git check excludes Git routing variables (such as
`GIT_DIR` and `GIT_WORK_TREE`), matching workspace preparation. Hooks and submitted
commands deliberately retain those variables and the rest of the worker environment.
Hooks execute through `/bin/sh -c`; after-create runs
only for a newly created worktree, before-job runs every time, and hook failure
prevents the command. Job executable/arguments execute directly without shell
interpolation. All phases inherit the launching runner's environment, **not**
the tmux server's stale environment; only `TMUX` and `TMUX_PANE` reflect the new
job pane. Execution starts at the worktree root. Timeout defaults to 30 minutes
and includes Git preparation and hooks.

SIGINT (human Ctrl-C), SIGTERM, and SIGHUP cancel the helper's context; a runner
cancellation request creates a private file polled every 100 ms. Active Git,
hook, and command subprocess groups are killed on cancellation/timeout.
Deliberate daemonization/process-group escape is not a sandbox guarantee.
Results report command exit codes, `128 + signal` where applicable, cancellation
130, timeout 124, and output/infrastructure failures 125 when a code is available.
Hook/preparation failures default to 1. Helpers print a visible completion footer.

## Wire/output/lifetime

Outbound WebSocket `/runner` registration sends `register` with `RunnerInfo` and
requires `registered` with a fresh registration ID. All subsequent messages carry
that ID. Server `heartbeat` requests receive a fresh-capacity `heartbeat_response`
with the same sequence. `job` starts execution; `cancel` requests cancellation.
A synchronized writer has ten-second write deadlines. Failed memory measurement
reports zero memory and unavailable capacity. Linux uses `/proc/meminfo`
`MemAvailable`; macOS estimates reclaimable availability as vm_stat's free +
inactive + speculative pages multiplied by its reported page size. This is an
estimate, not a hard cap, and needs actual macOS operator validation.

Output is tee'd to disk and the tmux terminal, never accumulated wholesale by the
worker. After completion the parent reads <=32 KiB chunks into `output` messages,
then sends `complete` (state, exit code, error, resolved commit, fresh runner
capacity). Result files are published by atomic rename. Completion waits for the
helper's exact stable pane ID to be dead before reading its result or making its
window reusable. Selecting an inspection pane does not
make an active job appear completed. Disk errors fail results; helper startup failures remain
visible in the pane even when result/output files cannot be created.

Private job directories live under `<worktrees>/.remote-runner-jobs/job-*`, with
0600 files and 0700 directories. Output, result, and cancellation files are retained
for manual diagnosis; there is no automatic age cleanup or output size cap. Ensure
adequate disk space and remove completed job directories manually when no longer
needed. Evicting a window does not delete these files or its worktree.

There is no reconnection, reconciliation, orphan adoption, or execution/occupancy
recovery. Window retention metadata is UI bookkeeping only: it contains no job
IDs, result paths, or execution descriptors, and never restores jobs or results.
Broken transport leaves job helpers running independently and late output is
discarded; retained files do not imply recoverable coordinator state. Before
manual registration/restart, Ctrl-C all old job panes, confirm their completion,
and stop the old runner. Startup rejects any still-live tagged helper with an
explicit Ctrl-C/wait instruction; it neither adopts nor kills that execution.
If tmux cannot be queried, the worker conservatively does not treat an unconfirmed
active window as idle/evictable.

## Integration coverage

Tests use real loopback coordinator/client networking, temporary Git repositories,
subprocesses, and separate test-owned tmux sockets. They cover starter config,
configuration validation, job args/environment/root/commit, committed hook lifecycle,
untracked-hook rejection, real job panes,
Ctrl-C and descendant cancellation, management cancellation, hook failure/timeout,
normalized alias reuse, eight-window retention and reduced-budget trimming across
same-session restarts, live-job and pending/untagged restart rejection,
interrupted launch gating, inspection panes added between eviction checks and
mutation, failed-eviction accounting, protected-pane cleanup rejection, admission
declines/fresh completion capacity, inherited Git routing with committed hooks,
window LRU/active protection, output chunk transfer and exit status, and
local-job survival after coordinator disconnection. macOS cross-compilation is
not runtime validation. Memory-measurement error injection, disk exhaustion,
heartbeat transport stalls, and abrupt externally destroyed tmux windows are not
currently integration-tested.
