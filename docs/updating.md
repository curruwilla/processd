# Updating

`processd setup` is **not** the update command: `systemctl enable --now` does not restart a service
that is already running, so a new binary would sit on disk unused. Setup is for the first install,
and for when the bind address, the unit path or the binary path changed.

Editing workers needs none of this — that is `processd reload`, in
[Operations](operations.md#changing-a-worker-or-a-service).

## The procedure

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

## What to know before you do it

* **Install the CLI and the daemon from the same build.** They are the same binary; a client older
  than the daemon simply lacks the newer commands, which is confusing to debug.
* **Migrations run automatically** at startup, once each, in filename order. There is no downgrade
  path once one has run — that is what step 2 is for.
* **Services with `retry.on_shutdown: true`** are returned to the queue at shutdown and come back by
  themselves on the next start, keeping their id. Without it they end as `CANCELED` and need
  `processd run`. See [Services](services.md#across-a-daemon-restart).
* **Tasks in flight** get `shutdown_grace` (default `30s`) to finish; whatever is left is killed. The
  generated unit sets `TimeoutStopSec` to that plus 15s, so systemd never cuts the daemon off in the
  middle of a graceful shutdown. A hand-written unit has to do the same — see
  [`examples/processd.service`](../examples/processd.service).
* **A new `processd.yaml` key takes effect on this restart too**, since the daemon reads its
  configuration only at startup.

## Verifying what you are installing

Checksums, cosign signatures and per-artifact SBOMs are published with every release —
[Installation → Verifying signatures and SBOMs](installation.md#verifying-signatures-and-sboms).

---

[Documentation index](README.md) · [Installation](installation.md) · [Operations](operations.md) ·
[Services](services.md)
