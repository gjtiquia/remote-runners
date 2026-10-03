# remote-runner-server

```sh
remote-runner-server
remote-runner-server -p 2462 -output-dir /path/to/output \
  -heartbeat-interval 10s -heartbeat-misses 3
remote-runner-server -h
```

Options only; positional arguments are rejected. Defaults:

| Option | Default | Meaning |
| --- | --- | --- |
| `-p` | `2461` | Network-wide TCP listener. `0` selects an ephemeral port. |
| `-output-dir` | `~/.remote-runners/output` | Parent for this coordinator's private disk-backed output spool. |
| `-heartbeat-interval` | `10s` | Coordinator heartbeat request interval; must be positive. |
| `-heartbeat-misses` | `3` | Consecutive unanswered rounds before deregistration; must be at least one. |

The actual bound address and operational errors are written to stderr. The
coordinator handler accepts outbound worker WebSockets at `/runner` from network
peers, but rejects all submission/management routes unless the **socket peer** is
loopback. Forwarded headers do not confer access. Workers explicitly trust this
unauthenticated coordinator; expose it only on an operator-managed trusted
network. There is no TLS/authentication layer here.

The first unanswered heartbeat round suspends new dispatch. A fresh response
before cutoff restores availability; cutoff deregisters the runner and fails its
active coordinator jobs. Disconnected remote processes are not killed.

## Shutdown and output ownership

SIGINT and SIGTERM drain HTTP requests (up to five seconds), then close the
coordinator, including hijacked runner WebSockets. On a drain timeout, remaining
HTTP connections are forcibly closed and the exit code is 1. Clean close removes
only this instance's private `remote-runners-*` spool directory, not its parent or
other files. Output is retained for the running coordinator's lifetime; completed
output is copied from disk without a total-output cap. Running output is not
available through management.

Registry, queue and job history are in memory. Restart forgets them even if a
crash leaves spool files behind; surviving files are not recovered. There is no
automatic cleanup/reconciliation of crashed instances. Stop old remote job
windows and confirm jobs have exited before manually restarting workers and
resubmitting. Coordinator shutdown does not guarantee termination of remote jobs.

Exit codes: **0** help or graceful signal shutdown; **1** infrastructure, serving,
shutdown or cleanup failure; **2** invalid CLI usage. There is no panic/recovery
startup path.

## Integration tests

Tests build the real command and launch owned subprocesses on temporary ports.
They use public HTTP/client and WebSocket contracts to exercise default output
location, SIGINT/SIGTERM shutdown, spool ownership, socket closure, restart state
loss, and heartbeat overrides. A non-loopback interface test verifies network
runner access and rejects every management route even with a spoofed forwarded
header; that test skips only when no non-loopback IPv4 interface exists. All
processes, logs and spools belong to the tests.
