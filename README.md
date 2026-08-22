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

## Install

Linux, one binary:

```bash
VERSION=0.1.0                               # github.com/curruwilla/processd/releases
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/processd_${VERSION}_linux_${ARCH}.tar.gz"
tar -xzf "processd_${VERSION}_linux_${ARCH}.tar.gz" processd
sudo install -m 0755 processd /usr/local/bin/processd

sudo processd setup                         # directories, config, token, systemd unit
```

Then declare a worker and run it — the whole first pass is in
**[Getting started](docs/getting-started.md)**. Distro packages, building from source, checksum and
signature verification: [Installation](docs/installation.md).

## Documentation

Everything lives in [`docs/`](docs/README.md), one page per part of the program.

| | |
|---|---|
| **Start here** | [Getting started](docs/getting-started.md) · [Installation](docs/installation.md) · [Examples](docs/examples.md) |
| **Declaring work** | [Configuration](docs/configuration.md) · [Workers](docs/workers.md) · [Retry and backoff](docs/retry.md) · [Services](docs/services.md) · [Scheduling](docs/scheduling.md) · [Notifications](docs/notifications.md) |
| **Interfaces** | [CLI](docs/cli.md) · [HTTP API](docs/api.md) · [Lifecycle](docs/lifecycle.md) · [Fleet](docs/fleet.md) |
| **Running a node** | [Operations](docs/operations.md) · [Updating](docs/updating.md) · [Monitoring](docs/monitoring.md) · [Security](docs/security.md) |
| **Contributing** | [Development](docs/development.md) |

Ready-to-copy configuration, systemd unit and worker files are in [`examples/`](examples/), annotated
in [Examples](docs/examples.md).

## Design principles

* **Simple by default** — no Redis, Kafka or Kubernetes.
* **API-first** — the CLI is a client of the same public API.
* **Single binary** — installing is copying one file.
* **Process-first** — the abstraction is a process and its execution, not a container.
* **Fail closed** — when in doubt, refuse: no token, no `user` while running as root, or a param
  outside its declared pattern, and nothing runs.
* **Linux-first** — it depends on POSIX process groups, signals and `/proc`.

## Contributing

Go 1.25+ and Linux. `make build`, `make test`, `make lint` — the rest is in
[Development](docs/development.md).

## License

[Apache 2.0](LICENSE)
