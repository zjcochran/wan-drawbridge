# Platforms

Code: `internal/hosts` (`local.go`, `swarm.go`), `internal/docker`.

Every host has a runtime (`hosts.Runtime`) that lists its workloads and
stops, starts, inspects and reads the counters of one. The executor treats
every runtime the same way (see [Container control](container-control.md)).
This page lists what each platform maps to.

| | Docker (`docker`) | Docker Swarm (`swarm`) |
| --- | --- | --- |
| Runtime | `hosts.Local` | `hosts.Swarm` |
| Where | The local socket, or an agent with `DRAWBRIDGE_AGENT_PLATFORM=docker` | An agent on a manager node with `DRAWBRIDGE_AGENT_PLATFORM=swarm` |
| Workloads | Containers | Services, and the node's standalone containers |
| `kind` | `container` | `service`, or `container` |
| Group (stack) | Compose project (`com.docker.compose.project`) | Stack (`com.docker.stack.namespace`) |
| Member | Compose service | Service name without `<stack>_` |
| Key | `project/service`, or the container name | `stack/service`, or the service name outside a stack |
| Running | Container state `running` or `restarting` | At least one task running (`ServiceStatus.RunningTasks > 0`) |
| Health | The container's healthcheck | `starting` while running tasks < desired tasks, then `healthy` |
| Dependencies | `com.docker.compose.depends_on` | None for services |
| Stop | `POST /containers/{id}/stop?t=30` | Replicated: `Replicas = 0`, label `drawbridge.replicas = <n>`. Global: placement constraint `node.labels.drawbridge-stopped==true` |
| `restore` | empty | `{"replicas": n}` or `{"global": true}` |
| Start | `POST /containers/{id}/start` | Replicated: `Replicas` = restore's, else the label's, else 1; the label is removed. Global: the constraint is removed |
| Data used | `rx_bytes`/`tx_bytes` from the stats call | None for services; containers as on Docker |
| Excluded | Drawbridge's own container | The agent's own container and its service; containers that are tasks (`com.docker.swarm.service.id`) |

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
