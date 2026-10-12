# ISP grouping and learning

Code: `internal/store/isps.go`, `internal/learner`, `monitor.learn`.

## Group keys

`store.GroupKey(name, asn)` maps an ASN's name to an ISP group key:

1. Take the first whitespace-separated word of the name, trim trailing `,.;`,
   upper-case it. This is the AS handle (`EXAMPLENET-64500`) for iptoasn names, or
   the first word of the organisation (`Examplenet`) for ipinfo names.
2. Split it on `-`. Keep parts from the left until reaching a part that is
   empty, all digits, exactly `AS`, or `AS` followed by digits.
3. Join the kept parts with `-`. If nothing was kept, the key is `AS<number>`.

| Name | Key |
| --- | --- |
| `EXAMPLENET-64500` | `EXAMPLENET` |
| `Examplenet Communications, LLC` | `EXAMPLENET` |
| `METROCABLE-64503-WA-OR-C3` | `METROCABLE` |
| `SKYLINK-AS64510` | `SKYLINK` |
| `BIGTEL-INTERNET4` | `BIGTEL-INTERNET4` |
| `BIGTEL-MOBILITY-LLC-AS64511` | `BIGTEL-MOBILITY-LLC` |
| `AS-CHOOPA` (ASN 20473) | `AS20473` (the first part is `AS`) |
| empty (ASN 64512) | `AS64512` |

The rule is intentionally conservative: it merges the common
`<NAME>-<number>` pattern and keeps different product lines of one company
apart when their handles differ.

## Seeing an ASN

`SeeASN(asn, name, now)` runs in one transaction:

- **Known ASN:** update `last_seen` (and the name, if a non-empty name is
  given). The ASN keeps its group, even if its name would now map elsewhere.
- **New ASN:** compute the group key; find the ISP with that `group_key` or
  create it (role `backup`, name = key); insert the ASN into it.
- Update the ISP's `last_seen` and return its ID and role.

New ISPs are always **backups**.

## Manual grouping

`MoveASN(asn, to)`:

- `to > 0`: move the ASN into that ISP.
- `to = 0`: create a new ISP with `group_key = manual-AS<asn>`, named after the
  ASN's name (or `AS<asn>`), and move the ASN there. A second split of the same
  ASN fails because the key is unique.
- The ASN is marked `manual = 1`. Manual ASNs are never regrouped.
- Any non-primary ISP left with no ASNs is deleted in the same transaction.

## Roles

At most one ISP has role `primary`, enforced by the partial unique index
`isps_one_primary`. `SetRole(id, role, pinned)` demotes the previous primary to
`backup` (and unpins it) in the same transaction. `pinned` is stored only for
the primary.

`ignored` ISPs: their checks become inconclusive in the monitor, they get no
observed time, and they are excluded from shares.

## Time accounting

For each conclusive, non-ignored check, `Observe(asn, seconds, now)` adds
`seconds` to:

- `asns.observed_seconds` (all-time total, shown as "All time"), and
- `asn_daily(asn, day)` where `day` is the UTC date (`YYYY-MM-DD`).

`seconds = min(now − previous check, 2 × check interval)`, or one interval for
the first check after start. The cap prevents a long gap (host asleep,
Drawbridge stopped) from being credited to whatever ISP is seen next.

`Shares(since)` sums `asn_daily` from `day(since)` onwards **through the
current grouping**, per non-ignored ISP. Because time is stored per ASN, a
merge or split immediately changes the shares of past days too.

## The learner

`learner.Decide(Input) Decision` is pure. Inputs:

| Field | Value from the monitor |
| --- | --- |
| `Pinned` | the primary's ID if it is pinned, else 0 |
| `Current` | the current primary's ID (pinned or learned), else 0 |
| `LearningDone` | `now − LearningStartedAt ≥ LearningPeriod` |
| `Week` | `Shares(now − 7 days)` |
| `Recent` | `Shares(now − 3 days)` |

Rules, first match wins:

1. **Pinned** → that ISP. Reason `pinned`.
2. **A current learned primary** → keep it. Additionally, if the leader of
   `Recent` is a different ISP with a share **> 80%** and `Recent` covers at
   least **2 days** of observed time, return it as `Suggest`.
3. **Learning not done** → no primary. Reason `learning`.
4. **Leader of `Week` has ≥ 60%** → it becomes primary. Reason `learned`.
5. Otherwise → no primary, `NeedsUser`. Reason `undecided`.

Constants: `PromoteShare = 0.60`, `RelearnShare = 0.80`,
`RelearnWindow = 3 days`, minimum coverage 2 days.

The leader is the ISP with the most seconds (ties: the first in query order);
its share is its seconds divided by the window's total.

### What the monitor does with the decision

`monitor.learn` runs before every state-machine step:

- If `DRAWBRIDGE_PRIMARY_ASN` is set, the learner is skipped. If that ASN has
  not been seen, the primary is 0 (no actions). Otherwise its ISP is pinned
  (re-pinned whenever it differs or is unpinned, logged as `primary_pinned`)
  and returned.
- A decided primary different from the current one is stored with
  `SetRole(primary, pinned = false)` and logged as `primary_learned`.
- `NeedsUser` logs `primary_undecided` once per process (or per re-learn).
- `Suggest` logs `primary_suggested` once per suggested ISP.

The learner never replaces a learned primary on its own. A long outage of the
primary looks the same in the data as a change of provider; only a person can
tell the difference.

### LearningStartedAt

Set when the `wan_state` row is first created (first start) and reset by
**Re-learn**. Re-learn also clears the primary (`ClearPrimary`: the primary
becomes an unpinned backup) and resets the "announced" flags so notices are
logged again.
