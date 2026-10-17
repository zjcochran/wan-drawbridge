# Container control

Code: `internal/executor`, `internal/hosts`, `internal/docker`,
`internal/store/policy.go`, `internal/store/runs.go`, `internal/store/hosts.go`.

## Hosts

A **host** is one place Drawbridge controls. Host 1, `local`, is the Docker
socket mounted into Drawbridge's container; it always exists. Each host has a
**runtime** (`hosts.Runtime`) that lists, inspects, stops, starts and reads
the network counters of its containers. The executor holds the hosts in a
`hosts.Registry` and works on all of them at once:

- **Listing** asks every host in parallel, with 10 s per host
  (`ListTimeout`). A host that answers is marked `online` and its inventory is
  synced. A host that does not is marked `offline` with the error, and **its
  containers are kept as they were**: an unreachable host never makes its
  containers look deleted.
- **Keys are per host.** Containers, stacks, run actions and usage are keyed
  by `(host_id, key)`, so the same Compose project on two hosts is two stacks
  with their own switches.
- **Stops and starts run on every host in parallel**, each host in its own
  dependency layers. Dependencies never link containers on different hosts.

The local runtime (`hosts.Local`) wraps the Docker client below and excludes
Drawbridge's own container.

## Docker client

`internal/docker` is a minimal Engine API client over the Unix socket (or
plain TCP). It uses unversioned API paths, so the daemon's default API version
applies. Calls:

| Method | API | Notes |
| --- | --- | --- |
| `Ping` | `GET /version` | Returns the engine version. |
| `List` | `GET /containers/json?all=1` | Running and stopped. `restarting` counts as running. |
| `Inspect` | `GET /containers/{id}/json` | Running flag and health status (`""` when no healthcheck). |
| `Stop` | `POST /containers/{id}/stop?t=30` | 204 and 304 (already stopped) are success. |
| `Start` | `POST /containers/{id}/start` | 204 and 304 (already running) are success. |
| `Events` | `GET /events?filters={"type":["container"]}` | Streamed JSON lines; `Action` and `Actor.ID` are used. |
| `Stats` | `GET /containers/{id}/stats?stream=false&one-shot=true` | `rx_bytes` and `tx_bytes` summed over `networks`; none when the container has no networks of its own. Read every minute during an outage for the [Reports page](../user/web-ui.md#how-data-is-measured). |

Errors carry Docker's `message` field, for example `docker: 404 Not Found: No
such container: …`.

## Keys and the inventory

A container's **key** is `project/service` when both Compose labels
(`com.docker.compose.project`, `com.docker.compose.service`) are present,
otherwise its name (without the leading `/`). Replicas of a scaled service
share a key. Keys are unique per host.

`Sync` lists every host (the local runtime drops Drawbridge's own container,
below) and, for each host that answered, calls `store.SyncInventory` for that
host, which in one transaction:

- inserts each new Compose project into `stacks` (switch off);
- updates `name`, `last_seen` and `label_policy` of known keys, clearing
  `needs_review` when a label is now present;
- inserts unknown keys with policy `inherit` and their `label_policy`,
  flagging them `needs_review` if they have no label and their stack already
  has any container with a non-`inherit` policy;
- deletes that host's containers whose `last_seen` is more than
  `store.GoneAfter` (10 min) old, then its stacks with no containers left.
  Present containers were just stamped, so this removes only ones missing
  that long; a re-create (remove, then create) keeps its row and setting. The
  web UI never lists a container that is not on its host; containers of a
  host that cannot be reached are listed as "unknown" (with "host offline"
  when there is more than one host).

`Sync` returns an error naming each host that could not be reached ("its
containers are kept as they were").

### Self-exclusion

A container is Drawbridge itself if it has the label `drawbridge.self=true`,
or if its ID starts with the process's hostname and the hostname is at least
12 characters (Docker sets a container's hostname to its short ID unless
overridden). Self is excluded from the inventory and from every run.

## Policies

Per container (`containers.policy`): `inherit`, `disable`, `keep`. Per
container from Docker (`containers.label_policy`, from the label
`drawbridge.on-failover`: `stop` → `disable`, `keep` → `keep`, anything else
→ `''`). Per stack (`stacks.disable_on_failover`): on or off.

```
effective(policy, label, stackDisabled) =
    policy                 if policy ≠ inherit
    label                  if label ≠ ''
    disable                if stackDisabled
    keep                   otherwise (including standalone containers)
```

`SetStackPolicy(project, on)`: set the stack flag **and** reset every container
in the project to `inherit` (clearing `needs_review`).

`SetContainerPolicy(key, policy)`: if the container's stack is on and the new
policy is `keep`, **convert** the stack first: turn the stack flag off and set
every other `inherit` container without a label in the project to `disable`
(labelled ones keep following their label). Then set the
container's policy and clear its `needs_review`. Reports whether it converted.

Both run in a transaction.

## Runs

A **run** is one failover episode: the set of containers Drawbridge stopped and
must restore. At most one run is open (`runs_one_open` partial unique index).

| Table | Purpose |
| --- | --- |
| `runs` | `trigger` (`auto`/`manual`/`drill`), `to_asn`, `started_at`, `ended_at`, `status` (`open`, `completed`, `failed`) |
| `run_actions` | One row per container in the run: `host_id`, `container_id`, `container_key`, `name`, `was_running`, `stopped_at`, `started_at`, `error`, `restore` (what starting it again needs; empty for Docker containers), `pending` (a start owed to a host that could not be reached) |

### Stop

`Executor.Stop(trigger, toASN)`:

1. `Sync`, then load effective policies.
2. Reuse the open run, or create one. A new run first takes over any starts
   still owed by earlier runs (`TakePendingStarts`): they become actions of
   the new run that were running and are stopped, so they start at its
   failback, not in the middle of it.
3. For every container whose effective policy is `disable`, insert a
   `run_action` with `was_running` = its current running state. **If the row
   already exists it is left unchanged** (`ON CONFLICT DO NOTHING`), so a
   repeated stop (recovery, policy change) can never overwrite the original
   `was_running`.
4. Stop the containers that are running, every host at once; on each host in
   **reverse dependency order** (dependents first), up to `Parallel` = 4 at a
   time within a layer, each with a 30 s stop timeout (Docker sends SIGTERM,
   then SIGKILL).
5. Record each result: success sets `stopped_at`, clears `started_at` and the
   error, and saves `restore`; failure records the error.

The run stays open. Hosts that could not be listed are remembered; the error
names each one ("its containers are stopped when it is back"), and `Retry`
(below) stops them once they answer.

### Start

`Executor.Start()` does nothing without an open run. Otherwise:

1. Load the run's actions and `Sync`.
2. Targets: actions with `was_running` and not yet `started`. An action on a
   host that could not be listed, or that is not connected, is marked
   `pending` with "waiting for the host to be reachable". The others are
   matched to a live container on their host by ID; if that ID is gone
   (re-created), by key, which may match several replicas. No match records
   "`<name>` no longer exists".
3. Start every host at once; on each host in **dependency order**
   (dependencies first), up to 4 at a time per layer, passing each action's
   `restore`. Between layers, wait for health.
4. Record each result: success sets `started_at`; failure records the error.
   Either way the action is no longer pending.
5. Close the run: `completed` if no errors, otherwise `failed`. Pending
   starts count as errors ("cannot reach host `<name>`; its containers are
   started when it is back"), so the run is `failed`, but they are still
   done by `Retry`.

A failed run is closed, so failed starts are not retried automatically; the
dashboard and `drawbridge_run_errors_total` report it. **Restore now** only
acts on an open run.

### Retry (hosts that come back)

The monitor calls `Executor.Retry()` once a minute (`monitor.RetryEvery`),
except while automation is paused. It does nothing unless something is owed:

- **During a run**, if the last stop pass could not reach a host, it runs
  `Stop` again with the run's trigger, which stops that host's containers once
  it answers (and anything newly disabled elsewhere).
- **With no open run**, it starts the pending actions of ended runs whose host
  answers again, oldest run first, as in Start steps 2 to 4. Hosts still
  unreachable are left for the next minute.

The dashboard shows, per host, how many containers will be started again once
Drawbridge can reach it.

### Health wait

`waitHealthy(layer)`: for each container in the layer, poll `Inspect` every 2 s
until it is healthy, not running, has no healthcheck, or inspection fails. The
deadline is 120 s for the whole layer; on timeout, log
`healthcheck wait timed out; starting dependents anyway` and continue.

### Dependency layers

`layers(jobs)` assigns each container a depth: 0 if it depends on nothing
in the set, otherwise one more than the deepest dependency in the set.
Dependencies come from the `com.docker.compose.depends_on` label
(`db:service_healthy:false,redis:service_started:true`; only the service names
are used) and are resolved **within the same project on the same host** to
every container of that service. Dependencies outside the set are ignored; cycles are broken by
ignoring the edge that closes them. Stop uses layers deepest first; start uses
them shallowest first.

### Apply (policy changes during failover)

When stack or container settings change in the web UI, the monitor posts
`PoliciesChanged`. If the state is `failover` and automation is not paused, it
runs `Executor.Apply()`:

1. `Stop(run.trigger, run.to_asn)`: newly disabled running containers are
   stopped and added to the run.
2. For every action that was running, is stopped, is not started, and whose
   effective policy is no longer `disable`: start it and set `started_at`.
   Actions on a host that is not connected are left for the failback.

A container started this way and disabled again later is stopped again, which
clears its `started_at`, so it is restored at failback.

CLI changes do not call `PoliciesChanged`; they take effect at the next Stop or
Start.

### Plan

`Executor.Plan` works out what Stop would do now, for the failover preview
(`/preview`). It lists every host like `Sync` (so the inventory and host
status are refreshed) and loads the effective policies, but opens no run and
stops nothing. Per host, in host ID order, it returns the running containers
set to `disable` grouped into the same layers Stop uses, reversed (dependents
first), each layer sorted by name; the ones set to `disable` that are already
stopped (`Idle`); the number of running containers that keep running; and,
for a host that could not be listed, the error.

## Crash recovery

The monitor saves the state machine **before** calling the executor, and the
executor records every container in `run_actions` **before** stopping it. On
start-up (and when automation is resumed), `monitor.recover()` runs:

| Saved state | Recovery |
| --- | --- |
| `failover` | `Stop("auto", current ASN)`: reuses the open run; containers already recorded keep their `was_running`; anything still running and disabled is stopped. |
| `primary`, `learning` | `Start()`: starts whatever an open run still holds, then closes it. No-op without a run. |
| pending states | Nothing; the next checks continue the countdown (or abandon it as stale). |

Scenarios:

- **Crash after `failover` was saved, before any container stopped:** recovery
  stops them.
- **Crash halfway through stopping:** recovery records nothing new for
  containers already in the run and stops the rest. A container that was
  stopped by the crashed run is now not running, but its `was_running = true`
  is preserved, so it is started at failback.
- **Crash halfway through starting:** the state was saved as `primary`;
  recovery starts the remaining ones and closes the run.

Also, leaving `learning` for `primary` without an action (after a re-learn
during a failover) calls `Start()`, so containers are not left stopped.

## Inventory watcher

`WatchInventory(ctx, docker, 30 s)`:

- syncs every host immediately, then every 30 s;
- in parallel, streams the local Docker's events; on `create`, `destroy` or
  `rename` it schedules a sync after a 2 s settle delay (bursts from
  `compose up` coalesce);
- if the event stream ends, logs a warning and reconnects after 5 s.

`OnChange` (which publishes the `inventory` SSE topic) is called after Stop,
Start, Apply and a Retry that had work, and after a sync only when the
fingerprint (`host/id=name:running` for every container, and which hosts
could not be reached) changed.

## Tunables

Set in `executor.New`; not exposed as configuration:

| Field | Value |
| --- | --- |
| `StopTimeout` | 30 s |
| `Parallel` | 4, per host |
| `HealthWait` | 120 s |
| `PollEvery` | 2 s |
| `ListTimeout` | 10 s, per host |

The web UI lists every host for live container state with 5 s per host; a
host that does not answer in time shows its containers as "unknown".
