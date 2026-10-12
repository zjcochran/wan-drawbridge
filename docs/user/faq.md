# FAQ

**Why stop containers at all during a failover?**
Backup connections are usually metered or slow (LTE/5G plans, satellite).
Downloads, backups to the cloud, media servers streaming to remote users and
similar workloads can burn through a data cap in hours. Drawbridge stops them
while you are on the backup and brings them back afterwards.

**Does Drawbridge switch my internet connection?**
No. Your router or modem does the failover. Drawbridge only notices which
provider your traffic is using and reacts by stopping and starting containers.

**How does it know I am on the backup?**
It asks public "what is my IP" services for your address, looks up which
network (ASN) the address belongs to, and compares that network's provider to
your primary. See [ISPs and the primary ISP](isps.md).

**What if both connections are from the same company?**
If their ASNs have names that group together (for example both `EXAMPLENET-…`),
move the backup's ASN into its own ISP on the ISPs page. If both connections
really use the same ASN, Drawbridge cannot tell them apart.

**How much data does it use?**
Each check makes three HTTPS requests, each on a new connection (so a switch of
WAN is never hidden by an old connection). Including the TLS handshake, that is
about 13–14 KB per check, measured. At the default one-minute interval:

| Period | Data |
| --- | --- |
| Per day | about 20 MB |
| Per month | about 600 MB |

Checks continue during a failover, so on a metered backup budget about 20 MB
per day of outage (slightly more in the first minutes, when checks run every 15
seconds). A two-minute interval halves this. The ASN database download adds
about 7 MB a week, on whichever connection is active at the time.

**What happens if the internet is completely down?**
Nothing. Checks are inconclusive, and inconclusive checks never trigger a
failover.

**What about IPv6?**
Checks use IPv4 only, so providers cannot disagree because one answered over
IPv6. A network without IPv4 internet access cannot be checked.

**Can it manage several Docker hosts?**
Not yet; one instance manages the host whose socket it has.

**Can it send notifications?**
Not built in yet. Use the Prometheus metrics with Alertmanager (see
[Monitoring](monitoring.md#alert-rules)).

**Will it ever stop itself?**
No. It skips its own container, recognised by the `drawbridge.self` label or
its container ID.

**Will it start containers I stopped myself?**
No. It only starts containers that it stopped and that were running when the
failover began.

**What if Drawbridge crashes while stopping containers?**
It saves its progress before acting. On restart it finishes the run, and still
starts exactly the right containers at failback.

**Is the Docker socket mount dangerous?**
Access to the Docker socket is equivalent to root on the host. Drawbridge only
lists, inspects, stops and starts containers and reads events, but treat its
web UI as an administrative interface: use strong passwords or single sign-on,
and do not expose it to the internet without HTTPS. See
[Security](../technical/security.md).

**Why no socket proxy?**
A deliberate choice to keep the setup simple. Drawbridge needs write access
(stop/start) anyway, which is the part a proxy would have to allow.

**Can I use PostgreSQL?**
No; Drawbridge uses an embedded SQLite database by design. It needs no
separate service and backs up as one folder.

**Where is the data?**
In the `/data/db` folder: one SQLite database plus `secret.key`. See
[Backup and restore](backup-and-restore.md).

**How do I move Drawbridge to another host?**
Download a backup file in **Settings → Backup and restore**, install
Drawbridge on the new host, and choose **Or restore a backup** in the first
step of the setup wizard. Users, settings, ISPs and container choices come
along; container choices apply to stacks with the same Compose project names.
See [Backup and restore → Moving to another host](backup-and-restore.md#moving-to-another-host).

**What is the licence?**
GNU Affero General Public License v3.0 or later. If you run a modified version
for others over a network, you must offer them its source; set
`DRAWBRIDGE_SOURCE_URL` to where it is.
