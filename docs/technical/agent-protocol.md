# Agent protocol

Code: `internal/agentauth` (keys, signatures, messages), `internal/agents`
(the manager's side), `internal/agent` (agent mode), `internal/store/agents.go`.

Agents call the manager; the manager never calls an agent. All agent routes
are on the manager under `/api/agent/v1/`, at the same address as the web UI.
They are outside session authentication, CSRF and the setup redirect.
Go's cross-origin protection lets them through, because agents send no
`Origin` or `Sec-Fetch-Site` header. Every route except `enroll` needs an
agent signature.

## Keys

Ed25519, encoded as:

| Kind | Format |
| --- | --- |
| Public key | `ed25519:` + unpadded base64url of the 32-byte key |
| Private key (agent's `key` file) | `ed25519-private:` + unpadded base64url of the 32-byte seed |
| Fingerprint, for display | The first 16 hex digits of the public key's SHA-256 |

The **manager keys** are kept in the `manager_keys` setting:
`{"current": "<id>", "keys": {"<id>": "<sealed private key>"}}`, sealed with
the instance key (`secret.key`). IDs are `k1`, `k2`, and so on. The first one
is made when the manager starts. Each agent host records the ID it has
pinned (`hosts.manager_key_id`), and commands to it are signed with that
key.

## Requests (agent to manager)

Every request has:

```
Authorization: Drawbridge-Agent host=<id>, ts=<unix seconds>, nonce=<16 random bytes, base64url>, sig=<base64url>
```

`sig` is the agent's signature over this text, lines joined with `\n`:

```
<method>
<path and query, as sent>
<host id>
<ts>
<nonce>
<hex SHA-256 of the body>
```

The manager refuses with `401` and `WWW-Authenticate: Drawbridge-Agent`
unless all of these hold:

1. The header parses: each of the four fields exactly once.
2. The host exists, is an agent host, and has a key (not revoked, not
   waiting to enroll). A disabled host gets `403 this host is disabled in
   Drawbridge`.
3. The signature verifies with the host's `public_key`, or its
   `previous_key` while a rotation is under way.
4. `ts` is within **5 minutes** of the manager's clock.
5. The nonce (per host) was not used in the last **10 minutes**. The
   manager keeps these in memory.
6. The key is at most **90 days** old (`key_created`).

A bad signature, a wrong clock or an expired key is logged and recorded as
`agent_refused`, at most once an hour per host. A request signed with the new
key while `previous_key` is set completes a rotation: `previous_key` is
cleared. Every accepted request sets `last_seen` and `last_address` (the
client address, read through trusted proxies like the web UI's), and brings
an offline host back online (`host_online`).

Bodies are limited to 4 MiB.

## Routes

| Method and path | Body | Answer |
| --- | --- | --- |
| `POST /api/agent/v1/enroll` | `{"token", "public_key", "hostname", "platform", "version"}`, not signed | `200 {"host_id", "name", "manager_key_id", "manager_key"}`; `400` for a malformed body or key; `401` for an unknown, used or expired token; `429` after 10 attempts in a minute from one client address |
| `GET /api/agent/v1/commands?wait=<seconds>` | | `200 {"commands": [...]}`, at once if commands are queued, else after `wait` seconds (at most 25) with an empty list. `401` if the host is revoked, disabled or removed while the poll is open |
| `POST /api/agent/v1/results` | `[{"id", "ok", "error", "data"}]` | `204`; results no one waits for any more are dropped |
| `POST /api/agent/v1/inventory` | `{"version", "platform", "workloads": [...]}` | `204`; the list replaces the host's inventory and is stored in `hosts.inventory` |
| `POST /api/agent/v1/rotate` | `{"new_key", "ts", "proof"}` | `204`; `400` for a malformed key or a proof that does not verify; `401` if signed with the previous key |

A workload in the inventory is
`{"id", "name", "kind", "group", "member", "running", "label", "depends_on"}`.
For Docker: `kind` is `container`, `group` and `member` are the Compose
project and service, `label` is the `drawbridge.on-failover` label as
`disable`, `keep` or empty, and `depends_on` lists the services from
`com.docker.compose.depends_on`.

## Enrollment

1. **Settings → Hosts → Add host** (or `drawbridge enroll-token`) inserts the
   host with `status = 'pending'`. It stores the SHA-256 of a one-time token
   `dbe_` + 32 random bytes (base64url), valid for 1 hour.
2. The agent loads or makes its key (`key`, mode 0600, in
   `DRAWBRIDGE_AGENT_DIR`). It posts the token and its public key to `enroll`.
3. In one transaction, the manager finds the host by the token's hash and
   checks the expiry. It stores the key, sets `key_created`, clears the token,
   sets `status = 'online'`, and records the manager key ID it will sign
   with. Then it records `host_enrolled`.
4. The agent compares the returned `manager_key` with
   `DRAWBRIDGE_MANAGER_KEY` and stops with an error if they differ. It saves
   `state.json`: host ID, name, pinned manager key and its ID, and
   `key_created`.

**New enrollment token** clears the key and puts the host back to `pending`
with a new token; the agent's open poll ends with `401`.

## Commands (manager to agent)

```json
{"id": "<32 hex>", "host": 7, "type": "stop", "workload": "<id>", "args": {...},
 "expires": 1760000000, "key_id": "k1", "sig": "<base64url>"}
```

`sig` is the manager key's signature over these lines, joined with `\n`:
`command`, `id`, `host`, `type`, `workload`, the `args` JSON as sent,
`expires`, `key_id`.

The agent runs a command only if all of these hold:

- the signature verifies with its pinned manager key, or, for 10 minutes
  after a `rekey`, with the key it replaced (commands sent just before the
  rekey may arrive with it);
- `host` is its own;
- it has not expired;
- its ID is not one it ran in the last 24 hours (kept in memory).

Each command runs in its own goroutine, and its result is posted on its own.

| Type | Args | Result data | Manager waits |
| --- | --- | --- | --- |
| `stop` | `{"timeout_seconds"}` | `{"restore"}` (base64; empty for Docker) | stop timeout + 30 s |
| `start` | `{"restore"}` | | 60 s |
| `inspect` | | `{"running", "health"}` | 60 s |
| `stats` | | `{"<workload id>": {"rx", "tx"}}` for every running workload with counters | 60 s; one answer serves every container's reading for 30 s |
| `rotate` | | | not waited for |
| `rekey` | `{"key_id", "key"}` | `{"key_id"}` | not waited for |

Workload commands look the workload up in a fresh listing. One that is not
there fails with "no workload … on this host", and one outside
`DRAWBRIDGE_AGENT_ALLOW` fails with "… is not in DRAWBRIDGE_AGENT_ALLOW".

Commands live only in the manager's memory: one is useful only while the
executor waits for its result. A command that is not answered in time fails
the executor's action. A host whose agent has not polled for 90 seconds fails
commands at once with "the agent has not been in touch for …", and the
executor treats it as unreachable (see
[Container control](container-control.md#retry-hosts-that-come-back)).
Expired commands are dropped when the agent next polls.

## The agent's loops

- **Polling:** `GET commands?wait=25` in a loop, with a 60-second client
  timeout. A successful poll touches `heartbeat` in the agent's folder, which
  the health check reads. On a network error it waits 5 s, doubling up to
  60 s; on `401` or `403` it waits 5 minutes.
- **Inventory:** posted at start, every 60 s, and 2 s after Docker reports
  `create`, `destroy`, `rename`, `start` or `die`. The manager syncs its
  inventory when the list changed.
- **Key age:** checked at start and every hour; the key is rotated when it is
  30 days old.

## Key rotation

1. The agent makes a new key and writes it to `key.next` first.
2. It posts `rotate` signed with its current key. The body is the new public
   key, a timestamp, and `proof`: the new key's signature over
   `rotate\n<host id>\n<new key>\n<ts>`. The timestamp must be within 5
   minutes.
3. The manager moves `public_key` to `previous_key`, stores the new key and
   `key_created`, and records `host_key_rotated`.
4. The agent renames `key.next` to `key` and saves the new `key_created`.
5. The first request signed with the new key clears `previous_key`.

If the agent stops between steps 3 and 4, it finds `key.next` at its next
start and posts the same rotation again, signed with the old key, which the
manager still accepts as `previous_key`. The manager answers `204` because
the new key is already current. A rotation signed with the previous key for
any other key is refused, so a stolen old key cannot rotate the agent away.

## Manager key rotation

**Rotate manager key** adds a key and makes it current, then records
`manager_key_rotated`. For each agent whose pinned key ID is not current, the
manager queues a `rekey` command signed with the key the agent has pinned,
carrying the new key. It does this at once for agents that are connected, and
at the next poll for the others, again after an hour without an answer. On
the agent's `ok` result, the manager records the new key ID for that host and
signs its commands with the new key from then on. Keys no enrolled agent
pins, other than the current one, are deleted.

## Offline detection

`agents.Manager.Watch` runs every 15 s:

- A connected host whose agent has not polled for **90 s** is marked
  `offline` with "no contact since …", and `host_offline` is recorded.
- A key within **7 days** of the 90-day limit records `host_key_expiring`,
  once per manager start (again after a rotation).

At start-up, agents of hosts that were online get 90 s to poll before they
count as offline. A manager restart therefore records no `host_offline`
for agents that reconnect in time.
