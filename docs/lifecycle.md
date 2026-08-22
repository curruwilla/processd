# Lifecycle

Every execution moves through one state machine. It is the table in
[`internal/core/state.go`](../internal/core/state.go), and a transition missing from it is a bug,
never a silent no-op.

```
CREATED  → QUEUED, STARTING, CANCELED
QUEUED   → STARTING, CANCELED, FAILED
STARTING → RUNNING, CRASHED
RUNNING  → COMPLETED, CRASHED, STOPPING
STOPPING → CANCELED, FAILED, CRASHED, QUEUED
CRASHED  → RETRYING, FAILED
RETRYING → STARTING, QUEUED, CANCELED
```

## States

| State | Terminal | Means |
|---|---|---|
| `CREATED` | no | accepted and persisted, not yet evaluated by the scheduler |
| `QUEUED` | no | no free slot, or the lock is held with `lock_conflict: queue` |
| `STARTING` | no | slot and lock acquired, `fork`/`exec` under way |
| `RUNNING` | no | PID alive and supervised |
| `STOPPING` | no | stop signal sent, waiting for the exit or for `SIGKILL` |
| `CRASHED` | no | unexpected end. An evaluation state: it goes to `RETRYING` or to `FAILED` |
| `RETRYING` | no | waiting out the backoff before the next attempt |
| `COMPLETED` | **yes** | exit code in `success_exit_codes`. Tasks only — no exit of a `service` is a success |
| `FAILED` | **yes** | definitive: `max_attempts`, an exit in `no_retry_exit_codes`, a timeout, an unrecoverable start error, `queue_timeout` |
| `CANCELED` | **yes** | stopped on purpose, daemon shutdown, lock refused, or a `service` refused for want of a slot (`reason: no_capacity`) |

Terminal states are immutable: running the same work again is a new execution with a new ID. A retry
does **not** create one — it increments `attempt` and reuses the record.

`STARTING`, `RUNNING` and `STOPPING` are the states that occupy a concurrency slot.

## Transitions and what triggers them

| From | To | Trigger |
|---|---|---|
| `CREATED` | `QUEUED` | no slot or lock available |
| `CREATED` | `STARTING` | slot and lock acquired |
| `CREATED`, `QUEUED` | `CANCELED` | `DELETE`, shutdown, or a `service` refused for want of a slot |
| `QUEUED` | `STARTING` | the scheduler allocated a slot and the lock |
| `QUEUED` | `FAILED` | `queue.item_ttl` expired (`reason: queue_timeout`) |
| `STARTING` | `RUNNING` | `exec` succeeded, PID recorded |
| `STARTING` | `CRASHED` | `exec` error (`reason: start_error`) |
| `RUNNING` | `COMPLETED` | a `task` exited with a code in `success_exit_codes` |
| `RUNNING` | `CRASHED` | unexpected exit, unsolicited signal, disappearance, or **any** exit of a `service` |
| `RUNNING` | `STOPPING` | `DELETE`, timeout, or shutdown |
| `STOPPING` | `CANCELED` | a user or shutdown stop completed |
| `STOPPING` | `FAILED` | a timeout stop completed with no retry (`reason: timeout`) |
| `STOPPING` | `CRASHED` | a timeout stop with `timeout` in `retry_on` |
| `STOPPING` | `QUEUED` | a shutdown stop with `retry.on_shutdown: true` |
| `CRASHED` | `RETRYING` | the retry policy allows another attempt |
| `CRASHED` | `FAILED` | retry disabled, `max_attempts`, or `no_retry_exit_codes` |
| `RETRYING` | `STARTING` | the backoff ended and a slot and lock are free |
| `RETRYING` | `QUEUED` | the backoff ended and there is no slot |
| `RETRYING` | `CANCELED` | `DELETE` during the backoff |

Two consequences worth reading twice:

* An exit in `no_retry_exit_codes` reaches `FAILED` **through** `CRASHED`. A running process that
  ends badly has crashed, whether or not another attempt is allowed; `RUNNING → FAILED` does not
  exist.
* Every intent-driven exit — user, timeout, shutdown — passes through `STOPPING`, even when the
  execution was still `RUNNING` at the moment it was classified.

## Reasons

A state carries a reason: `user_request`, `timeout`, `max_attempts`, `queue_timeout`, `shutdown`,
`daemon_restart`, `start_error`, `no_retry_exit_code`, `lock_conflict`, `orphaned`, `no_capacity`.

## Identity

The PID is not the identity. Every execution has a stable logical ID (`proc_01K...`) that survives
retries and daemon restarts, and a stored PID is only usable together with its `/proc` start time —
PIDs are recycled. Attempts are numbered within the execution: output, exit code and terminating
signal are recorded **per attempt** — see [Monitoring](monitoring.md#execution-output).

The persisted state is the source of truth; in-memory state is a cache rebuilt on startup.

## How the queue is drained

Queued work is scanned for what can start, not just read from the head: an execution blocked by its
lock or by its worker's `max_processes` must not stall everything behind it. Each pass reads a
bounded batch, and no single worker may fill that batch on its own — otherwise the deepest backlog on
the node would hide every other worker's work, which would then be neither started nor expired until
that backlog drained. Within one worker the order is strictly the order the executions were submitted
in.

## Services and the queue

A service never sits in the queue: it takes its slot at admission or is refused with `no_capacity`,
and it only passes through `QUEUED` on the way back from a daemon restart it was told to survive
(`retry.on_shutdown: true`). See [Services](services.md).

## What ends without a retry

* `processd stop <id>` / `DELETE /v1/processes/{id}` → `CANCELED`, `reason: user_request`, and
  **never** a retry.
* An exit code in `no_retry_exit_codes` → `CRASHED`, then `FAILED` immediately.
* A queue item over `queue.item_ttl` → `FAILED`, `reason: queue_timeout`. The wait is counted from
  when it entered the queue, so an execution returned there by `retry.on_shutdown` starts a fresh
  one. A service never expires this way.

Which of those send a notification is in [Notifications](notifications.md#events).

---

[Documentation index](README.md) · [Retry and backoff](retry.md) · [Services](services.md) ·
[HTTP API](api.md)
