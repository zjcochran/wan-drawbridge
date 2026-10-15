# Plan: multiple hosts (Docker agents, Swarm, Proxmox LXC, Kubernetes)

Status: **proposal for review**. Nothing here is built yet.

Today Drawbridge controls the containers of one Docker host, through the
socket mounted into its own container. This plan adds more places to stop and
start workloads during an outage. The Drawbridge server is called the
**manager** from here on.

| Platform | How the manager reaches it | Workloads it stops |
| --- | --- | --- |
| Local Docker (today) | The mounted socket, unchanged | Containers |
| Other Docker hosts | A Drawbridge **agent** on each host | Containers |
| Docker Swarm | An agent on a Swarm manager node | Services |
| Proxmox VE (LXC) | An agent that holds the Proxmox API token | LXC containers |
| Kubernetes | An agent running in the cluster | Deployments, StatefulSets, DaemonSets, CronJobs |

## Decisions

1. **Agents call the manager; the manager never calls an agent.** The REST
   API is on the manager, under `/api/agent/v1/`. Agents reach it at the same
   address as the web UI: through Traefik or another reverse proxy, or
   directly.
   - The manager needs no agent addresses, no route to agent hosts, and no
     open port on them. Agents behind NAT or on other networks work.
   - Commands reach an agent by **long polling**: the agent keeps one
     request open, and the manager answers it as soon as there is a command.
     This is plain HTTP, so it passes through any proxy.
2. **Authentication is done in the application, not with mutual TLS.** A
   reverse proxy terminates TLS, so client certificates would never reach
   the manager. Instead:
   - every agent request is **signed with the agent's Ed25519 key**;
   - every command is **signed with the manager's Ed25519 key**, which the
     agent pins at enrollment. A compromised or misconfigured proxy cannot
     forge commands to agents.
   - TLS still protects the connection, from Traefik or direct HTTPS.
3. **The agent is the same image in a different mode:** `command: agent`.
   - It is one image to build, scan and publish, and agent and manager
     versions always match. The binary already has subcommands.
   - Agent mode loads no web UI, database or WAN monitor, and **listens on
     no port at all**.
4. **LXC means Proxmox VE**, the most common way to run LXC. It runs through
   an agent in Proxmox mode, like every other platform. The agent holds the
   Proxmox API token; the manager never stores it and never needs to reach
   the Proxmox API.
5. **Kubernetes uses an agent inside the cluster**, deployed from a provided
   manifest with a ServiceAccount and narrow RBAC.
   - Drawbridge never stores a kubeconfig, which usually holds broad
     cluster-admin credentials.
   - Because the agent connects out, the cluster needs no Service,
     LoadBalancer or Ingress for it.
6. **Enrollment uses a one-time token.** There is no separate approval step.
   Adding a host in the UI creates the host first, so an agent can only take
   a slot an admin made.

## 1. Hosts and workloads (the core refactor)

### 1.1 Concepts

- A **host** is one place Drawbridge controls: the local Docker socket or
  one agent. Host 1 is always "local".
- A **workload** is what gets stopped and started: a container, a Swarm
  service, a Kubernetes workload or a Proxmox guest.
- A **group** is what the Stacks page shows. It is a Compose project, a Swarm
  stack (`com.docker.stack.namespace`), a Kubernetes namespace, or a Proxmox
  pool. The existing stack rules apply to groups unchanged:
  - changing a group's setting resets its workloads to inherit it;
  - overriding one workload converts a disabled group;
  - a new workload in a split group keeps running and is flagged for review.
- Keys become host-scoped. A workload is identified by `(host_id, key)`.
  `key` stays `project/service` (or the container name) for Docker. It is
  `namespace/kind/name` for Kubernetes, and `pool/vmid` for Proxmox (as built), with the
  guest's name shown.

### 1.2 Runtime interface

`internal/executor`'s `Docker` interface becomes `hosts.Runtime`, one per
host (built in phase 1, in `internal/hosts`):

```go
type Runtime interface {
	List(ctx context.Context) ([]Workload, error)
	Inspect(ctx context.Context, w Workload) (running bool, health string, err error)
	// Stop returns what Start needs to undo it (for example the replica
	// count), stored with the run.
	Stop(ctx context.Context, w Workload, timeout time.Duration) (restore []byte, err error)
	Start(ctx context.Context, w Workload, restore []byte) error
	Stats(ctx context.Context, w Workload) (rx, tx int64, ok bool, err error)
}
```

`hosts.Workload` generalises `docker.Container`:

```go
type Workload struct {
	Host          int64
	ID, Name      string
	Kind          string // container; later service, deployment, statefulset, daemonset, cronjob, lxc
	Group, Member string // e.g. Compose project and service; Key() is group/member or the name
	Running       bool
	Label         string   // from drawbridge.on-failover: "", "disable" or "keep"
	DependsOn     []string // members of the same group
}
```

Later phases add `Warnings` (for example "scaled by an HPA").

Implementations:

- **`hosts.Local`** (phase 1) wraps the `internal/docker` client and excludes
  Drawbridge's own container.
- **The agent runtime** (phase 2) is the manager's side of an agent. It
  answers `List` from the inventory the agent last pushed. `Stop`, `Start`,
  `Inspect` and `Stats` queue a command for the agent, then wait for its
  result until a deadline: the stop timeout plus 30 seconds.

The agent side (`internal/agent`) has one backend per platform: `docker`,
`swarm`, `proxmox`, `kubernetes`. All of them implement the same
`List/Inspect/Stop/Start/Stats` against the local API.

A **registry** holds one `Runtime` per enabled host. It changes when a host is
added, revoked or removed.

### 1.3 Executor changes

- **Fan out per host.** The local host is listed live. An agent host
  contributes its last pushed inventory.
  - A host that has not polled for 90 seconds is **offline**.
  - Its workloads stay in the inventory unchanged: an unreachable host never
    makes its workloads look deleted.
- **Stop and Start run per host in parallel.** Within a host, the existing
  dependency layers apply: Compose `depends_on` and Proxmox start order.
  Swarm and Kubernetes have no dependency data, so they use one layer.
  Ordering across hosts is out of scope.
- **Each action gets a host and a pending state.** `run_actions` gains
  `host_id`, `restore` and a `pending` flag. Built in phase 1:
  - **Stops:** hosts that could not be reached by a stop pass are
    remembered; `executor.Retry`, which the monitor calls every minute unless
    automation is paused, runs the stop pass again while the run is open, so
    their containers stop once they answer.
  - **Starts:** the failback starts every reachable host and closes the run.
    Starts owed to an unreachable host are marked `pending`; `Retry` does
    them once it answers. The dashboard shows them per host.
  - **A new failover first takes over pending starts**, so they start at its
    failback, not in the middle of it.
  - Phase 2 adds "Forget" on a host's pending starts, and removing a host
    forgets them.
- **Failures are per host.** One host failing records `run_failed` with the
  host name; the other hosts carry on.
- **`SelfID` applies to the local host only.** Each agent excludes its own
  container itself.

### 1.4 Schema

**Migration 00014_hosts (phase 1, built)** adds `hosts` with the columns the
local host needs, and host 1 "local":

| Column | Notes |
| --- | --- |
| `id`, `name` (unique), `kind` (`local`, `agent`), `platform` | |
| `status` | `pending`, `online`, `offline`, `revoked`, `disabled` |
| `last_seen`, `last_error` | Set each time the host is listed |
| `created_at`, `created_by` | |

It rebuilds these tables (SQLite cannot change a primary key), with the
existing rows copied in under `host_id = 1`. The tables keep their names:
renaming them to `workloads` and `groups` would touch every query for no gain.

| Table | Change |
| --- | --- |
| `containers` | `host_id`; primary key `(host_id, key)` |
| `stacks` | `host_id`; primary key `(host_id, project)` |
| `run_actions` | `host_id`, `restore`, `pending`; primary key `(run_id, host_id, container_id)` |
| `outage_usage` | `host_id`; primary key `(outage_id, host_id, container)` |

**Migration 00015_agents (phase 2, built)** adds what agents need to
`hosts`: `version`, `public_key` (the agent's current Ed25519 key),
`previous_key` (accepted until the new key is first used), `key_created`,
`enroll_hash` and `enroll_expires` (the one-time token), `last_address`,
`inventory` (the workloads the agent last pushed, as JSON) and
`manager_key_id` (the manager key the agent pinned).

Commands are **not** stored: a command is only useful while the executor
waits for its result, so the queue lives in the manager's memory. After a
manager restart, crash recovery issues the commands again.

`containers` also gains `kind` in the phase that first lists something other
than a container (Swarm).

The manager's Ed25519 signing keys live in the `manager_keys` row of
`settings`, each sealed with `secret.key` like the other secrets, with the ID
of the current one.

**Backups.** `backup.go` carries `hosts` (from phase 1) and `manager_keys`,
re-sealed with the restoring instance's key like the other secrets. Older backups have no `hosts`
table and no `host_id`: their rows go to host 1, and the local host is
created (built in phase 1). A restored manager keeps its key and the agents'
public keys, so existing agents keep working against it.

## 2. The agent

### 2.1 Running it

Same image, agent mode. It needs no `ports:` at all.

```yaml
services:
  drawbridge-agent:
    image: ghcr.io/zjcochran/wan-drawbridge:latest
    command: agent
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./agent:/data/agent          # its key and the pinned manager key; nothing else
    environment:
      DRAWBRIDGE_MANAGER: https://drawbridge.example.com
      DRAWBRIDGE_ENROLL_TOKEN: dbe_...                 # one-time; ignored once enrolled
      DRAWBRIDGE_MANAGER_KEY: ed25519:...              # pins the manager's signing key
      # DRAWBRIDGE_AGENT_ALLOW: media,downloads        # optional allowlist of groups
```

The UI generates this snippet with the token and manager key filled in.
`DRAWBRIDGE_MANAGER` can be the Traefik address or a direct
`http(s)://host:port`.

### 2.2 Manager API for agents

All routes are under `/api/agent/v1/`. They sit outside session auth, CSRF and
the setup redirect, like `/api/status`. Every route except `enroll` needs a
valid agent signature (2.4).

| Method and path | Purpose |
| --- | --- |
| `POST /api/agent/v1/enroll` | `{token, public_key, hostname, platform, version}` → `{host_id, name, manager_key, poll}` |
| `GET /api/agent/v1/commands?wait=25` | Long poll. Returns the pending commands at once, or waits up to 25 seconds and returns `204`. Each poll is also the heartbeat |
| `POST /api/agent/v1/results` | `{command_id, ok, error, data}` for one or more commands |
| `POST /api/agent/v1/inventory` | The full workload list, sent at start, on every change the agent sees, and every 60 seconds |
| `POST /api/agent/v1/usage` | Network counters, sent every minute only while the manager asks for them (during an outage) |
| `POST /api/agent/v1/rotate` | The agent's new public key, with proof that it holds it (2.5) |

- **The poll timeout is 25 seconds**, well below the default proxy read
  timeouts (60 seconds in Traefik), so polls end before a proxy cuts them.
  The agent polls again at once.
- **Latency.** A command reaches an agent within about a second, the time
  for the manager to answer the open poll.

### 2.3 Registration

1. **The admin adds the host.** Settings → Hosts → Add host → name →
   platform. Drawbridge creates the host (`pending`) and shows the snippet
   once, with:
   - a one-time token `dbe_…`: 32 random bytes, stored as a SHA-256 hash,
     expires after 1 hour, single use;
   - the manager's address;
   - the manager's public key.
2. **The agent starts** with no key yet. It generates an **Ed25519** key
   (file mode 0600 in `/data/agent`) and calls `POST /api/agent/v1/enroll`
   with the token, its public key, hostname, platform and version.
3. **The manager checks the request**, in constant time and rate-limited to
   10 a minute per address. The token must exist, be unexpired and unused,
   and belong to a pending host. The manager then marks the token used,
   stores the agent's public key, and returns the host ID and the manager's
   public key.
4. **The agent compares the returned manager key** with
   `DRAWBRIDGE_MANAGER_KEY`, and stops if they differ. A man in the middle
   on a plain-HTTP path fails here. The agent saves its host ID and the
   pinned key.
5. **The agent starts polling.** The host turns `online`, and the manager
   records `host_enrolled`, then the first inventory.

Re-enrolling a host means clicking "New enrollment token", which revokes the
current key.

### 2.4 Signatures

**Agent → manager.** Every request carries:

```
Authorization: Drawbridge-Agent host=<id>, ts=<unix seconds>, nonce=<16 random bytes, base64>, sig=<base64>
```

`sig` is the agent's Ed25519 signature over this text:

```
<method>\n<path and query>\n<host id>\n<ts>\n<nonce>\n<hex SHA-256 of the body>
```

The manager rejects the request with `401` unless all of these hold:

- the host exists and is not revoked or disabled;
- the signature verifies against `public_key`, or `previous_key` during a
  rotation;
- `ts` is within 5 minutes of the manager's clock;
- the nonce has not been seen in the last 10 minutes (an in-memory cache, so
  replays fail).

**Manager → agent.** Each command in a poll response is signed by the
manager's key:

```json
{"id": "...", "host": 7, "type": "stop", "workload": "...", "args": {...},
 "expires": 1760000000, "sig": "..."}
```

The agent runs a command only if:

- the signature verifies against the pinned manager key;
- `host` is its own ID;
- it has not expired;
- its ID has not been run before (agents remember IDs for 24 hours).

**Why both.** The agent's signature proves to the manager who is asking. The
manager's signature means no one between them, including a proxy, can make an
agent act. Plain HTTP (a direct connection on a LAN) is allowed, but the
Hosts page warns: signatures still stop forgery and replay, but the inventory
is readable on the wire.

### 2.5 Key rotation

- **Agent keys rotate every 30 days, automatically.**
  1. The agent generates a new key.
  2. It calls `POST /api/agent/v1/rotate`, signed with its **current** key.
     The body holds the new public key, plus a signature by the **new** key
     over `rotate\n<host id>\n<new key>\n<ts>`, proving it holds that key.
  3. The manager moves `public_key` to `previous_key` and stores the new
     key.
  4. The agent's first request signed with the new key clears
     `previous_key`, retiring the old key. The manager records
     `host_key_rotated`.

  A rotation interrupted after step 3 is safe: both keys are accepted until
  the new one is used.
- **"Rotate now"** on a host sends a signed `rotate` command, and the agent
  runs the same steps at once.
- **The maximum key age is 90 days.** A key older than that, for an agent
  that could not rotate (offline the whole time), is refused. The host shows
  "key expired: new enrollment token needed".
- **"Revoke"** clears the host's keys at once, so every request gets `401`.
  The status turns `revoked`. The agent logs the refusal and backs off to one
  try every 5 minutes, so a revoked agent does not hammer the manager.
- **Manager key rotation**, from a button with a confirmation:
  1. a new manager key is made;
  2. each agent gets a `rekey` command, signed with the **old** key and
     carrying the new one;
  3. each agent pins the new key and confirms;
  4. from then on the manager signs that agent's commands with the new key,
     and old keys are dropped once no agent uses them.

  The UI shows the progress: "4 of 5 agents use it". Agents that were
  offline move when they are back.
- **Notifications** (a new "Hosts" group, on by default):
  - a host offline for 90 seconds, and back online;
  - an agent key under 7 days from the 90-day limit;
  - an agent refused for a bad signature (rate-limited to one an hour per
    host).

### 2.6 What an agent can do

- **The commands are fixed and narrow:** `stop`, `start`, `inspect`, `stats`,
  `rotate`, `rekey`. There is no Docker API passthrough, no exec, pull,
  create, remove or mount. Even a compromised manager can only stop and start
  what the agent lists.
- **The allowlist is enforced on the agent.** With `DRAWBRIDGE_AGENT_ALLOW`,
  the agent lists and acts only on those groups, whatever the manager sends.
- **Logged.** The agent logs every command it runs and every one it refuses.

## 3. Platforms

The agent's platform is set when the host is added. The agent checks that it
matches what it finds, and refuses to start otherwise; for example, Swarm on
a node that is not a Swarm manager. The platform is shown on the Hosts page.

### 3.1 Docker hosts (`platform = docker`)

The behaviour is the same as the local host, including Compose `depends_on`
layers, healthcheck waits and data-used stats. The agent excludes its own
container.

### 3.2 Docker Swarm (`platform = swarm`, agent on a manager node)

- **Detection.** From `GET /info`: `Swarm.LocalNodeState = active` with
  `ControlAvailable`.
- **Deployment (as built in phase 3).** One agent controls the whole swarm.
  It runs as a one-replica service pinned to **one** manager node
  (`node.role == manager` and `node.hostname == …`), with its folder in a
  local volume on that node, like any other agent, and
  `DRAWBRIDGE_AGENT_PLATFORM=swarm`.
  - An earlier draft kept the agent's key in a Docker secret so the agent
    could move between manager nodes. That needs a key the agent cannot
    rotate, hosts found by key, and the manager key pinned only in the
    environment. It was dropped as too much machinery for little gain: if
    the pinned node is down, the swarm host is offline until it is back,
    and the catch-up of 1.3 applies.
- **The agent's own service** is excluded from the list, so a failover never
  stops the agent.
- **What is listed.** Workloads are services, grouped by stack. Containers
  that are tasks of a service are hidden. Plain standalone containers on that
  node are still listed as containers.
- **Stop.**
  - A replicated service is scaled to 0: the agent reads the service and
    updates it with `Replicas = 0` at its current version. `restore` holds
    the old count, also saved as the service label `drawbridge.replicas`.
  - A global service gets the placement constraint
    `node.labels.drawbridge-stopped == true`, which no node has.
- **Start.** The replica count comes back, or the constraint is removed.
- **Health.** A service is healthy when its running tasks equal its desired
  tasks.
- **No data-used stats.** A service's tasks run on several nodes, so the
  reports show "–".

### 3.3 Proxmox VE (`platform = proxmox`)

- **Where the agent runs.** Anywhere that can reach the Proxmox API: a small
  LXC or VM on the cluster, or a Docker host. It needs no Docker socket in
  this mode.
- **Its settings.**
  - `PROXMOX_URL` is any node of the cluster.
  - `PROXMOX_TOKEN_ID` and `PROXMOX_TOKEN_SECRET`, or `_FILE` variants for
    Docker secrets. The token stays on the agent; the manager never sees it.
  - `PROXMOX_FINGERPRINT` pins the cluster's TLS certificate, since Proxmox
    certificates are usually self-signed. On first start without it, the
    agent stops and prints the fingerprint it sees.
- **The token.** The docs give the exact `pveum` commands for a
  privilege-separated token whose role has only `VM.Audit` and
  `VM.PowerMgmt`, on one pool (recommended) or on `/vms`. As built: HA needs
  `Sys.Audit` on `/` to read and `Sys.Console` on `/` to change; without
  `Sys.Audit`, every container is stopped directly.
- **What is listed.** Workloads come from `GET /cluster/resources?type=vm`,
  filtered to `type = lxc`, grouped by pool and keyed `pool/<vmid>`. As
  built, a guest without a pool is standalone, keyed by its name. The tags
  `drawbridge-stop` and `drawbridge-keep` act as the on-failover label. QEMU
  VMs could be added later with the same calls.
- **Stop.** `POST /nodes/{node}/lxc/{vmid}/status/shutdown`, with the stop
  timeout and `forceStop=1`. The agent then waits for the task (UPID).
- **Start.** `POST /nodes/{node}/lxc/{vmid}/status/start`.
- **Guests under HA** (in `/cluster/ha/resources`) are stopped and started
  through HA instead, by setting `state` to `stopped` or `started`.
  Otherwise HA starts them straight back up.
- **Order.** The guest's `startup` order becomes the dependency layers.
- **Data used** comes from `netin`/`netout` (as built, read from
  `/cluster/resources`).
- **Its own guest.** As built, a guest named like the agent's hostname is
  left out.

### 3.4 Kubernetes (`platform = kubernetes`, agent in the cluster)

- **Deployment.** The `drawbridge-agent` manifest (in `deploy/kubernetes/`)
  creates:
  - a namespace, a ServiceAccount, and a Role per allowed namespace (or a
    ClusterRole if all namespaces are allowed);
  - a Deployment running `agent` with one replica, and a
    PersistentVolumeClaim for `/data/agent`.

  There is **no Service or Ingress**. The agent only needs egress to the
  manager's address.
- **API access.** The agent uses the in-cluster config: the token, CA and
  `KUBERNETES_SERVICE_HOST`. It talks to the API with a small REST client
  rather than `client-go`, as with Docker, to keep dependencies small.
- **RBAC:**
  - `get`, `list`, `watch` on deployments, statefulsets, daemonsets,
    cronjobs, pods and horizontalpodautoscalers;
  - `patch` on `deployments/scale`, `statefulsets/scale`, daemonsets and
    cronjobs.
- **Workloads.** Deployments, StatefulSets, DaemonSets and CronJobs, grouped
  by namespace. The agent's own Deployment is excluded.
- **Stop:**
  - **Deployment, StatefulSet:** patch `/scale` to 0. `restore` holds the old
    count, also saved as the annotation `drawbridge.io/replicas`.
  - **DaemonSet:** add the nodeSelector `drawbridge.io/stopped: "true"`,
    which no node has.
  - **CronJob:** `spec.suspend: true`.
- **Start.** Each change is undone.
- **Health.** Ready replicas equal the desired replicas.
- **Warnings shown in the UI:**
  - a workload with an HPA, because the HPA scales it back up (Drawbridge
    still acts, but warns);
  - a workload managed by Argo CD or Flux (their labels), which undo the
    change unless the app ignores replica differences or is suspended.

  The docs show the Argo CD `ignoreDifferences` rule.
- **No data-used stats.** Reading pod network counters needs `nodes/proxy`,
  which is too much access, so the reports show "–".

## 4. UI

- **Settings → Hosts** (admins; viewers read-only) lists each host with:
  - name, platform, status, last seen, the address its last request came
    from;
  - agent version, and a warning if it differs from the manager's;
  - key age ("rotated 12 days ago"), and "plain HTTP" when the agent does
    not use TLS.

  Actions: Add host, Rename, Rotate now, New enrollment token, Revoke,
  Disable, Remove (only when it has nothing pending), and Rotate manager key.
  The local host can be disabled but not removed.
- **Stacks and Containers**:
  - a **Host** column and a **host filter** (`?host=<id>`, kept in the URL
    like the other filters, and combinable with them);
  - workloads on an offline host are dimmed, with "host offline";
  - group names are unique per host, so the same Compose project on two
    hosts is two rows.

  Routes become `/stacks/{host}/{project}/containers` and
  `/stacks/{host}/{project}/policy`. The container policy forms send `host`.
- **Dashboard.** "Stopped by Drawbridge" is grouped by host. The status card
  shows "3 of 4 hosts online" when any host is offline.
- **Reports.** The data-used table gains a Host column.
- **Setup wizard.** Unchanged: the local socket. Hosts are added afterwards in
  Settings.

## 5. API, CLI, events and metrics

- **`/api/status`** gains `hosts`: name, platform, status, workload count,
  stopped count.
- **`/api/agent/v1/...`** is new, as in 2.2.
- **CLI:**
  - `drawbridge agent` runs agent mode;
  - `drawbridge hosts` lists the hosts;
  - `drawbridge revoke-host <name>` revokes one;
  - `drawbridge enroll-token <name>` prints a new token, for setups without a
    browser.
- **Events:** `host_added`, `host_enrolled`, `host_online`, `host_offline`,
  `host_key_rotated`, `host_revoked`, `host_removed`, `agent_refused`,
  `manager_key_rotated`. Run events gain `host`.
- **Metrics:** `drawbridge_host_up{host}` (built in phase 1).

## 6. Security summary

| Threat | Mitigation |
| --- | --- |
| A rogue agent joins | Enrollment needs a one-time token for a host an admin created: 1 h, single use, hashed, rate-limited |
| Man in the middle during enrollment | The agent pins the manager key given in the snippet |
| A proxy or network path that is compromised | It cannot forge commands (manager-signed) or agent requests (agent-signed); replays fail on timestamps, nonces and command IDs |
| A stolen agent key | Rotated every 30 days, refused after 90; "Revoke" cuts it off at once |
| A stolen manager key | "Rotate manager key" moves every agent to a new one |
| A compromised manager | Agents run only stop, start, inspect and stats, with an optional agent-side allowlist; no exec, create or Docker passthrough |
| A compromised agent | It holds only its own host's access (as the Docker socket already gives). It can push a false inventory for its own host, but cannot act on other hosts or read the manager's data |
| Exposure of agent hosts | Agents open no port; nothing on an agent host is reachable from the network because of Drawbridge |
| A leaked Proxmox token | Kept only on the agent; scoped to `VM.Audit` and `VM.PowerMgmt` on one pool; TLS pinned |
| Broad Kubernetes credentials | No kubeconfig stored; in-cluster ServiceAccount with namespace-scoped RBAC |
| Secrets in backups | The manager key is sealed and inside the encrypted backup; agent private keys never leave their hosts |

## 7. Implementation phases (one MR each, in order)

| # | MR | Contents | Label |
| --- | --- | --- | --- |
| 1 | Hosts and workloads | Migration 00014; `internal/hosts` (`Runtime` interface, registry, local runtime); executor fan-out, per-host failures, pending starts and `Retry`; Host column and filter on Stacks and Containers; host on the dashboard, reports, events, `/api/status` and metrics; `<host>:` in `set-stack`/`set-container`; backup translation. With only "local", behaviour is unchanged | minor |
| 2 | Agent and signatures | Migration 00015; `internal/agentauth` (Ed25519 signing, verification, nonce cache); `drawbridge agent` mode with the Docker backend; `/api/agent/v1/` routes; command queue and long poll; enrollment; key rotation, manager key rotation, revocation; Settings → Hosts; CLI; events, metrics, notifications | minor |
| 3 | Docker Swarm | Swarm runtime (services, stacks, replicated and global stop/start, health); `DRAWBRIDGE_AGENT_PLATFORM=swarm`; migration 16 (`containers.kind`); the agent pinned to one manager node | minor |
| 4 | Proxmox LXC | `internal/proxmox` client; Proxmox runtime in the agent (`DRAWBRIDGE_AGENT_PLATFORM=proxmox`); TLS pinning; HA handling; start order; tags as labels; usage | minor |
| 5 | Kubernetes | Small API client; workload kinds; HPA and GitOps warnings; `deploy/kubernetes/` manifest | minor |

Each phase:
- keeps 80% coverage per package;
- passes golangci-lint, ShellCheck and the security gate;
- updates the docs listed for it in section 8;
- updates CLAUDE.md: "one instance, one Docker host" becomes "one manager,
  many hosts", and the "deferred: multiple Docker hosts" line is removed.

## 8. Documentation

Every phase updates the docs in the same MR, with every figure (timings,
limits, routes, schema, events, metrics) checked against the code.
Screenshots are retaken from the fictional demo whenever a page they show
changes. This plan is removed once phase 5 is merged.

### New pages

| Page | Phase | Contents |
| --- | --- | --- |
| `docs/user/hosts.md` | 2, extended in 3–5 | What a host is; Settings → Hosts; adding a Docker host step by step (the snippet and each variable; Traefik or direct address); statuses; Rotate now, Revoke, New enrollment token, Disable, Remove, Rotate manager key; what happens to a host that is offline during a failover and the failback; the allowlist. Phases 3–5 add a section each: Swarm (the stack file, pinned to one manager node); Proxmox (the exact `pveum` commands for the role, user and token, pool setup, the fingerprint, HA); Kubernetes (applying the manifest, choosing namespaces, the Argo CD `ignoreDifferences` rule, HPAs) |
| `docs/technical/agent-protocol.md` | 2 | Each `/api/agent/v1/` route with request and response bodies and status codes; the long poll and its timeouts; the exact signed text for requests and commands; clock and nonce rules; enrollment; agent and manager key rotation, including how an interrupted rotation recovers; command IDs and expiry; versioning |
| `docs/technical/platforms.md` | 3–5 | How each platform maps onto workloads, groups and stop/start: Swarm replicas and the global-service constraint; Proxmox shutdown, HA and start order; Kubernetes scale, nodeSelector and suspend. What `restore` holds for each, and which platforms have data-used stats and why not the others |
| `deploy/kubernetes/README.md` | 5 | The manifest, and each RBAC rule and why it is needed |

### Changed pages

| Page | Phase | Change |
| --- | --- | --- |
| `README.md` | 1–5 | Feature list and "How it works" mention hosts and each platform as it lands |
| `docs/README.md` | 2–5 | Index entries for the new pages |
| `CLAUDE.md` | 1, 2 | "One instance, one Docker host" → "one manager, many hosts"; the "deferred: multiple Docker hosts" line removed; layout entries for `internal/agent`, `internal/agentauth`, `internal/runtime/...`; agents and each platform join "Not yet verified" |
| `docs/user/installation.md` | 2 | Running an agent; that it needs no open port, only a way to reach the manager |
| `docs/user/web-ui.md` | 1, 2, 4 | Host column and filter on Stacks and Containers; dimmed offline workloads; Settings → Hosts; dashboard grouping by host and "3 of 4 hosts online" |
| `docs/user/stacks-and-containers.md` | 1, 3–5 | Groups per host (the same project on two hosts is two rows); what a group is on each platform; HPA and GitOps warnings |
| `docs/user/during-an-outage.md` | 1, 2 | Stops and starts per host in parallel; pending actions for an offline host and when they are sent; "Forget" on pending actions |
| `docs/user/notifications.md` | 2 | The new events in the failures group |
| `docs/user/monitoring.md` | 1, 2 | New events and their payloads; `host` in run events; new metrics; `hosts` in `/api/status` and the Home Assistant example |
| `docs/user/cli.md` | 2, 3 | `agent`, `hosts`, `revoke-host`, `enroll-token`; `DRAWBRIDGE_AGENT_PLATFORM` (phase 3) |
| `docs/user/backup-and-restore.md` | 1, 2 | Hosts and the manager key are in backups; agents keep working after a restore; older backups load into host "local" |
| `docs/user/reverse-proxy.md` | 2 | `/api/agent/` goes through the proxy like the rest; the long poll and proxy timeouts (25 s polls fit the defaults); no extra routes or ports |
| `docs/user/troubleshooting.md` | 2–5 | Agent will not enroll (token used or expired, manager key mismatch, cannot reach `DRAWBRIDGE_MANAGER`); signatures refused (clock skew over 5 minutes, revoked, key expired); host offline; Proxmox permission and fingerprint errors; Kubernetes RBAC errors; a scaled workload coming back (HPA, GitOps) |
| `docs/user/faq.md` | 2 | Why agents connect to the manager; what an agent can and cannot do; plain HTTP versus HTTPS for agents |
| `docs/technical/architecture.md` | 1, 2 | Runtime interface and registry; per-host fan-out; command queue and long poll; agent mode; the diagrams |
| `docs/technical/container-control.md` | 1, 3–5 | Workloads, keys per host, layers within a host, pending actions, `restore` |
| `docs/technical/data-model.md` | 1, 2 | Migrations 14 and 15; `hosts`; new columns on `containers`, `stacks`, `run_actions` and `outage_usage`; the `manager_keys` setting |
| `docs/technical/http-interface.md` | 1, 2 | Host-scoped stack routes, `?host=`, Settings → Hosts actions, `/api/agent/v1/` routes |
| `docs/technical/configuration-reference.md` | 2, 4 | Agent variables: `DRAWBRIDGE_MANAGER`, `DRAWBRIDGE_ENROLL_TOKEN`, `DRAWBRIDGE_MANAGER_KEY`, `DRAWBRIDGE_AGENT_ALLOW`; and `PROXMOX_URL`, `PROXMOX_TOKEN_ID`, `PROXMOX_TOKEN_SECRET` (and `_FILE`), `PROXMOX_FINGERPRINT` |
| `docs/technical/security.md` | 2, 4, 5 | Signatures, keys, rotation and revocation; the threat table from section 6; Proxmox token scope and pinning; Kubernetes RBAC |
| `docs/technical/testing-and-ci.md` | 2–5 | The new fakes and end-to-end cases |

## 9. Testing

- **Unit tests use fakes only:**
  - the manager's agent API tested with an in-process agent;
  - an `httptest` Proxmox API and Kubernetes API;
  - the existing fake Docker, extended with swarm endpoints.
- **Signatures and enrollment, one case each:**
  - expired token, used token, token for another host;
  - wrong manager key in the snippet;
  - bad signature, wrong host ID, timestamp too old, replayed nonce;
  - a command signed by another key, for another host, expired, or replayed;
  - revoked host, key past 90 days;
  - rotation interrupted after the manager stored the new key;
  - manager key rotation with one agent offline.
- **Long poll:** a command is answered within a second; a poll ends at 25
  seconds; an agent that stops polling turns offline after 90 seconds.
- **End to end** (built in phase 2 as `internal/agents` tests rather than
  `internal/e2e`): a real manager and in-process agents on fake Docker:
  - a failover stops on both hosts;
  - one agent offline during the failover catches up when it returns;
  - the failback restarts everything and the run closes;
  - key rotation with a fake clock.
- **Not verified until you run it:** a real second Docker host, an agent
  through a real Traefik, a real Swarm, a real Proxmox cluster and a real
  Kubernetes cluster. These join the "Not yet verified" list.

## 10. Open points

- **Cross-host dependencies** ("start the database on host A before the app
  on host B"): not in this plan.
- **QEMU VMs on Proxmox**: a small addition on top of phase 4, if wanted.
- **One agent for several platforms** (for example Docker and Proxmox from
  the same agent): not in this plan; run one agent per platform.
