# Examples

Everything in [`examples/`](../examples/), what each file demonstrates, and where the reference for
it lives. The files are commented line by line and are meant to be copied and edited, not read once.

A distro package installs them at `/usr/share/doc/processd/examples/`.

## Node

| File | Demonstrates | Reference |
|---|---|---|
| [`examples/processd.yaml`](../examples/processd.yaml) | every daemon key with its default and the reason behind it — limits, queue, history, logs, the commented-out `notify` and `fleet` blocks, and an `auth.tokens` list with a scoped token and a `read_only` hub token | [Configuration](configuration.md), [Security](security.md), [Fleet](fleet.md) |
| [`examples/processd.service`](../examples/processd.service) | a systemd unit written by hand: `TimeoutStopSec` above `shutdown_grace`, `KillMode=mixed` so systemd signals the daemon and not the process groups it owns, an `LimitNOFILE` budget of about five descriptors per execution, and why the daemon starts as root | [Installation](installation.md#configuring-a-node-by-hand), [Updating](updating.md) |

## Workers

| File | Demonstrates | Reference |
|---|---|---|
| [`examples/workers.d/invoice.yaml`](../examples/workers.d/invoice.yaml) | a **task** in full: `params` validated by `pattern` and `enum` with a default, `{{id}}` substituted inside an argv element, a built environment plus `env_passthrough`, `timeout`, a per-worker `max_processes`, a parameterised `lock`, and a complete `retry` block | [Workers](workers.md), [Retry and backoff](retry.md) |
| [`examples/workers.d/api.yaml`](../examples/workers.d/api.yaml) | a **service**: no `timeout`, restart defaults left alone, `no_retry_exit_codes: [78]` for a configuration error, `reset_after` so a long uptime does not carry old restarts, `on_shutdown: true`, and the mandatory log rotation | [Services](services.md) |
| [`examples/workers.d/nightly-report.yaml`](../examples/workers.d/nightly-report.yaml) | a **scheduled** task: a five-field `cron` with an explicit `timezone`, `catch_up: false`, `jitter` to spread a fleet, `lock_conflict: reject` as the overlap policy, and a `notify` block with both a webhook and an `exec.worker` | [Scheduling](scheduling.md), [Notifications](notifications.md) |
| [`examples/workers.d/notify-slack.yaml`](../examples/workers.d/notify-slack.yaml) | a **notification target**: the full list of values a notification can offer, declared as ordinary params, and the rule that such a worker must not declare `notify` of its own | [Notifications](notifications.md#execworker) |

## Using them

```bash
sudo cp examples/workers.d/invoice.yaml /etc/processd/workers.d/
sudo $EDITOR /etc/processd/workers.d/invoice.yaml     # command, cwd, user are host-specific
sudo processd reload
processd workers
```

The loader validates the shape of a definition, not the host it will run on. Before the first run,
check that `command` exists here, that `cwd` exists, and that the `user` does — see
[Operations](operations.md#adding-a-worker-or-a-service).

---

[Documentation index](README.md) · [Getting started](getting-started.md) · [Workers](workers.md) ·
[Configuration](configuration.md)
