# Monitoring

Drawbridge offers four ways to watch it: Prometheus metrics, a health check,
the event log, and its container log.

## Prometheus metrics

Metrics are **off** unless you set a token:

```yaml
environment:
  DRAWBRIDGE_METRICS_TOKEN: a-long-random-string
```

Then `GET /metrics` with `Authorization: Bearer <token>` returns the Prometheus
text format. Without the token set, `/metrics` returns 404; with a wrong or
missing token, 401.

Scrape configuration:

```yaml
scrape_configs:
  - job_name: drawbridge
    authorization:
      credentials: a-long-random-string
    static_configs:
      - targets: ["drawbridge:8080"]
```

### Metric reference

Counters count since Drawbridge started; they reset on restart, as Prometheus
expects.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `drawbridge_build_info` | gauge | `version` | Always 1. |
| `drawbridge_wan_state` | gauge | `state` = `learning`, `primary`, `failover_pending`, `failover`, `failback_pending` | 1 for the current state, 0 for the others. |
| `drawbridge_state_since_timestamp_seconds` | gauge | | Unix time the current state began. |
| `drawbridge_on_primary` | gauge | | 1 when the last conclusive check was on the primary ISP. |
| `drawbridge_paused` | gauge | | 1 while automation is paused (including before the setup wizard is finished). |
| `drawbridge_isp_info` | gauge | `isp`, `asn`, `primary` | 1 for the ISP of the last conclusive check. Absent before the first one. |
| `drawbridge_last_check_timestamp_seconds` | gauge | | Unix time of the last check. |
| `drawbridge_last_check_duration_seconds` | gauge | | How long the last check took. |
| `drawbridge_checks_total` | counter | `result` = `conclusive`, `inconclusive` | Checks run. |
| `drawbridge_failovers_total` | counter | `trigger` = `auto`, `manual` | Failovers. |
| `drawbridge_failbacks_total` | counter | `trigger` = `auto`, `manual` | Failbacks (`manual` = Restore now). |
| `drawbridge_run_errors_total` | counter | | Container runs that ended with at least one error. |
| `drawbridge_flaps_total` | counter | | Failovers less than an hour after a failback. |
| `drawbridge_failback_hold_seconds` | gauge | | How long the primary must currently be stable before failback; grows while it flaps. |
| `drawbridge_pending_due_timestamp_seconds` | gauge | | When a pending failover or failback completes if nothing interrupts it; 0 when none is pending. |
| `drawbridge_containers_stopped` | gauge | | Containers Drawbridge stopped and has not started yet. |
| `drawbridge_backup_last_success_timestamp_seconds` | gauge | | When the last [scheduled backup](backup-and-restore.md#scheduled-backups) was written; 0 if none yet. |
| `drawbridge_notifications_total` | counter | `result` = `sent`, `failed` | Notification deliveries; `failed` counts those that failed after every retry. |
| `drawbridge_containers` | gauge | `on_failover` = `stop`, `keep` | Known containers by effective setting. |

### Alert rules

Drawbridge can also send these itself: see [Notifications](notifications.md).

```yaml
groups:
  - name: drawbridge
    rules:
      - alert: DrawbridgeOnBackupISP
        expr: drawbridge_wan_state{state="failover"} == 1
        for: 30m
        annotations:
          summary: "WAN on a backup ISP for 30 minutes; selected containers are stopped"

      - alert: DrawbridgeChecksFailing
        expr: time() - drawbridge_last_check_timestamp_seconds > 600
        annotations:
          summary: "Drawbridge has not completed a check in 10 minutes"

      - alert: DrawbridgeInconclusive
        expr: increase(drawbridge_checks_total{result="inconclusive"}[30m]) > 20
        annotations:
          summary: "Most WAN checks are inconclusive: the IP providers cannot be reached"

      - alert: DrawbridgeRunErrors
        expr: increase(drawbridge_run_errors_total[1h]) > 0
        annotations:
          summary: "Drawbridge failed to stop or start a container"

      - alert: DrawbridgePrimaryFlapping
        expr: drawbridge_failback_hold_seconds > 1200
        annotations:
          summary: "The primary ISP is flapping; failback waits {{ $value | humanizeDuration }}"

      - alert: DrawbridgeBackupStale
        expr: drawbridge_backup_last_success_timestamp_seconds > 0 and time() - drawbridge_backup_last_success_timestamp_seconds > 2 * 86400
        annotations:
          summary: "No scheduled backup has worked for two days"

      - alert: DrawbridgePaused
        expr: drawbridge_paused == 1
        for: 12h
        annotations:
          summary: "Drawbridge automation has been paused for 12 hours"
```

## Health check

- `GET /healthz` returns `200 ok` whenever the web server runs. It needs no
  sign-in and is never redirected to the setup wizard.
- The image's Docker `HEALTHCHECK` runs `/drawbridge healthcheck` every 30
  seconds (5 second timeout, 20 second start period, 3 retries). It requests
  `/healthz` on the port from `DRAWBRIDGE_LISTEN` and exits 1 if that fails.

The health check says the process is up, not that checks succeed. Use
`drawbridge_last_check_timestamp_seconds` for that.

## Event types

Every change of state and every administrative action is recorded in the event
log (Events page, CSV export). `by` in the details is the user who acted.

| Event | When | Details |
| --- | --- | --- |
| `state_changed` | The state machine moved | `from`, `to`, `asn`, `name`, `isp` |
| `failover` | A failover was confirmed | `asn`, `name`, `isp`, `paused` if automation was paused |
| `failback` | A failback was confirmed | as above |
| `flapping` | A failover came within an hour of a failback | `flaps`, `failback_wait` |
| `run_failed` | Stopping, starting or updating containers ended with an error | `action` (`stopping`, `starting`, `applying`), `error` |
| `isp_new` | An ASN was seen whose ISP group did not exist yet | `isp`, `name`, `asn` |
| `manual_failover` | Fail over now | `from`, `to`, `by` |
| `manual_restore` | Restore now | `from`, `to`, `by` |
| `drill_started` | Test failover | `by`, `minutes`, `until` (RFC 3339, UTC) |
| `drill_ended` | A failover test ended: time up (`by=timer`), End test now, Restore now or a failover (`by=failover` when automatic) | `by`; `kept_stopped` when a failover took over |
| `paused`, `resumed` | Automation paused or resumed (finishing the wizard logs `resumed`; a timed pause ending logs `resumed` with `by=timer`) | `by`; `until` (RFC 3339, UTC) on a timed pause |
| `primary_learned` | The learner chose a primary | `isp` |
| `primary_pinned` | A primary was pinned in the wizard or by `DRAWBRIDGE_PRIMARY_ASN` | `isp`, `asn` or `by` |
| `primary_undecided` | Learning ended without a clear primary | `shares` |
| `primary_suggested` | Another ISP now looks primary | `isp`, `current` |
| `relearn` | Re-learn primary ISP | `by` |
| `isp_role` | An ISP's role was changed in the UI | `isp`, `role`, `by` |
| `asn_moved` | An ASN was moved to another ISP in the UI | `asn`, `isp`, `by` |
| `stack_policy` | A stack switch was flipped | `stack`, `disable`, `by` |
| `container_policy` | A container setting changed | `container`, `policy`, `converted_stack`, `by` |
| `settings_changed` | Detection settings saved in Settings | `by` |
| `user_created` | A user was added: in the setup wizard (`by` = `setup`), by an admin, or by single sign-on (`by` = `sso`) | `user`, `role`, `by` |
| `user_disable`, `user_enable`, `user_password` | User operations on the Users page | `user_id`, `by` |
| `user_role` | Single sign-on changed a role through the admin group | `user`, `role`, `by` |
| `password_changed` | A user changed their own password | `user` |
| `sso_settings` | Single sign-on settings saved | `enabled`, `sso_only`, `by` |
| `sso_linked`, `sso_unlinked` | An account was linked to or unlinked from single sign-on | `user`; `sso_user` when linked |
| `notifications_changed` | A notification channel was added, changed or deleted | `channel`, `action` (`added`, `updated`, `deleted`), `by` |
| `backup_created` | A backup file was downloaded, or a scheduled one written (`by` = `schedule`) | `history` (`none`, `30d`, `60d`, `90d`, `all`), `by`; `file` for scheduled ones |
| `backup_failed` | A scheduled backup could not be written | `error`, `by` (`schedule`) |
| `backup_schedule` | The backup schedule was saved | `enabled`, `time`, `keep`, `history`, `by` |
| `backup_restored` | A backup file was restored (in Settings, or in the setup wizard with `by` = `setup`) | `created`, `app_version`, `history`, `by` |

Changes made with the command line are not recorded as events.

Events are kept indefinitely; only individual checks are pruned (see
[Data model → retention](../technical/data-model.md#retention)).

## Logs

Drawbridge logs to standard error in `key=value` form:

```sh
docker compose logs -f drawbridge
```

`LOG_LEVEL` is `debug`, `info` (default), `warn` or `error`. At `debug`, every
check is logged with its IP, ASN, ISP and state. Useful messages:

| Message | Level | Meaning |
| --- | --- | --- |
| `state changed` | info | Every state change, with the ISP. |
| `failover: stopping containers` / `failback: starting containers` | info | A run started, with the count. |
| `container action failed` | error | Docker refused to stop or start a container. |
| `check inconclusive` | info | Providers did not agree or could not be reached; lists each provider's result. |
| `primary ISP is flapping: waiting longer before failback` | warn | With the new wait. |
| `no ISP is clearly primary; pin one on the ISPs page` | warn | Learning ended undecided. |
| `failed sign-in` | warn | With user and client IP. |
| `cannot reach Docker; is /var/run/docker.sock mounted?` | error | At start-up. |
| `docker event stream ended; reconnecting` | warn | Docker restarted; Drawbridge reconnects every 5 seconds. |
