# Services

A `task` runs once and its success is final. A **service** is a worker that is not expected to exit:
any exit is abnormal and is followed by a restart.

```yaml
# /etc/processd/workers.d/api.yaml
version: 1
workers:
  - name: api
    type: service
    command: /usr/local/bin/api
    cwd: /srv/api
    user: api
    kill_grace: 30s
    retry:
      no_retry_exit_codes: [78]     # a bad config is not worth restarting into forever
      reset_after: 10m
      on_shutdown: true             # come back on the next daemon start
      backoff: { type: exponential, initial: 1s, max: 1m, jitter: 0.2 }
    logs:
      rotate: { max_files: 5 }
```

The commented version is [`examples/workers.d/api.yaml`](../examples/workers.d/api.yaml).

## What changes versus a task

| | `task` | `service` |
|---|---|---|
| `retry.enabled` | `false` by default | `true`, and `false` is refused at load |
| `retry.max_attempts` | `1` | `unlimited` |
| `retry.retry_on` | `[nonzero_exit, signal, start_error]` | the same plus `exit` — a clean exit restarts too |
| `retry.success_exit_codes` | `[0]` | none; declaring one is refused |
| `timeout` | allowed | **refused** — a service has no deadline to exceed |
| `schedule` | allowed | **refused** — it is already meant to be running |
| over `max_processes` | waits in the queue | refused with `503` / `no_capacity`, never queued |
| `logs.rotate.max_files` | optional | **mandatory** |

Log rotation is the one thing you must state. A single attempt can run for months, fill
`logs.max_bytes_per_stream` and then go silent; without rotation the stream simply stops storing.

Slots are taken at admission. Look at `processd status` for free slots before adding a service.

## Running one

| Action | Command | What happens |
|---|---|---|
| start | `processd run api` | one execution, supervised, restarted on any exit |
| stop for good | `processd stop <id>` | `SIGTERM` to the process group, `SIGKILL` after `kill_grace`. Ends as `CANCELED` with `reason: user_request` and **never** retries |
| restart | `processd restart <id>` | stops it, waits for the slot, creates a new execution from the current definition |
| reload its own config | `processd signal <id> SIGHUP` | the signal reaches the whole process group; the execution is untouched |
| watch it | `processd ps --type service` | the `RESTARTS` column is how hard the node is fighting to keep it up |

A reload only registers definitions; it never starts anything. A service needs one
`processd run <worker>` to create the execution that then stays alive.

A deliberate stop is the only way a service ends without coming back. Every other exit — clean, with
an error, or killed by a signal — is a restart, unless the exit code is listed in
`no_retry_exit_codes`.

## Changing one

**A reload never mutates a running process.** Every execution keeps the definition it was created
with, so `processd restart <id>` is what applies an edited file to a running service — it stops the
old execution and creates a new one, with a new id, from the definition just loaded.

`processd restart` checks the worker before it stops anything, and refuses when it is gone or
disabled — otherwise it would leave you with a stopped service and nothing to start again. The full
table of what to do per kind of change is in
[Operations](operations.md#changing-a-worker-or-a-service).

## Across a daemon restart

Set `retry.on_shutdown: true` and the execution is returned to the queue at shutdown and comes back
by itself on the next start, **keeping its id**. Without it, it ends as `CANCELED` and needs a new
`processd run`. This is the difference between `sudo systemctl restart processd` being a non-event
and being an outage — see [Updating](updating.md).

A service never sits in the queue otherwise: it takes its slot at admission or is refused, and it
only passes through `QUEUED` on the way back from a restart it was told to survive.

## Watching one

A healthy service produces no terminal state, so the ordinary counters stay silent about it. Alert on
`processd_service_restarts_total` instead — a service in a restart loop is invisible in every other
metric family. See [Monitoring](monitoring.md).

---

[Documentation index](README.md) · [Workers](workers.md) · [Retry and backoff](retry.md) ·
[Operations](operations.md) · [Monitoring](monitoring.md)
