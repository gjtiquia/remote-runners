# Coordinator/client contract

## Build identity

Go embeds VCS revision and modified-state metadata automatically; the install
command `go install ./cmd/...` remains unchanged. Thin clients and workers report
build metadata only and do not enforce compatibility locally. The coordinator is
the sole authority: before accepting work, it rejects peers whose build identity is
unknown, whose build is dirty, or whose identity does not match the coordinator.
Rejections are clear in server logs and in errors returned to the peer. Installing
an upgrade is not sufficient by itself: reinstall and restart the relevant
binaries. A dirty checkout must be committed or rebuilt from a clean checkout
before installation. A dirty or unknown coordinator build refuses to start.

Management requests and worker WebSocket handshakes report
`X-Remote-Runners-Revision` and `X-Remote-Runners-Modified` headers. The coordinator
checks the complete Git revision and requires a known clean build; missing,
malformed, dirty, or mismatched identities receive HTTP 412 before request handling
or WebSocket upgrade. Clients simply surface that response. These headers are
trusted identity reports, not authentication or cryptographic build attestation.
`Options.Build` and `Client.Build` allow explicit identity injection for embedded
hosts and deterministic tests; normal executables use their own build metadata.

`coordinator.New(Options)` creates an in-memory coordinator; serve `Handler()` with
an ordinary HTTP server. `Close()` is idempotent: stops its ticker and WebSockets,
closes spool files, and removes only its fresh owned temporary output directory.
It does **not** cancel or kill remote processes. The caller owns HTTP listener
shutdown. Restart forgets registry, queue, jobs, and output history.

## Lifecycle logs

The default logger writes component-prefixed, host-local date/time timestamps to
stderr. `Options.Logger` accepts a caller-owned `*log.Logger` for capture/routing;
configure its prefix/flags before `New` and keep its output alive through `Close`.
Logs identify accepted registrations (ID, priority, slots, socket peer), job
receipt (ID, quoted branch, project basename), dispatch, terminal outcome/exit
code, cancellation requests, accepted declines, and transport loss. First
heartbeat miss, recovery, cutoff, startup and shutdown are logged; healthy
heartbeats, management polling and output chunks are silent. Remote URLs,
credentials, command arguments, output payloads and raw runner errors are not
logged. Active cancellation is a request, not completion; disconnect/shutdown
logs do not imply remote processes were killed.

## HTTP management

Only the socket peer (`Request.RemoteAddr`) may authorize management. IPv4/IPv6
loopback peers are allowed; forwarded headers never confer access. `/runner` is
network-facing and explicitly trusted, without application authentication.

| Method | Route | Result |
| --- | --- | --- |
| POST | `/jobs` | Submission JSON → Job JSON, 201 |
| GET | `/jobs` | Job array in acceptance order |
| GET | `/jobs/{id}` | Job JSON |
| GET | `/jobs/{id}/output` | Completed output, streamed text |
| POST | `/jobs/{id}/cancel` | Empty request body, 204 |
| GET | `/runners` | RunnerInfo array, ascending runner ID |
| GET | `/runner` | WebSocket upgrade |

Errors are HTTP 400 (invalid input), 403 (non-loopback management), 404 (unknown
route/job), 405 (wrong method, with Allow), 409 (output not completed), 413
(oversized submission), 412 (build identity rejected), 500 (spool infrastructure
failure), or 503 (closed).
Submissions must be one JSON object, reject unknown fields, and fit 64 KiB.
Executable/source are required; arguments cannot contain NUL; branch names obey
Git ref naming rules. Zero timeout defaults to 30 minutes, negative is rejected.
Timeout execution/descendant termination is the runner's responsibility.

`client.New(baseURL)` exposes Submit, Jobs, Runners, Job, Cancel, Output using
caller contexts. Output copies the response directly to the supplied writer;
HTTP errors and copy failures propagate. No client result retry/recovery exists.

## Runner WebSocket

Use `internal/protocol.Message` JSON:

1. Runner sends `register` with RunnerInfo (nonempty ID up to 128 bytes, Name up
   to 256 bytes, priority 0–100, positive MaxJobs, nonnegative Slots/ActiveJobs).
2. Server returns `registered` with a fresh, random RegistrationID. All later
   messages in either direction carry that identity.
3. Server sends `heartbeat` with Sequence. Runner replies `heartbeat_response`
   with the same Sequence, RegistrationID, and fresh RunnerInfo.
4. Server sends `job` with Job; `cancel` identifies JobID.
5. If local admission capacity changed since its last report, the runner may send
   `decline` with JobID, RegistrationID, and fresh RunnerInfo in Runner, **only
   before starting any helper/preparation/hook/command**. This is unstarted
   admission rollback, never retry of executed work. The report excludes the
   declined assignment and must demonstrate unavailable capacity: zero Slots,
   ActiveJobs at least MaxJobs, or AvailableMemory below MinMemory. The coordinator
   requires the current assigned registration and matching valid Runner.ID;
   missing/invalid/still-available reports and declines after output are ignored,
   retaining the assignment until a valid decline, completion, or cutoff.
6. After execution, runner sends ordered `output` chunks (Data is base64 in JSON),
   then `complete` with JobID, State, optional ExitCode/Error/Commit, and fresh
   RunnerInfo in Runner **after releasing local capacity** (excluding this job).
   Allowed completion states: succeeded, failed, cancelled, timed_out. A valid
   matching report updates capacity before completion releases the assignment and
   schedules queued work; it does not clear heartbeat misses. Invalid reports
   are ignored without discarding the result. Runner is optional for legacy
   peers: without it, only occupancy known to include this job releases a reported
   slot. Ambiguous partial occupancy waits for fresh telemetry; memory never
   increases by inference. Current workers should always send fresh RunnerInfo.

Maximum decoded output chunk: **64 KiB** (`coordinator.MaxOutputChunk`). Maximum
WebSocket message: 128 KiB, including JSON/base64 overhead. Total job output has
no cap. A single deadline-bound writer per runner serializes outbound messages;
network writes never hold the shared scheduling mutex. Output writes go straight
to disk; blocking disk/network provides backpressure, not dropped chunks.

Only current registrations may update their assigned active jobs. A manual
replacement registration fails the previous registration's active jobs and
releases their locks. Humans must stop old jobs before registering again; no
occupancy reconciliation or automatic reconnection occurs.

## Scheduling and heartbeat semantics

Highest-priority eligible runner wins; equal-priority ties use ascending runner
ID (bytewise string order), never registration order. FIFO among eligible jobs:
blocked branches do not block unrelated branches. Branch locks use normalized
repository identity plus exact branch. SSH URL/scp spellings, host case/default
SSH port, local file URLs/symlinks normalize consistently with workspace identity;
transports/users/ports/case-sensitive paths remain distinct. Original remote
spelling stays in assignments. Scheduling and workspace allocation use the same
`repository.Identity` implementation.

Admission requires responsive transport, zero misses, room under MaxJobs,
positive reported local Slots (adjusted for unreported assignments), and available
memory at least MinMemory. ActiveJobs is coordinator-owned. Listed Slots are
adjusted usable execution slots; Available denotes current admission eligibility
(healthy heartbeat/transport, usable slots, and sufficient memory). Runner-reported
Misses and Available never override coordinator policy. No preemption or memory reservation.

A valid decline releases only its unstarted assignment's slot and branch lock,
returns the same job to its original acceptance position, and clears RunnerID
and StartedAt while preserving ID, source, and CreatedAt. Other running jobs are
untouched. Its unavailable snapshot prevents redispatch to that runner until
fresh capacity arrives; an eligible fallback may immediately take the queued
job. If cancellation was already requested, decline finalizes cancellation
instead of returning the job to the queue.

Queued cancellation becomes terminal immediately. Active cancellation is
idempotent and sends cancel, but stays running with its lock and slot until
completion or heartbeat cutoff.

Defaults: 10-second heartbeat interval and three consecutive missed responses.
Each Tick sends a request; at the next Tick, no fresh response counts as a miss.
Thus first request has zero misses, second round can record miss one, fourth
round can reach default cutoff. Each round updates **all** runners' misses before
any cutoff releases locks or dispatches work, so map iteration cannot assign work
to another already-overdue runner. First miss suspends dispatch. A fresh response
before cutoff restores availability; delayed responses are accepted only when
newer than the last accepted response and not beyond the issued sequence.
Duplicate/replayed responses cannot restore availability. Cutoff deregisters,
fails associated active jobs, releases slots/locks, and logs locally; it sends no
remote process-kill command. Old identities/results cannot revive registration.
Transport loss suspends dispatch immediately; active jobs await heartbeat cutoff.

For deterministic tests, supply Now and a long HeartbeatInterval, then advance
with `Tick(now)`. Tick observes at most one round when the configured interval has
elapsed. The background ticker remains enabled and uses Now. Positive custom
MissLimit is supported; zero selects defaults. Negative policy values reject New.

## Disk errors and retention

Each coordinator owns a fresh 0700 directory beneath OutputDir (empty uses the
OS temp root). Job output files are 0600. No file scan/history recovery occurs.
Successful completed output is immutable and served with Content-Length, so an
incomplete HTTP transfer is detectable. Existing output remains available for
failed/cancelled/timed-out jobs while this coordinator lives.

Failure to create a spool rejects submission rather than accepting an unrecorded
job. A spool write failure requests cancellation, retains the lock/slot until
runner completion or cutoff, and makes the final result an infrastructure failure
(State failed, no command ExitCode). Storage failure cannot preserve unwritten
bytes; diagnostics explicitly report it rather than silently claiming success.
Close returns file-close/removal errors. A crash can leave files, but they are not
recovered by the next coordinator. Open-file/disk exhaustion is an explicit
infrastructure failure; no disk quota or retained-history limit is imposed.
