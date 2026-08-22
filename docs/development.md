# Development

Requires **Go 1.25+** and **Linux**. 

## Commands

```bash
make install-tools     # golangci-lint and govulncheck
make build             # bin/processd, with version metadata
make test              # go test ./...
make test-race         # with the race detector
make test-integration  # end to end: real daemons, real processes
make cover             # coverage profile
make lint              # golangci-lint run ./...
make lint-fix          # golangci-lint run --fix ./...
make fmt               # golangci-lint fmt ./...
make vet               # go vet ./...
make audit             # govulncheck ./...
make tidy              # go mod tidy
make release-check     # validates .goreleaser.yml
make release-snapshot  # archives, packages and SBOMs into dist/, nothing published
make release-docker    # builds the container image locally
```

## Layout

```
cmd/processd/        entry point, delegates to internal/cli
internal/cli/        cobra command tree; every client command uses the public REST API
internal/daemon/     object graph and process lifecycle (manual constructor injection)
internal/api/        HTTP handlers, auth, error contract
internal/core/       domain types, lifecycle state machine; no I/O
internal/config/     daemon config and worker definitions, strict YAML decoding
internal/queue/      admission, slots, backoff
internal/supervisor/ per-execution supervision
internal/runner/     exec, process groups, signals, /proc fingerprinting (Linux only)
internal/logstore/   per-attempt capped log files, tail and follow
internal/cron/       five-field cron parsing and Next(); no goroutine, no I/O
internal/schedule/   the firing loop for `schedule:` workers
internal/notify/     outbound failure notifications: webhook and worker
internal/fleet/      reads other nodes: poller, proxy; never writes
internal/metrics/    Prometheus text-format counters and histograms
internal/webui/      embedded web console (go:embed)
internal/store/      persistence interface + SQLite implementation
```

## Invariants

These are not style preferences; a change that breaks one is a bug.

* **Fail closed.** An unknown config key, an undeclared param, a signal outside the allowlist, or a
  missing `user` while running as root must all be refused, never defaulted.
* **Arguments reach a process only through declared, validated params.** Substitution never splits an
  argv element, and commands never run through a shell.
* **The child environment is built, not inherited** — the daemon environment holds secrets.
* **Every process gets its own process group**, and all signalling targets the group.
* **A stored PID is only usable together with its `/proc` start time**; PIDs are recycled.
* **The store is the source of truth**; in-memory state is a cache rebuilt on startup.
* **State transitions go through the table in `internal/core/state.go`** — an undefined edge is a bug
  to report, not to silently allow.
* **A hub reads; it never decides.** It aggregates and proxies, keeps no authoritative copy of a
  remote execution, and dispatches only to the node the client named.
* **A node that did not answer has not said no.** Silence from a remote node is reported as unknown,
  never as a failure, and is never a reason to reschedule anything.

---

[Documentation index](README.md) · [Examples](examples.md)
