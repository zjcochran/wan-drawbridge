# WAN Drawbridge documentation

WAN Drawbridge watches which internet provider (ISP) your traffic leaves
through. When your connection moves from the primary ISP to a backup (a
cellular failover, say), it stops the Docker stacks and containers you chose;
when the primary is back and stable, it starts them again.

![Dashboard during a failover](images/dashboard-failover.jpg)

## User guides

Start here if you run Drawbridge.

| Guide | What it covers |
| --- | --- |
| [Installation](user/installation.md) | Requirements, the Compose file, first start, upgrading, uninstalling |
| [Setup wizard](user/setup-wizard.md) | The seven first-run steps, with screenshots |
| [Using the web UI](user/web-ui.md) | Every page: dashboard, stacks, containers, ISPs, events, settings, users |
| [Choosing what to stop](user/stacks-and-containers.md) | Stack rules, container overrides, new containers, examples |
| [ISPs and the primary ISP](user/isps.md) | How ISPs are recognised and grouped, pinning, learning, fixing a wrong grouping |
| [What happens during an outage](user/during-an-outage.md) | A minute-by-minute walk-through of a failover and failback, flapping, manual controls |
| [Single sign-on](user/single-sign-on.md) | Pocket ID or any OIDC provider: setup, groups, linking, SSO-only, lock-out recovery |
| [Behind a reverse proxy](user/reverse-proxy.md) | Traefik, Caddy, nginx, Nginx Proxy Manager, HAProxy and Apache examples, trusted proxies, HTTPS |
| [Backup and restore](user/backup-and-restore.md) | Encrypted backup files with optional history, folder backups, restoring, moving hosts |
| [Monitoring](user/monitoring.md) | Prometheus metrics, alert rules, the health check, logs |
| [Command line](user/cli.md) | Every `drawbridge` subcommand, with examples |
| [Troubleshooting](user/troubleshooting.md) | Symptoms, causes and fixes |
| [FAQ](user/faq.md) | Short answers to common questions |

## Technical documentation

Start here if you change Drawbridge or need to know exactly how it behaves.

| Document | What it covers |
| --- | --- |
| [Architecture](technical/architecture.md) | Components, goroutines, data flow, startup and shutdown |
| [WAN detection](technical/wan-detection.md) | Probing, quorum, ASN lookup and the failover state machine, with exact timings |
| [ISP grouping and learning](technical/isp-grouping-and-learning.md) | Group keys, time accounting, the learner's rules |
| [Container control](technical/container-control.md) | Policies, inventory, runs, dependency order, health waits, crash recovery |
| [Data model](technical/data-model.md) | Every SQLite table and column, migrations, retention |
| [Security](technical/security.md) | Authentication, sessions, CSRF, headers, proxies, secrets, threat model |
| [HTTP interface](technical/http-interface.md) | Every route, Server-Sent Events, metrics format |
| [Configuration reference](technical/configuration-reference.md) | Every environment variable and stored setting, with limits |
| [Testing and CI](technical/testing-and-ci.md) | Test layers, fakes, the coverage gate, GitLab pipeline and reports |
| [Development](technical/development.md) | Building, code layout, conventions, adding migrations, releasing |

## Conventions in these documents

- **ISP** means a group of one or more networks (ASNs) that belong to the same
  provider, for example Examplenet's AS64500 and AS64501. Drawbridge always
  compares ISPs, never single ASNs.
- **Primary** is the ISP you normally use. Every other ISP is a **backup**.
- **Failover** is Drawbridge stopping your selected containers because traffic
  is on a backup ISP; **failback** is starting them again.
- Durations are written the way Drawbridge accepts them: `45s`, `5m`, `2h`,
  `7d`.
- Defaults are given for an unchanged installation. Most timings can be
  changed in **Settings → WAN detection**.
