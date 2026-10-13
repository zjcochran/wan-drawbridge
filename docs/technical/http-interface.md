# HTTP interface

All routes are served by `web.Server.Handler()` using Go's `net/http` pattern
router. The interface is the web UI; there is no separate JSON API. Requests
from HTMX carry `HX-Request: true`; for those, redirects are sent as
`HX-Redirect` headers with status 204, and errors (4xx/5xx) are shown as toast
messages instead of replacing page content.

## Middleware order

From the outside in:

1. `securityHeaders`: sets the headers listed in [Security](security.md#response-headers).
2. `http.CrossOriginProtection`: rejects cross-origin unsafe requests.
3. `requireSetup`: until a user exists, redirects everything except
   `/healthz`, `/metrics`, `/static/…` and `/setup…` to `/setup`.
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
| GET | `/static/…` | Embedded assets, `Cache-Control: public, max-age=3600`. |
| GET | `/login` | Sign-in page; redirects to `/` if already signed in. Query `next`. |
| POST | `/login` | Form `username`, `password`, `next`. 401 on wrong credentials, 429 when rate-limited, 403 when password sign-in is off. Sets the session cookie and redirects to `next` (local paths only). |
| POST | `/logout` | Deletes the session, clears the cookie, redirects to `/login`. |
| GET | `/auth/oidc/login` | Starts single sign-on. Query `next`; `link=1` links to the signed-in account. 404 when SSO is off. |
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
| GET | `/stacks` | auth | `?partial=1` with `HX-Request` returns the table only. |
| GET | `/stacks/{project}/containers` | auth | Container list fragment for one stack. |
| GET | `/containers` | auth | Filters: `q`, `stack` (`-` = standalone), `state` (`running`, `stopped`, `gone`), `policy` (`disable`, `keep`, `review`); `partial=1`. |
| GET | `/isps` | auth | |
| GET | `/events` | auth | Query `type`, `before` (event ID cursor). 100 per page. |
| GET | `/events.csv` | auth | Query `type`. Up to 1,000 newest events; columns `time` (RFC 3339 UTC), `type`, `actor`, `details`. Cells starting with `= + - @ TAB CR` are prefixed with `'` against spreadsheet formula injection. |
| GET | `/settings` | auth | |
| GET | `/users` | admin | |

### Actions

All POST, all return an HTML fragment (or `HX-Redirect`), all need the CSRF
token.

| Path | Auth | Form | Effect |
| --- | --- | --- | --- |
| `/actions/pause` | admin | `for`: `30m`, `1h`, `4h`, `12h`, `24h`, or empty for until resumed; anything else is 400 | Pause automation |
| `/actions/resume` | admin | | Resume automation and reconcile |
| `/actions/check` | admin | | Run a check now |
| `/actions/failover` | admin | | Fail over now |
| `/actions/restore` | admin | | Restore now |
| `/stacks/{project}/policy` | admin | `disable=on` or absent | Set the stack switch |
| `/containers/policy` | admin | `key`, `policy` | Set one container (filters are echoed back) |
| `/containers/bulk` | admin | `keys` (repeated), `bulk_policy` | Set several containers |
| `/isps/{id}/role` | admin | `role` | Primary (pinned), backup or ignored |
| `/isps/{id}/rename` | admin | `name` | Empty resets |
| `/isps/relearn` | admin | | Forget the primary, restart learning |
| `/asns/{asn}/move` | admin | `to` = ISP ID or `new` | Regroup an ASN |
| `/settings/detection` | admin | detection fields | Validate and apply |
| `/settings/password` | auth | `current`, `new`, `confirm` | Change own password; ends all own sessions |
| `/settings/appearance` | auth | `theme`, `mode` | Save own colour theme and mode; 204, or 400 for an unknown value |
| `/settings/sso` | admin | SSO fields | Save single sign-on settings |
| `/settings/sso/test` | admin | `issuer` | Try discovery |
| `/settings/sso/unlink` | auth | | Unlink own SSO identities (refused without a password) |
| `/settings/restore` | admin | Multipart: `file`, `passphrase`; body at most 128 MB | Restore a backup file; ends every session; `HX-Redirect: /login` |
| `/users` | admin | `username`, `password`, `role` | Create user |
| `/users/{id}/disable` | admin | | Not yourself; not the last admin |
| `/users/{id}/enable` | admin | | |
| `/users/{id}/password` | admin | `password` | Reset; ends their sessions |

Validation errors return **422** with a plain-text message; unexpected errors
return **500** "something went wrong: …" and are logged.

`POST /settings/backup` (admin) is a plain form post, not HTMX, so the browser
saves the response. Form: `_csrf`, `history` (`none`, `30`, `60`, `90`,
`all`), `passphrase`, `confirm`. It answers with the backup file
(`Content-Type: application/json`, `Content-Disposition: attachment`,
`Cache-Control: no-store`), or, if the form is invalid, 303 to
`/settings?flash=<reason>`.

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
