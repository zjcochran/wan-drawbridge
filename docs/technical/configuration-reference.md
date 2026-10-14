# Configuration reference

Drawbridge is configured with environment variables. Detection settings can
also be changed in the web UI (Settings → WAN detection), which stores them in
the database.

**Precedence for detection settings:** built-in default → environment variable
→ value saved in the UI. Once detection settings have been saved in the UI (or
the setup wizard's step 4), the saved values are used and the environment
variables for those settings are ignored. The other variables always come from
the environment.

All variables are read once at start-up. Invalid values stop start-up with
`drawbridge: invalid configuration:` and every problem listed.

## Paths and serving

| Variable | Default | Description |
| --- | --- | --- |
| `DRAWBRIDGE_DB_DIR` | `/data/db` | Folder for `drawbridge.db` and `secret.key`. Created with mode 0750 if missing. Mount it to persist and back up. |
| `DRAWBRIDGE_CACHE_DIR` | `/data/cache` | Folder for the downloaded ASN database. Safe to lose. |
| `DRAWBRIDGE_BACKUP_DIR` | `/data/backups` | Folder for [scheduled backups](../user/backup-and-restore.md#scheduled-backups). Created (mode 0750) when the first one is written. |
| `DRAWBRIDGE_LISTEN` | `:8080` | Address the web server listens on (`host:port`). |
| `DRAWBRIDGE_BASE_URL` | unset | Public URL without a path, e.g. `https://drawbridge.example.com`. A trailing `/` is removed. Used for the OIDC callback, the `Secure` cookie flag, and as a trusted origin. |
| `DRAWBRIDGE_TRUSTED_PROXIES` | unset | Comma- or newline-separated IPs or CIDR ranges of reverse proxies whose `X-Forwarded-*` headers are trusted. An invalid entry stops start-up. |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | `unix://<path>` or `tcp://<host>:<port>` (plain HTTP). |
| `TZ` | UTC | Time zone for the UI and logs (IANA name; zone data is built in). |

## Authentication and secrets

| Variable | Default | Description |
| --- | --- | --- |
| `DRAWBRIDGE_SECRET_KEY` | unset | Any string; its SHA-256 is the AES-256-GCM key for stored secrets. When unset, the key is read from (or generated into) `$DRAWBRIDGE_DB_DIR/secret.key`. Changing it makes the saved OIDC client secret unreadable. |
| `DRAWBRIDGE_LOCAL_LOGIN` | unset | `force`: password sign-in works even if single sign-on only is set. `auto` or unset: follow the setting. Anything else is an error. |
| `DRAWBRIDGE_METRICS_TOKEN` | unset | Enables `/metrics` behind `Authorization: Bearer <token>`. |

## Detection

| Variable | Default | Range | Description |
| --- | --- | --- | --- |
| `DRAWBRIDGE_CHECK_INTERVAL` | `60s` | 15s – 1h | Time between checks. While a change is pending, checks run every `min(15s, interval)`. |
| `DRAWBRIDGE_FAILOVER_AFTER` | `45s` | 0 – 30m | Time a backup ISP must be seen before failover. |
| `DRAWBRIDGE_FAILBACK_AFTER` | `5m` | 0 – 6h | Time the primary must be continuously clean before failback. |
| `DRAWBRIDGE_FAILBACK_MAX_HOLD` | `2h` | 0 – 24h, ≥ failback after | Ceiling for the failback wait while flapping. |
| `DRAWBRIDGE_QUORUM` | `2` | 1 – number of providers | Providers that must agree on the IP. |
| `DRAWBRIDGE_TIMEOUT` | `5s` | 1s – 30s | Per-provider timeout. |
| `DRAWBRIDGE_LEARNING_PERIOD` | `24h` | 1h – 336h | Watching time before a primary is chosen automatically. |
| `DRAWBRIDGE_RETENTION` | `720h` | 24h – 8760h | How long individual checks are kept. |
| `DRAWBRIDGE_IP_PROVIDERS` | `https://1.1.1.1/cdn-cgi/trace`, `https://api.ipify.org`, `https://icanhazip.com` | ≥ 1, each `http://` or `https://` | Comma- or newline-separated URLs. |
| `DRAWBRIDGE_PRIMARY_ASN` | unset | ASN number | Pins the ISP that this ASN belongs to as primary, once seen, overriding the UI. |

Environment variables take Go durations (`90s`, `1h30m`; no `d`). The web UI
also accepts days (`7d`).

## Logging and the source link

| Variable | Default | Description |
| --- | --- | --- |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `DRAWBRIDGE_SYSLOG` | off | Also send the log to a syslog server: `udp://host[:514]`, `tcp://host[:514]` or `tls://host[:6514]`. See [Sending the log to a syslog server](../user/monitoring.md#sending-the-log-to-a-syslog-server). |
| `DRAWBRIDGE_SYSLOG_FORMAT` | `rfc5424` | `rfc5424` or `rfc3164`. |
| `DRAWBRIDGE_SYSLOG_FACILITY` | `daemon` | A syslog facility name: `kern`, `user`, `mail`, `daemon`, `auth`, `syslog`, `lpr`, `news`, `uucp`, `cron`, `authpriv`, `ftp`, `local0` to `local7`. |
| `DRAWBRIDGE_SYSLOG_CA_FILE` | none | `tls://` only: PEM file of extra CAs to trust. |
| `DRAWBRIDGE_SOURCE_URL` | the URL set at build time (published images: `https://github.com/zjcochran/wan-drawbridge`) | Link to the source code in the page footer (AGPL §13). Set it when you run a modified version. |

## Build-time variables

Set with `-ldflags "-X main.version=… -X main.sourceURL=…"`, or the Docker build
arguments `VERSION` and `SOURCE_URL`:

| Variable | Default | Used for |
| --- | --- | --- |
| `main.version` | `dev` | `drawbridge version`, the footer, Settings, `drawbridge_build_info` |
| `main.sourceURL` | empty | Default for `DRAWBRIDGE_SOURCE_URL` |

## Stored settings

Settings changed in the UI live in the `settings` table (see
[Data model](data-model.md#settings)):

| Setting | Changed in |
| --- | --- |
| Detection settings | Settings → WAN detection, setup wizard step 4 |
| Single sign-on | Settings → Single sign-on, setup wizard step 2 |
| Paused, and the end of a timed pause | Dashboard |
| Notification channels | Settings → Notifications |
| Backup schedule, and the outcome of the latest scheduled backup | Settings → Backup and restore |
| Setup complete | Setup wizard step 7 |

Stack and container settings, ISP roles and users are in their own tables;
each user's colour theme and light/dark mode are columns of `users`
(Settings → Your account → Appearance).

## Fixed values

Not configurable; listed for reference.

| Value | Setting |
| --- | --- |
| Flap window (failover this soon after a failback is a flap) | 1 h |
| Flap reset (no failover for this long clears flaps) | 6 h |
| Stale evidence | 3 × check interval |
| Learning: share to become primary | 60% of the last 7 days |
| Learning: share to suggest a new primary | > 80% of the last 3 days, with ≥ 2 days of data |
| ASN database refresh | when older than 7 days, checked daily |
| Inventory sync | every 30 s, and 2 s after container create/destroy/rename |
| Container stop timeout | 30 s |
| Timed pause lengths | 30 min, 1 h, 4 h, 12 h, 24 h, or until resumed |
| Failover test lengths | 2, 5, 10 or 15 min |
| Scheduled backup check | every minute, and once at start |
| Notification queue | 64 events; retried after 5 s and 30 s |
| Syslog queue | 1024 lines; reconnect after 1 s, doubling to 30 s; 10 s connect and send timeouts; 2 s to send what is queued at shutdown |
| Parallel container operations per layer | 4 |
| Health wait per dependency layer | 120 s, polled every 2 s |
| Session lifetime | 7 days, sliding |
| Sign-in limit | 5 failures per 15 minutes, per IP and per username |
| Minimum password length | 12 |
| OIDC sign-in validity | 10 minutes |
| SSE keep-alive | 25 s |
| HTTP read-header timeout | 10 s |
| Graceful shutdown | 5 s |
