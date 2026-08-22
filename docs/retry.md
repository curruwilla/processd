# Retry and backoff

The `retry` block of a [worker](workers.md) decides whether an attempt repeats, how long it waits
first, and when it gives up.

`enabled` is tri-state: absent is not the same as `false`. A `task` without the key does not retry; a
`service` without it restarts, and an explicit `enabled: false` on a service is refused at load.

Writing the rest of the policy without turning it on is refused too: a `retry` block that sets
`max_attempts`, `retry_on`, `reset_after`, `on_shutdown` or `backoff` while `enabled` is not `true`
fails the load rather than being ignored. `success_exit_codes` and `no_retry_exit_codes` are the
exception — they decide what an exit *means*, which a task answers whether or not it ever tries
again.

```yaml
retry:
  enabled: true
  max_attempts: 5
  retry_on: [nonzero_exit, signal, start_error]
  no_retry_exit_codes: [64, 65, 78]
  reset_after: 10m
  backoff: { type: exponential, initial: 5s, max: 5m, multiplier: 2, jitter: 0.2 }
```

## Keys

| Key | Type | Default (`task`) | Default (`service`) | Values |
|---|---|---|---|---|
| `enabled` | bool | `false` | `true`, and must not be `false` | without it, one attempt and done |
| `max_attempts` | attempts | `1` | `unlimited` | total, first attempt included. `unlimited` is accepted only on a service |
| `retry_on` | list of enum | `[nonzero_exit, signal, start_error]` | the same plus `exit` | `nonzero_exit`, `signal`, `start_error`, `timeout`, `exit`. `exit` is any exit, a clean one included, and only a service may use it |
| `success_exit_codes` | list of int | `[0]` | none, and declaring one is refused | a listed code → `COMPLETED` |
| `no_retry_exit_codes` | list of int | `[]` | `[]` | a listed code → `FAILED` immediately, no retry |
| `reset_after` | duration | `0` = off | `0` = off | an attempt that ran longer zeroes the attempt counter |
| `on_shutdown` | bool | `false` | `false` | `true` returns the execution to the queue at shutdown instead of cancelling it |
| `backoff.type` | enum | `exponential` | `exponential` | `exponential`, `linear`, `fixed` |
| `backoff.initial` | duration | `5s` | `5s` | delay of the first repeat |
| `backoff.max` | duration | `5m` | `5m` | ceiling of the delay |
| `backoff.multiplier` | float > 0 | `2` | `2` | used only by `exponential` |
| `backoff.jitter` | float 0..1 | `0` | `0` | spreads the delay by `±jitter`; without it everything that failed together repeats together |

`attempts` and `duration` syntax are in [Configuration](configuration.md#value-syntax).

## Curves

| Type | Delay before attempt *n* |
|---|---|
| `fixed` | `initial` |
| `linear` | `initial × attempt` |
| `exponential` | `initial × multiplier^(attempt-1)` |

The `max` ceiling is applied before the jitter.

## What it means in practice

* **The lock is held across a retry.** Releasing it during the backoff would let another execution
  take it mid-retry. See [Workers → Locks](workers.md#locks).
* **`reset_after` is what makes a long-lived service survivable.** An attempt that ran longer than it
  zeroes the counter, so a service that has been up for a month does not carry the three restarts it
  had on its first day.
* **`no_retry_exit_codes` beats every trigger.** A configuration error (`78` / `EX_CONFIG`) is not
  worth restarting into forever.
* **A deliberate stop never retries.** `processd stop <id>` — `DELETE /v1/processes/{id}` — ends as
  `CANCELED` with `reason: user_request` and no attempt follows.
* **`on_shutdown: true` survives a daemon restart.** The execution is returned to the queue and comes
  back by itself on the next start, keeping its id. Without it, it ends as `CANCELED` and needs a new
  `processd run`. See [Updating](updating.md). The wait starts when it goes back in line, so
  `queue.item_ttl` measures the queue and not how long the execution had already been running.

Every state a retry passes through — `CRASHED → RETRYING → STARTING` — is in
[Lifecycle](lifecycle.md), and a retry that spends its budget is what fires the
`retries_exhausted` [notification](notifications.md).

---

[Documentation index](README.md) · [Workers](workers.md) · [Services](services.md) ·
[Lifecycle](lifecycle.md)
