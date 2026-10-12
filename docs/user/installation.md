# Installation

Drawbridge runs as one container next to the containers it manages. It needs
the Docker socket to stop and start them, outbound HTTPS to find your public
IP, and one folder for its database.

## Requirements

| Requirement | Why |
| --- | --- |
| Linux host with Docker Engine and Docker Compose v2 | Drawbridge manages containers on the same host through the Docker API. |
| `amd64` or `arm64` | Images are built for both. |
| Outbound HTTPS (TCP 443) over IPv4 | Each check asks public-IP services for your address. IPv6-only hosts cannot be checked. |
| Access to `iptoasn.com` on first start (recommended) | Drawbridge downloads an offline IP-to-ASN database (about 7 MB compressed) and refreshes it weekly. Without it, lookups fall back to `ipinfo.io`. |
| About 200 MB of RAM and 20 MB of disk | The ASN table (about 460,000 address ranges) is held in memory, which accounts for most of the memory use (measured at about 170 MB resident). The database grows with check history, 30 days by default. |

Drawbridge is designed for **one Docker host and one instance**. Do not run
two instances against the same Docker host or the same database: they would
both act on failovers. It is not built for load balancing.

The host's traffic and Drawbridge's own traffic must take the same route to
the internet as the rest of your network. If Drawbridge's container is routed
through a VPN, it sees the VPN's public IP and cannot tell which ISP you are
on.

## Get Drawbridge

### Published image (recommended)

Every release is published as a multi-arch image (`amd64`, `arm64`) to the
private GitHub Container Registry package `ghcr.io/zjcochran/wan-drawbridge`,
tagged with the version (`0.1.0`), the minor version (`0.1`) and `latest`.
Release notes are on GitHub (**Releases**) and GitLab (**Deploy → Releases**).

The package is private, so log in once on the Docker host with a GitHub
personal access token (classic) that has the `read:packages` scope:

```sh
echo '<token>' | docker login ghcr.io -u <github-user> --password-stdin
```

The [Compose file](#the-compose-file) below uses `latest`. To control when
you upgrade, use the minor version instead:

```yaml
services:
  drawbridge:
    image: ghcr.io/zjcochran/wan-drawbridge:0.1   # follows 0.1.x fixes; pin 0.1.0 to stay put
```

### From source

```sh
git clone https://github.com/zjcochran/wan-drawbridge.git
cd wan-drawbridge
git checkout v0.1.0   # or the latest release
```

Then build the image instead of pulling it: in the Compose file, replace the
`image:` line with

```yaml
    build: .
    image: wan-drawbridge:local
```

and start it with `docker compose up -d --build`.

## The Compose file

The repository's `compose.yaml`:

```yaml
services:
  drawbridge:
    image: ghcr.io/zjcochran/wan-drawbridge:latest # or pin a release, e.g. 0.1.0
    restart: unless-stopped
    ports:
      - "8080:8080"
    labels:
      drawbridge.self: "true" # never stop this container, even if its stack is selected
    volumes:
      - ./db:/data/db # only the SQLite database: back up this folder
      - cache:/data/cache # downloaded ASN database, re-downloaded if lost
      - /var/run/docker.sock:/var/run/docker.sock
    environment:
      TZ: Etc/UTC

volumes:
  cache:
```

What each part does:

- **`ports`** publishes the web UI on port 8080 of the host. To listen on
  another port, change the left-hand number (`"9000:8080"`).
- **`labels: drawbridge.self: "true"`** marks this container as Drawbridge
  itself. Drawbridge never stops a container with this label, so you can put
  it in a stack that you also select for failover. It also recognises itself
  by its container ID, so the label is a safety net.
- **`./db:/data/db`** holds everything worth keeping: `drawbridge.db` (with its
  `-wal` and `-shm` files while running) and `secret.key`. Back up this folder.
  See [Backup and restore](backup-and-restore.md).
- **`cache:/data/cache`** holds the downloaded ASN database. It can be lost; it
  is downloaded again.
- **`/var/run/docker.sock`** gives Drawbridge the Docker API. It must be
  mounted read-write: stopping and starting containers are write operations.
  Drawbridge runs as root inside its container because the socket is only
  writable by root or the host's `docker` group, whose ID differs between
  hosts. There is no socket proxy, by design.
- **`TZ`** sets the time zone used in the UI and logs. The image includes time
  zone data, so any IANA name works.

All settings are environment variables; see the
[configuration reference](../technical/configuration-reference.md). The ones
you are most likely to add:

```yaml
    environment:
      TZ: Europe/Berlin
      DRAWBRIDGE_PRIMARY_ASN: "64500"                         # pin the primary ISP instead of learning it
      DRAWBRIDGE_BASE_URL: https://drawbridge.example.com    # behind a reverse proxy
      DRAWBRIDGE_TRUSTED_PROXIES: 192.0.2.0/24              # the proxy's address or network
      DRAWBRIDGE_METRICS_TOKEN: change-me                    # turns on /metrics
```

### Rootless Docker or a different socket

Mount your socket at `/var/run/docker.sock` inside the container, or set
`DOCKER_HOST`:

```yaml
    volumes:
      - /run/user/1000/docker.sock:/var/run/docker.sock
```

`DOCKER_HOST` accepts `unix:///path/to/docker.sock` or `tcp://host:port`
(plain HTTP, no TLS). TLS-protected TCP endpoints are not supported.

## First start

```sh
docker compose up -d   # add --build when building from source
docker compose logs -f drawbridge
```

A healthy first start logs, in order:

```
level=INFO msg="connected to Docker" version=28.3.0
level=INFO msg="web UI listening" addr=:8080
level=INFO msg="new containers found" keys="[media/player media/db …]"
level=INFO msg="asn database downloaded" path=/data/cache/ip2asn-v4.tsv.gz
level=INFO msg="asn database loaded" ranges=458942
level=INFO msg="monitor started" state=learning interval=1m0s paused=true
```

`paused=true` is expected: until the setup wizard is finished, Drawbridge
checks your connection but does not stop or start any containers.

Open `http://<host>:8080`. Every page redirects to the
[setup wizard](setup-wizard.md) until the first admin account exists.

> **Secure the first visit.** Until the admin account is created, anyone who
> can reach port 8080 can create it. Run the wizard straight after the first
> start, or publish the port only on a trusted network.

## Checking the installation

- **Health:** `docker compose ps` shows `healthy` after about 20 seconds. The
  image's `HEALTHCHECK` runs `/drawbridge healthcheck`, which requests
  `/healthz`.
- **Version:** `docker compose exec drawbridge /drawbridge version`.
- **Docker access:** Settings → System shows "connected, version …".
- **WAN:** the dashboard shows your public IP and ISP after the first check
  (one minute at most).

## Upgrading

1. Back up the `db` folder (see [Backup and restore](backup-and-restore.md)).
2. Fetch the new version and rebuild:

   With the published image, change the tag in the Compose file (`latest`
   and `0.1` move on their own) and run:

   ```sh
   docker compose pull && docker compose up -d
   ```

   From source:

   ```sh
   git fetch --tags
   git checkout v0.1.1   # the release you want; read its release notes first
   docker compose up -d --build
   ```

   Releases with a new **major** version (the first number) may need changes
   to your setup; their notes say which.

3. Check the logs. Database migrations run automatically at start-up; a
   failed migration stops the container with `migrate: …` in the log and
   leaves the database unchanged.

Upgrading during a failover is safe. Drawbridge stores its state and the list
of containers it stopped before acting, and picks up where it left off (see
[Container control → crash recovery](../technical/container-control.md#crash-recovery)).

Downgrading is not supported once a newer version has migrated the database.
Restore the backup taken before the upgrade instead.

## Uninstalling

1. On the dashboard, if Drawbridge has stopped containers, click
   **Restore now** first, or run
   `docker compose exec drawbridge /drawbridge status` to see what it holds.
2. `docker compose down -v` removes the container and the cache volume.
3. Delete the `db` folder if you do not want to keep the history.

Removing Drawbridge never removes or changes your other containers; at worst,
containers it stopped stay stopped until you start them.
