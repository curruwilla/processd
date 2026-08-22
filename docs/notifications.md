# Notifications

A failed execution is otherwise silent unless somebody is watching the console or a Prometheus rule
is already written. This is the hole every wrapper script was written to fill.

```yaml
- name: nightly-report
  command: /usr/bin/php
  notify:
    on: [retries_exhausted, timeout]
    webhook:
      url: https://hooks.example.com/processd
      timeout: 5s
      retry: 2
      headers: { X-Processd-Channel: incidents }
    exec:
      worker: notify-slack
```

A worker with no `notify` of its own uses the daemon-wide one from `processd.yaml`, which has the
same shape. A worker that declares one **replaces** it outright rather than merging — two
half-policies deciding one delivery is harder to read than either of them.

## Keys

| Key | Type | Default | Values and rules |
|---|---|---|---|
| `on` | list of enum | — required whenever anything else is set | `failed`, `crashed`, `retries_exhausted`, `timeout` |
| `webhook.url` | URL | — | `http` or `https`, with a host. Anything else fails the load |
| `webhook.method` | string | `POST` | |
| `webhook.headers` | map | `{}` | sent as written; `Content-Type: application/json` is always set |
| `webhook.timeout` | duration | `5s` | bounds one attempt. Must be greater than zero |
| `webhook.retry` | int ≥ 0 | `0` | extra attempts after a failure, 2s apart |
| `webhook.include_log_tail` | int ≥ 0 | `0` = none | last N lines of the attempt's output. **Off by default and opt-in on purpose** — logs carry secrets far more often than anyone intends |
| `exec.worker` | worker name | — | runs that worker; see [below](#execworker) |

## Events

**The events do not overlap.** One outcome sends exactly one notification, and the daemon picks
which:

| Event | Means |
|---|---|
| `crashed` | an attempt ended badly and **another will follow** |
| `timeout` | it is over, and the last attempt was killed by its own deadline |
| `retries_exhausted` | it is over, and a retry policy spent its budget |
| `failed` | it is over, with no retry policy to spend — including `no_retry_exit_codes` |

Nothing is sent for a success, for `DELETE /v1/processes/{id}`, or for a shutdown: a human who
stopped an execution already knows.

## `exec.worker`

Runs a worker instead of, or as well as, the webhook. It obeys the same rule as every other
execution — **nothing reaches a process that the worker did not declare**. The outcome is offered as
params, and only the ones the target declares are passed:

```
event  process_id  worker  state  reason  attempt  exit_code  signal  node
```

Rules that fail the load rather than the failure they were meant to report: the target must exist (in
any file), must be a `task`, must not declare `notify` of its own — a notifier that notifies about
its own failure is a loop with no bound — and must not `require` a param outside that list.

The same loop is closed at run time, where the load-time rule cannot see it: **an execution the
notifier created never produces a notification of its own**. The daemon-wide policy applies to every
worker that declares none, the target included, so without this a notification worker that cannot
run would report its own failure — and run again, once per failure, for as long as the node is up.

A complete target worker is
[`examples/workers.d/notify-slack.yaml`](../examples/workers.d/notify-slack.yaml).

## Payload

**What it carries**, and deliberately does not: identity, outcome, timing, and the `metadata` a
client put there itself. There is no environment, no command and no argument list. The daemon
environment holds secrets by design, and a webhook is the one place they would leave the node.

```json
{
  "event": "retries_exhausted",
  "node": "app-01",
  "sent_at": "2026-08-22T00:30:58Z",
  "process": {
    "id": "proc_01M0KE1F77NPXZAG15REPZVJC7", "worker": "nightly-report", "type": "task",
    "state": "FAILED", "reason": "max_attempts", "attempt": 3, "restarts": 0,
    "exit_code": 7, "metadata": {"invoice": "42"},
    "created_at": "...", "started_at": "...", "finished_at": "...", "duration_ms": 812
  }
}
```

## Delivery

**Best effort, and it never touches the execution it describes.** A notification that cannot be
delivered is logged and dropped; it never fails, delays or retries the work. The queue is bounded, so
a node whose workers are all failing does not grow a notification backlog on top of the incident, and
shutdown waits at most 5s for what is queued.

## Node-wide fallback

The same block in `processd.yaml` covers every worker that does not declare its own:

```yaml
notify:
  on: [retries_exhausted, timeout]
  webhook:
    url: https://hooks.example.com/processd
    timeout: 5s
    retry: 2
```

---

[Documentation index](README.md) · [Workers](workers.md) · [Retry and backoff](retry.md) ·
[Configuration](configuration.md) · [Monitoring](monitoring.md)
