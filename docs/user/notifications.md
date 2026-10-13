# Notifications

Drawbridge can tell you when something happens: a failover, containers that
failed to stop or start, a flapping primary ISP. It sends to
[ntfy](https://ntfy.sh), [Gotify](https://gotify.net), Discord, or any URL that
accepts a JSON POST (Home Assistant, n8n, an Apprise API server, your own
script).

Notifications are set up by admins in **Settings → Notifications**. You can
add as many channels as you like; each chooses which events it gets.

## Adding a channel

Open **Add a channel** and fill in:

| Field | Meaning |
| --- | --- |
| Name | Your label, for example `Phone`. |
| Kind | ntfy, Gotify, Discord or Webhook (JSON). |
| URL | Where to send; see the table below. Stored encrypted, and only the host is shown afterwards. |
| Token | Optional for ntfy and webhooks, required for Gotify. Stored encrypted. |
| Send | Which groups of events this channel gets (see [What is sent](#what-is-sent)). |
| Send to this channel | Untick to pause the channel without deleting it. |

**Send test** sends a test message with the form as it is, saved or not, and
shows "✓ Sent" or the error. **Save** stores the channel. On a saved channel,
leave URL and Token blank to keep the stored ones; tick **Remove the saved
token** to delete the token.

| Kind | URL | Token |
| --- | --- | --- |
| ntfy | The topic URL: `https://ntfy.sh/your-topic`, or your own server's | An access token, for a protected topic (sent as `Authorization: Bearer`) |
| Gotify | The server address: `https://gotify.example.com` (Drawbridge posts to `/message`) | An **application** token (sent as `X-Gotify-Key`); required |
| Discord | The channel's webhook URL: **Edit Channel → Integrations → Webhooks → Copy Webhook URL** | Not used: the URL is the secret |
| Webhook | Any `http://` or `https://` URL | Optional, sent as `Authorization: Bearer` |

Each saved channel shows when it last sent: **Last sent 3m ago**, or **Last
send failed** with the error on hover. This is kept in memory, so it is empty
after a restart until the next message.

## What is sent

| Group | Events | On for a new channel |
| --- | --- | --- |
| Failover and failback, and failover tests | `failover`, `failback`, `manual_failover`, `manual_restore`, `drill_started`, `drill_ended` | Yes |
| Failures: containers that did not stop or start, scheduled backups | `run_failed`, `backup_failed` | Yes |
| Flapping primary ISP | `flapping` | Yes |
| New ISPs and primary ISP changes | `isp_new`, `primary_learned`, `primary_suggested`, `primary_undecided` | Yes |
| Automation paused or resumed | `paused`, `resumed` | No |
| Backups made or restored | `backup_created` (downloaded or scheduled), `backup_restored` | No |

The events are described in [Monitoring](monitoring.md#event-types). Examples
of the messages:

| Event | Title | Message |
| --- | --- | --- |
| `failover` | Failed over to a backup ISP | Traffic is on SKYLINK-64510 (AS64510). Containers set to stop on failover are being stopped. |
| `failback` | Back on the primary ISP | Traffic is on EXAMPLENET-64500 (AS64500) again. Containers Drawbridge stopped are being started. |
| `run_failed` | Stopping containers failed | (the error). See the dashboard for which containers are affected. |
| `flapping` | The primary ISP is flapping | It failed again soon after failing back (2 times in a row), so failback now waits 20m0s of stable connection. |
| `isp_new` | New ISP seen | Drawbridge saw BIGTEL (AS64511) for the first time. Until it is learned or set as primary on the ISPs page, it counts as a backup. |

If automation is paused, failover and failback messages say that no container
was stopped or started.

Priorities: container failures are 5 (urgent), failovers and failed scheduled
backups 4 (high), everything else 3 (default), on ntfy's 1 to 5 scale. Gotify
gets them as 10, 8 and 5.

## Message formats

- **ntfy**: the message as the body, with the `Title`, `Priority` and `Tags`
  headers (`warning` for priority 4 and 5).
- **Gotify**: `{"title", "message", "priority"}`.
- **Discord**: `{"username": "WAN Drawbridge", "content": "**Title**\nMessage"}`.
- **Webhook**: the full message as JSON:

```json
{
  "event": "failover",
  "title": "Failed over to a backup ISP",
  "message": "Traffic is on SKYLINK-64510 (AS64510). Containers set to stop on failover are being stopped.",
  "priority": 4,
  "time": "2027-01-15T07:00:00Z",
  "details": {"asn": 64510, "isp": 2, "name": "SKYLINK-64510"}
}
```

`details` holds the event's own fields, as in the event log. Every request has
the `User-Agent` `wan-drawbridge/<version>`.

## Delivery

- Messages go out in the background, in the order the events happened; a
  slow channel never holds up the monitor. All channels for one event are
  sent at the same time.
- Each request has a 10 second timeout. A failure is retried after 5 seconds
  and again after 30 seconds. A `4xx` answer other than `429 Too Many
  Requests` is not retried, since it would fail again (a wrong token or URL).
- Up to 64 events wait in the queue; if it is full, further events are not
  sent (and logged as `notification queue full`). They are still in the event
  log.
- Messages for events still in the queue when Drawbridge stops are lost.
- Failures are logged (`notification failed`) and counted in the metric
  `drawbridge_notifications_total{result="failed"}`.

If your network is down, so are internet-based channels: a failover message
reaches you over the backup connection, which is up by then.

## Backups and secrets

Channel URLs and tokens are encrypted in the database with the same key as the
single sign-on client secret (`secret.key` or `DRAWBRIDGE_SECRET_KEY`). A
backup file carries them inside its own encryption, so a restore on a new
instance keeps working channels. See [Backup and restore](backup-and-restore.md).

Adding, changing and deleting channels are recorded as `notifications_changed`
events.
