# Backup and restore

There are two ways to back up Drawbridge:

- **A backup file from Settings** (admins): the configuration, and history
  if you choose, in one encrypted file. It restores into any Drawbridge of the
  same or a newer version, including a fresh install from the setup wizard.
  Use it to move Drawbridge or to keep its configuration safe.
- **A copy of the database folder**: everything, including the current
  failover state and the list of containers Drawbridge stopped. Use it for
  scheduled backups of the host.

## Backup files

### Making one

**Settings → Backup and restore** (admins only):

1. Choose **History**: *None, configuration only*, *Last 30 days*, *Last 60
   days*, *Last 90 days* or *All time*.
2. Enter a passphrase of at least 12 characters, twice.
3. Click **Download backup**. The browser saves
   `drawbridge-backup-YYYYMMDD-HHMMSS.json` (the time is UTC).

A `backup_created` event is recorded. If the passphrases do not match or are
too short, no file is made and Settings shows why.

**Keep the passphrase.** It cannot be recovered, and without it the file
cannot be restored.

### What is in it

| Part | Contents |
| --- | --- |
| Configuration (always) | Detection settings; ISPs with their names, roles and pinned primary; ASNs and how they are grouped; stack switches and container settings, including containers waiting for review; users with their roles, disabled flags and password hashes; single sign-on settings with the client secret; single sign-on account links; notification channels with their URLs and tokens; whether setup is complete and whether automation is paused |
| History (optional) | Checks, events and daily ISP totals from the chosen period |

Not included: sign-in sessions, the current WAN state (primary, failover,
pending changes), the list of containers Drawbridge stopped, `secret.key`
and the ASN database.

Checks and daily totals are only kept for the retention period (Settings →
WAN detection, 30 days by default), so a longer range adds older events but no
older checks. Events are kept indefinitely.

The file is encrypted with the passphrase (argon2id and AES-256-GCM; details
in [Security](../technical/security.md#backup-files)). Readable without the
passphrase: that it is a Drawbridge backup, the Drawbridge version that made
it, when, and which history range it holds.

### Restoring one

**Settings → Backup and restore → Restore** (admins only): choose the file,
enter its passphrase and click **Restore backup**, then confirm.

Restoring **replaces**:

- the whole configuration listed above, users included;
- the history, if the backup has history. A configuration-only backup leaves
  the current history as it is.

Afterwards everyone is signed out, including you; sign in with an account
from the backup. A `backup_restored` event is recorded, and Drawbridge runs a
check straight away with the restored settings. Container settings are matched
by Compose project and service (or container name), so they apply to the
same stacks on another host.

A restore is refused, and nothing changes, when:

| Message | Reason |
| --- | --- |
| "wrong passphrase, or the file is damaged" | The passphrase is not the one the file was made with, or the file was changed. |
| "this is not a Drawbridge backup file" | Another kind of file. |
| "this backup was made by a newer version of Drawbridge (…)" | Upgrade Drawbridge first, then restore. |
| "… while containers are stopped for a failover …" | Drawbridge holds containers stopped. Restore after failback, or after **Restore now**. |
| "the backup has no enabled admin account" | Nobody could manage Drawbridge afterwards. |
| "the file is larger than 128 MB" | Make a backup with less history. |

### Restoring during setup

On a fresh install, step 1 of the [setup wizard](setup-wizard.md#1-admin-account)
offers **Or restore a backup** instead of creating an admin. The restore
brings back the accounts and finishes setup; sign in with an account from the
backup. If the backup was made while automation was on, it is on as soon as
the restore completes.

## Database folder backups

### What to back up

Everything worth keeping is in **one folder**, mounted at `/data/db` (`./db`
next to the Compose file by default):

| File | Contents | Needed? |
| --- | --- | --- |
| `drawbridge.db` | The SQLite database: users, settings, ISPs, container settings, the current state and stopped-container list, events, check history | Yes |
| `drawbridge.db-wal`, `drawbridge.db-shm` | SQLite's write-ahead log and its index, present while Drawbridge runs. Recent changes can live only in the `-wal` file. | Yes, together with the database |
| `secret.key` | The key that encrypts the stored single sign-on client secret and notification URLs and tokens. Only created if `DRAWBRIDGE_SECRET_KEY` is not set. | Yes, if you use single sign-on or notifications |

Nothing else needs backing up. The ASN database in `/data/cache` is downloaded
again when missing. The Compose file and any environment variables are your
own configuration; keep them in version control or your usual backups.

If you set `DRAWBRIDGE_SECRET_KEY`, keep that value somewhere safe instead of
`secret.key`.

### Taking a consistent backup

The three database files must be copied **together and at the same moment**.
Pick one of these methods.

#### Stop, copy, start (simplest)

```sh
docker compose stop drawbridge
tar czf drawbridge-$(date +%F).tgz db/
docker compose start drawbridge
```

When Drawbridge stops cleanly, SQLite merges the log into `drawbridge.db`, so
the copy is complete. A short stop is safe even during a failover; see
[What happens during an outage](during-an-outage.md#when-drawbridge-itself-restarts).

#### Filesystem snapshot (no downtime)

A ZFS, Btrfs or LVM snapshot of the folder captures all files at one instant,
which SQLite treats like a power cut: consistent, with the last moments
recovered from the log on next open. Back up from the snapshot.

#### SQLite online backup (no downtime)

With the `sqlite3` tool on the host:

```sh
sqlite3 db/drawbridge.db ".backup 'drawbridge-backup.db'"
cp db/secret.key secret.key.backup
```

This produces a single, consistent file even while Drawbridge writes.

#### What not to do

Copying `drawbridge.db` alone while Drawbridge runs can produce a backup that
is missing recent changes or is corrupt. Always include the `-wal` file, or use
one of the methods above.

### Restoring the folder

1. Stop Drawbridge: `docker compose stop drawbridge`.
2. Replace the contents of `db/` with the backup. For an online-backup file,
   copy it to `db/drawbridge.db` and delete any `db/drawbridge.db-wal` and
   `db/drawbridge.db-shm` left from before.
3. Put `secret.key` back (or keep the same `DRAWBRIDGE_SECRET_KEY`).
4. Start: `docker compose start drawbridge`.

On start, Drawbridge applies any newer database migrations and then
**reconciles containers with the restored state**: if the backup was taken
during a failover, it makes sure the selected containers are stopped; if it was
taken on the primary, it starts anything an unfinished run still holds. Check
the dashboard after restoring.

If `secret.key` is lost, everything works except single sign-on, which reports
"cannot decrypt stored secret". Enter the client secret again under **Settings
→ Single sign-on**.

## Moving to another host

With a backup file:

1. Make a backup file in Settings.
2. Install Drawbridge on the new host (see [Installation](installation.md)).
3. In the setup wizard, choose **Or restore a backup**.

With the database folder:

1. Take a folder backup as above.
2. Install Drawbridge on the new host.
3. Restore the `db/` folder before the first start.

Container settings are keyed by Compose project and service (or container
name), not by container ID, so they apply on the new host as long as the stacks
have the same project names there.

## Starting over

Stop Drawbridge, delete the contents of `db/`, and start it. The setup wizard
runs again. Your containers are not affected, but any containers Drawbridge was
holding stopped stay stopped; start them yourself or click **Restore now**
first.
