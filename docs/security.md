# Security

The design rule is **fail closed**: no token, no `user` while running as root, or a param outside its
declared pattern, and nothing runs.

## What the daemon does by default

* Processes are started with `exec.Command(cmd, args...)`, **never** `sh -c`.
* In the default `execution_mode: workers` only pre-configured workers run, and arguments reach them
  only through [params](workers.md#params) validated by regex or enum.
* Token authentication is mandatory on every endpoint except `GET /v1/health`.
* The default bind is `127.0.0.1`.
* The child environment is **built, not inherited** — the daemon environment holds secrets.
* Every process gets its own process group, and all signalling targets the group.
* Nothing runs as root without an explicit opt-in.

## There is no native TLS

The default bind is `127.0.0.1`; put a reverse proxy (nginx, Caddy, Traefik) in front of it for
anything remote. A hub reaching nodes over `https://` is the same arrangement seen from the other
side — see [Fleet](fleet.md).

## Tokens

Every request needs one. Give each client its own, scoped to the workers it may run:

```yaml
auth:
  tokens:
    - name: billing-cron
      hash: "sha256:..."      # printf '%s' "$TOKEN" | processd token hash
      workers: ["invoice-process"]
    - name: ops
      hash: "sha256:..."      # no workers key: every worker
    - name: hub
      hash: "sha256:..."
      read_only: true         # refuses every state-changing call
```

The configuration stores **digests, never secrets**. The token name is what shows up in the audit
trail.

`read_only: true` refuses every state-changing call with that token. It is what a hub uses to read a
node, and what a dashboard should be given.

### Rotating

```bash
sudo processd setup --rotate-token
sudo systemctl restart processd
```

The daemon reads authentication **only at startup**, so the restart is not optional. On a hub the
node tokens are different: `token_file` is re-read on every poll, so rotating a node's token takes
effect within one `fleet.poll_interval`.

## Two settings to leave alone

* **Keep `execution_mode: workers`** (the default). The `raw` mode lets a client choose the command
  and is remote code execution by design; it also requires an explicit `allowed_commands` allowlist.
* **Keep `allow_root_processes: false`.** With it off, a worker without `user` is refused instead of
  silently running as root.

## What leaves the node

Nothing, unless you configure it. The one place data can leave is a
[notification webhook](notifications.md#payload), and its payload carries identity, outcome, timing
and client `metadata` — no environment, no command, no argument list. `include_log_tail` is off by
default and opt-in on purpose: logs carry secrets far more often than anyone intends.

## Signals

Only `SIGTERM`, `SIGINT`, `SIGQUIT`, `SIGHUP`, `SIGUSR1`, `SIGUSR2`, `SIGKILL`, `SIGSTOP` and
`SIGCONT` are accepted; anything else is refused with `400`. Every signal reaches the whole process
group — signalling only the PID would leave grandchildren alive.

## Supply chain

Release artifacts ship checksums, a cosign signature bound to the publishing workflow's identity, and
one SPDX SBOM per artifact — [Installation](installation.md#verifying-signatures-and-sboms).

---

[Documentation index](README.md) · [Configuration](configuration.md) · [Workers](workers.md) ·
[Fleet](fleet.md) · [HTTP API](api.md)
