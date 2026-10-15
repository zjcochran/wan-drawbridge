# Platforms

Code: `internal/hosts` (`local.go`, `swarm.go`, `proxmox.go`), `internal/docker`,
`internal/proxmox`.

Every host has a runtime (`hosts.Runtime`) that lists its workloads and
stops, starts, inspects and reads the counters of one. The executor treats
every runtime the same way (see [Container control](container-control.md)).
This page lists what each platform maps to.

| | Docker (`docker`) | Docker Swarm (`swarm`) | Proxmox VE (`proxmox`) |
| --- | --- | --- | --- |
| Runtime | `hosts.Local` | `hosts.Swarm` | `hosts.Proxmox` |
| Where | The local socket, or an agent with `DRAWBRIDGE_AGENT_PLATFORM=docker` | An agent on a manager node with `DRAWBRIDGE_AGENT_PLATFORM=swarm` | An agent with `DRAWBRIDGE_AGENT_PLATFORM=proxmox` that reaches the Proxmox API |
| Workloads | Containers | Services, and the node's standalone containers | LXC containers (VMs are left out) |
| `kind` | `container` | `service`, or `container` | `lxc` |
| ID | Container ID | Service ID | Proxmox resource ID, `lxc/<vmid>` |
| Group (stack) | Compose project (`com.docker.compose.project`) | Stack (`com.docker.stack.namespace`) | Pool |
| Member | Compose service | Service name without `<stack>_` | VMID |
| Key | `project/service`, or the container name | `stack/service`, or the service name outside a stack | `pool/<vmid>`, or the name outside a pool (`CT <vmid>` without one) |
| Running | Container state `running` or `restarting` | At least one task running (`ServiceStatus.RunningTasks > 0`) | Status `running` |
| Health | The container's healthcheck | `starting` while running tasks < desired tasks, then `healthy` | None |
| Dependencies | `com.docker.compose.depends_on` | None for services | Within a pool: the containers with a lower start order (`startup: order=N`); containers without one depend on every ordered one |
| On-failover label | `drawbridge.on-failover` | `drawbridge.on-failover` on the service | Tags `drawbridge-stop`, `drawbridge-keep` |
| Stop | `POST /containers/{id}/stop?t=30` | Replicated: `Replicas = 0`, label `drawbridge.replicas = <n>`. Global: placement constraint `node.labels.drawbridge-stopped==true` | `POST …/status/shutdown` with `timeout=30&forceStop=1`, then the task is waited for. Under HA: `PUT /cluster/ha/resources/ct:<vmid>` `state=stopped`, then the container's status is waited for (stop timeout + 30 s) |
| `restore` | empty | `{"replicas": n}` or `{"global": true}` | `{"ha": true}` under HA, else empty |
| Start | `POST /containers/{id}/start` | Replicated: `Replicas` = restore's, else the label's, else 1; the label is removed. Global: the constraint is removed | `POST …/status/start`, waiting for the task; under HA (from `restore`, or HA-managed now) `state=started`, waiting up to 2 minutes |
| Data used | `rx_bytes`/`tx_bytes` from the stats call | None for services; containers as on Docker | `netin`/`netout` of running containers, from `/cluster/resources` |
| Excluded | Drawbridge's own container | The agent's own container and its service; containers that are tasks (`com.docker.swarm.service.id`) | A container named like the agent's hostname |

## Docker Swarm details

The Swarm runtime uses these Engine API calls, besides the container calls:

| Call | API |
| --- | --- |
| `Swarm` | `GET /info`: `Swarm.NodeID`, `LocalNodeState`, `ControlAvailable` (manager) |
| `Services` | `GET /services?status=true`: spec name, labels, mode, and `ServiceStatus` task counts |
| `ServiceSpec` | `GET /services/{id}`: `Version.Index` and the whole `Spec`, kept as generic JSON so an update keeps every field Drawbridge does not know |
| `UpdateService` | `POST /services/{id}/update?version=<index>` with the changed spec |

An update made from an older version is refused by Docker ("update out of
sequence"). The runtime then reads the spec again and retries, up to three
times.

In agent mode the agent checks at start that its node is a swarm manager
(`ControlAvailable`), and stops otherwise. The agent's own service is found
through its container: the container whose ID starts with the agent's
hostname, and its `com.docker.swarm.service.id` label. A service with the
label `drawbridge.self=true` is left out too.

A stopped service shows on the Containers page as not running, because its
task count is 0. When a start has no restore data (an action recorded before
it was stopped, for example), the service's own `drawbridge.replicas` label
still gives the right count.

## Proxmox VE details

The Proxmox client (`internal/proxmox`) uses the API at `PROXMOX_URL` +
`/api2/json`, with `Authorization: PVEAPIToken=<token id>=<secret>`:

| Call | API |
| --- | --- |
| `Guests` | `GET /cluster/resources?type=vm`, LXC entries only |
| `StartupOrder` | `GET /nodes/{node}/lxc/{vmid}/config`, `order=` from `startup`; only for containers in a pool |
| `HAManaged` | `GET /cluster/ha/resources`, the `sid`s |
| `Shutdown`, `Start` | `POST /nodes/{node}/lxc/{vmid}/status/shutdown` or `/start`, then `GET /nodes/{node}/tasks/{upid}/status` every 2 s until `stopped`; an `exitstatus` other than `OK` is an error |
| `SetHAState` | `PUT /cluster/ha/resources/ct:{vmid}` with `state`, then `Guests` every 2 s until the container's status matches |

TLS: the certificate chain is not checked against a CA. Instead the
server's leaf certificate must have the SHA-256 fingerprint in
`PROXMOX_FINGERPRINT` (compared without colons, case-insensitively), and
TLS 1.2 is the minimum. Without a fingerprint, every call fails with "PROXMOX_FINGERPRINT is not set" and the fingerprint the server
presented. Requests time out after 30 s.

If reading `/cluster/ha/resources` fails (the token lacks `Sys.Audit` on
`/`), containers are treated as not HA-managed and are shut down and started
directly.
