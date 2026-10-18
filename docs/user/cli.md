# Command line

The `drawbridge` binary is both the server and a small command-line tool for
scripting and recovery. Run it inside the container:

```sh
docker compose exec drawbridge /drawbridge <command> [arguments]
```

Without a command, it runs the server (that is what the container does).

Commands that read or change settings work directly on the database in
`DRAWBRIDGE_DB_DIR`, so they work whether or not the server is running. Each
one first refreshes the container list from the local Docker; if it cannot be
reached it prints a warning and uses the stored list.

Output is aligned text meant for people; exit status is 0 on success and 1 on
any error, with the message on standard error prefixed `drawbridge:`. Wrong
arguments print the usage summary.

## version

```sh
$ /drawbridge version
v1.2.0
```

Prints the build version (`dev` for local builds).

## healthcheck

```sh
$ /drawbridge healthcheck
```

Requests `/healthz` from the running server on the port in
`DRAWBRIDGE_LISTEN` (on `127.0.0.1` when the address has no host or is
`0.0.0.0`/`::`). Exits 0 if it answers 200 within 3 seconds; otherwise prints
`unhealthy: …` and exits 1. The image's Docker `HEALTHCHECK` uses it.

## status

```sh
$ /drawbridge status
state        failover (since 2026-10-11 16:48:33)
current ASN  64510
primary ISP  1 (pinned: true)
open run     #1 since 2026-10-11 16:48:33
  media-db-1       on local, was running: true, stopped: true
  media-indexer-1  on local, was running: true, stopped: true
```

The saved state, the ISP of the last conclusive check, the primary ISP's ID,
and the open run (the containers Drawbridge is holding stopped), with any
error per container. "open run none" means nothing is stopped.

This is the state saved by the server; while the server runs it is at most one
check old.

## stacks

```sh
$ /drawbridge stacks
HOST   STACK  STATUS    DISABLING  CONTAINERS
local  media  partial   4          5
local  proxy  none      0          2
```

`HOST` is the host the stack is on (`local` for the Docker socket Drawbridge
mounts). `STATUS` is `disabled` (stack switch on), `partial` (some containers stop) or
`none`. `DISABLING` counts containers whose effective setting is to stop.

## containers

```sh
$ /drawbridge containers
HOST   KEY           STACK  POLICY   EFFECTIVE  NOTE
local  media/player  media  keep     keep
local  media/db      media  disable  disable
local  automation           inherit  keep
local  dns                  inherit  keep
```

`POLICY` is the container's own setting (`inherit`, `disable`, `keep`);
`EFFECTIVE` is what happens on failover. `NOTE` is `from label` when the
container follows its `drawbridge.on-failover` label, `from stack` when it
stops because of its stack, or `new, not configured` for
[new containers](stacks-and-containers.md#new-containers) awaiting review.

## set-stack

```sh
$ /drawbridge set-stack media on
stack media: disable on failover on; its containers now follow the stack
```

`on` or `off`. Like the switch on the Stacks page, it resets every container
in the stack to follow it. The stack is on the local host; for a stack on
another host, put the host's name in front: `set-stack nas:media on`. An
unknown host name is an error (`no host named "…"`).

## set-container

```sh
$ /drawbridge set-container media/player keep
container media/player: keep
the stack-wide setting was removed; the other containers in the stack stay disabled individually
```

Policies: `inherit`, `disable`, `keep`. The key is `project/service` for
Compose containers or the container name for standalone ones (see the
`containers` command), on the local host; for another host, put its name in
front: `set-container nas:media/player keep`. The second line appears when the change
[converted the stack](stacks-and-containers.md#rule-2-overriding-one-container).

## isps

```sh
$ /drawbridge isps
ID  ISP         ROLE              DATA LIMITS (OUTAGE / MONTH)  LAST 7 DAYS  ASNS
1   EXAMPLENET  primary (pinned)  -                             148h12m0s    AS64500 EXAMPLENET-64500, AS64501 EXAMPLENET-64501
2   SKYLINK     backup            5 GB / 100 GB                 2h3m0s       AS64510 SKYLINK-AS64510
```

The IDs are what `set-isp`, `set-data-limits` and `move-asn` take. `(manual)`
marks ASNs grouped by hand. **Data limits** shows `-` when both are off, and
`off` for one that is.

## set-isp

```sh
$ /drawbridge set-isp 1 primary
ISP 1 is now primary
```

Roles: `primary` (pinned; the previous primary becomes a backup), `backup`,
`ignored`. See [ISPs and the primary ISP](isps.md#roles). `metered` and
`unmetered` set the ISP's
[Metered switch](isps.md#metered-and-unmetered-isps) instead; the `isps`
command shows an unmetered primary or backup as `(unmetered)`.

## set-data-limits

```sh
$ /drawbridge set-data-limits 2 5 100
ISP 2 data limits: 5 GB per outage, 100 GB per month
```

Sets an ISP's [data limits](isps.md#data-limits): GB per outage, then GB per
month, decimals allowed, each from 0 to 1,000,000; `0` turns a limit off. An
unknown ISP or a bad number exits with code 1.

## move-asn

```sh
$ /drawbridge move-asn AS64501 1
AS64501 is now in ISP 1
$ /drawbridge move-asn 64501 new
AS64501 is now in ISP 7
```

Moves an ASN (with or without the `AS` prefix) into ISP `<id>`, or into a new
ISP of its own with `new`. The ASN must have been seen in a check.

## reset-password

```sh
$ echo 'a new long password' | docker compose exec -T drawbridge /drawbridge reset-password alice
new password (at least 12 characters), then Enter:
password for alice changed; their sessions were signed out
```

Reads one line from standard input as the new password (at least 12
characters). The username is case-insensitive. It also **re-enables** the
account if it was disabled, and signs the user out everywhere. Use it when you
are locked out; it works for accounts that only had single sign-on too, giving
them a password.

Interactively (`docker compose exec drawbridge /drawbridge reset-password
alice`) the password is echoed as you type; prefer the piped form.

## reset-2fa

```sh
$ docker compose exec drawbridge /drawbridge reset-2fa alice
two-factor sign-in for alice is off; their recovery codes were deleted
```

Turns off a user's [two-factor sign-in](web-ui.md#two-factor-sign-in) and
deletes their recovery codes, for a lost phone when no other admin can do it
in Settings → Users. The username is case-insensitive.

## hosts

```sh
$ docker compose exec drawbridge /drawbridge hosts
ID  HOST   KIND   STATUS   LAST SEEN            VERSION
1   local  local  online   2026-10-14 15:18:30
2   nas    agent  online   2026-10-14 15:18:22  v0.14.0
```

Every [host](hosts.md): its ID, name, kind (`local` or `agent`), status
(`pending`, `online`, `offline`, `revoked`, `disabled`), when it last answered
and, for agents, their version.

## enroll-token

```sh
$ docker compose exec drawbridge /drawbridge enroll-token nas
DRAWBRIDGE_MANAGER=https://drawbridge.example.com
DRAWBRIDGE_ENROLL_TOKEN=dbe_...
DRAWBRIDGE_MANAGER_KEY=ed25519:...
# the token works once, within an hour
```

Prints the variables an agent needs to enroll, for a host named as given:
the host is added if it does not exist, and gets a new token if it does,
which stops its current agent until it enrolls again. `DRAWBRIDGE_MANAGER` is
`DRAWBRIDGE_BASE_URL`; without it, the line says to put the web UI's address
there. Needs the instance key (`secret.key` or `DRAWBRIDGE_SECRET_KEY`), as
the server does.

## revoke-host

```sh
$ docker compose exec drawbridge /drawbridge revoke-host nas
host nas revoked: its agent is refused from now on
```

Deletes an agent host's key, so its agent is refused from its next request.
For a revoke that also takes effect in the running server's views at once,
use Settings → Hosts.

## agent

```sh
drawbridge agent
```

Runs [agent mode](hosts.md), with the settings from `DRAWBRIDGE_MANAGER`,
`DRAWBRIDGE_ENROLL_TOKEN`, `DRAWBRIDGE_MANAGER_KEY`, `DRAWBRIDGE_AGENT_PLATFORM`,
`DRAWBRIDGE_AGENT_ALLOW`, `DRAWBRIDGE_AGENT_DIR`, `DOCKER_HOST` and
`LOG_LEVEL`. It opens no database
and no port. In a Compose file, this is `command: agent`.

## Notes

- Changes made with the CLI are **not** recorded in the event log.
- Changing stack or container settings with the CLI while in Failover does not
  stop or start anything immediately; the change applies at the next failover
  or failback. Use the web UI for immediate effect.
- The CLI uses the same environment variables as the server, so run it in the
  same container (`docker compose exec`), not in a fresh `docker run`, unless
  you mount the same `db` folder and set the same variables.
