# Monitoring

Four places tell you how a node is doing: the health endpoint, the node summary, Prometheus, and the
output of the executions themselves. The web console puts them in a browser.

| Signal | How |
|---|---|
| liveness | `GET /v1/health` — public, no token. `?deep=1` also pings the store and answers `503` when it is unreachable |
| node summary | `processd status`, or `GET /v1/stats` for the same numbers as JSON |
| Prometheus | scrape `GET /v1/metrics` (text format, needs the token) |
| daemon log | `journalctl -u processd`, JSON lines from `log/slog` |
| execution output | `processd logs <id>`, `-f` to follow a live attempt |

## Metrics

```text
processd_daemon_up
processd_slots_used / processd_slots_max
processd_workers
processd_running_attempts
processd_queue_depth
processd_processes{state}
processd_processes_running{worker}
processd_processes_queued{worker}
processd_processes_total{worker,status}        # counter
processd_process_attempts_total{worker}        # counter
processd_service_restarts_total{worker}        # counter
processd_process_duration_seconds{worker}      # histogram
processd_running_cpu_seconds{worker}
processd_running_rss_bytes{worker}
```

Counters and the histogram live in memory and reset with the daemon — which is what Prometheus
expects from a process-local counter: a restart shows up as a reset, not as a hole. CPU and memory
are sampled from `/proc/<pid>/stat` at read time, always checking the process start time first,
because PIDs are recycled.

**Alert on `processd_service_restarts_total`.** A healthy [service](services.md) produces no terminal
state, so the ordinary counters stay silent about it; a service in a restart loop is invisible in
every other family.

## Execution output

stdout and stderr are captured **per attempt**, with the exit code and the terminating signal:

```bash
processd logs <id>                       # the latest attempt
processd logs <id> --attempt 2           # an earlier one
processd logs <id> --stream stderr
processd logs <id> --tail 200
processd logs <id> -f                    # follow a live attempt
```

Over the API that is `GET /v1/processes/{id}/logs`, and
`GET /v1/processes/{id}/logs/stream` for Server-Sent Events — one `line` event per line, `end` when
the attempt finishes.

Size is capped per stream per attempt by `logs.max_bytes_per_stream`; reaching the cap marks the
attempt `log_truncated`. Retention (`logs.retention`) belongs to the daemon, rotation
(`logs.rotate.max_files`) to the worker — and rotation is mandatory on a service, whose single
attempt can run long enough to fill the cap and then go silent.

Retention never collects an attempt that is still writing, however old its last line is.

## Web console

<http://127.0.0.1:7373/ui/> — node dashboard, filtered executions, live CPU and memory, streaming
logs, and a form to run a worker. On a hub it also gains a **Fleet** tab and a node selector, both
hidden on a daemon that aggregates nothing ([Fleet](fleet.md)).

It is a static page served **without a token**: it asks the operator for one and then calls the same
authenticated API as any other client. Turn it off with:

```yaml
ui:
  enabled: false
```

## Failure notifications

Metrics tell you afterwards. To be told at the moment an execution ends badly — by webhook or by
running another worker — see [Notifications](notifications.md).

---

[Documentation index](README.md) · [Operations](operations.md) · [Notifications](notifications.md) ·
[HTTP API](api.md) · [Services](services.md)
