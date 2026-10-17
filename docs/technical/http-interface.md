# HTTP interface

All routes are served by `web.Server.Handler()` using Go's `net/http` pattern
router. The interface is the web UI, two read-only JSON routes,
`/api/status` and `/api/health`, and the agent API under `/api/agent/v1/`. Requests
from HTMX carry `HX-Request: true`; for those, redirects are sent as
`HX-Redirect` headers with status 204, and errors (4xx/5xx) are shown as toast
messages instead of replacing page content.

## Middleware order

From the outside in:

1. `securityHeaders`: sets the headers listed in [Security](security.md#response-headers).
2. `http.CrossOriginProtection`: rejects cross-origin unsafe requests.
3. `requireSetup`: until a user exists, redirects everything except
   `/healthz`, `/metrics`, `/api/…`, `/static/…` and `/setup…` to `/setup`.
4. The router, with per-route `auth` (signed in; POSTs need the CSRF token)
   or `admin` (auth plus admin role).

Unauthenticated requests to protected routes redirect to
`/login?next=<original URI>`; HTMX requests get 401 with `HX-Redirect: /login`.
Non-admins get 403 "admins only" on admin routes.

## Routes

### Public

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/healthz` | `200 ok`. No auth. |
| GET | `/metrics` | Prometheus text. 404 unless `DRAWBRIDGE_METRICS_TOKEN` is set; then `Authorization: Bearer` required (401). |
| GET | `/api/status` | JSON status; see [Monitoring → JSON status API](../user/monitoring.md#json-status-api). `Authorization: Bearer <API token>` required (401 with `WWW-Authenticate` otherwise); each use sets the token's `last_used`. `Cache-Control: no-store`. |
| GET | `/api/health` | 200 when healthy, 503 with the problems otherwise; see [Monitoring → Health status API](../user/monitoring.md#health-status-api). Query `ignore` (comma-separated: `failover`, `checks`, `inconclusive`, `hosts`, `paused`; 400 for another name). Same token rules as `/api/status`. |
| any | `/api/agent/v1/…` | The [agent API](agent-protocol.md#routes): enroll, commands, results, inventory, rotate. Agents sign their requests; no session or CSRF token. |
| GET | `/static/…` | Embedded assets. Pages link them as `/static/<file>?v=<hash>`, where the hash covers every static file, so an upgrade that changes one gives new URLs; those get `Cache-Control: public, max-age=31536000, immutable`, a link without `v` gets `public, max-age=3600`. |
| GET | `/login` | Sign-in page; redirects to `/` if already signed in. Query `next`. |
| POST | `/login` | Form `username`, `password`, `next`. 401 on wrong credentials, 429 when rate-limited, 403 when password sign-in is off. Sets the session cookie and redirects to `next` (local paths only). For an account with two-factor sign-in, it instead answers 200 with the code step and sets the `drawbridge_2fa` cookie (path `/login`, HttpOnly, SameSite=Strict, 5 minutes). |
| POST | `/login/code` | Form `code` (an app code or a recovery code), with the `drawbridge_2fa` cookie. 401 for a wrong code or an expired step (5 minutes, 5 tries), 429 when rate-limited. On success, sets the session cookie and redirects to `next` from the password step. |
| POST | `/logout` | Deletes the session, clears the cookie, redirects to `/login`. |
| GET | `/auth/oidc/login` | Starts single sign-on. Query `next`; `link=1` links to the signed-in account and returns to `/settings/account`. 404 when SSO is off. |
| GET | `/auth/oidc/callback` | Completes single sign-on. Query `code`, `state` (or `error`). |

### Setup

| Method | Path | Auth | Notes |
| --- | --- | --- | --- |
| GET | `/setup` | none for step 1; admin afterwards | Query `step` (1–7). |
| POST | `/setup/admin` | none; 403 once any user exists | Form `username`, `password`, `confirm`. |
| POST | `/setup/restore` | none; 403 once any user exists | Multipart: `file`, `passphrase`; body at most 128 MB. Restores a backup file and redirects to `/login`; on failure, 422 with step 1 and the reason. |
| POST | `/setup/detection` | admin | Detection form fields (below). Redirects to step 5. |
| POST | `/setup/check` | admin | Runs a check; returns the result fragment. |
| POST | `/setup/primary` | admin | Form `choice` = `pin` (with `isp`) or `learn`. Redirects to step 6. |
| POST | `/setup/finish` | admin | Marks setup complete, resumes automation, redirects to `/`. |

### Pages and fragments

| Method | Path | Auth | Notes |
| --- | --- | --- | --- |
| GET | `/` | auth | Dashboard. Query `flash` shows a message. |
| GET | `/partials/status` | auth | Status card fragment (refreshed on SSE `state`/`inventory`). |
| GET | `/partials/timeline` | auth | 24-hour timeline fragment. |
| GET | `/sse` | auth | Server-Sent Events stream (below). |
| GET | `/stacks` | auth | Filter `host` (a host ID). `?partial=1` with `HX-Request` returns the table only. |
| GET | `/stacks/{host}/{project}/containers` | auth | Container list fragment for one stack on one host (host ID). 404 if `{host}` is not a number. |
| GET | `/containers` | auth | Filters: `q`, `host` (a host ID), `stack` (`-` = standalone), `state` (`running`, `stopped`), `policy` (`disable`, `keep`, `review`), `hidden=1` (include containers matching a container filter); `partial=1`. |
| GET | `/isps` | auth | |
| GET | `/events` | auth | Query `type`, `before` (event ID cursor). 100 per page. |
| GET | `/events.csv` | auth | Query `type`. Up to 1,000 newest events; columns `time` (RFC 3339 UTC), `type`, `actor`, `details`. Cells starting with `= + - @ TAB CR` are prefixed with `'` against spreadsheet formula injection. |
| GET | `/reports` | auth | Query `before` (outage ID cursor). 50 outages per page. |
| GET | `/preview` | auth | Failover preview: what a failover would stop now (`executor.Plan`). Lists every host first, like a sync, and changes nothing else. |
| GET | `/reports/outages/{id}` | auth | One outage and its data used per container; 404 if there is no such outage. |
| GET | `/settings` | auth | The first Settings tab, WAN detection. Query `flash` shows a message. |
| GET | `/settings/detection`, `/settings/container-filters`, `/settings/hosts`, `/settings/account`, `/settings/system` | auth | Settings tabs. Hosts shows its buttons and the manager key to admins only. |
| GET | `/settings/notifications`, `/settings/backup`, `/settings/users`, `/settings/sso`, `/settings/api` | admin | Settings tabs for admins. |
| GET | `/settings/backups/{name}` | admin | Downloads a file from the backup folder. Only names matching `drawbridge-backup-YYYYMMDD-HHMMSS.json` are served; anything else is 404. |
| GET | `/users` | admin | 301 to `/settings/users`, where Users moved. |
| GET | `/users/{id}/picture` | auth | A user's profile picture, for that user and admins; 404 for others or without a picture. `Cache-Control: private, max-age=86400`: links carry `?v=<updated_at>`. |

### Actions

All POST, all return an HTML fragment (or `HX-Redirect`), all need the CSRF
token.

| Path | Auth | Form | Effect |
| --- | --- | --- | --- |
| `/actions/pause` | admin | `for`: `30m`, `1h`, `4h`, `12h`, `24h`, or empty for until resumed; anything else is 400 | Pause automation |
| `/actions/resume` | admin | | Resume automation and reconcile |
| `/actions/check` | admin | | Run a check now |
| `/actions/failover` | admin | | Fail over now |
| `/actions/drill` | admin | `for`: `2m`, `5m`, `10m`, `15m`; anything else is 400 | Start a failover test; 422 if containers are already stopped or the state is not primary or learning |
| `/actions/drill-end` | admin | | End a failover test now |
| `/actions/restore` | admin | | Restore now |
| `/stacks/{host}/{project}/policy` | admin | `disable=on` or absent; `host` = the page's host filter, echoed back | Set the stack switch of a stack on one host (host ID); 404 if `{host}` is not a number |
| `/containers/policy` | admin | `container_host` (host ID; the local host when absent), `key`, `policy` | Set one container (filters are echoed back) |
| `/containers/bulk` | admin | `keys` (repeated, each `<host ID>:<key>`; 422 if one is not), `bulk_policy` | Set several containers |
| `/isps/{id}/role` | admin | `role` | Primary (pinned), backup or ignored |
| `/isps/{id}/rename` | admin | `name` | Empty resets |
| `/isps/{id}/metered` | admin | `metered=on` or absent | Metered or unmetered; 422 for an unknown ISP |
| `/isps/relearn` | admin | | Forget the primary, restart learning |
| `/asns/{asn}/move` | admin | `to` = ISP ID or `new` | Regroup an ASN |
| `/settings/detection` | admin | detection fields | Validate and apply |
| `/settings/container-filters` | admin | `patterns` (one per line) | Save the container filters; returns the form fragment; 422 for more than 100 patterns or one longer than 200 characters |
| `/settings/test-schedule` | admin | `enabled` (`on` or absent), `every` (`week`, `month`), `weekday` (0–6, Sunday first), `day` (1–28), `time` (`HH:MM`), `minutes` (2, 5, 10, 15) | Save the failover test schedule; returns its fragment; 422 with the reason if invalid |
| `/settings/password` | auth | `current`, `new`, `confirm` | Change own password; ends all own sessions |
| `/settings/2fa/start` | auth | | Returns the two-factor card with a new secret, its QR code, and the secret sealed for the next step. Nothing is stored. 422 unless the account has a password, no linked SSO, password sign-in is allowed, and two-factor is off |
| `/settings/2fa/enable` | auth | `sealed`, `code`, `password` | Turn on, if the password and the code for the sealed secret are right; returns the card with the 10 recovery codes. 422 otherwise |
| `/settings/2fa/codes` | auth | `password` | Replace the recovery codes; returns them. 422 when off or the password is wrong |
| `/settings/2fa/off` | auth | `password` | Turn off and delete the recovery codes. 422 when the password is wrong |
| `/settings/appearance` | auth | `theme`, `mode` | Save own colour theme and mode; 204, or 400 for an unknown value |
| `/settings/picture` | auth | `picture` (multipart file) | Upload own profile picture: PNG, JPEG, GIF or WebP by sniffed content, at most 1 MiB; 422 otherwise. Redirects to `/settings/account` |
| `/settings/picture/remove` | auth | | Remove own picture. If it came from single sign-on, `users.sso_picture` is turned off so sign-ins stop setting it. Redirects to `/settings/account` |
| `/settings/picture/sso` | auth | | Turn `users.sso_picture` back on; the picture comes at the next SSO sign-in. Redirects to `/settings/account` |
| `/settings/backup/schedule` | admin | `enabled`, `time` (`HH:MM`), `keep` (1–365), `history`, `passphrase`, `confirm` | Save the backup schedule; a blank passphrase keeps the saved one. Returns the schedule fragment; 422 with the reason if invalid |
| `/settings/backup/run` | admin | | Make a scheduled backup now; returns the schedule fragment |
| `/settings/notifications` | admin | `id` (empty to add), `name`, `kind`, `url`, `token`, `remove_token`, `topics` (repeated), `enabled` | Add or update a channel; blank `url`/`token` keep the saved ones. Returns the channel list; 422 with the reason if invalid, 404 for an unknown `id` |
| `/settings/notifications/delete` | admin | `id` | Delete a channel; returns the channel list |
| `/settings/notifications/test` | admin | as above | Send a test message with the form's values (saved ones for blanks); returns "✓ Sent…" or "✗ error" |
| `/settings/sso` | admin | SSO fields | Save single sign-on settings |
| `/settings/sso/test` | admin | `issuer` | Try discovery |
| `/settings/api-tokens` | admin | `name` (1–64 characters) | Make an API token; returns the tab fragment showing the token once; 422 for a bad name |
| `/settings/api-tokens/{id}/delete` | admin | | Revoke a token; 422 for an unknown one |
| `/settings/hosts` | admin | `name` (see [Hosts and agents](../user/hosts.md#adding-a-docker-host) for the rules) | Add an agent host; returns the tab fragment with its one-time token in an example per platform (Docker host, Docker Swarm, Proxmox VE, Kubernetes); 422 for a bad or taken name |
| `/settings/hosts/{id}/{op}` | admin | `name` for `rename` | `op`: `rename`, `token` (new enrollment token, shown once), `revoke`, `disable`, `enable`, `remove`, `rotate` (the agent rotates its key at its next poll), `forget` (owed starts). Returns the tab fragment; 404 for an unknown `op` or an `{id}` that is not a number; 422 with the reason otherwise (unknown host, removing the local host, a bad name) |
| `/settings/hosts/manager-key` | admin | | Rotate the manager key; returns the tab fragment |
| `/settings/sso/unlink` | auth | | Unlink own SSO identities (refused without a password); redirects to `/settings/account` |
| `/settings/restore` | admin | Multipart: `file`, `passphrase`; body at most 128 MB | Restore a backup file; ends every session; `HX-Redirect: /login` |
| `/settings/users` | admin | `username`, `password`, `role` | Create user |
| `/settings/users/{id}/disable` | admin | | Not yourself; not the last admin |
| `/settings/users/{id}/enable` | admin | | |
| `/settings/users/{id}/role` | admin | `role` (`viewer`, `admin`) | Not yourself; not the last enabled admin; not an account without a password while an SSO admin group is set |
| `/settings/users/{id}/delete` | admin | | Not yourself; not the last enabled admin. Their sessions and SSO links go too. |
| `/settings/users/{id}/two-factor` | admin | | Turn off that user's two-factor sign-in and delete their recovery codes |
| `/settings/users/{id}/password` | admin | `password` | Reset; ends their sessions. Refused for accounts without a password (SSO only). |

Validation errors return **422** with a plain-text message; unexpected errors
return **500** "something went wrong: …" and are logged.

`POST /settings/backup` (admin) is a plain form post, not HTMX, so the browser
saves the response. Form: `_csrf`, `history` (`none`, `30`, `60`, `90`,
`all`), `passphrase`, `confirm`. It answers with the backup file
(`Content-Type: application/json`, `Content-Disposition: attachment`,
`Cache-Control: no-store`), or, if the form is invalid, 303 to
`/settings/backup?flash=<reason>`.

### Detection form fields

`check_interval`, `failover_after`, `failback_after`, `failback_max_hold`,
`timeout`, `learning_period`, `retention` (durations: Go syntax plus `d` for
days), `quorum` (integer), `ip_providers` (one URL per line or
comma-separated). All errors are reported together.

### SSO form fields

`enabled`, `issuer`, `client_id`, `client_secret` (blank keeps the saved one),
`clear_secret`, `display_name`, `allowed_group`, `admin_group`,
`auto_provision`, `sso_only` (checkboxes are `on` or absent), and `from=setup`
in the wizard.

## Server-Sent Events

`GET /sse` (signed in) returns `text/event-stream` with
`Cache-Control: no-cache` and `X-Accel-Buffering: no`. The stream starts with
the comment `: connected`, sends `: keepalive` every 25 seconds, and sends
events:

```
event: state
data: 1791750580

event: inventory
data: 1791750582
```

| Event | Published when |
| --- | --- |
| `state` | The monitor's status changed (every check, state change, pause, run progress) |
| `inventory` | Containers were added, removed, started or stopped, or a run changed |

Publishing never blocks, and bursts are coalesced: many changes in quick
succession produce one event per topic. `data` is the Unix time; clients only
use the event name. Pages subscribe with
HTMX's SSE extension (`hx-ext="sse" sse-connect="/sse"` on `<body>`) and
`hx-trigger="sse:state"` on the fragments to refresh, which then re-fetch
themselves. Each client has an 8-event buffer; a slow client misses events
rather than blocking others (the next event refreshes it anyway).

## Metrics format

See [Monitoring](../user/monitoring.md#metric-reference) for the metric list.
The response is Prometheus text exposition format 0.0.4 with `# HELP` and
`# TYPE` lines; label values are escaped.
