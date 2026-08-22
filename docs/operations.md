# Operations

Day-to-day work on a running node: adding workers, changing them, and knowing what to type when
something is wrong.

## Command cheat sheet

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

Every flag is in [CLI](cli.md).

## Adding a worker or a service

1. **Write the file** in `/etc/processd/workers.d/`. The loader validates the shape of a definition,
   not the host it will run on, so check these yourself — otherwise the reload succeeds and the first
   run fails:
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

Field reference: [Workers](workers.md). Ready-made files: [Examples](examples.md).

## Changing a worker or a service

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

Changing `processd.yaml` itself is different: it is read at startup only, so it takes a
`sudo systemctl restart processd` — see [Configuration](configuration.md#applying-a-change).

## Backup and retention

| What | Where | Matters because |
|---|---|---|
| state | `/var/lib/processd` (SQLite) | executions, history, locks, idempotency keys |
| configuration and token | `/etc/processd` | every configured client authenticates against it |
| output | `/var/log/processd` | per-attempt stdout and stderr |

Back the state up with the daemon stopped, so the database file is consistent — and always before an
upgrade. Retention is enforced by the daemon itself: `history.retention` and `history.max_rows` for
executions, `logs.retention` for output files.

## Runbook

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

Replacing the binary has its own page: [Updating](updating.md).

---

[Documentation index](README.md) · [Workers](workers.md) · [Services](services.md) ·
[Monitoring](monitoring.md) · [Updating](updating.md) · [Security](security.md)
