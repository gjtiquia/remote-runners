# remote-run-utils

```sh
remote-run-utils [-p PORT] runners
remote-run-utils [-p PORT] jobs
remote-run-utils [-p PORT] job ID
remote-run-utils [-p PORT] output ID
remote-run-utils [-p PORT] cancel ID
remote-run-utils -h
```

The default coordinator is `http://127.0.0.1:2461`. `-p` must be between 1 and
65535. **Options precede the subcommand**; options after it and extra arguments
are errors. This CLI does not connect to remote management endpoints.

| Command | stdout on success |
| --- | --- |
| `runners` | JSON array of runner identity/name, priority, concurrency, memory, current slots/active jobs, availability and heartbeat misses. |
| `jobs` | JSON array of all queued, running and terminal jobs, in submission order. |
| `job ID` | JSON object with submission/source/arguments, timeout, status, timestamps and available runner/result/commit/error details. |
| `output ID` | Raw completed text, byte-for-byte; no header, JSON escaping or added newline. |
| `cancel ID` | Nothing; sends the cancellation request. |

JSON is one compact value followed by a newline; empty listings are `[]`.
Timeouts in the current wire schema are integer nanoseconds and timestamps are
Go JSON/RFC3339 time strings. Job states are `queued`, `running`, `succeeded`,
`failed`, `cancelled` and `timed_out`. Treat IDs as opaque values returned by the
coordinator rather than constructing them yourself.

Output is available only after a terminal result (otherwise HTTP 409). The CLI
uses `client.Output`, which streams the HTTP body with `io.Copy` to stdout; it
does not load the entire output into RAM or deliberately truncate it. A transfer
or stdout failure can leave partial output and yields exit code 1.

Queued cancellation removes execution eligibility. Running cancellation is a
request, not a wait for termination; inspect `job ID` afterward. Terminal
cancellation is idempotent. No command removes worktrees or remote windows.
Missing IDs yield an actionable HTTP 404 error identifying the operation and ID.
Help and errors go to stderr, leaving stdout suitable for JSON consumers or log
redirection.

Exit codes: **0** successful management operation/help (not the job's exit code);
**1** coordinator/network/output failure; **2** invalid CLI usage. There are no
retries, reconnects or durable state recovery. Coordinator restart forgets all
job and runner IDs.

## Integration tests

Tests build the command, invoke actual subprocess entrypoints and use a real
loopback coordinator/client. Worker wire fixtures register and send output via
WebSocket, not mocked clients. Coverage includes JSON listings/detail, queued
cancellation, usage/help/defaults, missing IDs, unavailable coordinator, pending
output rejection, and an 8,912,896-byte raw output transfer captured to a file.
The tests own every coordinator, connection, process and temporary file.
