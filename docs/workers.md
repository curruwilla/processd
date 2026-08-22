# Workers

A worker is a command the API is allowed to run. Nothing else runs: in the default
`execution_mode: workers` a client names a worker, never a command line.

Worker files are `*.yaml` in `workers_dir` (`/etc/processd/workers.d` by default), each with
`version: 1` and a `workers` list. One file can declare several workers; the name is a key of the
daemon, not of the file, and must be unique across all of them. One worker per file keeps a failed
reload pointing at the right place.

A `.yml` file is not an error — it is simply never read.

```yaml
# /etc/processd/workers.d/backup.yaml
version: 1
workers:
  - name: backup
    command: /usr/bin/rsync
    args: ["-a", "/data/{{bucket}}/", "/backup/{{bucket}}/"]
    params:
      bucket: { required: true, pattern: "^[a-z0-9-]{1,32}$" }
    cwd: /
    user: backup
    timeout: 1h
    max_processes: 2
    lock: "backup:{{bucket}}"
    retry:
      enabled: true
      max_attempts: 3
      backoff: { type: exponential, initial: 10s, max: 2m, jitter: 0.2 }
```

```bash
sudo processd reload      # re-reads workers.d, prints "N workers loaded"
processd workers          # confirm what the daemon loaded
```

Loading is all-or-nothing: an invalid worker in any file fails the whole reload, the daemon keeps the
previous set, and the error names the file. A bad edit never makes a worker disappear from a live
node. Adding, changing and removing workers on a running node is [Operations](operations.md).

Commented, complete examples: [`examples/workers.d/invoice.yaml`](../examples/workers.d/invoice.yaml)
(task), [`examples/workers.d/api.yaml`](../examples/workers.d/api.yaml) (service),
[`examples/workers.d/nightly-report.yaml`](../examples/workers.d/nightly-report.yaml) (scheduled, with
notifications), [`examples/workers.d/notify-slack.yaml`](../examples/workers.d/notify-slack.yaml)
(notification target).

## Fields

| Key | Type | Default | Values and rules |
|---|---|---|---|
| `name` | string | — required | unique across every file |
| `enabled` | bool | `true` | `false` still loads the worker, and refuses executions with `422` |
| `type` | enum | `task` | `task` terminates and its success is final; `service` must not terminate and any exit restarts it. The worker owns the type — a request may state it, but only to agree |
| `command` | path | — required | **absolute**, executed directly, never through a shell |
| `args` | list of strings | `[]` | may contain `{{param}}`; substitution never splits an element |
| `params` | map | `{}` | what a request may send — see [below](#params) |
| `cwd` | path | `/` | must be absolute; a directory that does not exist fails the start, not the load |
| `user` | string | empty | system user **name**, not a uid. Empty while the daemon runs as root refuses the start, unless `allow_root_processes: true` |
| `group` | string | the user's primary group | system group name; supplementary groups are applied |
| `env` | map | `{}` | the child environment is built, not inherited: the daemon environment holds secrets |
| `env_passthrough` | list of strings | `[]` | names forwarded from the daemon environment, e.g. `[PATH, LANG, TZ]` |
| `timeout` | duration | `0` = none | when it expires: `SIGTERM` to the group → `kill_grace` → `SIGKILL`, and the outcome counts as the `timeout` retry trigger. Refused on a `service`: it has no deadline to exceed. A request may override it only if `overridable` lists `timeout`, and that value is Go syntax only |
| `kill_grace` | duration | `15s` | wait between `SIGTERM` and `SIGKILL` |
| `max_processes` | int ≥ 0 | `0` = only the global limit | per-worker ceiling. A task over it **waits in the queue**; a service over it is refused with `503`, never queued |
| `lock` | string | empty = no lock | mutual-exclusion key, may contain `{{param}}` |
| `lock_conflict` | enum | `queue` | `queue` waits for the lock; `reject` answers `409` immediately |
| `overridable` | list of enum | `[]` | what a request may override: `env`, `timeout`, `lock`. Anything else → `400` |
| `retry` | object | off | see [Retry and backoff](retry.md) |
| `schedule` | object | none | fires the worker on its own; see [Scheduling](scheduling.md). Refused on a `service` — a service is already meant to be running |
| `notify` | object | the daemon-wide `notify`, if any | who to tell when an execution ends badly; see [Notifications](notifications.md) |
| `logs.max_bytes_per_stream` | byte size | the daemon value (`32MiB`) | cap per stream per attempt; retention belongs to the daemon |
| `logs.rotate.max_files` | int ≥ 0 | `0` = no rotation | how many rotated files to keep behind the live one. Without rotation the stream stops storing once the cap is full — **mandatory for a `service`** |

The loader validates the shape of a definition, not the host it will run on. Check these yourself,
otherwise the reload succeeds and the first run fails:

* `command` exists on this host — the loader only checks that the path is absolute;
* `cwd` exists;
* the `user` exists — `id www-data`.

## `params`

Arguments reach a process **only** through declared, validated params.

| Key | Type | Default | Rule |
|---|---|---|---|
| `required` | bool | `false` | missing in the request → `400` |
| `pattern` | RE2 regex | empty | compiled when the worker loads; a value outside it → `400` |
| `enum` | list of strings | `[]` | a value outside the list → `400` |
| `default` | string | empty | used when an optional param is absent |

Substitution rules:

1. `{{name}}` is resolved **inside** elements of `args` and in `lock`, nowhere else.
2. It never creates, splits or joins argv elements: a value with spaces stays one argument.
3. A placeholder not declared in `params` **fails the load** — a typo never reaches the command line
   as a literal `{{id}}`.
4. A param sent but not declared → `400`.
5. An argv element that is only an absent optional placeholder is dropped, instead of becoming `""`.

```bash
processd run invoice-process --param id=42 --param verbose=1
```

## Locks

`lock` is a mutual-exclusion key, and `{{param}}` inside it is what makes it useful: `invoice:{{id}}`
serialises invoice 42 against itself while leaving invoice 43 alone. `lock_conflict` decides what a
second execution does while the key is held — `queue` waits its turn, `reject` answers `409`
immediately and records nothing.

A lock belongs to a running attempt, never to a place in line: an execution waiting for a slot holds
none, whatever its `lock_conflict` says, and claims it when the scheduler starts it. `reject` answers
`409` against work that is actually running, not against work that is queued behind a full node.

The lock is held across a retry: releasing it during the backoff would let another execution take it
mid-retry.

A scheduled worker with no `lock` of its own gets `schedule:<name>`, which is how overlap between
firings is decided — see [Scheduling](scheduling.md#overlap).

---

[Documentation index](README.md) · [Retry and backoff](retry.md) · [Services](services.md) ·
[Scheduling](scheduling.md) · [Notifications](notifications.md) · [Operations](operations.md)
