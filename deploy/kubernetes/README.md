# Drawbridge agent for Kubernetes

`drawbridge-agent.yaml` runs a WAN Drawbridge agent inside a cluster. It
connects out to Drawbridge, so the cluster needs no Service, LoadBalancer or
Ingress for it: only egress to Drawbridge's address. See [Hosts and
agents](../../docs/user/hosts.md#adding-kubernetes) for what it does.

**Not yet tested on a real cluster.** The manifest and the agent's
Kubernetes support have only been tested against a simulated API so far.
Check the agent's log and the first failover test closely.

## Install

1. In Drawbridge, **Settings → Hosts → Add host**. Open the **Kubernetes** tab it
   shows and replace the Secret in `drawbridge-agent.yaml` with that one. It
   holds `DRAWBRIDGE_MANAGER`, `DRAWBRIDGE_ENROLL_TOKEN` (valid for one hour,
   once) and `DRAWBRIDGE_MANAGER_KEY`.
2. `kubectl apply -f drawbridge-agent.yaml`
3. The host shows **Online** in Drawbridge within a few seconds. If it does
   not, `kubectl -n drawbridge logs deploy/drawbridge-agent` says why.

After it enrolled, the token in the Secret is no longer used; you can remove
it.

## What it creates

| Object | Why |
| --- | --- |
| Namespace `drawbridge` | Keeps the agent apart |
| ServiceAccount `drawbridge-agent` | The agent's identity towards the API |
| ClusterRole `drawbridge-agent` | The permissions below, and nothing else |
| ClusterRoleBinding `drawbridge-agent` | Grants them in every namespace (see below to limit them) |
| Secret `drawbridge-agent` | The manager address, enrollment token and manager key |
| PersistentVolumeClaim `drawbridge-agent` (64 MiB) | The agent's key and state, which must survive the pod being replaced |
| Deployment `drawbridge-agent` | One replica, `Recreate`, non-root (UID 65532), read-only root filesystem, no capabilities; labelled `drawbridge.io/self: "true"` so the agent never stops itself |

## Permissions, and why

| API group | Resources | Verbs | Used for |
| --- | --- | --- | --- |
| `apps` | `deployments`, `statefulsets`, `daemonsets` | `get`, `list` | Listing workloads and checking they are ready |
| `apps` | `deployments/scale`, `statefulsets/scale` | `get`, `patch` | Reading the replica count, scaling to 0 and back |
| `apps` | `daemonsets` | `patch` | Adding and removing the `drawbridge.io/stopped` node selector |
| `batch` | `cronjobs` | `get`, `list`, `patch` | Listing CronJobs and suspending or resuming them |
| `autoscaling` | `horizontalpodautoscalers` | `list` | Warning about workloads an HPA would scale back up |

The agent cannot read pods, Secrets or ConfigMaps, cannot exec, and cannot
create or delete anything.

## Limiting it to some namespaces

1. Delete the ClusterRoleBinding from the file.
2. For each namespace, add a RoleBinding to the same ClusterRole:

   ```yaml
   apiVersion: rbac.authorization.k8s.io/v1
   kind: RoleBinding
   metadata:
     name: drawbridge-agent
     namespace: media
   roleRef:
     apiGroup: rbac.authorization.k8s.io
     kind: ClusterRole
     name: drawbridge-agent
   subjects:
     - kind: ServiceAccount
       name: drawbridge-agent
       namespace: drawbridge
   ```

3. Set `DRAWBRIDGE_AGENT_ALLOW` to the namespaces, comma-separated
   (`media,downloads`). The agent then lists each of them on its own, which a
   RoleBinding allows; without the variable it lists all namespaces at once,
   which needs the ClusterRoleBinding.

## GitOps and autoscalers

Drawbridge changes the replica count, a DaemonSet's node selector or a
CronJob's `suspend`. Tools that keep the cluster in line with Git, and
HorizontalPodAutoscalers, change them back. The Containers page warns about
such workloads:

- **Argo CD:** ignore the fields Drawbridge changes in the Application:

  ```yaml
  spec:
    ignoreDifferences:
      - group: apps
        kind: Deployment
        jsonPointers: [/spec/replicas]
      - group: apps
        kind: StatefulSet
        jsonPointers: [/spec/replicas]
      - group: apps
        kind: DaemonSet
        jsonPointers: [/spec/template/spec/nodeSelector/drawbridge.io~1stopped]
      - group: batch
        kind: CronJob
        jsonPointers: [/spec/suspend]
    syncPolicy:
      syncOptions: [RespectIgnoreDifferences=true]
  ```

- **Flux:** suspend the Kustomization or HelmRelease during an outage, or
  leave those workloads to keep running.
- **HPA:** an autoscaled workload is scaled back up to the HPA's
  `minReplicas`, which is at least 1. Set such workloads to keep running.
