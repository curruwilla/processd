# Installation

One binary. Installing is copying one file; everything else — directories, configuration, token,
systemd unit — is what [`processd setup`](#prepare-the-node) does in a single command.

Requirements: **Linux**, and a system path to install into. Building from source needs **Go 1.25+**.

## Install the binary

```bash
VERSION=0.1.0                               # github.com/curruwilla/processd/releases
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/processd_${VERSION}_linux_${ARCH}.tar.gz"
tar -xzf "processd_${VERSION}_linux_${ARCH}.tar.gz" processd
sudo install -m 0755 processd /usr/local/bin/processd
```

Every release also publishes `checksums.txt`:

```bash
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/checksums.txt"
sha256sum --check --ignore-missing checksums.txt
```

### Distro package

`.deb`, `.rpm` and `.apk`, for `amd64` and `arm64`:

```bash
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/processd_${VERSION}_linux_${ARCH}.deb"
sudo dpkg -i "processd_${VERSION}_linux_${ARCH}.deb"        # or: sudo rpm -i ...rpm
```

The package installs the binary at `/usr/bin/processd` and the examples at
`/usr/share/doc/processd/examples/`. Configuration, token and systemd unit stay with
`processd setup`.

### From source

```bash
git clone https://github.com/curruwilla/processd && cd processd
make build                                  # bin/processd, CGO-free
sudo install -m 0755 bin/processd /usr/local/bin/processd
```

Install the binary in a system path — `/usr/local/bin` or `/usr/bin`. The systemd unit points at the
binary that ran `processd setup`, so a unit aimed at a build tree breaks the moment that tree is
rebuilt or moved. The full build, test and lint targets are in [Development](development.md).

## Prepare the node

```bash
sudo processd setup
```

One command delivers the whole node:

* creates `/etc/processd/workers.d`, `/var/lib/processd` and `/var/log/processd`;
* writes `/etc/processd/processd.yaml`;
* mints an API token, stored as a digest in the configuration and in plain text at
  `/etc/processd/token` (mode `0600`, owned by root);
* installs `/etc/systemd/system/processd.service`, pointing at the binary that ran it, and starts it;
* prints the token and every path, address and check command.

Client commands read `/etc/processd/token` when neither `--token` nor `PROCESSD_TOKEN` is set, so as
root on the node you never paste the secret. Running setup again keeps the installed token instead of
invalidating every configured client; `--rotate-token` replaces it on purpose.

| Flag | Effect |
|---|---|
| `--dry-run` | reports what it would do without touching anything |
| `--rotate-token` | replaces the installed token instead of keeping it |
| `--listen addr` | changes the bind address written to the configuration |
| `--systemd=false` | skips the unit |
| `--start=false` | installs and enables without starting |
| `--output json` | returns the same report for scripts |

The configuration is rewritten from the values the daemon parsed — comments are lost, keys are
reordered — and the previous file is kept next to it as `processd.yaml.bak-<timestamp>`.

Check it:

```bash
systemctl status processd
processd status
journalctl -u processd -f
```

`processd setup` is **not** the update command — see [Updating](updating.md).

## Configuring a node by hand

Everything setup does is ordinary files, so it can all be done without it.

```bash
sudo mkdir -p /etc/processd/workers.d /var/lib/processd /var/log/processd
TOKEN=$(openssl rand -hex 32)
printf '%s' "$TOKEN" | processd token hash   # prints sha256:...
```

Only the digest goes in the file. The token is read from stdin so it appears neither in the process
list nor in the shell history.

```yaml
# /etc/processd/processd.yaml
listen: 127.0.0.1:7373
data_dir: /var/lib/processd
log_dir: /var/log/processd
workers_dir: /etc/processd/workers.d

max_processes: 50

auth:
  tokens:
    - name: dev
      hash: "sha256:<paste the digest here>"
```

Every key is in [Configuration](configuration.md); a fully commented file is
[`examples/processd.yaml`](../examples/processd.yaml).

Run it in the foreground with `processd serve --config /etc/processd/processd.yaml`, or copy
[`examples/processd.service`](../examples/processd.service) to `/etc/systemd/system/`. Whatever unit
you write, `TimeoutStopSec` has to exceed `shutdown_grace`, otherwise systemd kills the daemon in the
middle of stopping the process groups it supervises.

## Verifying signatures and SBOMs

`checksums.txt` is signed with [cosign](https://docs.sigstore.dev/) in keyless mode: the signature is
bound to the identity of the workflow that published the release, not to a private key. Verifying
needs cosign v2+:

```bash
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/checksums.txt"
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/checksums.txt.sigstore.json"

cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/curruwilla/processd/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  checksums.txt
```

With `checksums.txt` verified, the `sha256sum --check` above covers every binary and package.

Each `.tar.gz`, `.deb`, `.rpm` and `.apk` ships an SPDX SBOM next to it (`<artifact>.spdx.json`):
every Go module that went into the binary, with its version, so "is this install affected by that
CVE?" can be answered without rebuilding anything.

```bash
jq -r '.packages[] | "\(.name) \(.versionInfo)"' "processd_${VERSION}_linux_${ARCH}.tar.gz.spdx.json"
```

---

[Documentation index](README.md) · [Getting started](getting-started.md) ·
[Configuration](configuration.md) · [Updating](updating.md)
