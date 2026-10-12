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

Each file has an `Up` and a `Down` section. Downgrades are not run by
Drawbridge.

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
| `oidc` | single sign-on settings (`web.OIDCSettings`); `client_secret` is sealed (`v1:…`) |

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

### stacks

| Column | Type | Notes |
| --- | --- | --- |
| `project` | TEXT PK | Compose project name |
| `disable_on_failover` | INTEGER | The stack switch |
| `updated_at`, `updated_by` | INTEGER, TEXT | |

### containers

| Column | Type | Notes |
| --- | --- | --- |
| `key` | TEXT PK | `project/service` or container name |
| `project`, `service` | TEXT | `''` for standalone containers |
| `name` | TEXT | Latest container name |
| `policy` | TEXT | `inherit` (default), `disable`, `keep` |
| `needs_review` | INTEGER | New container in a stack with per-container settings |
| `first_seen`, `last_seen` | INTEGER | |

### runs

| Column | Type | Notes |
| --- | --- | --- |
| `id` | INTEGER PK | |
| `started_at`, `ended_at` | INTEGER | |
| `trigger` | TEXT | `auto` or `manual` |
| `to_asn` | INTEGER | Backup ASN at failover; NULL for manual failovers before any check |
| `status` | TEXT | `open`, `completed`, `failed` |

Index `runs_one_open`: unique on `status` where `status = 'open'`.

### run_actions

| Column | Type | Notes |
| --- | --- | --- |
| `run_id` | INTEGER → runs | |
| `container_id` | TEXT | The ID stopped |
| `container_key` | TEXT | For finding a re-created container |
| `name` | TEXT | |
| `was_running` | INTEGER | Running when first recorded; never overwritten |
| `stopped_at`, `started_at` | INTEGER | NULL until done |
| `error` | TEXT | Last error; `''` after a success |

Primary key `(run_id, container_id)`.

### users

| Column | Type | Notes |
| --- | --- | --- |
| `id` | INTEGER PK | |
| `username` | TEXT UNIQUE COLLATE NOCASE | Case-insensitive |
| `password_hash` | TEXT | argon2id PHC string; NULL for SSO-only users |
| `role` | TEXT | `admin` or `viewer` |
| `disabled` | INTEGER | |
| `created_at`, `last_login` | INTEGER | |

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
