# Processd

*Também disponível em [português do Brasil](README.pt-BR.md).*

> **Run any CLI process through a simple API and keep it alive.**

Lightweight process manager written in Go. It runs and supervises CLI processes through a REST API:
one binary, one configuration file, one API. It sits in the gap between local supervisors
(Supervisor, PM2) and distributed orchestrators (Nomad, Kubernetes).

**Status: alpha.** The MVP is complete — the daemon runs, supervises, persists and recovers real
processes — but it has not been used in production yet. Treat the first install as a pilot.

## What it does

* Runs CLI processes through a REST API, with validated arguments and no shell involved.
* Two execution types: a `task` runs once and its success is final; a `service` is not expected to
  exit, and any exit restarts it.
* Identifies every execution by a stable logical ID that survives retries and daemon restarts.
* Caps concurrency globally and per worker, and queues the excess.
* Captures stdout and stderr per attempt, with the exit code and terminating signal, and streams
  the output live.
* Applies timeouts, retry with backoff and locks against concurrent runs.
* Fires a worker on its own cron schedule, reports the next run, and records the occurrences it
  missed while it was down.
* Tells somebody when an execution ends badly, through a webhook or by running another worker.
* Reads other nodes — one console, one `ps`, log streams proxied — and runs work on the one you
  name, without ever choosing that node for you.
* Persists state in SQLite and reconciles what it finds after a restart.
* Exposes Prometheus metrics, per-execution CPU and memory, and an embedded web console.
* Shuts the daemon and the whole process tree down gracefully.

## What it does not do

Container orchestration, service mesh, distributed consensus, automatic placement across servers,
auto-scaling, provisioning, desired state and replicas, native TLS, OpenTelemetry tracing. It does
not replace systemd for system services.

It supervises processes **on the host it runs on**, using the runtimes, the project directories and
the user accounts that are already there.

---

## Quick start

### 1. Install the binary

```bash
VERSION=0.1.0                               # github.com/curruwilla/processd/releases
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/processd_${VERSION}_linux_${ARCH}.tar.gz"
tar -xzf "processd_${VERSION}_linux_${ARCH}.tar.gz" processd
sudo install -m 0755 processd /usr/local/bin/processd
```

Every release also publishes `checksums.txt`:

```bash
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/checksums.txt"
sha256sum --check --ignore-missing checksums.txt
```

<details>
<summary>Distro package, or building from source</summary>

`.deb`, `.rpm` and `.apk`, for `amd64` and `arm64`:

```bash
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/processd_${VERSION}_linux_${ARCH}.deb"
sudo dpkg -i "processd_${VERSION}_linux_${ARCH}.deb"        # or: sudo rpm -i ...rpm
```

The package installs the binary at `/usr/bin/processd` and the examples at
`/usr/share/doc/processd/examples/`. Configuration, token and systemd unit stay with
`processd setup`.

From source (Go 1.25+, Linux):

```bash
git clone https://github.com/curruwilla/processd && cd processd
make build                                  # bin/processd, CGO-free
sudo install -m 0755 bin/processd /usr/local/bin/processd
```

Install the binary in a system path — `/usr/local/bin` or `/usr/bin`. The systemd unit points at the
binary that ran `processd setup`, so a unit aimed at a build tree breaks the moment that tree is
rebuilt or moved.

</details>

### 2. Prepare the node

```bash
sudo processd setup
```

One command delivers the whole node:

* creates `/etc/processd/workers.d`, `/var/lib/processd` and `/var/log/processd`;
* writes `/etc/processd/processd.yaml`;
* mints an API token, stored as a digest in the configuration and in plain text at
  `/etc/processd/token` (mode `0600`, owned by root);
* installs `/etc/systemd/system/processd.service`, pointing at the binary that ran it, and starts it;
* prints the token and every path, address and check command.

Client commands read `/etc/processd/token` when neither `--token` nor `PROCESSD_TOKEN` is set, so as
root on the node you never paste the secret. Running setup again keeps the installed token instead of
invalidating every configured client; `--rotate-token` replaces it on purpose.

Other flags: `--dry-run` reports what it would do without touching anything, `--listen` changes the
bind address, `--systemd=false` skips the unit, `--start=false` installs and enables without starting,
`--output json` returns the same report for scripts.

The configuration is rewritten from the values the daemon parsed — comments are lost, keys are
reordered — and the previous file is kept next to it as `processd.yaml.bak-<timestamp>`.

Check it:

```bash
systemctl status processd
processd status
journalctl -u processd -f
```

### 3. Declare a worker

A worker is a command the API is allowed to run. One worker per file keeps a failed reload pointing
at the right place:

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

### 4. Run it

```bash
processd run backup --param bucket=invoices    # returns the execution id
processd ps
processd logs -f proc_01KABCDEF...
processd stop proc_01KABCDEF... --grace 15s
```

The same thing over the API:

```bash
curl -X POST http://127.0.0.1:7373/v1/processes \
  -H "Authorization: Bearer $(sudo cat /etc/processd/token)" \
  -H "Content-Type: application/json" \
  -d '{"worker":"backup","params":{"bucket":"invoices"}}'
```

```json
{ "id": "proc_01KABCDEF...", "status": "STARTING", "pid": null, "attempt": 1 }
```

From another machine, or as another user, point the client at the node:

```bash
export PROCESSD_SERVER=http://127.0.0.1:7373
export PROCESSD_TOKEN=...
```

The web console is at <http://127.0.0.1:7373/ui/>: node dashboard, filtered executions, live
CPU/memory, streaming logs, and a form to run a worker. It is a static page served without a token —
it asks the operator for one and then calls the same API. Turn it off with `ui: { enabled: false }`.
Prometheus metrics are at `/v1/metrics`.

---

## Operating a node

### Command cheat sheet

| I want to | Command |
|---|---|
| see health and free slots | `processd status` |
| see what is running | `processd ps`, `processd ps --type service`, `processd ps --status FAILED` |
| load worker files after an edit | `sudo processd reload` |
| see what the daemon has loaded | `processd workers` |
| run a task | `processd run <worker> --param name=value` |
| start a service | `processd run <worker>` |
| read the output | `processd logs <id>`, add `-f` to follow |
| stop something | `processd stop <id> [--grace 15s]` |
| restart something with the current definition | `processd restart <id>` |
| poke a running process | `processd signal <id> SIGHUP` |

### Adding a worker or a service

1. **Write the file** in `/etc/processd/workers.d/`. The loader validates the shape of a
   definition, not the host it will run on, so check these yourself — otherwise the reload succeeds
   and the first run fails:
   * `command` exists on this host (the loader only checks that the path is absolute);
   * `cwd` exists;
   * the `user` exists — `id www-data`.

   Two things the loader does catch: a `name` already used by another file, and a filename that does
   not end in `.yaml` — a `.yml` file is not an error, it is simply never read.

2. **Load it:**

   ```bash
   sudo processd reload
   processd workers
   ```

   Loading is all-or-nothing: an invalid worker in any file fails the whole reload, the daemon keeps
   the previous set, and the error names the file. A bad edit never makes a worker disappear from a
   live node.

3. **Start it.** A reload only registers definitions; it never starts anything.
   * A **task** runs when you ask: `processd run backup --param bucket=invoices`.
   * A **service** needs one `processd run <worker>` to create the execution that then stays alive.
     Check it with `processd ps --type service`.

A service takes its slot at admission or is refused with `no_capacity` — it is never queued. Look at
`processd status` for free slots before adding one.

### Changing a worker or a service

Edit the file, then reload:

```bash
sudo processd reload
```

**A reload never mutates a running process.** Every execution keeps the definition it was created
with, so what to do next depends on what changed:

| Situation | What to do |
|---|---|
| a task worker | nothing — the next `processd run` uses the new definition |
| a running service | `processd restart <id>`: stops it and creates a new execution, with a new id, from the definition just loaded |
| only the program's own configuration changed | `processd signal <id> SIGHUP`, if it reloads on `SIGHUP` — nothing restarts and the id stays |
| the worker was renamed | the new name is registered; the old service keeps running under the old definition. `processd stop <old id>`, then `processd run <new name>` |
| the worker was disabled (`enabled: false`) | new executions are refused with `422`; what already runs is untouched. Stop it with `processd stop <id>` |
| the worker file was deleted | same as disabled, and worse: once the running attempt ends there is no policy left to bring it back. Stop it explicitly |

`processd restart` checks the worker before it stops anything, and refuses when it is gone or
disabled — otherwise it would leave you with a stopped service and nothing to start again.

### Running services

| Action | Command | What happens |
|---|---|---|
| start | `processd run api` | one execution, supervised, restarted on any exit |
| stop for good | `processd stop <id>` | `SIGTERM` to the process group, `SIGKILL` after `kill_grace`. Ends as `CANCELED` with `reason: user_request` and **never** retries |
| restart | `processd restart <id>` | stops it, waits for the slot, creates a new execution from the current definition |
| reload its own config | `processd signal <id> SIGHUP` | the signal reaches the whole process group; the execution is untouched |
| watch it | `processd ps --type service` | the `RESTARTS` column is how hard the node is fighting to keep it up |

A deliberate stop is the only way a service ends without coming back. Every other exit — clean, with
an error, or killed by a signal — is a restart, unless the exit code is listed in
`no_retry_exit_codes`.

### Updating processd

`processd setup` is **not** the update command: `systemctl enable --now` does not restart a service
that is already running, so a new binary would sit on disk unused. Setup is for the first install,
and for when the bind address, the unit path or the binary path changed.

```bash
# 1. know which binary the unit actually starts
systemctl cat processd | grep ExecStart

# 2. back up the state — migrations are one-way
sudo systemctl stop processd
sudo cp -a /var/lib/processd /var/lib/processd.bak-$(date +%F)

# 3. replace that binary
sudo install -m 0755 ./processd /usr/local/bin/processd

# 4. start it and check
sudo systemctl start processd
processd status                 # version, slots, running, queued
processd ps --type service      # is everything back up?
```

* **Install the CLI and the daemon from the same build.** They are the same binary; a client older
  than the daemon simply lacks the newer commands, which is confusing to debug.
* **Migrations run automatically** at startup, once each, in filename order. There is no downgrade
  path once one has run — that is what step 2 is for.
* **Services with `retry.on_shutdown: true`** are returned to the queue at shutdown and come back by
  themselves on the next start, keeping their id. Without it they end as `CANCELED` and need
  `processd run`.
* **Tasks in flight** get `shutdown_grace` (default `30s`) to finish; whatever is left is killed. The
  generated unit sets `TimeoutStopSec` to that plus 15s, so systemd never cuts the daemon off in the
  middle of a graceful shutdown.

Editing workers needs none of this — that is `processd reload`.

### Backup and retention

| What | Where | Matters because |
|---|---|---|
| state | `/var/lib/processd` (SQLite) | executions, history, locks, idempotency keys |
| configuration and token | `/etc/processd` | every configured client authenticates against it |
| output | `/var/log/processd` | per-attempt stdout and stderr |

Back the state up with the daemon stopped, so the database file is consistent — and always before an
upgrade. Retention is enforced by the daemon itself: `history.retention` and `history.max_rows` for
executions, `logs.retention` for output files.

### Monitoring

| Signal | How |
|---|---|
| liveness | `GET /v1/health` — public, no token. `?deep=1` also pings the store and answers `503` when it is unreachable |
| node summary | `processd status`, or `GET /v1/stats` for the same numbers as JSON |
| Prometheus | scrape `GET /v1/metrics` (text format, needs the token) |
| daemon log | `journalctl -u processd`, JSON lines from `log/slog` |
| execution output | `processd logs <id>`, `-f` to follow a live attempt |

A healthy `service` produces no terminal state, so the ordinary counters stay silent about it. Alert
on `processd_service_restarts_total` instead — a service in a restart loop is invisible in every
other family.

### Securing the node

* **There is no native TLS.** The default bind is `127.0.0.1`; put a reverse proxy (nginx, Caddy,
  Traefik) in front of it for anything remote.
* **Every request needs a token**, except `GET /v1/health`. Give each client its own, scoped to the
  workers it may run:

  ```yaml
  auth:
    tokens:
      - name: billing-cron
        hash: "sha256:..."      # printf '%s' "$TOKEN" | processd token hash
        workers: ["invoice-process"]
      - name: ops
        hash: "sha256:..."      # no workers key: every worker
  ```

  The token name is what shows up in the audit trail. Rotate with
  `sudo processd setup --rotate-token`, then `sudo systemctl restart processd` — the daemon reads
  authentication only at startup.
* **Keep `execution_mode: workers`** (the default). The `raw` mode lets a client choose the command
  and is remote code execution by design; it also requires an explicit `allowed_commands` allowlist.
* **Keep `allow_root_processes: false`.** With it off, a worker without `user` is refused instead of
  silently running as root.

### Runbook

| Task | Command |
|---|---|
| add a worker | write `workers.d/<name>.yaml`, `sudo processd reload`, `processd workers` |
| start a new service | `processd run <worker>` |
| deploy a worker change | edit the file, `sudo processd reload` |
| apply that change to a running service | `processd restart <id>` |
| stop a service for good | `processd stop <id>` |
| investigate a failure | `processd ps --status FAILED`, then `processd logs <id>` |
| rotate the API token | `sudo processd setup --rotate-token`, `sudo systemctl restart processd` |
| update processd | back up `/var/lib/processd`, replace the binary, `sudo systemctl restart processd` |
| restart the whole node | `sudo systemctl restart processd` — services with `on_shutdown: true` come back on their own |

---

<details>
<summary>Configuring a node by hand, without <code>processd setup</code></summary>

```bash
sudo mkdir -p /etc/processd/workers.d /var/lib/processd /var/log/processd
TOKEN=$(openssl rand -hex 32)
printf '%s' "$TOKEN" | processd token hash   # prints sha256:...
```

Only the digest goes in the file. The token is read from stdin so it appears neither in the process
list nor in the shell history.

```yaml
# /etc/processd/processd.yaml
listen: 127.0.0.1:7373
data_dir: /var/lib/processd
log_dir: /var/log/processd
workers_dir: /etc/processd/workers.d

max_processes: 50

auth:
  tokens:
    - name: dev
      hash: "sha256:<paste the digest here>"
```

Run it in the foreground with `processd serve --config /etc/processd/processd.yaml`, or copy
[`examples/processd.service`](examples/processd.service) to `/etc/systemd/system/`. Whatever unit you
write, `TimeoutStopSec` has to exceed `shutdown_grace`, otherwise systemd kills the daemon in the
middle of stopping the process groups it supervises.

</details>

<details>
<summary>Verifying signatures and SBOMs</summary>

`checksums.txt` is signed with [cosign](https://docs.sigstore.dev/) in keyless mode: the signature is
bound to the identity of the workflow that published the release, not to a private key. Verifying
needs cosign v2+:

```bash
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/checksums.txt"
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/checksums.txt.sigstore.json"

cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/curruwilla/processd/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  checksums.txt
```

With `checksums.txt` verified, the `sha256sum --check` above covers every binary and package.

Each `.tar.gz`, `.deb`, `.rpm` and `.apk` ships an SPDX SBOM next to it (`<artifact>.spdx.json`):
every Go module that went into the binary, with its version, so "is this install affected by that
CVE?" can be answered without rebuilding anything.

```bash
jq -r '.packages[] | "\(.name) \(.versionInfo)"' "processd_${VERSION}_linux_${ARCH}.tar.gz.spdx.json"
```

</details>

---

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

Decoding is **strict** everywhere: an unknown key is a load error, never a silent default. A typo in
a security-relevant key such as `allow_root_processes` has to fail loudly.

## Daemon configuration

`/etc/processd/processd.yaml`. Every key is optional; the defaults below are what a missing file
gives you.

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
| `notify` | object | none | the fallback notification policy, in the same shape a worker uses. A worker that declares its own replaces it |
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

## Worker definition

`*.yaml` files in `workers_dir`, each with `version: 1` and a `workers` list. One file can declare
several workers; the name is a key of the daemon, not of the file, and must be unique across all of
them.

A `service` is a worker that is not expected to exit. Its restart defaults come for free; only log
rotation is mandatory, because a single attempt can run for months, fill the cap and then go silent:

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

### Fields

| Key | Type | Default | Values and rules |
|---|---|---|---|
| `name` | string | — required | unique across every file |
| `enabled` | bool | `true` | `false` still loads the worker, and refuses executions with `422` |
| `type` | enum | `task` | `task` terminates and its success is final; `service` must not terminate and any exit restarts it. The worker owns the type — a request may state it, but only to agree |
| `command` | path | — required | **absolute**, executed directly, never through a shell |
| `args` | list of strings | `[]` | may contain `{{param}}`; substitution never splits an element |
| `params` | map | `{}` | what a request may send (table below) |
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
| `retry` | object | off | table below |
| `schedule` | object | none | fires the worker on its own; table below. Refused on a `service` — a service is already meant to be running |
| `notify` | object | the daemon-wide `notify`, if any | who to tell when an execution ends badly; table below |
| `logs.max_bytes_per_stream` | byte size | the daemon value (`32MiB`) | cap per stream per attempt; retention belongs to the daemon |
| `logs.rotate.max_files` | int ≥ 0 | `0` = no rotation | how many rotated files to keep behind the live one. Without rotation the stream stops storing once the cap is full — **mandatory for a `service`** |

### `params`

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

### `retry`

`enabled` is tri-state: absent is not the same as `false`. A `task` without the key does not retry; a
`service` without it restarts, and an explicit `enabled: false` on a service is refused at load.

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

Curves: `fixed` = `initial`; `linear` = `initial × attempt`; `exponential` =
`initial × multiplier^(attempt-1)`. The `max` ceiling is applied before the jitter.

The lock is held across a retry: releasing it during the backoff would let another execution take it
mid-retry.

### `schedule`

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

| Key | Type | Default | Values and rules |
|---|---|---|---|
| `cron` | string | — required | five fields, `minute hour day-of-month month day-of-week`, or one of `@hourly`, `@daily`, `@midnight`, `@weekly`, `@monthly`, `@yearly`, `@annually`. Without it, the other keys are a typo and **fail the load** |
| `timezone` | IANA zone | `UTC` | never the host zone: the same file has to describe the same instants on every node |
| `catch_up` | bool | `false` | `false` records the occurrences missed while the daemon was down and moves on. `true` runs the **most recent** missed occurrence once — never one per occurrence |
| `jitter` | duration | `0` | spreads the firing randomly over `[0, jitter]`, so a fleet sharing a schedule does not hit the same dependency at the same second |

Expression syntax: `*`, `5`, `1-5`, `*/15`, `1-5/2`, `5/20` (every 20th from 5), and comma-separated
lists of any of those. Month and weekday accept three-letter names (`jan`, `fri`); Sunday is both `0`
and `7`. When **both** day fields are restricted they are combined with OR, not AND — the Vixie rule,
so `0 0 20 * fri` means "the 20th, and every Friday".

Rules that fail the load rather than the firing:

* Every param must be answerable without a request. A `required: true` param on a scheduled worker is
  refused — a firing sends no params, so give it a `default` or drop the requirement.
* A `service` cannot be scheduled.
* A broken expression or an unknown zone fails the reload that introduced it, not the tick at 03:00
  that nobody is watching.

Overlap is not a separate knob. A scheduled worker with no `lock` of its own gets `schedule:<name>`,
and `lock_conflict` decides what the next firing does while the previous one is still running:
`queue` waits its turn, `reject` refuses it and records a `CANCELED` execution with
`reason: lock_conflict`. Either way the firing is in the history.

**Daylight saving time** is arithmetic, not a special case, and the two directions differ on purpose:

* *Spring forward* — the wall clock never shows the skipped hour, so a schedule inside it does not
  fire that day. It is skipped, not moved.
* *Fall back* — the repeated hour would hand the same local time to two instants, so the second is
  skipped. `03:00 daily` means one firing that day.

What a firing carries: the execution is created from the current worker definition with no params
beyond their defaults, and its `metadata` records `processd.trigger: schedule` and
`processd.occurrence`, the scheduled instant it belongs to. That instant is not the creation time —
jitter and dispatch delay move the second, never the grid.

`processd workers` and `GET /v1/workers` report `next_run`, `last_fired_at` and `missed_runs`. A
schedule whose next firing nobody can see is the crontab entry this replaced.

### `notify`

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

| Key | Type | Default | Values and rules |
|---|---|---|---|
| `on` | list of enum | — required whenever anything else is set | `failed`, `crashed`, `retries_exhausted`, `timeout` |
| `webhook.url` | URL | — | `http` or `https`, with a host. Anything else fails the load |
| `webhook.method` | string | `POST` | |
| `webhook.headers` | map | `{}` | sent as written; `Content-Type: application/json` is always set |
| `webhook.timeout` | duration | `5s` | bounds one attempt. Must be greater than zero |
| `webhook.retry` | int ≥ 0 | `0` | extra attempts after a failure, 2s apart |
| `webhook.include_log_tail` | int ≥ 0 | `0` = none | last N lines of the attempt's output. **Off by default and opt-in on purpose** — logs carry secrets far more often than anyone intends |
| `exec.worker` | worker name | — | runs that worker; see below |

**The events do not overlap.** One outcome sends exactly one notification, and the daemon picks which:

| Event | Means |
|---|---|
| `crashed` | an attempt ended badly and **another will follow** |
| `timeout` | it is over, and the last attempt was killed by its own deadline |
| `retries_exhausted` | it is over, and a retry policy spent its budget |
| `failed` | it is over, with no retry policy to spend — including `no_retry_exit_codes` |

Nothing is sent for a success, for `DELETE /v1/processes/{id}`, or for a shutdown: a human who
stopped an execution already knows.

`exec.worker` runs a worker instead of, or as well as, the webhook. It obeys the same rule as every
other execution — **nothing reaches a process that the worker did not declare**. The outcome is
offered as params, and only the ones the target declares are passed:

```
event  process_id  worker  state  reason  attempt  exit_code  signal  node
```

Rules that fail the load rather than the failure they were meant to report: the target must exist
(in any file), must be a `task`, must not declare `notify` of its own — a notifier that notifies
about its own failure is a loop with no bound — and must not `require` a param outside that list.

**What the payload carries**, and deliberately does not: identity, outcome, timing, and the
`metadata` a client put there itself. There is no environment, no command and no argument list. The
daemon environment holds secrets by design, and a webhook is the one place they would leave the node.

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

Delivery is **best effort, and never touches the execution it describes**. A notification that
cannot be delivered is logged and dropped; it never fails, delays or retries the work. The queue is
bounded, so a node whose workers are all failing does not grow a notification backlog on top of the
incident, and shutdown waits at most 5s for what is queued.

## Lifecycle

```
CREATED  → QUEUED, STARTING, CANCELED
QUEUED   → STARTING, CANCELED, FAILED
STARTING → RUNNING, CRASHED
RUNNING  → COMPLETED, CRASHED, STOPPING
STOPPING → CANCELED, FAILED, CRASHED, QUEUED
CRASHED  → RETRYING, FAILED
RETRYING → STARTING, QUEUED, CANCELED
```

`COMPLETED`, `FAILED` and `CANCELED` are terminal and immutable — running the same work again is a
new execution with a new ID. A transition missing from the table in `internal/core/state.go` is a
bug, never a silent no-op. States carry a reason: `user_request`, `timeout`, `max_attempts`,
`queue_timeout`, `shutdown`, `daemon_restart`, `start_error`, `no_retry_exit_code`, `lock_conflict`,
`orphaned`, `no_capacity`.

A service never sits in the queue: it takes its slot at admission or is refused, and it only passes
through `QUEUED` on the way back from a daemon restart it was told to survive.

## CLI

One binary, client of the same public API. `--server`/`PROCESSD_SERVER` and `--token`/
`PROCESSD_TOKEN` configure it; without an explicit token it reads the file written by
`processd setup` next to the configuration. Every persistent flag has a `PROCESSD_`-prefixed
variable (`--log-level` → `PROCESSD_LOG_LEVEL`).

| Command | What it does |
|---|---|
| `processd setup [--dry-run] [--rotate-token] [--listen addr] [--systemd=false] [--start=false] [--output json]` | installs the node: directories, configuration, token, systemd unit, and prints all of it |
| `processd serve --config <path>` | runs the daemon |
| `processd status` | health, version, slots, running and queued |
| `processd ps [--status S] [--type task\|service] [--worker w] [--node n\|*] [--limit n] [--cursor c] [--output table\|json]` | lists executions; `--node` reads a fleet node instead of this one |
| `processd run <worker> [--param name=value] [--lock k] [--node n]` | creates an execution; `--node` runs it on that fleet node |
| `processd logs <id> [--stream stdout\|stderr\|both] [--attempt n] [--tail n] [-f] [--node n]` | captured output, `-f` streams it |
| `processd stop <id> [--grace 15s] [--node n]` | `SIGTERM` to the group, `SIGKILL` after the grace |
| `processd restart <id> [--grace 15s] [--param n=v] [--wait 1m] [--node n]` | stops it and creates a new execution from the current worker definition |
| `processd signal <id> <SIGNAL> [--node n]` | sends an allowlisted signal to the group |
| `processd workers` | loaded workers, with their declared params, cron expression and next run |
| `processd fleet [--output table\|json]` | on a hub: every node it reads, with the reason for any that is unreachable |
| `processd reload` | re-reads `workers.d` |
| `processd token hash` | reads a token from stdin and prints the configuration digest |

Accepted signals: `SIGTERM`, `SIGINT`, `SIGQUIT`, `SIGHUP`, `SIGUSR1`, `SIGUSR2`, `SIGKILL`,
`SIGSTOP`, `SIGCONT`. Anything else is refused with `400`, and every signal reaches the whole
**process group** — signalling only the PID would leave grandchildren alive.

## HTTP API

Base `/v1`, JSON in and out. Every endpoint needs `Authorization: Bearer <token>` except
`GET /v1/health`.

| Endpoint | Notes |
|---|---|
| `POST /v1/processes` | `{"worker":"...","params":{...}}`. `201` admitted, `202` queued. An optional `Idempotency-Key` replays the original response with `Idempotent-Replay: true` for as long as that execution is retained; the same key with a different body → `409`. On a hub, an optional `node` dispatches it there instead |
| `GET /v1/processes` | filters: `status` (repeatable), `type`, `worker`, `lock`, `created_after`, `created_before`; `limit` default 50, max 500; cursor paging |
| `GET /v1/processes/{id}` | full representation, including live CPU and memory while it runs |
| `DELETE /v1/processes/{id}?grace=15s` | `CANCELED` with `reason: user_request`, and **never** triggers a retry |
| `POST /v1/processes/{id}/signal` | `{"signal":"SIGUSR1"}` |
| `GET /v1/processes/{id}/logs` | `?stream=stdout\|stderr\|both&attempt=N&tail=N` |
| `GET /v1/processes/{id}/logs/stream` | Server-Sent Events, one `line` event per line, `end` when the attempt finishes |
| `GET /v1/workers` | workers the token may see; a scheduled one carries `schedule` with `cron`, `timezone`, `next_run`, `last_fired_at`, `last_missed_at` and `missed_runs` |
| `POST /v1/reload` | re-reads `workers.d`, all-or-nothing |
| `GET /v1/health[?deep=1]` | public; `deep` also pings the store |
| `GET /v1/stats` | slots, queue depth, states, service counters |
| `GET /v1/metrics` | Prometheus text format |

Errors always look the same: `{"error":{"code":"param_invalid","message":"...","details":{...}}}`.

| Status | When |
|---|---|
| `400` | invalid payload, invalid or undeclared param, unsupported `type`, signal outside the allowlist |
| `401` / `403` | no valid token / token not allowed for that worker, a read-only token on a state-changing call, or a raw command in `workers` mode |
| `404` | unknown worker or execution |
| `409` | lock held with `lock_conflict: reject`, signal on a non-running execution, idempotency key reused with a different body |
| `422` | worker disabled |
| `429` | queue full (`queue.max_depth`) |
| `503` | daemon shutting down, or a service with no free slot (`no_capacity`) |
| `504` | a hub dispatched to a node that did not answer (`dispatch_unknown`). **Not a failure** — the execution may be running; retry with the same `Idempotency-Key` |

## Fleet

A **hub** is an ordinary processd that also reads other nodes. It aggregates their read API and
proxies reads to them, and it **never writes to one**.

```yaml
# /etc/processd/processd.yaml, on the hub only
fleet:
  poll_interval: 10s
  nodes:
    - name: app-01
      url: https://10.0.0.11:7373
      token_file: /etc/processd/nodes/app-01.token
    - name: app-02
      url: https://10.0.0.12:7373
      token_file: /etc/processd/nodes/app-02.token
```

```bash
processd fleet                    # every node: reachable, version, slots, running, queued, last seen
processd ps --node app-01         # that node's executions, with its own paging
processd ps --node '*'            # newest across every node, merged
```

The console gains a **Fleet** tab and a node selector on the processes view, both of which stay
hidden on a daemon that aggregates nothing.

**Nothing is configured on the node.** The hub polls; there is no registration protocol and no
hub-side state on the node, so a node does not know it is being read and needs no change to join a
fleet. That simplification is only available because the aggregation never writes.

| Endpoint | Notes |
|---|---|
| `GET /v1/fleet/nodes` | the last poll of every node: `reachable`, `version`, `last_seen`, `stats`, and `error` when it is not answering |
| `GET /v1/fleet/nodes/{node}/{path...}` | proxies any read to that node's `/v1/{path}`, including `logs/stream`. Streams as it arrives; a path that climbs out of `/v1` is refused |
| `GET /v1/processes?node=app-01` | that node's listing, with its own cursor |
| `GET /v1/processes?node=*` | newest first across every node, each row tagged with `node` |
| `POST /v1/processes` with `"node"` | dispatches to that node; the answer is the node's own, plus `X-Processd-Node`. `504 dispatch_unknown` when the node did not answer |
| `POST`/`DELETE /v1/fleet/nodes/{node}/{path...}` | forwards a write to the node the client named. The node applies its own rules to the hub's token, so a `read_only` one refuses it |

On a daemon with no `fleet`, those routes **do not exist** — the absence is the statement that there
is no fleet, rather than an empty list that looks like a broken one.

Things worth knowing before relying on it:

* **A hub being down changes nothing.** Every node keeps running, supervising and retrying exactly as
  it was. The worst case is a stale panel.
* **The token is the hub's, never the caller's.** A client authenticates to the hub; the hub
  authenticates to each node with the token from `token_file`, which it re-reads on every poll — so
  rotating a node's token takes effect within one interval, not at the next restart. Give the hub a
  `read_only: true` token, and the node will refuse a write even if the hub is ever asked to make one.
* **One unreachable node degrades the answer, never fails it.** A merged listing returns what the
  live nodes had and names the ones that did not answer in `unreachable`; a node status keeps its last
  known numbers next to the reason it is now unreachable.
* **A merged page has no cursor.** There is no ordering across nodes to page through, and inventing
  one would be a distributed index nobody asked for. Page deeper by naming a single node.
* **Version skew is expected.** Node answers are decoded loosely, so a counter or a field from a
  newer node is passed through rather than dropped.
* **Logs are proxied, never copied.** The output stays on the node that produced it.

### Running work on a node

Every command that acts on one execution takes `--node`, and the hub forwards it to the node that was
named. **The client chooses the node; the hub never does.**

```bash
processd run sleeper --node app-01
processd logs <id> --node app-01 -f
processd signal <id> SIGUSR1 --node app-01
processd stop <id> --node app-01
processd restart <id> --node app-01
```

Over the API, that is `POST /v1/processes` with a `node` field, or any method against
`/v1/fleet/nodes/{node}/{path...}`. The `node` field is stripped before the body reaches the node,
which has no idea what a fleet is.

Three things make this a router and not a scheduler:

* **The execution lives only on the node.** The hub keeps no copy of what it forwarded, so its own
  `processd ps` stays empty. There is nothing to reconcile, and therefore nothing to reconcile wrongly.
* **`node` is never inferred.** Without it the execution is local and explicit; the hub does not
  "pick one".
* **Silence is not a failure.** A node that did not answer has not said no — the execution may be
  running right now. The hub answers `504` with `dispatch_unknown` and tells you to retry with the
  same `Idempotency-Key`, and it never marks anything failed or reschedules it anywhere.

**Whether a hub may write at all is your decision, and you make it with a token.** Install a
`read_only: true` token for a node and that node refuses every write from the hub, whatever the hub is
asked to do:

```console
$ processd run sleeper --node app-02
error: read_only_token: token "hub" is read-only and may not POST
```

The `Idempotency-Key` is the client's, forwarded as sent, not one the hub invents: the hub answers
`dispatch_unknown` and stops, so the client is the one that retries and the key has to be stable
across its retries.

**Distributed scheduling is not part of this and is not coming** — see the roadmap.

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

## Security

Processes are started with `exec.Command(cmd, args...)`, never `sh -c`. In the default
`execution_mode: workers` only pre-configured workers run, and arguments reach them only through
params validated by regex or enum. Token authentication is mandatory, the default bind is
`127.0.0.1`, the child environment is built rather than inherited, every process gets its own
process group, and nothing runs as root without an explicit opt-in.

## Design principles

* **Simple by default** — no Redis, Kafka or Kubernetes.
* **API-first** — the CLI is a client of the same public API.
* **Single binary** — installing is copying one file.
* **Process-first** — the abstraction is a process and its execution, not a container.
* **Fail closed** — when in doubt, refuse: no token, no `user` while running as root, or a param
  outside its declared pattern, and nothing runs.
* **Linux-first** — it depends on POSIX process groups, signals and `/proc`.

## Roadmap

| Phase | Delivery | Needs |
|---|---|---|
| 1 ✅ | local process manager: API, states, persistence, auth | — |
| 2 ✅ | supervisor: queue, locks, retry/backoff, timeout, recovery | — |
| 3 ✅ | observability: metrics, log streaming, CPU/memory, web console | — |
| 4 ✅ | `type: service`: continuous restart and log rotation | — |
| 5 ✅ | scheduled triggers: `schedule` on a worker, a visible next run, an overlap policy | — |
| 6 ✅ | failure notifications: a webhook or a worker call on failure, crash or exhausted retries | — |
| 7 ✅ | fleet view: read-only aggregation of several nodes, one console, log streams proxied | — |
| 8 ✅ | explicit remote dispatch: start an execution on a node the client names | 7 |

Phases 5 and 6 are local: they need no second machine and change nothing about how a node is
addressed. Phase 7 only reads, so a hub that is down leaves every node running exactly as it was, and
phase 8 writes only to the node a client named.

**Distributed scheduling is a non-goal, not a later phase.** Automatic placement, least-loaded,
constraints, replicas, failover between nodes and distributed locks sit next to Raft in
[`docs/SPEC.md`](docs/SPEC.md) §2.2 — a decision, not a backlog item. The reasoning is in §22.

---

## Development

Requires Go 1.25+ and Linux.

```bash
make install-tools     # golangci-lint and govulncheck
make build             # bin/processd
make test              # go test ./...
make test-race         # with the race detector
make test-integration  # end to end: real daemons, real processes
make lint              # golangci-lint
make fmt               # formatting
make audit             # govulncheck
make release-check     # validates .goreleaser.yml
make release-snapshot  # archives, packages and SBOMs into dist/, nothing published
```

Layout:

```
cmd/processd/        entry point
internal/cli/        cobra command tree, client of its own API
internal/daemon/     object graph and process lifecycle
internal/api/        HTTP handlers, auth, error contract
internal/core/       domain types and state machine, no I/O
internal/config/     daemon configuration and worker definitions
internal/queue/      admission, slots, backoff
internal/supervisor/ per-execution supervision
internal/runner/     exec, process groups, signals, /proc
internal/logstore/   per-attempt log files, tail and follow
internal/metrics/    Prometheus text-format counters and histograms
internal/webui/      embedded web console (go:embed)
internal/store/      persistence interface + SQLite
```

Releases are automatic: pushing a `v*` tag triggers
[`.github/workflows/release.yml`](.github/workflows/release.yml), which runs the tests and
GoReleaser, then publishes the archives, the `.deb`/`.rpm`/`.apk` packages, one SPDX SBOM per
artifact, `checksums.txt` and its cosign signature.

```bash
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

Configuration and systemd examples live in [`examples/`](examples/).

## License

[Apache 2.0](LICENSE)
