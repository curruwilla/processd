# Getting started

From nothing to a supervised process in four steps. Each step links to the page that explains it in
full; nothing here is a summary you will have to unlearn later.

Processd is **Linux-only** and needs a system it can start processes on: it depends on POSIX process
groups, signals and `/proc`.

## 1. Install the binary

```bash
VERSION=0.1.0                               # github.com/curruwilla/processd/releases
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/processd_${VERSION}_linux_${ARCH}.tar.gz"
tar -xzf "processd_${VERSION}_linux_${ARCH}.tar.gz" processd
sudo install -m 0755 processd /usr/local/bin/processd
```

Distro packages, building from source, checksums, cosign signatures and SBOMs are all in
[Installation](installation.md).

## 2. Prepare the node

```bash
sudo processd setup
```

One command delivers the whole node: directories, `/etc/processd/processd.yaml`, an API token, the
systemd unit pointing at the binary that ran it, and a report of every path and address it used.

```bash
systemctl status processd
processd status
journalctl -u processd -f
```

Client commands read `/etc/processd/token` when neither `--token` nor `PROCESSD_TOKEN` is set, so as
root on the node you never paste the secret. The flags setup accepts — `--dry-run`, `--rotate-token`,
`--listen`, `--systemd=false`, `--start=false`, `--output json` — and the by-hand alternative are in
[Installation](installation.md#prepare-the-node).

## 3. Declare a worker

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

Every field is in [Workers](workers.md); the retry block is in [Retry and backoff](retry.md). A
fuller version of this file, commented line by line, is
[`examples/workers.d/invoice.yaml`](../examples/workers.d/invoice.yaml).

## 4. Run it

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
Prometheus metrics are at `/v1/metrics`. Both are covered in [Monitoring](monitoring.md).

## Where to go next

* Something that must not exit → [Services](services.md), and
  [`examples/workers.d/api.yaml`](../examples/workers.d/api.yaml).
* Something that runs at 03:00 → [Scheduling](scheduling.md), and
  [`examples/workers.d/nightly-report.yaml`](../examples/workers.d/nightly-report.yaml).
* Being told when it breaks → [Notifications](notifications.md), and
  [`examples/workers.d/notify-slack.yaml`](../examples/workers.d/notify-slack.yaml).
* Day-to-day commands → [Operations](operations.md).
* Exposing the node to anything but localhost → [Security](security.md).

---

[Documentation index](README.md) · [Installation](installation.md) · [Workers](workers.md)
