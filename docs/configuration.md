# Configuration

`/etc/processd/processd.yaml` configures the daemon. Worker definitions live in their own files —
see [Workers](workers.md).

Decoding is **strict** everywhere: an unknown key is a load error, never a silent default. A typo in
a security-relevant key such as `allow_root_processes` has to fail loudly.

A fully commented file is [`examples/processd.yaml`](../examples/processd.yaml).

## Value syntax

These three types show up all over the configuration.

| Type | Accepted | Rejected |
|---|---|---|
| **duration** | Go syntax — `500ms`, `30s`, `5m`, `1h30m`, `2h45m10s`; units `ns`, `us`, `ms`, `s`, `m`, `h`. Plus `d` (day) and `w` (week) on their own: `30d`, `2w`, `1.5d` | a bare number (`30`), a mixed `d`/`w` form (`1d12h`), a spelled-out unit (`1 hour`) |
| **byte size** | IEC `KiB`, `MiB`, `GiB` (×1024) and SI `KB`, `MB`, `GB` (×1000), with decimals: `32MiB`, `1.5GiB`, `512KB`. A bare integer is bytes: `1048576` | a bare decimal (`1.5`), unknown suffixes (`32M`, `32 mb`) |
| **attempts** | a positive integer, or the word `unlimited` — the latter only on a `service`. `0` reads as "key not set", so the default applies | negative counts, `unlimited` on a `task` |

Two places take a duration over HTTP instead of from YAML, and those accept **Go syntax only**, with
no `d`/`w`: the `timeout` field of `POST /v1/processes` and the `grace` query parameter of
`DELETE /v1/processes/{id}` (`--grace` in the CLI).

## Daemon keys

Every key is optional; the defaults below are what a missing file gives you.

| Key | Type | Default | Values and rules |
|---|---|---|---|
| `listen` | string | `127.0.0.1:7373` | HTTP bind. No native TLS — put a proxy in front to expose it. Must not be empty |
| `data_dir` | path | `/var/lib/processd` | SQLite state |
| `log_dir` | path | `/var/log/processd` | per-attempt output files |
| `workers_dir` | path | `/etc/processd/workers.d` | where `*.yaml` worker files are read |
| `max_processes` | int > 0 | `50` | ceiling for the whole node |
| `shutdown_grace` | duration | `30s` | budget given to the process tree at shutdown |
| `orphan_policy` | enum | `kill` | `kill` terminates a process that outlived the daemon before retrying; `leave` keeps it and does not retry |
| `execution_mode` | enum | `workers` | `workers` runs pre-configured workers only; `raw` accepts a client-chosen command and then requires `allowed_commands` |
| `allowed_commands` | list of paths | `[]` | absolute paths, used only in `raw` mode |
| `allow_root_processes` | bool | `false` | allows running without `user` when the daemon is root |
| `queue.max_depth` | int > 0 | `1000` | a full queue answers `429` |
| `queue.item_ttl` | duration | `1h` | an item that waited longer fails with `queue_timeout`. A service never expires this way |
| `history.retention` | duration | `30d` | GC of finished executions |
| `history.max_rows` | int | `500000` | ceiling of retained rows |
| `logs.max_bytes_per_stream` | byte size > 0 | `32MiB` | cap per stream per attempt; reaching it marks `log_truncated` |
| `logs.retention` | duration | `14d` | GC of log files |
| `notify` | object | none | the fallback notification policy, in the same shape a worker uses. A worker that declares its own replaces it — see [Notifications](notifications.md) |
| `fleet.nodes[].name` | string | — required | unique; how the node is named in every fleet answer |
| `fleet.nodes[].url` | URL | — required | `http` or `https`, with a host |
| `fleet.nodes[].token_file` | path | — required | absolute path to a **read-only** token for that node. There is deliberately no inline token: this file stores digests, never secrets |
| `fleet.poll_interval` | duration > 0 | `10s` | how often each node is asked how it is doing |
| `fleet.timeout` | duration > 0 | `5s` | bounds one call to one node |
| `ui.enabled` | bool | `true` | the web console at `/ui/` |
| `auth.tokens[].name` | string | — required | identifies the token in the audit trail |
| `auth.tokens[].hash` | string | — required | `sha256:...` from `processd setup` or `processd token hash` |
| `auth.tokens[].workers` | list | `[]` = all | restricts the token to specific workers |
| `auth.tokens[].read_only` | bool | `false` | refuses every state-changing call with this token. What a hub uses to read a node, and what a dashboard should get |

The `fleet` block turns an ordinary daemon into a hub; on a daemon without it, the fleet routes do
not exist at all. See [Fleet](fleet.md).

## Applying a change

The daemon reads `processd.yaml` **at startup only**:

```bash
sudo systemctl restart processd
```

That includes authentication — a rotated or newly added token takes effect on the restart, not
before. `processd reload` re-reads `workers_dir` and nothing else; see
[Operations](operations.md#changing-a-worker-or-a-service).

---

[Documentation index](README.md) · [Workers](workers.md) · [Security](security.md) ·
[Fleet](fleet.md)
