# Backup and restore

## What to back up

Everything worth keeping is in **one folder**, mounted at `/data/db` (`./db`
next to the Compose file by default):

| File | Contents | Needed? |
| --- | --- | --- |
| `drawbridge.db` | The SQLite database: users, settings, ISPs, container settings, the current state and stopped-container list, events, check history | Yes |
| `drawbridge.db-wal`, `drawbridge.db-shm` | SQLite's write-ahead log and its index, present while Drawbridge runs. Recent changes can live only in the `-wal` file. | Yes, together with the database |
| `secret.key` | The key that encrypts the stored single sign-on client secret. Only created if `DRAWBRIDGE_SECRET_KEY` is not set. | Yes, if you use single sign-on |

Nothing else needs backing up. The ASN database in `/data/cache` is downloaded
again when missing. The Compose file and any environment variables are your
own configuration; keep them in version control or your usual backups.

If you set `DRAWBRIDGE_SECRET_KEY`, keep that value somewhere safe instead of
`secret.key`.

## Taking a consistent backup

The three database files must be copied **together and at the same moment**.
Pick one of these methods.

### Stop, copy, start (simplest)

```sh
docker compose stop drawbridge
tar czf drawbridge-$(date +%F).tgz db/
docker compose start drawbridge
```

When Drawbridge stops cleanly, SQLite merges the log into `drawbridge.db`, so
the copy is complete. A short stop is safe even during a failover; see
[What happens during an outage](during-an-outage.md#when-drawbridge-itself-restarts).

### Filesystem snapshot (no downtime)

A ZFS, Btrfs or LVM snapshot of the folder captures all files at one instant,
which SQLite treats like a power cut: consistent, with the last moments
recovered from the log on next open. Back up from the snapshot.

### SQLite online backup (no downtime)

With the `sqlite3` tool on the host:

```sh
sqlite3 db/drawbridge.db ".backup 'drawbridge-backup.db'"
cp db/secret.key secret.key.backup
```

This produces a single, consistent file even while Drawbridge writes.

### What not to do

Copying `drawbridge.db` alone while Drawbridge runs can produce a backup that
is missing recent changes or is corrupt. Always include the `-wal` file, or use
one of the methods above.

## Restoring

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

1. Take a backup as above.
2. Install Drawbridge on the new host (see [Installation](installation.md)).
3. Restore the `db/` folder before the first start.

Container settings are keyed by Compose project and service (or container
name), not by container ID, so they apply on the new host as long as the stacks
have the same project names there.

## Starting over

Stop Drawbridge, delete the contents of `db/`, and start it. The setup wizard
runs again. Your containers are not affected, but any containers Drawbridge was
holding stopped stay stopped; start them yourself or click **Restore now**
first.
