# CLI

One binary. The CLI is a client of the same public [HTTP API](api.md) — there is no private channel
between them, so anything the CLI does can be done with `curl`.

## Configuring the client

| Flag | Variable | Default |
|---|---|---|
| `--server` | `PROCESSD_SERVER` | `http://127.0.0.1:7373` |
| `--token` | `PROCESSD_TOKEN` | the file written by `processd setup`, next to the configuration |
| `--log-level` | `PROCESSD_LOG_LEVEL` | |

Every persistent flag has a `PROCESSD_`-prefixed variable. Without an explicit token the client reads
`/etc/processd/token`, so as root on the node you never paste the secret.

```bash
export PROCESSD_SERVER=http://127.0.0.1:7373
export PROCESSD_TOKEN=...
```

## Commands

| Command | What it does |
|---|---|
| `processd setup [--dry-run] [--rotate-token] [--listen addr] [--systemd=false] [--start=false] [--output json]` | installs the node: directories, configuration, token, systemd unit, and prints all of it — see [Installation](installation.md#prepare-the-node) |
| `processd serve --config <path>` | runs the daemon |
| `processd status` | health, version, slots, running and queued |
| `processd ps [--status S] [--type task\|service] [--worker w] [--node n\|*] [--limit n] [--cursor c] [--output table\|json]` | lists executions; `--node` reads a [fleet](fleet.md) node instead of this one |
| `processd run <worker> [--param name=value] [--lock k] [--node n]` | creates an execution; `--node` runs it on that fleet node |
| `processd logs <id> [--stream stdout\|stderr\|both] [--attempt n] [--tail n] [-f] [--node n]` | captured output, `-f` streams it |
| `processd stop <id> [--grace 15s] [--node n]` | `SIGTERM` to the group, `SIGKILL` after the grace. It returns as soon as the daemon accepts the stop; the grace runs on the node, so interrupting the command does not cut it short. `processd ps` shows the outcome |
| `processd restart <id> [--grace 15s] [--param n=v] [--wait 1m] [--node n]` | stops it and creates a new execution from the current worker definition |
| `processd signal <id> <SIGNAL> [--node n]` | sends an allowlisted signal to the group |
| `processd workers` | loaded workers, with their declared params, cron expression and next run |
| `processd fleet [--output table\|json]` | on a hub: every node it reads, with the reason for any that is unreachable |
| `processd reload` | re-reads `workers.d` |
| `processd token hash` | reads a token from stdin and prints the configuration digest |

## Signals

Accepted: `SIGTERM`, `SIGINT`, `SIGQUIT`, `SIGHUP`, `SIGUSR1`, `SIGUSR2`, `SIGKILL`, `SIGSTOP`,
`SIGCONT`. Anything else is refused with `400`.

Every signal reaches the whole **process group** — signalling only the PID would leave grandchildren
alive.

```bash
processd signal proc_01K... SIGHUP        # tell a service to reload its own config
```

## Cheat sheet

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

Grace and timeout values passed over the wire — `--grace`, and a `timeout` override on `run` — are
**Go duration syntax only**, with no `d` or `w`. See
[Configuration → Value syntax](configuration.md#value-syntax).

Same client, one machine over: add `--node` to reach a node through a hub — [Fleet](fleet.md).

---

[Documentation index](README.md) · [HTTP API](api.md) · [Operations](operations.md) ·
[Fleet](fleet.md)
