# Data model

Drawbridge stores everything in one SQLite database,
`$DRAWBRIDGE_DB_DIR/drawbridge.db`, opened with:

```
journal_mode = WAL      concurrent readers while writing; -wal and -shm files
busy_timeout = 5000     wait up to 5 s for a lock
foreign_keys = 1
```

Conventions: timestamps are Unix seconds (UTC) in `INTEGER` columns; days are
`'YYYY-MM-DD'` strings in UTC; booleans are `0`/`1`. Zero timestamps mean
"never".

## Migrations

Schema changes are [goose](https://github.com/pressly/goose) SQL migrations in
`internal/store/migrations`, embedded in the binary and applied in order at
every start-up (`goose.Up`). Applied versions are tracked in
`goose_db_version`.

| Version | File | Change |
| --- | --- | --- |
| 1 | `00001_init.sql` | settings, isps, asns, asn_daily, checks, wan_state, events |
| 2 | `00002_docker.sql` | stacks, containers, runs, run_actions |
| 3 | `00003_users.sql` | users, sessions |
| 4 | `00004_oidc.sql` | oidc_identities |
| 5 | `00005_flapping.sql` | time-based state machine: `wan_state` gains `pending_since`, `last_evidence`, `flaps`, `last_failover`, `last_failback`; drops `pending_count` |
| 6 | `00006_appearance.sql` | `users` gains `theme` and `mode` |
| 7 | `00007_container_labels.sql` | `containers` gains `label_policy` |
| 8 | `00008_user_pictures.sql` | user_pictures |
| 9 | `00009_sso_picture.sql` | `users` gains `sso_picture` |
| 10 | `00010_outages.sql` | outages, outage_usage. At the first start with an empty `outages` table, past outages are recreated from the event log |
| 11 | `00011_metered.sql` | `isps` gains `metered` (default 1) |
| 12 | `00012_api_tokens.sql` | api_tokens |
| 13 | `00013_two_factor.sql` | `users` gains `totp_secret` and `totp_step`; recovery_codes |
| 14 | `00014_hosts.sql` | hosts, with host 1 "local". `containers`, `stacks`, `run_actions` and `outage_usage` are rebuilt with a `host_id` in their primary key, existing rows on host 1; `run_actions` gains `restore` and `pending` |
| 15 | `00015_agents.sql` | `hosts` gains `version`, `public_key`, `previous_key`, `key_created`, `enroll_hash`, `enroll_expires`, `last_address`, `inventory`, `manager_key_id`; unique index on `enroll_hash` where set |

Each file has an `Up` and a `Down` section. Downgrades are not run by
Drawbridge.

Backup files (`internal/backup`) carry tables row by row with their column
names, and record the schema version they were made at. A restore accepts
backups from the same or an older schema: columns added since then take their
defaults. A migration that renames or drops a column of a backed-up table
(`store.ConfigTables`, `checks`, `events`, `asn_daily`, `outages`,
`outage_usage`) must also teach
`store.Import` to translate older backups. A backup from before hosts has no
`hosts` table and no `host_id` columns: its rows take `host_id` 1, and the
local host is created.

## Tables

### settings

Key-value store of JSON values.

| Column | Type | Notes |
| --- | --- | --- |
| `key` | TEXT PK | |
| `value` | TEXT | JSON |

| Key | Value |
| --- | --- |
| `detection` | `config.Detection` as JSON: `check_interval`, `failover_after`, `failback_after`, `failback_max_hold`, `quorum`, `timeout`, `learning_period`, `ip_providers`, `retention`. Durations in nanoseconds. Missing fields take defaults when loaded. |
| `setup_complete` | `true` once the wizard has finished |
| `paused` | `true` while automation is paused |
| `paused_until` | end of a timed pause (RFC 3339), or the zero time `0001-01-01T00:00:00Z` when the pause has no end |
| `oidc` | single sign-on settings (`web.OIDCSettings`); `client_secret` is sealed (`v1:…`) |
| `backup_schedule` | `enabled`, `time` (`HH:MM`), `keep`, `history`, `passphrase` (sealed) |
| `backup_schedule_last` | the latest scheduled backup: `at`, `file`, `error`, and `success` (the latest that worked) |
| `test_schedule` | `enabled`, `every` (`week` or `month`), `weekday` (0–6), `day` (1–28), `time` (`HH:MM`), `minutes` |
| `test_schedule_last` | `slot` (the scheduled time handled last), `at`, `result` (`started`, or why not) |
| `manager_keys` | The manager's agent signing keys: `current` (ID) and `keys`, ID (`k1`, `k2`, …) to sealed `ed25519-private:` key. See [Agent protocol](agent-protocol.md#keys) |
| `notifications` | `notify.Settings`: `channels`, each with `id`, `name`, `kind` (`ntfy`, `gotify`, `pushover`, `telegram`, `discord`, `email`, `webhook`), `url`, `token` and `user` (all sealed), `to`, `from`, `topics`, `enabled` |

### isps

ISP groups.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | INTEGER PK | |
| `group_key` | TEXT UNIQUE | From `GroupKey`, or `manual-AS<n>` for split-out ASNs |
| `name` | TEXT | Initially the group key |
| `display_name` | TEXT | Set by Rename; `''` = use `name` |
| `role` | TEXT | `primary`, `backup` (default), `ignored` |
| `pinned` | INTEGER | Primary chosen by a person |
| `metered` | INTEGER | Default 1. Only a metered backup triggers a failover; an unmetered one counts like the primary |
| `first_seen`, `last_seen` | INTEGER | |

Index `isps_one_primary`: unique on `role` where `role = 'primary'`, so at most
one primary.

### asns

| Column | Type | Notes |
| --- | --- | --- |
| `asn` | INTEGER PK | |
| `name` | TEXT | Latest non-empty name seen |
| `isp_id` | INTEGER → isps | |
| `manual` | INTEGER | Grouped by hand; never regrouped |
| `observed_seconds` | INTEGER | All-time observed time |
| `first_seen`, `last_seen` | INTEGER | |

### asn_daily

Observed time per ASN per UTC day, for learning and the "Last 7 days" column.

| Column | Type |
| --- | --- |
| `asn` | INTEGER |
| `day` | TEXT |
| `observed_seconds` | INTEGER |

Primary key `(asn, day)`.

### checks

One row per check.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | INTEGER PK | |
| `ts` | INTEGER | Indexed |
| `ip` | TEXT | NULL when the probe was inconclusive |
| `asn` | INTEGER | NULL when not resolved; set for ignored ISPs even though the check counts as inconclusive |
| `conclusive` | INTEGER | As used by the state machine (ignored ISPs are 0) |
| `providers` | TEXT | JSON array: `url`, `ip`, `error`, `latency_ms` per provider |
| `latency_ms` | INTEGER | Slowest provider |

### wan_state

Single row (`id = 1`), the persisted state machine.

| Column | Type | Notes |
| --- | --- | --- |
| `state` | TEXT | `learning`, `primary`, `failover_pending`, `failover`, `failback_pending` |
| `since` | INTEGER | When the state began |
| `current_asn` | INTEGER | ASN of the last conclusive check; NULL before the first |
| `learning_started_at` | INTEGER | Start of the learning period |
| `pending_since` | INTEGER | See [WAN detection](wan-detection.md#persisted-fields) |
| `last_evidence` | INTEGER | |
| `flaps` | INTEGER | |
| `last_failover`, `last_failback` | INTEGER | |

### events

Audit log. See [Monitoring → event types](../user/monitoring.md#event-types).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | INTEGER PK | Paging cursor |
| `ts` | INTEGER | Indexed |
| `type` | TEXT | |
| `actor` | TEXT | Always `system` at present; the acting user is in the payload's `by` |
| `payload` | TEXT | JSON object |

### outages

One row per outage: from a failover until the failback. `store.SaveState`
keeps it in step with `wan_state`, in the same transaction: entering
`failover` or `failback_pending` with no open outage opens one, and any other
state closes it, both at `wan_state.since`. The [Reports
page](../user/web-ui.md#reports) reads it.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | INTEGER PK | Paging cursor |
| `started` | INTEGER | Indexed |
| `ended` | INTEGER | NULL while it lasts. A unique partial index allows one open outage |
| `isp_id` | INTEGER | The first ISP that is not the primary seen during it (from `wan_state.current_asn`); NULL until one is. No foreign key: the report shows the ISP's current name, or nothing if it is gone |
| `started_by` | TEXT | User of Fail over now; `''` when automatic |
| `ended_by` | TEXT | User of Restore now; `''` when automatic |

When the table is empty at start-up, and when a backup without `outages` is
restored with history, outages are recreated from `events`: a
`state_changed` or `manual_failover` event to `failover` from any other state
than `failover` or `failback_pending` starts one, and the next such event (or
`manual_restore`) from one of those two to another state ends it.

### outage_usage

Network traffic per container and outage, written by
`executor.WatchUsage` (see [How data is measured](../user/web-ui.md#how-data-is-measured)).

| Column | Type | Notes |
| --- | --- | --- |
| `outage_id` | INTEGER | PK with `host_id` and `container`. No foreign key |
| `host_id` | INTEGER | The container's host; default 1. No foreign key |
| `container` | TEXT | Container key: `project/service` or the name |
| `container_id` | TEXT | Docker ID at the last reading; a new one counts in full |
| `rx`, `tx` | INTEGER | Bytes received and sent during the outage |
| `last_rx`, `last_tx` | INTEGER | Docker's counters at the last reading |
| `ts` | INTEGER | Last reading |

### hosts

The places Drawbridge controls. Host 1 is the local Docker socket and always
exists; the others are [agents](../user/hosts.md).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | INTEGER PK | |
| `name` | TEXT UNIQUE | `local` for host 1 |
| `kind` | TEXT | `local` or `agent` |
| `platform` | TEXT | `docker`; for agents, as the agent reports it |
| `status` | TEXT | `pending`, `online`, `offline`, `revoked`, `disabled`. The executor sets `online` or `offline` each time it lists the host; it never changes `revoked` or `disabled` |
| `last_seen` | INTEGER | The last time the host answered (for an agent: its last accepted request); NULL if never |
| `last_error` | TEXT | Why the host could not be listed the last time; `''` when it answered |
| `created_at`, `created_by` | INTEGER, TEXT | |
| `version` | TEXT | Agents: the version the agent reported |
| `public_key` | TEXT | Agents: the current `ed25519:` key; `''` before enrollment and after a revoke or a new token |
| `previous_key` | TEXT | Agents: the key the last rotation replaced, accepted until the new one is first used |
| `key_created` | INTEGER | Agents: when `public_key` was set; keys older than 90 days are refused |
| `enroll_hash` | TEXT | Agents: SHA-256 of the one-time enrollment token; `''` once used. Unique where set |
| `enroll_expires` | INTEGER | Agents: when the token stops working (1 hour after it was made) |
| `last_address` | TEXT | Agents: the client address of the last accepted request |
| `inventory` | TEXT | Agents: JSON of the workloads the agent last pushed, so the manager knows them after a restart |
| `manager_key_id` | TEXT | Agents: the manager key the agent has pinned; commands to it are signed with that key |

### stacks

| Column | Type | Notes |
| --- | --- | --- |
| `host_id` | INTEGER | PK with `project`; default 1. No foreign key |
| `project` | TEXT | Compose project name |
| `disable_on_failover` | INTEGER | The stack switch |
| `updated_at`, `updated_by` | INTEGER, TEXT | |

Deleted by the inventory sync of its host once no container on that host has
its `project`. The same project on two hosts is two stacks.

### containers

| Column | Type | Notes |
| --- | --- | --- |
| `host_id` | INTEGER | PK with `key`; default 1. No foreign key |
| `key` | TEXT | `project/service` or container name |
| `project`, `service` | TEXT | `''` for standalone containers |
| `name` | TEXT | Latest container name |
| `policy` | TEXT | `inherit` (default), `disable`, `keep` |
| `label_policy` | TEXT | From the `drawbridge.on-failover` label at the last sync: `''`, `disable` or `keep` |
| `needs_review` | INTEGER | New container without a label in a stack with per-container settings |
| `first_seen`, `last_seen` | INTEGER | The inventory sync of the host deletes its rows with `last_seen` over 10 minutes old (`store.GoneAfter`); a host that cannot be reached keeps its rows |

Index `containers_project` on `(host_id, project)`.

### runs

| Column | Type | Notes |
| --- | --- | --- |
| `id` | INTEGER PK | |
| `started_at`, `ended_at` | INTEGER | |
| `trigger` | TEXT | `auto`, `manual`, or `drill` (a failover test) |
| `to_asn` | INTEGER | Backup ASN at failover; NULL for manual failovers before any check |
| `status` | TEXT | `open`, `completed`, `failed` |

Index `runs_one_open`: unique on `status` where `status = 'open'`.

### run_actions

| Column | Type | Notes |
| --- | --- | --- |
| `run_id` | INTEGER → runs | |
| `host_id` | INTEGER | The container's host; default 1. No foreign key |
| `container_id` | TEXT | The ID stopped |
| `container_key` | TEXT | For finding a re-created container |
| `name` | TEXT | |
| `was_running` | INTEGER | Running when first recorded; never overwritten |
| `stopped_at`, `started_at` | INTEGER | NULL until done |
| `error` | TEXT | Last error; `''` after a success |
| `restore` | TEXT | What starting it again needs, from the host's runtime; `''` for Docker containers |
| `pending` | INTEGER | 1: a start owed to a host that could not be reached when the run ended. Cleared when it is started (or fails), or when a new run takes it over |

Primary key `(run_id, host_id, container_id)`. Index `run_actions_pending` on
`pending` where `pending = 1`.

### users

| Column | Type | Notes |
| --- | --- | --- |
| `id` | INTEGER PK | |
| `username` | TEXT UNIQUE COLLATE NOCASE | Case-insensitive |
| `password_hash` | TEXT | argon2id PHC string; NULL for SSO-only users |
| `role` | TEXT | `admin` or `viewer` |
| `disabled` | INTEGER | |
| `created_at`, `last_login` | INTEGER | |
| `theme` | TEXT | Colour theme: `default`, `nord`, `solarized`, `gruvbox`, `catppuccin`, `tokyo-night`; default `default` |
| `mode` | TEXT | `auto`, `light` or `dark`; default `auto` |
| `sso_picture` | INTEGER | Whether single sign-on may set the profile picture; default 1. Turned off when the user removes a picture that came from it |
| `totp_secret` | TEXT | Two-factor sign-in: the TOTP secret, sealed with the instance key; `''` = off |
| `totp_step` | INTEGER | The last 30-second time step whose code was accepted; codes for it or earlier steps are refused |

### recovery_codes

| Column | Type | Notes |
| --- | --- | --- |
| `user_id` | INTEGER → users, ON DELETE CASCADE | PK with `hash` |
| `hash` | TEXT | Hex SHA-256 of the code (`xxxx-xxxx-xxxx`, 12 characters from 31 letters and digits); deleted when used |

### sessions

| Column | Type | Notes |
| --- | --- | --- |
| `id` | TEXT PK | SHA-256 (hex) of the cookie token; the token itself is never stored |
| `user_id` | INTEGER → users, ON DELETE CASCADE | |
| `csrf` | TEXT | Per-session CSRF token |
| `created_at`, `expires_at` | INTEGER | |
| `ip`, `user_agent` | TEXT | At sign-in (user agent truncated to 256 bytes) |

### oidc_identities

| Column | Type | Notes |
| --- | --- | --- |
| `issuer`, `subject` | TEXT | Primary key |
| `user_id` | INTEGER → users, ON DELETE CASCADE | |
| `email` | TEXT | Updated at each link |
| `linked_at` | INTEGER | |

### api_tokens

Tokens for `/api/status`, made in Settings → API tokens.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | INTEGER PK | |
| `name` | TEXT | 1–64 characters |
| `hash` | TEXT UNIQUE | Hex SHA-256 of the token (`dbt_` + 43 base64url characters from 32 random bytes); the token itself is not stored |
| `created_at` | INTEGER | |
| `created_by` | TEXT | User who made it |
| `last_used` | INTEGER | 0 = never |

Not in backup files.

### user_pictures

| Column | Type | Notes |
| --- | --- | --- |
| `user_id` | INTEGER PK → users, ON DELETE CASCADE | |
| `content_type` | TEXT | Sniffed: `image/png`, `image/jpeg`, `image/gif` or `image/webp` |
| `data` | BLOB | At most 1 MiB (`web.MaxPictureBytes`) |
| `source` | TEXT | `upload` or `sso`. An `sso` picture never replaces an `upload` one |
| `updated_at` | INTEGER | Also the `?v=` of the picture URL, so a new picture is a new URL |

Not in backup files.

## Retention

Housekeeping runs at start-up and every 24 hours:

| Data | Kept |
| --- | --- |
| `checks` | `Retention` (default 30 days) |
| `asn_daily` | `max(Retention, 8 days)`, so learning always has 7 full days |
| `sessions` | Until `expires_at` |
| Everything else | Indefinitely |

Pruning deletes rows; the file does not shrink until SQLite reuses the pages.
At a 1-minute interval, a day of checks is about 1,440 rows.

## Reading the database by hand

The database is safe to read while Drawbridge runs:

```sh
sqlite3 -readonly db/drawbridge.db \
  "SELECT datetime(ts,'unixepoch'), type, payload FROM events ORDER BY id DESC LIMIT 20"
```

Do not write to it while Drawbridge runs; the monitor holds state in memory
and would overwrite or ignore your changes. Use the web UI or the CLI.
