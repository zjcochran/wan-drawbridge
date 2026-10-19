# Versions and compatibility

From 1.0.0, WAN Drawbridge follows [semantic versioning](https://semver.org/):
`MAJOR.MINOR.PATCH`. This page says what that promises, so you know which
updates are safe to take without reading every change.

| Release | Means | What you do |
| --- | --- | --- |
| Patch (1.0.**1**) | Fixes and documentation only | Update |
| Minor (1.**1**.0) | New features; everything below keeps working | Update; read the release notes for what is new |
| Major (**2**.0.0) | Something below changes in a way that can break your setup | Read the release notes first: they say what changes and what to do |

## What is covered

Within a major version, these keep working as documented. New ones may be
added in a minor release; existing ones are not removed, renamed or changed in
meaning until the next major version:

- **Configuration:** the environment variables in the
  [configuration reference](../technical/configuration-reference.md), with
  their names, accepted values and defaults; the `/data/db` folder and what it
  holds.
- **Compose labels:** `drawbridge.on-failover` and its values `stop` and
  `keep` (see [Choosing what to stop](stacks-and-containers.md#labels-in-your-compose-files)),
  on Docker containers, Swarm services and Kubernetes workloads, and the
  Proxmox VE tags `drawbridge-stop` and `drawbridge-keep`.
- **The image:** `ghcr.io/zjcochran/wan-drawbridge`, its tags (each release
  as `1.2.3`, the newest of each minor version as `1.2`, and `latest`),
  `command: agent` for an agent, and the health check. A `1.2` tag only ever
  moves to patch releases.
- **The JSON APIs:** [`/api/status`](monitoring.md#json-status-api) and
  [`/api/health`](monitoring.md#health-status-api), their API tokens, their
  fields and their status codes. Fields may be added; clients should ignore
  ones they do not know. Health checks may be added; `ignore` keeps
  accepting the names that exist today.
- **Metrics:** the [Prometheus metric](monitoring.md#metric-reference) names,
  types and labels, and the `/metrics` token.
- **Events and notifications:** the [event types](monitoring.md#event-types)
  and their fields, which webhook channels receive as JSON, and the
  notification topics. Fields may be added; message wording may change.
- **The command line:** the [`drawbridge` subcommands](cli.md), their
  arguments and their exit codes. The layout of their human-readable output
  may change.
- **Backup files:** a backup made by any 1.x version restores into the same
  or any later 1.x version, from Settings or the setup wizard. (A backup
  cannot be restored into an older version than the one that made it.)
- **Agents:** the agent API (`/api/agent/v1/`, see the
  [agent protocol](../technical/agent-protocol.md)) and its keys. Within a
  major version, an agent keeps working with a newer manager, so you can
  update the manager first and the agents after. Keep them on the same version
  when you can: the Hosts page marks an agent whose version differs.
- **Your data:** upgrading keeps the database, settings, users, ISPs, stack
  and container settings, hosts and history. Database migrations run by
  themselves at start.

## What is not covered

These can change in any release:

- **The web UI:** page layout, wording, colours, and the addresses of pages
  and forms other than the APIs above. Bookmarks of pages keep working where
  possible, but scripts should use the APIs.
- **Log messages** and the human-readable output of the command line.
- **Default timings** you have not set yourself, when a release says why.
  Settings you saved are kept.
- **The Go packages** in `internal/`: they are not a library.
- **Experimental platforms:** [Docker Swarm, Proxmox VE and Kubernetes
  agents](hosts.md#tested-so-far) are marked experimental, because they have
  not yet been tested on real equipment. How they stop and start workloads,
  and the permissions they need, may change in a minor release until they
  are no longer marked experimental.

## Upgrading and downgrading

- Upgrade from any earlier version, including any 0.x, straight to the newest
  release: migrations run one after the other. See
  [Upgrading](installation.md#upgrading).
- Downgrading is not supported once a newer version has migrated the
  database. Back up the `db` folder, or make a
  [backup file](backup-and-restore.md), before you upgrade if you might want
  to go back, and restore that into the older version.
- Security fixes go into the newest release only.
