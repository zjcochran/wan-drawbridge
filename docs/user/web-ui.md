# Using the web UI

The web UI is a set of server-rendered pages that update themselves live: when
the WAN state changes or a container starts or stops, open pages refresh the
affected part within a second, without reloading. It works on phones (the
navigation bar and the Settings tabs scroll sideways) and follows your system's light or dark mode,
in the colour theme you choose (see [Appearance](#appearance)).

Your picture and username sit at the top right. Click them for a menu with
**Your account** (Settings → Your account) and **Sign out**; a click elsewhere
or Escape closes it. On a phone only the picture is shown.

There are two roles:

- **Admins** can change everything.
- **Viewers** see every page, but in Settings only the **WAN detection**
  (read-only), **Your account** and **System** tabs. They can change only their
  own password, single sign-on link and appearance. Controls they cannot use
  are not shown.

Every page footer shows the version and links to the source code, as the AGPL
requires.

## Dashboard

![Dashboard on the primary ISP](../images/dashboard.jpg)

### Status card

The coloured dot and label show the state:

| Label | Colour | Meaning |
| --- | --- | --- |
| Learning | grey | No primary ISP is known yet. Nothing is stopped. |
| On primary | green | Traffic leaves through the primary ISP. |
| Failover pending | amber | Traffic is on a backup ISP; Drawbridge is confirming it. Shows "failing over in 32s unless the primary returns". |
| Failover | red | Confirmed on a backup. Selected containers are stopped. |
| Failback pending | amber | The primary is back; Drawbridge is making sure it stays. Shows "failing back in 4m 56s if the primary stays stable". |

Badges next to the label:

- **Paused**: automation is paused; states still change but no container is
  touched.
- **stopping containers / starting containers / applying containers**: a
  container run is in progress.

If the primary ISP has been flapping, a banner explains how long failback now
waits (see [flapping](during-an-outage.md#flapping)).

The facts below the label:

| Fact | Meaning |
| --- | --- |
| Current ISP | The ISP of the last conclusive check, with the ASN that announced the IP. |
| Primary ISP | The primary, or "Not known yet" with a link to the ISPs page. |
| Public IP | The IPv4 address the providers agreed on. |
| Last check | How long ago; "· inconclusive" if the last check could not determine the IP. |
| In this state | How long the current state has lasted. |

### Buttons (admins)

| Button | When shown | What it does |
| --- | --- | --- |
| Pause automation / Resume automation | Always | Pausing keeps checking but stops all container actions, for the time chosen next to the button (30 minutes to 24 hours, default 1 hour) or until resumed. Resuming brings containers in line with the current state at once: stopped if in Failover, started again if on the primary. |
| Check now | Always | Runs a check immediately instead of waiting for the next one. |
| Fail over now | When nothing is stopped | After a confirmation, stops every container set to stop, as if a backup ISP had been confirmed. |
| Test failover | On the primary or learning, when nothing is stopped | After a confirmation, stops every container set to stop for 2 to 15 minutes, then starts them again; the WAN state does not change. See [Manual controls](during-an-outage.md#manual-controls). |
| End test now | During a failover test | Starts the containers the test stopped. |
| Restore now | While Drawbridge holds stopped containers, or the state is Failover or Failback pending | After a confirmation, starts what Drawbridge stopped and returns to the primary state. If traffic is still on a backup, the normal failover checks run again. |

See [manual controls](during-an-outage.md#manual-controls) for the details.

### Stopped by Drawbridge

Lists every container in the current failover run and what happened to it:

| Entry | Meaning |
| --- | --- |
| stopped | Drawbridge stopped it and will start it at failback. |
| started again | Started during this failover (its setting changed). |
| was already stopped; left alone | It was not running when the failover began, so it will not be started at failback. |
| waiting | Not processed yet. |
| *error text* in red | Stopping or starting it failed; the Docker error is shown. |

"Nothing. Every container is as you left it." means there is no open run.

### Last 24 hours

A timeline of every check: green for the primary, amber for a backup, grey for
inconclusive checks and light grey for no data (Drawbridge not running, or a
gap longer than three check intervals). Hover a segment for the ISP and times.
A short failover is a thin sliver at this scale.

### Recent events

The last few audit events. **All events** opens the Events page.

### New containers banner

"N new containers joined stacks with per-container settings and keep running
until you decide" appears when a stack you configured container by container
gets a new service. **Review them** opens the Containers page filtered to
them. See [new containers](stacks-and-containers.md#new-containers).

## Stacks

![Stacks page](../images/stacks.jpg)

One row per Docker Compose project:

| Column | Meaning |
| --- | --- |
| Stack | The Compose project name. |
| Containers | "3 running of 5". |
| On failover | **Stops (stack rule)**: the stack switch is on. **Partial · 4 of 5**: some containers stop through their own settings. **Not affected**: nothing in it stops. |
| Stop whole stack | The switch (admins). |
| Show containers | Expands the stack's containers with their state and whether each stops. |

Flipping the switch **resets every container in the stack** to follow it, on or
off. Exceptions are made on the Containers page.

![A stack after one container was overridden](../images/stacks-partial.jpg)

## Containers

![Containers page](../images/containers-override.jpg)

Every container in Docker, except Drawbridge itself.

**Filters** (combine freely; the URL keeps them, so you can bookmark a view):

- Search by name or key.
- Stack: all, standalone only, or one stack.
- State: running or stopped. "unknown" is shown when Docker cannot be
  reached; then the list also includes containers removed in the last 10
  minutes.
- Setting: stops on failover, keeps running, or "New, not configured".

**Columns:** container name (and its key, `project/service`, when different),
stack, state, what happens on failover (with where that comes from: "from
stack", "own setting", "default", or "new, not configured"), and the setting
(admins).

**Setting** for a container in a stack: *Follow stack*, *Stop on failover* or
*Keep running*. For a standalone container, *Default (keep running)* replaces
*Follow stack*.

Choosing *Keep running* for a container whose stack is switched on asks for
confirmation, because it converts the stack rule into per-container settings.
See [Choosing what to stop](stacks-and-containers.md#rule-2-overriding-one-container).

**Bulk edit:** tick containers (or the header box to select all shown), choose
a setting next to "Apply to selected", and apply.

## ISPs

![ISPs page](../images/isps.jpg)

One row per ISP:

| Column | Meaning |
| --- | --- |
| ISP | Name (editable by admins; empty resets it), "current" badge, group key and when last seen. |
| Role | Primary (pinned or "learned"), Backup or Ignored. |
| Last 7 days | Share of observed time, excluding ignored ISPs. |
| All time | Hours observed. |
| Networks (ASNs) | Each ASN with its name, "grouped by hand" if moved, and a **Move to…** menu. |

**Re-learn primary ISP** forgets the primary and starts a new learning period.
See [ISPs and the primary ISP](isps.md).

## Events

![Events page](../images/events.jpg)

The audit log, newest first, 100 per page (**Older** for the next page). Filter
by type; **Export CSV** downloads up to 1,000 events of the selected type as
`drawbridge-events.csv`. The list of event types is in
[Monitoring → events](monitoring.md#event-types).

## Settings

![Settings, WAN detection tab](../images/settings.jpg)

Settings has one tab per section, each at its own address, so a tab can be
bookmarked. `/settings` opens the first one.

| Tab | Address | Who | Contents |
| --- | --- | --- | --- |
| WAN detection | `/settings/detection` | everyone | The detection settings described in the [setup wizard](setup-wizard.md#4-wan-detection). Viewers see them read-only. Changes apply from the next check. |
| Notifications | `/settings/notifications` | admins | Channels that get events; see [Notifications](notifications.md). |
| Backup and restore | `/settings/backup` | admins | Download an encrypted backup file, with history of the last 30, 60 or 90 days, all time, or none; schedule daily backups into a folder; restore one. See [Backup and restore](backup-and-restore.md#backup-files). |
| Users | `/settings/users` | admins | Accounts; see [Users](#users). |
| Single sign-on | `/settings/sso` | admins | See [Single sign-on](single-sign-on.md). |
| Your account | `/settings/account` | everyone | Link or unlink your single sign-on account; change your password (this signs you out everywhere, including this browser); your profile picture (see [Profile pictures](#profile-pictures)); your colour theme and mode (see [Appearance](#appearance)). |
| System | `/settings/system` | everyone | Drawbridge version, Docker connection and version, database path and size. |

Viewers do not see the admin tabs; opening one's address gives 403.

### Users

![Settings, Users tab](../images/users.jpg)

**Settings → Users** (admins) lists every account with its role, how it signs in (password, single sign-on,
or both), last sign-in and status. For other users you can change the **Role**
(viewer or admin; not for accounts without a password while an SSO admin group
is set, since the group decides theirs), **Disable** (signs them out
everywhere), **Enable**, **Delete** (also removes their sessions and
single sign-on links; it asks first and cannot be undone) and **Reset
password**. Reset password is shown only for accounts that have a password, not
for ones that sign in with single sign-on only. **Add a user** creates a viewer
or admin with a password.

Safety rules:

- You cannot disable, delete or change the role of yourself.
- The last enabled admin cannot be disabled, deleted or turned into a viewer.

Usernames are case-insensitive: `Alice` and `alice` are the same account.

## Appearance

### Profile pictures

**Settings → Your account → Profile picture** uploads a PNG, JPEG, GIF or WebP
image of up to 1 MB (other types, SVG included, are refused). It shows in the
top-right menu and, for admins, in Settings → Users. **Remove picture** deletes
an uploaded one.

Accounts linked to single sign-on also get the picture from the provider's
`picture` claim at each SSO sign-in (see
[Single sign-on](single-sign-on.md#profile-pictures)). An uploaded picture
always wins; remove it to go back to the provider's. Without any picture, the
first letter of the username is shown instead.

Pictures are not in backup files; see
[Backup and restore](backup-and-restore.md#backup-files).

**Settings → Your account → Appearance** sets the colours for your account only. It applies
on every device you sign in on, and changes show at once.

- **Theme**: Drawbridge (the default), Nord, Solarized, Gruvbox, Catppuccin or
  Tokyo Night. Each has a light and a dark version; the cards show both.
- **Mode**: **Automatic** (the default) follows your device's light or dark
  setting; **Light** and **Dark** fix it.

Every theme keeps text, links, buttons and status colours at a contrast of at
least 4.5:1 (body text 7:1) in both versions. The sign-in page, before anyone is
signed in, uses the default theme in automatic mode. Themes need a browser from
2024 or later (Chrome and Edge 123, Firefox 120, Safari 17.5).

## Phone and dark mode

| Phone | Dark mode |
| --- | --- |
| ![Dashboard on a phone](../images/phone-dashboard.jpg) | ![Dashboard in dark mode](../images/dark-dashboard.jpg) |
| ![Containers on a phone](../images/phone-containers.jpg) | ![Stacks in dark mode](../images/dark-stacks.jpg) |

## Notes

- Over plain HTTP, browsers allow only about six open connections to one host.
  Each open Drawbridge tab keeps one for live updates, so with many tabs open
  pages can stall. Serving Drawbridge over HTTPS (through a reverse proxy)
  lifts this limit.
- Toast messages at the bottom confirm saves and show errors.
