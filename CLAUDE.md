# Processd

Lightweight process manager written in Go: runs and supervises CLI processes through a REST API.
The specification in `docs/SPEC.md` is the authority — when code and spec disagree, one of them is a
bug, so fix both in the same change.

## Language

This is a global open-source project. **Every artifact is written in English**: code, identifiers,
comments, commit messages, PR and issue titles, API fields, config keys, state names, log and error
messages, and the README. `docs/SPEC.md` is currently a PT-BR working document; the English version
will replace it and become canonical.

## Go development

Before any Go coding, review, debugging, troubleshooting, or setup task, load the `samber/cc-skills-golang@golang-how-to` skill first — it routes to whichever other Go skills the task needs.

## Commands

```bash
make build        # build bin/processd with version metadata
make test         # go test ./...
make test-race    # go test -race ./...
make lint         # golangci-lint run ./...
make fmt          # golangci-lint fmt ./...
make audit        # govulncheck ./...
make install-tools
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
internal/metrics/    Prometheus text-format counters and histograms
internal/webui/      embedded web console (go:embed)
internal/store/      persistence interface + SQLite implementation
```

## Invariants

- **Fail closed.** An unknown config key, an undeclared param, a signal outside the allowlist, or a
  missing `user` while running as root must all be refused, never defaulted.
- **Arguments reach a process only through declared, validated params.** Substitution never splits an
  argv element, and commands never run through a shell.
- **The child environment is built, not inherited** — the daemon environment holds secrets.
- **Every process gets its own process group**, and all signalling targets the group.
- **A stored PID is only usable together with its `/proc` start time**; PIDs are recycled.
- **The store is the source of truth**; in-memory state is a cache rebuilt on startup.
- **State transitions go through the table in `internal/core/state.go`** — an undefined edge is a bug
  to report, not to silently allow.
