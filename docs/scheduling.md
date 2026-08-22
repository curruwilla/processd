# Scheduling

A scheduled worker is fired by the daemon that runs it. There is no crontab entry holding an API
token, and no external caller whose absence is invisible.

```yaml
- name: nightly-report
  command: /usr/bin/php
  args: [/var/www/app/artisan, report:build, "--range={{range}}"]
  params:
    range: { enum: [daily, weekly], default: daily }
  lock_conflict: reject           # skip a firing while the previous one still runs
  schedule:
    cron: "15 3 * * *"
    timezone: America/Sao_Paulo
    catch_up: false
    jitter: 90s
```

The commented version is
[`examples/workers.d/nightly-report.yaml`](../examples/workers.d/nightly-report.yaml).

## Keys

| Key | Type | Default | Values and rules |
|---|---|---|---|
| `cron` | string | — required | five fields, `minute hour day-of-month month day-of-week`, or one of `@hourly`, `@daily`, `@midnight`, `@weekly`, `@monthly`, `@yearly`, `@annually`. Without it, the other keys are a typo and **fail the load** |
| `timezone` | IANA zone | `UTC` | never the host zone: the same file has to describe the same instants on every node |
| `catch_up` | bool | `false` | `false` records the occurrences missed while the daemon was down and moves on. `true` runs the **most recent** missed occurrence once — never one per occurrence |
| `jitter` | duration | `0` | spreads the firing randomly over `[0, jitter]`, so a fleet sharing a schedule does not hit the same dependency at the same second |

## Expression syntax

`*`, `5`, `1-5`, `*/15`, `1-5/2`, `5/20` (every 20th from 5), and comma-separated lists of any of
those. Month and weekday accept three-letter names (`jan`, `fri`); Sunday is both `0` and `7`.

When **both** day fields are restricted they are combined with OR, not AND — the Vixie rule, so
`0 0 20 * fri` means "the 20th, and every Friday".

## Rules that fail the load

Not the firing at 03:00 that nobody is watching:

* Every param must be answerable without a request. A `required: true` param on a scheduled worker is
  refused — a firing sends no params, so give it a `default` or drop the requirement.
* A `service` cannot be scheduled: it is already meant to be running.
* A broken expression or an unknown zone fails the reload that introduced it.

## Overlap

Overlap is not a separate knob. A scheduled worker with no `lock` of its own gets `schedule:<name>`,
and `lock_conflict` decides what the next firing does while the previous one is still running:

| `lock_conflict` | Next firing |
|---|---|
| `queue` (default) | waits its turn |
| `reject` | is refused, and recorded as a `CANCELED` execution with `reason: lock_conflict` |

Either way the firing is in the history.

## Daylight saving time

Arithmetic, not a special case, and the two directions differ on purpose:

* *Spring forward* — the wall clock never shows the skipped hour, so a schedule inside it does not
  fire that day. It is skipped, not moved.
* *Fall back* — the repeated hour would hand the same local time to two instants, so the second is
  skipped. `03:00 daily` means one firing that day.

## What a firing carries

The execution is created from the current worker definition with no params beyond their defaults, and
its `metadata` records `processd.trigger: schedule` and `processd.occurrence`, the scheduled instant
it belongs to. That instant is not the creation time — jitter and dispatch delay move the second,
never the grid.

## Seeing the next run

```bash
processd workers
```

`processd workers` and `GET /v1/workers` report `next_run`, `last_fired_at`, `last_missed_at` and
`missed_runs`. A schedule whose next firing nobody can see is the crontab entry this replaced.

---

[Documentation index](README.md) · [Workers](workers.md) · [Retry and backoff](retry.md) ·
[Notifications](notifications.md) · [CLI](cli.md)
