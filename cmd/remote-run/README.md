# Submission prefix

```sh
remote-run bun run test:e2e
remote-run -p 3000 -timeout 45m bun run test:e2e
remote-run -repository ../app -remote origin -branch main bun run test:e2e
```

Options precede the executable. Parsing stops at the first executable, so
`remote-run bun run dev -p 3000` passes `-p 3000` to Bun. `--` can explicitly
end CLI options. Commands execute directly as an argument vector, not through a
shell. Spaces, quotes, dollar signs and embedded punctuation remain literal.
Exact standalone pipe/chaining/redirection operator tokens are rejected;
compound logic belongs in a project script. Operators consumed by the caller's
shell cannot be detected, and arbitrary punctuation inside an argument is not
interpreted or rejected.

## Source preflight

Defaults are the invoking Git checkout (root inferred from any subdirectory),
`origin`, and the current local branch. `-repository` selects another checkout
directory; `-remote` accepts a configured remote name or URL/path; `-branch`
selects an existing local branch. Explicit local paths should contain `/`.

The checkout must be attached and clean, including non-ignored untracked files.
Preflight fetches into a private ref and rejects unpublished, ahead, behind and
diverged source with specific remedies. It never commits, pushes, resets or
switches the local checkout. See `internal/workspace/README.md` for exact override
and identity semantics. Local paths work only when the runner can reach the same
repository; use SSH/HTTPS remotes across machines.

Jobs describe a branch, not a submission-time commit. A later push can change a
queued job's source; the runner records its resolved commit. Execution starts at
the prepared worktree root, not the requester's corresponding subdirectory.

## Waiting and results

The coordinator is `http://127.0.0.1:2461`; `-p` selects ports 1–65535. The stable
`job ID: ...` is printed to stderr immediately after acceptance. The CLI polls
status every 200 ms, without live output. On terminal state it copies the final
HTTP output body to stdout through `client.Output`/`io.Copy`; no whole-output RAM
buffer is used. Diagnostics and `exit code: ...` go to stderr. The timeout is
30 minutes by default, must be positive, includes remote preparation/hooks and
execution, and excludes queue waiting.

Exit codes:

- Remote command exit code when available; signal exits use the runner's
  `128 + signal` convention.
- `125`: local preflight, network/output, or infrastructure failure without a
  command result.
- `130`: cancellation (including Ctrl-C or SIGTERM of the submission CLI).
- `124`: remote execution timeout.
- `2`: invalid CLI usage. `-h` exits zero without Git/network access.

A remote command can itself use 124/125/130; inspect job state to disambiguate.
Remote hook/preparation failures mirror the worker's reported code when present.

Ctrl-C/SIGTERM requests cancellation using an independent five-second context,
then waits up to ten seconds for terminal state and, where available, up to ten
seconds to transfer final output. Thus cancellation remains effective after the
original observation context is cancelled. If collection fails or the runner is
unreachable, stderr retains the ID so `remote-run-utils -p PORT job ID` / `output
ID` can inspect it later. Cancellation cannot guarantee output from disconnected
workers. A signal during Git preflight stops ordinary Git descendants and exits
130 without submission.

Submission requests have a ten-second deadline; status requests have a
five-second deadline. Normal completed-output transfer has no fixed duration or
size limit and remains signal-cancellable. No request is automatically retried.
Any failed/interrupted submission may have been accepted before the response
failed: inspect `remote-run-utils -p PORT jobs` before resubmitting.

## Validation

Tests invoke the real command entrypoint in subprocesses and use the public
coordinator/client API, real temporary Git checkouts, the actual worker executable
and isolated test-owned tmux sockets. Coverage includes prefix argument
preservation, root inference, source/timeout overrides, actionable preflight
failure, queued/running Ctrl-C, no live output, timeout diagnostics, help/usage,
exit mirroring, and an 8 MiB output payload copied to disk without truncation.
Actual macOS/laptop runtime and authenticated remote-network trials remain
operator validation. Output disk exhaustion, stalled HTTP transfers, and signal
arrival during ambiguous submission acceptance are not integration-tested.
