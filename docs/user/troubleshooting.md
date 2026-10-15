# Troubleshooting

Start with three things: the dashboard, the Events page, and the log
(`docker compose logs --tail 100 drawbridge`; add `LOG_LEVEL=debug` to see
every check).

## Cannot reach Docker

**Symptoms:** the log says `cannot reach Docker; is /var/run/docker.sock
mounted?`; the wizard's Docker step shows an error; container states show
"unknown"; Settings → System shows the error.

**Causes and fixes:**

- The socket is not mounted. Add
  `- /var/run/docker.sock:/var/run/docker.sock` under `volumes:` and recreate
  the container.
- Rootless Docker or a non-standard socket: mount that socket at
  `/var/run/docker.sock`, or set `DOCKER_HOST` (see
  [Installation](installation.md#rootless-docker-or-a-different-socket)).
- `permission denied`: something forces a non-root user (a `user:` line in the
  Compose file, or a security profile). Drawbridge must run as root to use the
  socket; remove the `user:` line.
- `DOCKER_HOST` set to a TLS endpoint (`tcp://…:2376`): not supported; use the
  socket.

Without Docker, Drawbridge keeps checking the WAN and recording state; it
simply cannot stop anything.

## Every check is inconclusive

**Symptoms:** "Last check … · inconclusive"; the log repeats
`check inconclusive` with each provider's error; grey timeline.

A check is conclusive only when at least **quorum** providers (default 2)
report the same IPv4 address within the timeout (default 5 s). Look at the
provider errors in the log line:

| Error | Cause | Fix |
| --- | --- | --- |
| `dial tcp4: … i/o timeout`, `no route to host` | No outbound IPv4 HTTPS from the container | Check the host's firewall and Docker networking; IPv6-only networks are not supported |
| `lookup … no such host` | DNS fails in the container, or a DNS filter blocks the providers | Allow `api.ipify.org` and `icanhazip.com` in Pi-hole/AdGuard; `1.1.1.1` needs no DNS |
| `status 403` / `status 429` | A provider refuses or rate-limits you | Use a longer check interval, or replace that provider |
| `not IPv4` / `not an IP` | A provider URL returns something else | Use a URL that returns a bare IP or Cloudflare's `ip=` trace format |
| `x509: certificate …` | TLS interception by a proxy or firewall | Exempt the providers from inspection |

If only one provider works, either fix the others or set the quorum to 1
(less protection against a wrong answer).

## The ISP is wrong

**Symptom:** the dashboard shows an ISP you do not expect.

- **Behind a VPN:** if the host or Drawbridge's container routes through a VPN,
  the public IP is the VPN's. Drawbridge must see the same public IP as your
  household. Exclude it from the VPN.
- **CGNAT or a business line:** your ISP may announce your address from an ASN
  with an unexpected name. Look at the ISPs page; move the ASN into the right
  ISP.
- **New ASN of your primary ISP not grouped with it:** see the next section.

## Drawbridge fails over although the primary works

1. Open **Events** and find the `failover` event: it names the ASN and ISP that
   were seen.
2. If that ASN belongs to your primary provider, move it into the primary ISP
   (ISPs page → **Move to…**). The next check sees the primary, and failback
   follows after the normal wait.
3. If checks alternate between providers reporting different IPs (dual-WAN
   load balancing, or policy routing that sends some HTTPS traffic out the
   backup), Drawbridge cannot work reliably: it needs all its traffic to take
   the household's main route. Drawbridge is not designed for load-balanced
   connections.

## Drawbridge does not fail over

Check in this order:

1. **Is automation paused?** The dashboard shows a **Paused** badge (with the
   end time for a timed pause). Before
   the setup wizard is finished, automation is always paused.
2. **Is a primary known?** If the dashboard says "Primary ISP: Not known yet",
   the state is Learning and nothing is stopped. Pin a primary on the ISPs
   page.
3. **Is the backup seen as a backup?** Run **Check now** while on the backup.
   If the current ISP is your primary ISP, the backup's ASN is grouped with it
   (both connections from one provider). Move it out with **Move to… → Its own
   ISP**. If the backup ISP's role is **Ignored**, set it to Backup.
4. **Are checks conclusive on the backup?** Some cellular networks block or
   slow the IP providers. Inconclusive checks never trigger a failover.
5. **Is anything set to stop?** The Stacks page should show at least one stack
   as "Stops" or "Partial", or the Containers page some "Stops".
6. **Is the failover delay longer than the outage?** With "Fail over after" at
   45 s, outages shorter than that are ignored by design.

## Containers are not started again

- **The primary is not stable.** In Failback pending, every check for the whole
  wait must be clean. If one IP provider keeps reporting a different address,
  or keeps failing checks, failback restarts every time. The log at `debug`
  shows each check. Replace an unreliable provider.
- **The primary is flapping.** The dashboard banner shows the current wait (up
  to 2 hours). This is intended. **Restore now** starts containers at once.
- **The container was not running before the failover.** Drawbridge only
  restarts what it stopped. Its entry says "was already stopped; left alone".
- **The container was removed and re-created with a different name or
  project.** Drawbridge looks for it by ID, then by key; if neither matches,
  its entry shows "… no longer exists". Start it yourself.
- **Its host could not be reached at the failback.** The dashboard says
  how many containers on that host will be started again once Drawbridge can
  reach it. Drawbridge tries every minute (not while automation is paused);
  get the host or its agent running. To give up on them, use **Forget owed
  starts** in Settings → Hosts.
- **Docker refused to start it.** The entry shows the Docker error. Fix the
  cause, then **Restore now** (it retries everything not yet started).

## Something is stuck "stopping" or "starting"

A run waits for each container to stop (up to 30 s before Docker kills it) and,
when starting, for healthchecks (up to 2 minutes per dependency level).
During a run, checks pause. If the badge stays for more than a few minutes,
look for `container action failed` in the log. Restarting Drawbridge is safe:
it resumes the run.

## An agent does not connect

Look at the agent's log (`docker compose logs drawbridge-agent`) and at
Settings → Hosts:

| Agent log or Hosts page | Cause and fix |
| --- | --- |
| `not enrolled yet: set DRAWBRIDGE_ENROLL_TOKEN and DRAWBRIDGE_MANAGER_KEY` | The agent has no identity yet and no token. Copy both from the Compose file Settings → Hosts showed, or make a new token. |
| `enrollment failed: … unknown or already used enrollment token` | A token works once. The agent may have enrolled already with an earlier folder; make a **New enrollment token** and start the agent with it. |
| `enrollment failed: … the enrollment token has expired` | Tokens last one hour. Make a new one. |
| `enrollment failed: the manager's key is not the one in DRAWBRIDGE_MANAGER_KEY` | `DRAWBRIDGE_MANAGER` points at another Drawbridge, or the manager key was rotated since the Compose file was made. Check the address and copy the key shown under Settings → Hosts. |
| `cannot reach the manager` | The agent cannot open `DRAWBRIDGE_MANAGER`. Check the address from the agent's host, for example `curl -I https://drawbridge.example.com/healthz`. |
| `the manager refuses this agent` with `bad signature` | The agent's key is not the one the manager knows: its `/data/agent` folder was lost or copied from another host. Make a **New enrollment token**. |
| `… with the request's time is … from the manager's` | The two clocks differ by more than 5 minutes. Fix the time (NTP) on whichever is wrong. |
| `… with key older than 90 days` | The agent was off for too long to rotate its key. Make a **New enrollment token**. |
| `this host was revoked, disabled or removed` | As it says; enable it, or make a new enrollment token. |
| `DRAWBRIDGE_AGENT_PLATFORM=swarm needs a swarm manager node` | The swarm agent runs on a worker. Constrain it to `node.role == manager`. |
| `PROXMOX_FINGERPRINT is not set; the server's certificate has …` | Compare the fingerprint shown with the one in the Proxmox web UI (node → System → Certificates) and set it. |
| `… the server's certificate fingerprint is …, not PROXMOX_FINGERPRINT` | The certificate changed (renewed, or another node), or the URL is wrong. Check it the same way. |
| `cannot use the Proxmox API: … 401 …` | Wrong `PROXMOX_TOKEN_ID` or secret. |
| Proxmox containers missing from the list, or `403 Permission check failed` when stopping | The token lacks `VM.Audit` or `VM.PowerMgmt` on their pool; see [Adding Proxmox VE](hosts.md#adding-proxmox-ve-lxc-containers). |
| An HA-managed container starts again right after the failover | The token cannot read HA (`Sys.Audit` on `/`), so Drawbridge shut it down directly and HA restarted it. Give it `Sys.Audit` and `Sys.Console` on `/`. |
| `not running in a Kubernetes pod` | `DRAWBRIDGE_AGENT_PLATFORM=kubernetes` outside a cluster. Deploy it with the manifest in `deploy/kubernetes`. |
| `cannot use the Kubernetes API (check the agent's RBAC): … forbidden` | The ServiceAccount lacks a permission, or `DRAWBRIDGE_AGENT_ALLOW` is unset while only RoleBindings exist (it then lists all namespaces). See `deploy/kubernetes/README.md`. |
| A Kubernetes workload comes back right after the failover | An HPA or a GitOps tool restored it; the Containers page shows a warning. See `deploy/kubernetes/README.md`. |
| Host **Offline**, agent log quiet | The agent's container is not running, or its Docker cannot be reached by it (`cannot reach Docker`). |

The agent's container shows `unhealthy` in `docker ps` while it has not
reached the manager in the last 90 seconds.

## Locked out

- **Forgot your password:**
  `echo 'new long password' | docker compose exec -T drawbridge /drawbridge reset-password <user>`.
  This also re-enables a disabled account.
- **Lost the phone with your authenticator app:** sign in with a recovery
  code, then turn two-factor sign-in off and on again with the new phone. No
  codes left: another admin can **Turn off 2FA** for you in Settings → Users,
  or run `docker compose exec drawbridge /drawbridge reset-2fa <user>`.
- **"That code is not right" with the right app:** the phone's clock is off
  by more than 30 seconds. Set it to automatic time.
- **"Too many failed sign-ins":** five failures for one username or one
  address within 15 minutes block further tries for that username or address
  until the oldest failure is 15 minutes old. Restarting Drawbridge clears it.
  Behind a reverse proxy, set `DRAWBRIDGE_TRUSTED_PROXIES` so the limit applies
  per visitor (see [Behind a reverse proxy](reverse-proxy.md)).
- **Single sign-on only, and the provider is down:** set
  `DRAWBRIDGE_LOCAL_LOGIN=force` and recreate the container (see
  [Single sign-on](single-sign-on.md#if-your-provider-is-unreachable)).
- **Disabled the only admin by editing the database:** use `reset-password`,
  which re-enables the account.
- **After restoring a backup:** everyone is signed out, and only the accounts
  in the backup exist. Sign in with one of them; if you do not know a
  password, use `reset-password` as above.

## Restoring a backup is refused

The message says why, and nothing was changed. The reasons and what to do are
listed in [Backup and restore → Restoring one](backup-and-restore.md#restoring-one).
The common ones: a wrong passphrase (it cannot be recovered), a backup from a
newer Drawbridge (upgrade first), or containers stopped for a failover
(restore after failback).

## Single sign-on errors

| Message | Cause |
| --- | --- |
| `cannot reach https://… ` on the sign-in page or Test connection | Drawbridge cannot load the issuer's discovery document. Check the issuer URL, DNS and that the provider is reachable from the container. |
| "Could not complete the sign-in … Check the client ID and secret." | The code exchange failed: wrong secret, or the callback URL registered at the provider differs. |
| "The sign-in token from … was not valid." | Issuer mismatch (the issuer URL must equal the token's `iss` exactly), wrong client ID, or clock skew between hosts. |
| "That sign-in link expired or was already used." | More than 10 minutes passed, the page was reloaded, or Drawbridge restarted during the sign-in. Try again. |
| `redirect_uri` error at the provider | The callback URL shown in Settings does not match what is registered. Set `DRAWBRIDGE_BASE_URL` behind a proxy. |
| "cannot decrypt stored secret" | `secret.key` or `DRAWBRIDGE_SECRET_KEY` changed. Enter the client secret again. |
| The admin group or allowed group has no effect | The log line `msg="sso sign-in"` lists the `groups` the provider sent. An empty list means the provider sent none: check that the client may read groups. Otherwise use a name from that list (case does not matter). |

## Web UI problems

- **Pages do not update live:** the reverse proxy buffers `/sse`. See
  [Behind a reverse proxy](reverse-proxy.md#live-updates-server-sent-events).
  With many tabs open over plain HTTP, the browser's limit of about six
  connections per host can also block updates and stall pages; close tabs or
  use HTTPS.
- **"invalid or missing CSRF token; reload the page":** the page was loaded
  in an old session (you signed in again elsewhere, or your password was
  changed). Reload.
- **Saving anything fails with a 403 that mentions a cross-origin request:**
  the browser's `Origin` does not match the host Drawbridge sees, typically
  because a proxy rewrites `Host`. Set `DRAWBRIDGE_BASE_URL` to the public URL.
- **Times are in UTC:** set `TZ` in the Compose file.

## The database

- **`open /data/db/drawbridge.db: unable to open database file`:** the folder
  is not writable or not mounted. Check the volume line and permissions.
- **`database is locked`:** another process holds the database for more than 5
  seconds; typically a second Drawbridge or a backup tool holding a write
  lock. Run only one instance.
- **`migrate: …` at start-up:** a migration failed; the database is unchanged.
  Restore the previous version, or report the error.
