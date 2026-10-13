# Command line

The `drawbridge` binary is both the server and a small command-line tool for
scripting and recovery. Run it inside the container:

```sh
docker compose exec drawbridge /drawbridge <command> [arguments]
```

Without a command, it runs the server (that is what the container does).

Commands that read or change settings work directly on the database in
`DRAWBRIDGE_DB_DIR`, so they work whether or not the server is running. Each
one first refreshes the container list from Docker; if Docker cannot be
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
  media-db-1     was running: true, stopped: true
  media-indexer-1       was running: true, stopped: true
```

The saved state, the ISP of the last conclusive check, the primary ISP's ID,
and the open run (the containers Drawbridge is holding stopped), with any
error per container. "open run none" means nothing is stopped.

This is the state saved by the server; while the server runs it is at most one
check old.

## stacks

```sh
$ /drawbridge stacks
STACK  STATUS    DISABLING  CONTAINERS
media  partial   4          5
proxy  none      0          2
```

`STATUS` is `disabled` (stack switch on), `partial` (some containers stop) or
`none`. `DISABLING` counts containers whose effective setting is to stop.

## containers

```sh
$ /drawbridge containers
KEY                 STACK  POLICY   EFFECTIVE  NOTE
media/player          media  keep     keep
media/db      media  disable  disable
automation              inherit  keep
dns                     inherit  keep
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
in the stack to follow it.

## set-container

```sh
$ /drawbridge set-container media/player keep
container media/player: keep
the stack-wide setting was removed; the other containers in the stack stay disabled individually
```

Policies: `inherit`, `disable`, `keep`. The key is `project/service` for
Compose containers or the container name for standalone ones (see the
`containers` command). The second line appears when the change
[converted the stack](stacks-and-containers.md#rule-2-overriding-one-container).

## isps

```sh
$ /drawbridge isps
ID  ISP       ROLE               LAST 7 DAYS  ASNS
1   EXAMPLENET   primary (pinned)   148h12m0s    AS64500 EXAMPLENET-64500, AS64501 EXAMPLENET-64501
2   SKYLINK  backup             2h3m0s       AS64510 SKYLINK-AS64510
```

The IDs are what `set-isp` and `move-asn` take. `(manual)` marks ASNs grouped
by hand.

## set-isp

```sh
$ /drawbridge set-isp 1 primary
ISP 1 is now primary
```

Roles: `primary` (pinned; the previous primary becomes a backup), `backup`,
`ignored`. See [ISPs and the primary ISP](isps.md#roles).

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

## Notes

- Changes made with the CLI are **not** recorded in the event log.
- Changing stack or container settings with the CLI while in Failover does not
  stop or start anything immediately; the change applies at the next failover
  or failback. Use the web UI for immediate effect.
- The CLI uses the same environment variables as the server, so run it in the
  same container (`docker compose exec`), not in a fresh `docker run`, unless
  you mount the same `db` folder and set the same variables.
