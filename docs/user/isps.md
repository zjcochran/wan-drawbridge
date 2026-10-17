# ISPs and the primary ISP

Drawbridge decides whether you are on your primary connection by asking
**which network your public IP belongs to**. This page explains how it
recognises ISPs, how the primary is chosen, and how to fix things when it
gets them wrong.

## From IP address to ISP

1. Each check finds your public IPv4 address (see
   [WAN detection](../technical/wan-detection.md)).
2. The address is looked up in an offline copy of the
   [iptoasn.com](https://iptoasn.com) database, which maps address ranges to
   **ASNs** (autonomous system numbers, the networks providers announce) and
   their names, for example `AS64500 EXAMPLENET-64500`. Addresses not in the
   database are looked up at `ipinfo.io`.
3. The ASN is placed in an **ISP group**. Large providers use many ASNs
   (Examplenet uses AS64500, AS64501, AS64502 and more), and your connection can
   come up on any of them after a reconnect. Treating each ASN as a different
   ISP would cause false failovers, so Drawbridge always works with groups.

### Automatic grouping

The group is derived from the ASN's name: the leading part of the AS handle,
up to the first part that is an AS number.

| ASN name | Group |
| --- | --- |
| `EXAMPLENET-64500` | `EXAMPLENET` |
| `EXAMPLENET-64501` | `EXAMPLENET` |
| `METROCABLE-64503-WA-OR-C3` | `METROCABLE` |
| `SKYLINK-AS64510` | `SKYLINK` |
| `BIGTEL-INTERNET4` | `BIGTEL-INTERNET4` |
| `BIGTEL-MOBILITY-LLC-AS64511` | `BIGTEL-MOBILITY-LLC` |
| (no usable name) | `AS64512` |

Note the last BigTel rows: BigTel's fibre and cellular networks stay separate,
which is what you want if one is your primary and the other your backup.

The first time an ASN is seen, it joins its group for good. Later changes are
made by hand (below) and are never undone automatically.

## Roles

Each ISP has one role:

| Role | Meaning |
| --- | --- |
| **Primary** | Your normal connection. Exactly one ISP can be primary. It is either **pinned** (chosen by you, never changed automatically) or **learned**. |
| **Backup** | Any other ISP. Traffic on a backup triggers a failover. Every newly seen ISP is a backup. |
| **Ignored** | Checks that land on this ISP are treated as if they found nothing: they neither trigger a failover nor count toward failback, and its time does not count for learning. |

Use **Ignored** sparingly, for example for a VPN exit that some checks
occasionally leak through. Ignoring your real backup would stop Drawbridge from
ever failing over.

## Metered and unmetered backups

Every backup ISP also has a **Metered** switch, on the ISPs page:

- **On (metered):** traffic on it triggers a failover, as described above.
  Every newly seen ISP starts metered, and so does every ISP that existed
  before this switch was added.
- **Off (unmetered):** traffic on it counts as if it were on the primary. It
  never triggers a failover; if Drawbridge is in a failover when traffic
  moves to it, the failback wait starts, as when the primary comes back.
  Use it for a second wired line, or any backup without a data cap.

The primary and ignored ISPs have no switch: the primary is never a reason
to fail over, and ignored ISPs never count. An ISP keeps its setting when its
role changes, so a former primary that becomes a backup is metered again
only if it was before. The dashboard marks the current ISP **unmetered** when
it is an unmetered backup. Changes apply from the next check; each one is
recorded as an `isp_metered` event. On the command line: `drawbridge set-isp
<id> metered` or `unmetered`.

## Data limits

A metered backup ISP can have **data limits**, set in its row on the ISPs
page (admins; viewers see them read-only) or with `drawbridge
set-data-limits`. When the containers that keep running pass one during an
outage on that ISP, Drawbridge sends a `data_alert` to the
[notification channels](notifications.md#what-is-sent) with **Failover and
failback, failover tests, and data limits reached** ticked:

| Limit | Counts |
| --- | --- |
| GB per outage | The traffic during a single outage on this ISP |
| GB per month | The traffic during this ISP's outages in the calendar month, in the server's time zone (`TZ`). An outage still going when the month began counts in full. |

- GB are decimal (1 GB = 1,000,000,000 bytes), from 0 to 1,000,000, with
  decimals allowed. Blank or 0 turns a limit off; every ISP starts with both
  off.
- An outage belongs to the first backup ISP seen during it, as on the
  [Reports](web-ui.md#reports) page.
- The traffic is what Reports shows: each running container's network
  counters, sampled every minute while an outage lasts. It includes traffic
  on the local network and between containers, and not other devices on your
  network.
- Each alert is sent once: once per outage, and once per month for each ISP.
  The check runs after each sample, so an alert comes within about a minute
  of passing the limit.
- A limit only alerts: it changes nothing about what is stopped.
- Saving records an `isp_data_limits` event.
- Only metered backups show the fields, since only they cause outages; an
  ISP keeps its limits when its role or Metered switch changes.

## How the primary is chosen

In order of precedence:

1. **`DRAWBRIDGE_PRIMARY_ASN`** (environment variable). Once that ASN has been
   seen, its ISP is pinned as primary, and re-pinned on every check. While it
   is set, the primary cannot be changed in the UI for longer than one check.
   Until the ASN has been seen, no primary is known and nothing is stopped.
2. **A pinned primary**: chosen in the wizard ("Yes, pin it as primary"), on
   the ISPs page, or with `drawbridge set-isp <id> primary`.
3. **Learning.** Without a pinned primary, Drawbridge watches for the learning
   period (default one day, counted from first start or from the last
   re-learn). Then the ISP with at least **60%** of the observed time over the
   last 7 days becomes the primary (learned, not pinned).
   - If no ISP reaches 60%, nothing is chosen, a `primary_undecided` event and
     a warning are logged, and the dashboard says "Not known yet". Pin one.
   - While no primary is known, the state is **Learning** and no containers
     are stopped.

Observed time is counted per check: each conclusive check credits the ISP with
the time since the previous check, capped at twice the check interval so that a
pause (host asleep, Drawbridge stopped) is not credited all at once.

### When your provider changes

A learned primary is never replaced automatically, because a long outage of
your primary looks just like switching providers. Instead, if a different ISP
carries more than **80%** of the traffic time over the last 3 days (with at
least 2 days of data in that window), Drawbridge logs a `primary_suggested`
event and a warning, once. Pin the new ISP on the ISPs page, or click
**Re-learn primary ISP**.

## Fixing things on the ISPs page

![ISPs page](../images/isps.jpg)

**Set the primary.** Choose *Primary* in the Role column. It is pinned. The
previous primary becomes a backup.

**Rename.** Type a name and click **Rename**. An empty name restores the group
name. Names are only labels.

**Two ISPs that are really one** (for example an ASN whose name did not match
its siblings): on one of the ASNs, choose **Move to…** and the other ISP. The
ASN is marked "grouped by hand". Its history moves with it: shares are summed
through the current grouping, so learning sees the corrected picture
immediately.

**One ISP that is really two** (for example a provider whose fixed and mobile
networks share a name, when one is your primary and the other your backup):
choose **Move to… → Its own ISP** on the ASN that belongs elsewhere. It becomes
a new ISP named after the ASN. Then set roles as needed.

An ISP left with no ASNs is deleted, unless it is the primary.

**Re-learn primary ISP** clears the primary (pinned or learned) and restarts
the learning period. While learning, nothing is stopped; if Drawbridge is in a
failover at that moment, containers it stopped are started again as soon as a
new primary is known and seen. Pinning an ISP ends learning immediately.

The same operations are available on the [command line](cli.md): `isps`,
`set-isp`, `move-asn`.

## Unknown and new networks

An ISP Drawbridge has never seen is a metered **backup** by definition. If your primary
provider starts using an ASN whose name does not match its group, every check
sees a backup ISP, and Drawbridge fails over once "Fail over after" has passed;
waiting does not help, because the IP really is on that ASN. The `failover`
event names the ASN. Move the ASN into your primary's ISP with **Move to…**:
the next check sees the primary, and the normal failback wait starts.

Names from the online fallback (`ipinfo.io`) look like
`Examplenet Communications, LLC`; only the first word is used for the group
(`EXAMPLENET`), so they usually land in the same group as the offline names.
