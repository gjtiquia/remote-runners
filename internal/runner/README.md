# Runner contract

`cmd/remote-runner` delegates to `runner.Main(args) int`. `runner.LoadConfig(path)`
initializes missing/empty/whitespace-only JSON, returns an instruction to edit it,
and validates configured values before requiring tmux. `runner.Run(ctx, Config)`
connects once and returns on disconnection; cancellation of its context disconnects
only. It deliberately does **not** stop local jobs.

## Machine setup

First run creates `~/.remote-runners/config.json`, alongside other runner state:

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
and machine configuration use `.remote-runners`. Defaults use the actual home directory.

The CLI moves a regular legacy `~/.remote-runner/config.json` to the new location
when it is absent, preserving bytes and never overwriting an existing destination.
If both files exist, the new location wins and the old file remains untouched.
Symlinked legacy files or cross-filesystem moves need manual migration; startup
reports the paths instead of creating an unrelated starter. Explicit
`LoadConfig(path)` calls still use the caller's chosen path.
Unspecified optional values retain defaults; explicitly invalid values fail.
Paths should be absolute. There is no application authentication or sandbox.

Run in one **dedicated** tmux session, with no unrelated windows. Startup verifies
`TMUX`, `TMUX_PANE`, session ID, and runner window ID through `display-message`.
Use a current tmux supporting argv-style `new-window`, explicit `-e` environment
overrides, and synchronous `if-shell -F` guards (tested with tmux 3.5a). Job shells
use this session's effective `default-shell`: Zsh with its standard `zsh/parameter`
module, or Bash 5.1+. The older macOS system Bash is unsupported. Terminal
inspection/mutation calls have deadlines.

## Job execution

A worker-owned window is reused per shared `repository.Identity(Remote)` plus
exact branch; remote aliases for the same worktree share a window. Only completed
windows with a verified idle shell may be reused/evicted. At the limit, the
least-recently-used idle window is removed. Active windows and the runner window are protected; worktrees are
not deleted. Advertised slots are `min(max_jobs, max_windows) - active`, with
preparation and hooks counted active. An assignment without local capacity
returns `ErrNoCapacity` **before execution** and sends `decline` with job ID,
registration ID, and the unavailable snapshot that caused admission refusal.
Heartbeat and completion capacity sampling is serialized with wire emission so
older snapshots cannot overtake newer completion reports. Preparation, hook,
command, and infrastructure failures remain terminal, never retries.

### Persistent shell and admission

Each new window launches a real interactive **non-login** shell, not a helper
pane. Private startup wrappers/prompt hooks under its private job directory load
normal Zsh `.zshenv`/`.zshrc` (respecting `ZDOTDIR`) or Bash `.bashrc`; no user
dotfiles are edited. Your real prompt remains visible. Hooks preserve existing
prompt behavior while recording busy/idle state. Bash requires `promptvars` and
supported `PROMPT_COMMAND`; unsupported or altered hooks fail closed.

The worker's baseline environment keys are explicit tmux `new-window -e`
overrides, except pane-owned `TMUX`, `TMUX_PANE`, and `TERM`. Ordinary tmux global
variables may also exist in the baseline: this is not strict environment isolation.
Normal startup exports and later manual exports define the actual pane environment.
The helper inherits that environment rather than a descriptor environment snapshot.

After confirming window ownership, the runner sends a quoted private
`job-exec DESCRIPTOR_PATH` invocation plus Enter. The helper receives structured
JSON arguments and uses Go `exec.CommandContext` directly: no interpolation of
submitted arguments and no shell alias/function support as job executables.
The private helper CLI returns the recorded command status, so native foreground
job control also preserves it. The shell wrapper returns to the prepared worktree
root; Bash prompt restoration also handles a helper suspended and later resumed
with `fg`, without replacing the user's `fg` builtin. Repeated jobs reuse the same window, pane,
and shell; there is no helper-pane death/respawn lifecycle or delayed launch gate.
Startup alone never launches a helper, even after a parent crash.

Leave an **untouched primary prompt**, and never type concurrently with submission.
Manual foreground commands (including builtins), running or stopped background
jobs, or an active helper refuse admission immediately: job `failed`, exit 125,
`worktree window is busy`. No manual-busy wait, requeue, or cancellation input is
injected. A newly created shell alone may wait up to ten seconds for startup.
Half-typed text and continuation prompts cannot be detected externally; a prompt
marker is not a keyboard lock. Copy mode, extra panes, a replaced shell, and
invalid hook state are rejected fail closed. After a background job completes,
press Enter to refresh prompt state if needed.

### Retention and helper identity

`@remote-runner-window` version 2 is UI-only metadata: a base64-serialized normalized
source/branch key, shell nonce, exact window/pane IDs, and last-use timestamp. It
contains no job IDs, result paths, execution descriptors, or job history. Reuse
requires the owned prompt nonce, shell PID matching `pane_pid`, no active helper,
and the original single live pane outside copy mode. Admission and eviction
recheck these guards inside tmux's command queue. Eviction targets only the exact
idle pane with `kill-pane`, never the whole window with `kill-window`.
Job windows set `remain-on-exit off`, including retained live shells on restart.
Typing `exit` closes the job pane normally (the window closes when no panes remain).
Before admission, a successful session listing drops closed inactive UI entries
so the next job can create a new shell and reclaim its window budget. Missing
windows never release active execution accounting; existing panes still require
all identity and idle checks.

Startup accepts tagged **live idle shells**, but rejects running old helpers,
manual applications, untagged windows, malformed identities, replaced/dead shells,
and extra panes. Close inspection panes before reuse/restart. Before connecting,
startup trims verified idle windows to the current `max_windows` budget, including
a reduced limit. Refused/uncertain eviction keeps accounting and blocks unsafe
registration; no user-owned pane is destroyed to reclaim a window.

**Version 1 upgrade:** stop old jobs and manually close completed legacy
(dead-helper) job windows before starting the updated runner. They are never
force-killed or adopted.

`JobDescriptor` is the private helper's JSON contract: roots, protocol job,
output/result/cancel paths, helper PID, shell-return marker, and shell-result
script paths. `JobExec(path)` is exported for integration use; ordinary applications
should use `Run`. The helper removes the consumed descriptor because arguments
may contain secrets.

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
commands deliberately retain those variables and the rest of the pane environment.
Hooks execute through `/bin/sh -c`; after-create runs
only for a newly created worktree, before-job runs every time, and hook failure
prevents the command. Job executable/arguments execute directly without shell
interpolation. Preparation, hooks, and commands inherit the persistent shell's
actual pane environment, including startup/manual exports. Execution starts at
the prepared worktree root. Timeout defaults to 30 minutes
and includes Git preparation and hooks.

SIGINT (human Ctrl-C), SIGTERM, and SIGHUP cancel the helper's context; a runner
cancellation request creates a private file polled every 100 ms. Active Git,
hook, and command subprocess groups are killed on cancellation/timeout.
Deliberate daemonization/process-group escape is not a sandbox guarantee.
Results report command exit codes, `128 + signal` where applicable, cancellation
130, timeout 124, and output/infrastructure failures 125 when a code is available.
Hook/preparation failures default to 1. Helpers print a visible completion footer.

## Progress visibility

Runner stderr logs startup roots/tmux checks, connection/registration, received
jobs, capacity refusals, actual new/reused window and pane IDs, helper launch,
cancellation, output-transfer start/finish (or failure), completion, and
disconnection with the reminder that local jobs continue. Job-pane stderr logs
Git/worktree preparation, the actual ready root/commit, creation/before-job hooks,
the executable basename, and completion including cancellation/timeout outcomes.
Both use component-tagged local wall-clock `YYYY/MM/DD HH:mm:ss` timestamps and
quoted identifiers/labels. New diagnostics never print environments, complete
argv, credentials, or raw remote URLs, and never log routine heartbeat/polling
success. Existing Git/hook/command output is unchanged and may contain whatever
those programs print.

Helper progress bypasses the stdout/disk payload tee: it remains visible in the
job terminal but is not transferred as job output. The existing completion footer
and command stdout/stderr payload capture are unchanged.

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
helper's recorded process PID to have exited and the shell-return marker to exist
before reading its result (with conservative handling for initialization failure
or pane death). A prompt, result file, or shell-return marker alone is insufficient:
a stopped helper is still alive. The current worker can clear a stopped helper's
active lease after `fg` and observed exit. Selecting an inspection pane never
proves completion. Disk errors fail results; helper startup failures remain
visible even when result/output files cannot be created.

Private job directories live under `<worktrees>/.remote-runner-jobs/job-*`, with
0600 files and 0700 directories, including generated shell startup files and
prompt hooks. Output, result, and cancellation files remain for manual diagnosis;
there is no automatic age cleanup or output size cap. Keep job directories while
the associated window exists; manually delete them **only after closing that
window**. Eviction does not delete these files or the worktree. Ensure adequate
disk space.

There is no reconnection, reconciliation, orphan adoption, or execution/occupancy
recovery. Window retention metadata is UI bookkeeping only: it contains no job
IDs, result paths, or execution descriptors, and never restores jobs or results.
Broken transport leaves job helpers running independently and late output is
discarded; retained files do not imply recoverable coordinator state. Before
manual registration/restart, stop old jobs/manual applications, confirm process
exit, and stop the old runner. Leave retained shells at untouched primary prompts
without extra panes or copy mode. Startup rejects an active old helper; it neither
adopts nor kills that execution. Suspended helpers must really exit before reuse;
prompt restoration handles the prepared root/status after `fg`. If identity or
completion cannot be confirmed, close the window manually. There is no orphan
recovery. If tmux cannot be queried, an unconfirmed window is not idle/evictable.

## Integration coverage

Tests use real loopback coordinator/client networking, temporary Git repositories,
subprocesses, and separate test-owned tmux sockets. They cover starter config,
configuration validation, job args/environment/root/commit, committed hook lifecycle,
untracked-hook rejection, persistent Bash/Zsh prompts and shell identity,
startup/manual exports, prompt status and hooks, busy foreground/builtin and
background-job refusal, stopped helpers, copy mode and replaced-shell rejection,
Ctrl-C/descendant and management cancellation, hook failure/timeout, normalized
alias reuse, eight-window retention and reduced-budget restart trimming, live-job
and untagged restart rejection, guarded eviction with extra inspection panes,
failed-eviction accounting, admission declines/fresh capacity, inherited Git
routing with committed hooks, output chunks/status, and local-job survival after
disconnection.

`go test -race` uses race-build-tag fixtures to instrument the actual worker/helper
subprocesses as well as the harness; it requires CGO and a C compiler. Regular
builds require neither. Zsh tests use `zsh` on `PATH` when available; optionally
set `REMOTE_RUNNERS_TEST_ZSH` to a custom executable and
`REMOTE_RUNNERS_TEST_ZSH_MODULE_PATH` to its module directory for a privately
extracted Zsh. macOS cross-compilation is not actual runtime validation.
Memory-measurement error injection, disk exhaustion, heartbeat transport stalls,
and abrupt externally destroyed tmux windows are not currently integration-tested.
