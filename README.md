# WAN Drawbridge

Watches which ISP your traffic leaves through. When it moves from the primary
ISP to a backup (a cellular failover, say), Drawbridge stops a chosen set of
Docker stacks and containers; when the primary is back and stable, it starts
them again.

![Dashboard during a failover](docs/images/dashboard-failover.jpg)

- Recognises ISPs by the network (ASN) your public IP belongs to, and groups a
  provider's ASNs into one ISP.
- Learns your primary ISP, or lets you pin it.
- Confirms a failover for 45 seconds and a failback for 5 minutes, and backs
  off further when the primary keeps dropping.
- What stops is chosen per stack or per container, in the UI or with a
  `drawbridge.on-failover` label in your Compose files.
- Stops dependents first and starts dependencies first, waiting for
  healthchecks; only restarts what it stopped; survives restarts mid-run.
- Web UI with a setup wizard, live updates, phone and dark mode support, and six
  colour themes per user; local accounts and single sign-on (Pocket ID or any
  OIDC provider); Prometheus metrics; a CLI for scripting and recovery.
- Encrypted backup files of the configuration, with optional history, that
  restore from Settings or straight from the setup wizard on a new host.
- One container, one SQLite database folder, the Docker socket. No Node.js, no
  external database.

## Quick start

Images for `amd64` and `arm64` are published with every release as
`ghcr.io/zjcochran/wan-drawbridge`. The package is private, so log in to
`ghcr.io` first, then start it with the repository's `compose.yaml`:

```sh
echo '<token>' | docker login ghcr.io -u <github-user> --password-stdin
docker compose up -d
```

Open `http://<host>:8080` and follow the setup wizard. Until the wizard is
finished, Drawbridge checks your connection but does not stop or start any
containers. See [Installation](docs/user/installation.md) for the token,
pinning a version and building from source.

## Documentation

Full documentation is in [`docs/`](docs/README.md).

**User guides:**
[Installation](docs/user/installation.md) ·
[Setup wizard](docs/user/setup-wizard.md) ·
[Using the web UI](docs/user/web-ui.md) ·
[Choosing what to stop](docs/user/stacks-and-containers.md) ·
[ISPs](docs/user/isps.md) ·
[During an outage](docs/user/during-an-outage.md) ·
[Single sign-on](docs/user/single-sign-on.md) ·
[Reverse proxy](docs/user/reverse-proxy.md) ·
[Backup and restore](docs/user/backup-and-restore.md) ·
[Monitoring](docs/user/monitoring.md) ·
[Command line](docs/user/cli.md) ·
[Troubleshooting](docs/user/troubleshooting.md) ·
[FAQ](docs/user/faq.md)

**Technical:**
[Architecture](docs/technical/architecture.md) ·
[WAN detection](docs/technical/wan-detection.md) ·
[ISP grouping and learning](docs/technical/isp-grouping-and-learning.md) ·
[Container control](docs/technical/container-control.md) ·
[Data model](docs/technical/data-model.md) ·
[Security](docs/technical/security.md) ·
[HTTP interface](docs/technical/http-interface.md) ·
[Configuration reference](docs/technical/configuration-reference.md) ·
[Testing and CI](docs/technical/testing-and-ci.md) ·
[Development](docs/technical/development.md)

## Development

```sh
go test -race ./...                                                                  # unit and end-to-end tests
go test -race -coverprofile=coverage.out ./... && test/coverage.sh coverage.out 80   # coverage gate
test/smoke/smoke.sh                                                                  # real Docker smoke test
```

## License

Copyright the WAN Drawbridge contributors. Licensed under the
[GNU Affero General Public License v3.0 or later](LICENSE)
(`AGPL-3.0-or-later`). Every page links to the source of the running version,
as the AGPL requires; set `DRAWBRIDGE_SOURCE_URL` if you run a modified
version.
