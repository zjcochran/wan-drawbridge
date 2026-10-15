# Hosts and agents

A **host** is a Docker engine Drawbridge stops and starts containers on.
There is always one: **local**, the engine whose socket is mounted into
Drawbridge's container. Other Docker hosts each run a small **agent**, which
connects to Drawbridge and does what it asks.

Drawbridge itself is called the **manager** here. It never connects to an
agent: agents connect to the manager, at the same address as the web UI
(through your reverse proxy, or directly). Agent hosts need no open port, and
the manager needs no route to them.

![Settings → Hosts](../images/hosts.jpg)

## Tested so far

Agents are new, and so far they have only been tested inside Drawbridge's
own test suite, against simulated Docker engines and APIs. None of the setups
below has yet been tried on real equipment, so expect rough edges and check
the first failover test closely:

- An agent on a real second Docker host, and an agent reaching Drawbridge
  through a real reverse proxy.

## Adding a Docker host

1. In **Settings → Hosts**, enter a name under **Add a host** and click **Add
   host**. A name is 1 to 32 letters, digits, dots, dashes or underscores,
   starting with a letter or digit, and must be unused. The host appears as
   **Waiting for the agent**.
2. Drawbridge shows a Compose file for the agent, **once**:

   ```yaml
   services:
     drawbridge-agent:
       image: ghcr.io/zjcochran/wan-drawbridge:latest
       command: agent
       restart: unless-stopped
       volumes:
         - /var/run/docker.sock:/var/run/docker.sock
         - ./agent:/data/agent
       environment:
         DRAWBRIDGE_MANAGER: https://drawbridge.example.com
         DRAWBRIDGE_ENROLL_TOKEN: dbe_...
         DRAWBRIDGE_MANAGER_KEY: ed25519:...
         # DRAWBRIDGE_AGENT_ALLOW: media,downloads
   ```

3. Start it on the other host: `docker compose up -d`. Within a few seconds
   the host shows **Online**, and its stacks and containers appear on the
   Stacks and Containers pages with a **Host** column and filter.

The agent is the same image as Drawbridge, started with `command: agent`. It
runs no web UI, has no database and opens no port. Keep the agent's version
the same as the manager's: the Hosts page marks an agent whose version
differs.

| Variable | Meaning |
| --- | --- |
| `DRAWBRIDGE_MANAGER` | The manager's address, as your browser reaches the web UI. The Compose file uses `DRAWBRIDGE_BASE_URL` if it is set, otherwise the address of the page you are on |
| `DRAWBRIDGE_ENROLL_TOKEN` | A one-time token: it works **once**, within **one hour**. It is not needed after the agent enrolled, and is ignored then |
| `DRAWBRIDGE_MANAGER_KEY` | The manager's public key. The agent refuses to enroll if the manager it reaches has another key, so a wrong address or someone in between cannot take the agent over |
| `DRAWBRIDGE_AGENT_ALLOW` | Optional: a comma-separated list of stacks (Compose projects). The agent then lists and acts on containers of those stacks only, whatever the manager asks; standalone containers are left out |
| `DRAWBRIDGE_AGENT_DIR` | Where the agent keeps its key and state; default `/data/agent`. Mount it, as above, so the agent keeps its identity across re-creation |
| `DOCKER_HOST` | The Docker engine; default `unix:///var/run/docker.sock` |
| `LOG_LEVEL` | `debug`, `info` (default), `warn` or `error`, as for the manager |

The token and key can also be made on the command line:
`drawbridge enroll-token <name>` (see [Command line](cli.md#enroll-token)).

If `DRAWBRIDGE_MANAGER` is plain `http://`, the agent logs a warning: requests
and commands are still signed, so they cannot be forged or replayed, but the
container list can be read on the way. Use HTTPS when the agent reaches the
manager over a network you do not control.

## What the Hosts page shows

| Column | Meaning |
| --- | --- |
| Host | The name, and whether it is the local socket or an agent, with its platform (`docker`). |
| Status | **Online**: the agent was in touch within the last 90 seconds. **Offline**: it was not; the time it was last seen and the error are shown. **Waiting for the agent**: a token was made and has not been used yet; it is valid until the time shown. **Revoked** and **Disabled**: below. Also: "N containers to start once it is back" when starts are owed to it. |
| Agent | The agent's version, its key's fingerprint (the first 16 hex digits of its SHA-256), how old the key is and when it rotates. |

Everyone can open the Hosts page; only admins see the buttons and the
manager key.

## Hosts that go offline

An agent polls the manager all the time. If it has not been in touch for 90
seconds, its host turns **Offline**, a `host_offline` event is recorded, and
notifications in the "Hosts" group are sent. Its containers stay on the
Stacks and Containers pages with their settings, shown as "host offline".
When it is back, `host_online` is recorded.

A host that is offline when a failover or failback runs is caught up with
once it is back; see [hosts that cannot be
reached](during-an-outage.md#hosts-that-cannot-be-reached).

## Actions

| Action | What it does |
| --- | --- |
| Rotate key now | The agent makes a new key at its next poll and replaces its old one (below). |
| New enrollment token | Shows a new Compose file with a new one-time token. The agent's current key stops working at once; the host waits for the agent to enroll again. Use it when an agent lost its folder, or after a revoke. |
| Revoke | The agent is refused from now on: its key is deleted. Its containers stay listed, as unknown, until you remove the host. |
| Disable / Enable | A disabled host is left out of every failover and failback, and its agent is told so. The local host can be disabled too. Enabling puts it back. |
| Forget owed starts | Shown when starts are owed to the host: Drawbridge will not start those containers. |
| Remove | Deletes the host, with its stack and container settings and any owed starts. Events and data-used history are kept. The local host cannot be removed. |
| Rename | Any host, the local one included. The name is used in events, notifications and `set-stack nas:media on` on the command line. |

Every action is recorded as an event (see [Monitoring](monitoring.md#event-types)).

## Keys

Each agent makes its own Ed25519 key when it first starts. The private key
never leaves the agent's folder; the manager only knows the public key.

- **Rotation.** The agent replaces its key every **30 days** by itself, and
  when you click **Rotate key now**. It sends the new key signed with the old
  one, with proof that it holds the new one. The manager accepts both keys
  until the agent first uses the new one, so a rotation cut short (a restart
  in the middle) cannot lock the agent out. A `host_key_rotated` event is
  recorded.
- **Expiry.** The manager refuses a key older than **90 days**: an agent that
  could not rotate for that long (because it was off) needs a new enrollment
  token. Seven days before, `host_key_expiring` is recorded and notified.
- **The manager's key.** Every command the manager sends is signed with the
  manager's key, which the agent pinned when it enrolled. **Rotate manager
  key** (under the host list) makes a new one: agents that are online get it
  at once, signed with the old key, and the others when they are back. The
  page shows how many agents use the current key. Old keys are deleted once no
  agent uses them. New Compose files carry the new key.

## What an agent can and cannot do

An agent only does these things, and only when the manager asks with a
command signed by the manager's key: list its containers, report one's state
and health, stop one, start one, read network counters, and rotate its own
key or pin a new manager key. It has no way to run a command in a container,
pull or create anything, or reach the Docker API in any other way. A
compromised manager can therefore only stop and start what the agent lists,
and `DRAWBRIDGE_AGENT_ALLOW` narrows that further.

The agent still has the Docker socket mounted, which gives full control of
that Docker host to anyone who takes over the agent's container, as for
Drawbridge itself.

## The agent's health check

The image's `HEALTHCHECK` runs `drawbridge healthcheck`. In an agent (when
`DRAWBRIDGE_MANAGER` is set) it passes when the agent reached the manager
within the last 90 seconds; Docker shows the container as unhealthy
otherwise.

## Through a reverse proxy

Agents use the paths under `/api/agent/v1/` at the manager's address, so a
proxy that forwards the web UI forwards them too. An agent keeps one request
open for up to 25 seconds while it waits for commands, well under the
60-second read timeout proxies use by default. See [Behind a reverse
proxy](reverse-proxy.md#agents).
