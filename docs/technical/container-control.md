# Container control

Code: `internal/executor`, `internal/docker`, `internal/store/policy.go`,
`internal/store/runs.go`.

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
share a key.

`Sync` lists Docker's containers, drops Drawbridge's own (below), and calls
`store.SyncInventory`, which in one transaction:

- inserts each new Compose project into `stacks` (switch off);
- updates `name`, `last_seen` and `label_policy` of known keys, clearing
  `needs_review` when a label is now present;
- inserts unknown keys with policy `inherit` and their `label_policy`,
  flagging them `needs_review` if they have no label and their stack already
  has any container with a non-`inherit` policy;
- deletes containers whose `last_seen` is more than `store.GoneAfter`
  (10 min) old, then stacks with no containers left. Present containers
  were just stamped, so this removes only ones missing that long; a
  re-create (remove, then create) keeps its row and setting. The web UI
  never lists a container that is not in Docker.

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
| `run_actions` | One row per container in the run: `container_id`, `container_key`, `name`, `was_running`, `stopped_at`, `started_at`, `error` |

### Stop

`Executor.Stop(trigger, toASN)`:

1. `Sync`, then load effective policies.
2. Reuse the open run, or create one.
3. For every container whose effective policy is `disable`, insert a
   `run_action` with `was_running` = its current running state. **If the row
   already exists it is left unchanged** (`ON CONFLICT DO NOTHING`), so a
   repeated stop (recovery, policy change) can never overwrite the original
   `was_running`.
4. Stop the containers that are running, in **reverse dependency order**
   (dependents first), up to `Parallel` = 4 at a time within a layer, each with
   a 30 s stop timeout (Docker sends SIGTERM, then SIGKILL).
5. Record each result: success sets `stopped_at`, clears `started_at` and the
   error; failure records the error.

The run stays open.

### Start

`Executor.Start()` does nothing without an open run. Otherwise:

1. Load the run's actions and `Sync`.
2. Targets: actions with `was_running` and not yet `started`. Each is matched to
   a live container by ID; if that ID is gone (re-created), by key, which may
   match several replicas. No match records "`<name>` no longer exists".
3. Start in **dependency order** (dependencies first), up to 4 at a time per
   layer. Between layers, wait for health.
4. Record each result: success sets `started_at`; failure records the error.
5. Close the run: `completed` if no errors, otherwise `failed`.

A failed run is closed, so it is not retried automatically; the dashboard and
`drawbridge_run_errors_total` report it. **Restore now** only acts on an open
run.

### Health wait

`waitHealthy(layer)`: for each container in the layer, poll `Inspect` every 2 s
until it is healthy, not running, has no healthcheck, or inspection fails. The
deadline is 120 s for the whole layer; on timeout, log
`healthcheck wait timed out; starting dependents anyway` and continue.

### Dependency layers

`Layers(containers)` assigns each container a depth: 0 if it depends on nothing
in the set, otherwise one more than the deepest dependency in the set.
Dependencies come from the `com.docker.compose.depends_on` label
(`db:service_healthy:false,redis:service_started:true`; only the service names
are used) and are resolved **within the same project** to every container of
that service. Dependencies outside the set are ignored; cycles are broken by
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

A container started this way and disabled again later is stopped again, which
clears its `started_at`, so it is restored at failback.

CLI changes do not call `PoliciesChanged`; they take effect at the next Stop or
Start.

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

- syncs immediately, then every 30 s;
- in parallel, streams Docker events; on `create`, `destroy` or `rename` it
  schedules a sync after a 2 s settle delay (bursts from `compose up` coalesce);
- if the event stream ends, logs a warning and reconnects after 5 s.

`OnChange` (which publishes the `inventory` SSE topic) is called after Stop,
Start and Apply, and after a sync only when the fingerprint
(`id=name:running` for every container) changed.

## Tunables

Set in `executor.New`; not exposed as configuration:

| Field | Value |
| --- | --- |
| `StopTimeout` | 30 s |
| `Parallel` | 4 |
| `HealthWait` | 120 s |
| `PollEvery` | 2 s |
