# Notifications

Drawbridge can tell you when something happens: a failover, containers that
failed to stop or start, a flapping primary ISP. It sends to
[ntfy](https://ntfy.sh), [Gotify](https://gotify.net),
[Pushover](https://pushover.net), Telegram, Discord, email, or any URL that
accepts a JSON POST (Home Assistant, n8n, an Apprise API server, your own
script).

Notifications are set up by admins in **Settings → Notifications**. You can
add as many channels as you like; each chooses which events it gets.

## Adding a channel

Open **Add a channel** and fill in:

| Field | Meaning |
| --- | --- |
| Name | Your label, for example `Phone`. |
| Kind | ntfy, Gotify, Pushover, Telegram, Discord, Email (SMTP) or Webhook (JSON). The form shows only the fields the kind uses (all of them without JavaScript). |
| URL | Where to send; see the table below. Stored encrypted, and only the host is shown afterwards. |
| Token or password | See the table below. Stored encrypted. |
| User | Pushover: your user key. Email: the SMTP username. Stored encrypted. |
| Send to | Telegram: the chat ID. Email: the recipients, separated by commas. |
| From | Email: the sender address. |
| Send | Which groups of events this channel gets (see [What is sent](#what-is-sent)). |
| Send to this channel | Untick to pause the channel without deleting it. |

**Send test** sends a test message with the form as it is, saved or not, and
shows "✓ Sent" or the error. **Save** stores the channel. On a saved channel,
leave URL, Token and User blank to keep the stored ones; tick **Remove the
saved token** (or URL, for Telegram, or username, for email) to delete it.

| Kind | URL | Token |
| --- | --- | --- |
| ntfy | The topic URL: `https://ntfy.sh/your-topic`, or your own server's | An access token, for a protected topic (sent as `Authorization: Bearer`) |
| Gotify | The server address: `https://gotify.example.com` (Drawbridge posts to `/message`) | An **application** token (sent as `X-Gotify-Key`); required |
| Pushover | Not used: messages go to `https://api.pushover.net/1/messages.json` | The application's API token, from **Create an Application/API Token** on pushover.net; required. **User** is your user key; required |
| Telegram | Optional: only for your own [Bot API server](https://core.telegram.org/bots/api#using-a-local-bot-api-server). Blank uses `https://api.telegram.org` | The bot token from @BotFather; required. **Send to** is the chat ID (a group's starts with `-`); required. Add the bot to the group, or send it a message first, so it may write to you |
| Discord | The channel's webhook URL: **Edit Channel → Integrations → Webhooks → Copy Webhook URL** | Not used: the URL is the secret |
| Email | `smtp://host:port` (port 587 if left out) to upgrade with STARTTLS, or `smtps://host:port` (port 465 if left out) for TLS from the start | The SMTP password. With a **User**, Drawbridge signs in (AUTH PLAIN, only ever over TLS); without one it sends without signing in. **From** and **Send to** are required |
| Webhook | Any `http://` or `https://` URL | Optional, sent as `Authorization: Bearer` |

Email is always encrypted: a server reached with `smtp://` that does not offer
STARTTLS is refused ("the server does not offer STARTTLS"), and so is a
certificate the system does not trust or that does not match the host
(TLS 1.2 or newer).

Each saved channel shows when it last sent: **Last sent 3m ago**, or **Last
send failed** with the error on hover. This is kept in memory, so it is empty
after a restart until the next message.

## What is sent

| Group | Events | On for a new channel |
| --- | --- | --- |
| Failover and failback, and failover tests | `failover`, `failback`, `manual_failover`, `manual_restore`, `drill_started`, `drill_ended`, `drill_skipped` | Yes |
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
| `isp_new` | New ISP seen | Drawbridge saw BIGTEL (AS64511) for the first time. Until it is learned or set as primary on the ISPs page, it counts as a metered backup. |

If automation is paused, failover and failback messages say that no container
was stopped or started.

Priorities: container failures are 5 (urgent), failovers and failed scheduled
backups 4 (high), everything else 3 (default), on ntfy's 1 to 5 scale. Gotify
gets them as 10, 8 and 5; Pushover as 1, 1 and 0 (Pushover's emergency
priority, which repeats until acknowledged, is not used); email marks 5 and 4
with `X-Priority: 1` and `2`.

## Message formats

- **ntfy**: the message as the body, with the `Title`, `Priority` and `Tags`
  headers (`warning` for priority 4 and 5).
- **Gotify**: `{"title", "message", "priority"}`.
- **Pushover**: a form with `token`, `user`, `title`, `message`, `priority`
  and `timestamp` (the event's time).
- **Telegram**: `sendMessage` with `chat_id` and `text` ("Title", a blank line,
  then the message) as plain text, link previews off.
- **Discord**: `{"username": "WAN Drawbridge", "content": "**Title**\nMessage"}`.
- **Email**: subject `WAN Drawbridge: <title>`, a plain-text body (UTF-8,
  quoted-printable) with the message, the event type and its time in UTC.
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

`details` holds the event's own fields, as in the event log. Every HTTP request
has the `User-Agent` `wan-drawbridge/<version>`.

## Delivery

- Messages go out in the background, in the order the events happened; a
  slow channel never holds up the monitor. All channels for one event are
  sent at the same time.
- Each request has a 10 second timeout (email: 10 seconds to connect, 30 for
  the whole exchange). A failure is retried after 5 seconds and again after
  30 seconds. A `4xx` answer other than `429 Too Many Requests`, an SMTP
  `5xx` reply and a server without STARTTLS are not retried, since they would
  fail again (a wrong token, URL or address).
- Errors never show a URL's path, which can hold a secret (a Discord
  webhook's, a Telegram bot token): only its scheme and host.
- Up to 64 events wait in the queue; if it is full, further events are not
  sent (and logged as `notification queue full`). They are still in the event
  log.
- Messages for events still in the queue when Drawbridge stops are lost.
- Failures are logged (`notification failed`) and counted in the metric
  `drawbridge_notifications_total{result="failed"}`.

If your network is down, so are internet-based channels: a failover message
reaches you over the backup connection, which is up by then.

## Backups and secrets

Channel URLs, tokens and users are encrypted in the database with the same key as the
single sign-on client secret (`secret.key` or `DRAWBRIDGE_SECRET_KEY`). A
backup file carries them inside its own encryption, so a restore on a new
instance keeps working channels. See [Backup and restore](backup-and-restore.md).

Adding, changing and deleting channels are recorded as `notifications_changed`
events.
