# Processd documentation

Processd runs and supervises CLI processes through a REST API: one binary, one configuration file,
one API. These pages split what used to be a single README into one page per part of the program.

## Start here

| Page | What it covers |
|---|---|
| [Getting started](getting-started.md) | install, prepare a node, declare the first worker, run it |
| [Installation](installation.md) | binary, distro packages, from source, `processd setup`, manual setup, signature and SBOM verification |
| [Examples](examples.md) | the files in [`examples/`](../examples/), annotated and cross-referenced |

## Declaring work

| Page | What it covers |
|---|---|
| [Configuration](configuration.md) | value syntax (durations, byte sizes, attempts) and every `processd.yaml` key |
| [Workers](workers.md) | worker files, every field, and `params` — the only way an argument reaches a process |
| [Retry and backoff](retry.md) | when an attempt repeats, how long it waits, and when it stops |
| [Services](services.md) | `type: service`: what changes, how to run, change and stop one |
| [Scheduling](scheduling.md) | `schedule:` — cron syntax, timezones, catch-up, overlap, DST |
| [Notifications](notifications.md) | `notify:` — webhook and worker delivery when an execution ends badly |

## Interfaces

| Page | What it covers |
|---|---|
| [CLI](cli.md) | every command, its flags and its environment variables |
| [HTTP API](api.md) | endpoints, the error contract and every status code |
| [Lifecycle](lifecycle.md) | the execution state machine and the reasons a state carries |
| [Fleet](fleet.md) | a hub that reads other nodes, and explicit dispatch to a node you name |

## Running a node

| Page | What it covers |
|---|---|
| [Operations](operations.md) | command cheat sheet, adding and changing workers, backup, retention, runbook |
| [Updating](updating.md) | replacing the binary without losing state |
| [Monitoring](monitoring.md) | health, stats, Prometheus metrics, logs, the web console |
| [Security](security.md) | tokens, scopes, TLS, root, `raw` mode |

## Contributing

| Page | What it covers |
|---|---|
| [Development](development.md) | build, test, lint, layout, release process |

---

Back to the [project README](../README.md) · also available in
[Brazilian Portuguese](../README.pt-BR.md).
