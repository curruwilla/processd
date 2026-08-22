# HTTP API

Base `/v1`, JSON in and out. Every endpoint needs `Authorization: Bearer <token>` except
`GET /v1/health`. The [CLI](cli.md) is a client of exactly these endpoints.

```bash
curl -X POST http://127.0.0.1:7373/v1/processes \
  -H "Authorization: Bearer $(sudo cat /etc/processd/token)" \
  -H "Content-Type: application/json" \
  -d '{"worker":"backup","params":{"bucket":"invoices"}}'
```

```json
{ "id": "proc_01KABCDEF...", "status": "STARTING", "pid": null, "attempt": 1 }
```

## Endpoints

| Endpoint | Notes |
|---|---|
| `POST /v1/processes` | `{"worker":"...","params":{...}}`. `201` admitted, `202` queued. An optional `Idempotency-Key` replays the original response with `Idempotent-Replay: true` for as long as that execution is retained; the same key with a different body → `409`. On a hub, an optional `node` dispatches it there instead |
| `GET /v1/processes` | filters: `status` (repeatable), `type`, `worker`, `lock`, `created_after`, `created_before`; `limit` default 50, max 500; cursor paging |
| `GET /v1/processes/{id}` | full representation, including live CPU and memory while it runs |
| `DELETE /v1/processes/{id}?grace=15s` | `202`, and the stop runs on its own: the grace belongs to the process, not to the request, so the answer does not wait it out and hanging up does not cut it short. Ends as `CANCELED` with `reason: user_request`, and **never** triggers a retry |
| `POST /v1/processes/{id}/signal` | `{"signal":"SIGUSR1"}` — the allowlist is in [CLI → Signals](cli.md#signals) |
| `GET /v1/processes/{id}/logs` | `?stream=stdout\|stderr\|both&attempt=N&tail=N` |
| `GET /v1/processes/{id}/logs/stream` | Server-Sent Events, one `line` event per line, `end` when the attempt finishes |
| `GET /v1/workers` | workers the token may see; a scheduled one carries `schedule` with `cron`, `timezone`, `next_run`, `last_fired_at`, `last_missed_at` and `missed_runs` |
| `POST /v1/reload` | re-reads `workers.d`, all-or-nothing |
| `GET /v1/health[?deep=1]` | public; `deep` also pings the store |
| `GET /v1/stats` | slots, queue depth, states, service counters |
| `GET /v1/metrics` | Prometheus text format — see [Monitoring](monitoring.md) |

A hub adds `/v1/fleet/...` and a `node` parameter to some of the above; on a daemon with no `fleet`
those routes do not exist at all. See [Fleet](fleet.md#endpoints).

## Durations over the wire

The `timeout` field of `POST /v1/processes` and the `grace` query parameter of
`DELETE /v1/processes/{id}` accept **Go syntax only** — no `d`, no `w`. Everything else about value
syntax is in [Configuration](configuration.md#value-syntax).

An override is only accepted when the worker's `overridable` lists that field; anything else → `400`.

## Errors

Errors always look the same:

```json
{"error":{"code":"param_invalid","message":"...","details":{...}}}
```

| Status | When |
|---|---|
| `400` | invalid payload, invalid or undeclared param, unsupported `type`, signal outside the allowlist, or a raw `command` sent together with `worker`, `params` or `timeout` — those belong to a worker, and a raw command has none |
| `401` / `403` | no valid token / token not allowed for that worker, a read-only token on a state-changing call, or a raw command in `workers` mode |
| `404` | unknown worker or execution |
| `409` | lock held with `lock_conflict: reject`, signal on a non-running execution, idempotency key reused with a different body (`idempotency_reuse`), or the same key still being submitted by another request (`idempotency_in_flight` — repeat it) |
| `422` | worker disabled |
| `429` | queue full (`queue.max_depth`) |
| `503` | daemon shutting down, or a service with no free slot (`no_capacity`) |
| `504` | a hub dispatched to a node that did not answer (`dispatch_unknown`). **Not a failure** — the execution may be running; retry with the same `Idempotency-Key` |

## Idempotency

`Idempotency-Key` on `POST /v1/processes` replays the original response — with
`Idempotent-Replay: true` — for as long as that execution is retained (`history.retention`,
`history.max_rows`). The same key with a different body is a `409 idempotency_reuse`.

The key is **claimed before the work starts**, not recorded after it: the case it exists for is a
client that timed out and retried, and the first request is often still in flight when the second
arrives. Whichever request claims the key runs the work; the others replay it, or get `409
idempotency_in_flight` when the winner has not recorded its execution yet — repeat the request with
the same key. A submission refused before anything ran (`429`, `503`) releases its claim, so the
same key can be used again.

Once the execution is purged the key goes with it, and the same key starts new work rather than
answering `404`.

It is the client's key, and on a hub it is forwarded as sent rather than invented: a `504
dispatch_unknown` means the hub stopped and the client is the one that retries, so the key has to be
stable across those retries. See [Fleet](fleet.md#running-work-on-a-node).

## Listing ranges

`created_after` and `created_before` are RFC 3339 instants and are exclusive on both sides. They
compare at full precision, so a bound written in whole seconds — `2026-08-22T10:00:00Z` — includes
everything created during that second on the `created_after` side and excludes it on the
`created_before` side.

---

[Documentation index](README.md) · [CLI](cli.md) · [Lifecycle](lifecycle.md) ·
[Security](security.md) · [Fleet](fleet.md)
