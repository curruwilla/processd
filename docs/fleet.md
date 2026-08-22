# Fleet

A **hub** is an ordinary processd that also reads other nodes. It aggregates their read API and
proxies reads to them, and it **never writes to one** unless a client names the node itself.

A hub reads; it never decides. Distributed scheduling — placement, replicas, failover — is a declared
non-goal, not a later phase.

## Configuring a hub

```yaml
# /etc/processd/processd.yaml, on the hub only
fleet:
  poll_interval: 10s
  nodes:
    - name: app-01
      url: https://10.0.0.11:7373
      token_file: /etc/processd/nodes/app-01.token
    - name: app-02
      url: https://10.0.0.12:7373
      token_file: /etc/processd/nodes/app-02.token
```

```bash
processd fleet                    # every node: reachable, version, slots, running, queued, last seen
processd ps --node app-01         # that node's executions, with its own paging
processd ps --node '*'            # newest across every node, merged
```

The console gains a **Fleet** tab and a node selector on the processes view, both of which stay
hidden on a daemon that aggregates nothing.

**Nothing is configured on the node.** The hub polls; there is no registration protocol and no
hub-side state on the node, so a node does not know it is being read and needs no change to join a
fleet. That simplification is only available because the aggregation never writes.

Key reference: [Configuration](configuration.md#daemon-keys). A commented block is in
[`examples/processd.yaml`](../examples/processd.yaml).

## Endpoints

| Endpoint | Notes |
|---|---|
| `GET /v1/fleet/nodes` | the last poll of every node: `reachable`, `version`, `last_seen`, `stats`, and `error` when it is not answering |
| `GET /v1/fleet/nodes/{node}/{path...}` | proxies any read to that node's `/v1/{path}`, including `logs/stream`. Streams as it arrives; a path that climbs out of `/v1` is refused |
| `GET /v1/processes?node=app-01` | that node's listing, with its own cursor |
| `GET /v1/processes?node=*` | newest first across every node, each row tagged with `node` |
| `POST /v1/processes` with `"node"` | dispatches to that node; the answer is the node's own, plus `X-Processd-Node`. `504 dispatch_unknown` when the node did not answer |
| `POST`/`DELETE /v1/fleet/nodes/{node}/{path...}` | forwards a write to the node the client named. The node applies its own rules to the hub's token, so a `read_only` one refuses it |

On a daemon with no `fleet`, those routes **do not exist** — the absence is the statement that there
is no fleet, rather than an empty list that looks like a broken one.

## Things worth knowing before relying on it

* **A hub being down changes nothing.** Every node keeps running, supervising and retrying exactly as
  it was. The worst case is a stale panel.
* **The token is the hub's, never the caller's.** A client authenticates to the hub; the hub
  authenticates to each node with the token from `token_file`, which it re-reads on every poll — so
  rotating a node's token takes effect within one interval, not at the next restart. Give the hub a
  `read_only: true` token, and the node will refuse a write even if the hub is ever asked to make one.
* **One unreachable node degrades the answer, never fails it.** A merged listing returns what the
  live nodes had and names the ones that did not answer in `unreachable`; a node status keeps its last
  known numbers next to the reason it is now unreachable.
* **A merged page has no cursor.** There is no ordering across nodes to page through, and inventing
  one would be a distributed index nobody asked for. Page deeper by naming a single node.
* **Version skew is expected.** Node answers are decoded loosely, so a counter or a field from a
  newer node is passed through rather than dropped.
* **Logs are proxied, never copied.** The output stays on the node that produced it.

## Running work on a node

Every command that acts on one execution takes `--node`, and the hub forwards it to the node that was
named. **The client chooses the node; the hub never does.**

```bash
processd run sleeper --node app-01
processd logs <id> --node app-01 -f
processd signal <id> SIGUSR1 --node app-01
processd stop <id> --node app-01
processd restart <id> --node app-01
```

Over the API, that is `POST /v1/processes` with a `node` field, or any method against
`/v1/fleet/nodes/{node}/{path...}`. The `node` field is stripped before the body reaches the node,
which has no idea what a fleet is.

Three things make this a router and not a scheduler:

* **The execution lives only on the node.** The hub keeps no copy of what it forwarded, so its own
  `processd ps` stays empty. There is nothing to reconcile, and therefore nothing to reconcile wrongly.
* **`node` is never inferred.** Without it the execution is local and explicit; the hub does not
  "pick one".
* **Silence is not a failure.** A node that did not answer has not said no — the execution may be
  running right now. The hub answers `504` with `dispatch_unknown` and tells you to retry with the
  same `Idempotency-Key`, and it never marks anything failed or reschedules it anywhere.

## Keeping a hub read-only

**Whether a hub may write at all is your decision, and you make it with a token.** Install a
`read_only: true` token for a node and that node refuses every write from the hub, whatever the hub is
asked to do:

```console
$ processd run sleeper --node app-02
error: read_only_token: token "hub" is read-only and may not POST
```

The `Idempotency-Key` is the client's, forwarded as sent, not one the hub invents: the hub answers
`dispatch_unknown` and stops, so the client is the one that retries and the key has to be stable
across its retries. See [HTTP API → Idempotency](api.md#idempotency).

---

[Documentation index](README.md) · [Configuration](configuration.md) · [CLI](cli.md) ·
[HTTP API](api.md) · [Security](security.md)
