# Behind a reverse proxy

Drawbridge serves plain HTTP on port 8080. To reach it by name over HTTPS, put
it behind your reverse proxy (Traefik, Caddy, nginx, …).

## The two settings

```yaml
environment:
  DRAWBRIDGE_BASE_URL: https://drawbridge.example.com
  DRAWBRIDGE_TRUSTED_PROXIES: 192.0.2.0/24
```

**`DRAWBRIDGE_BASE_URL`** is the address people type, without a path. It is
used for:

- the single sign-on callback URL (`<base>/auth/oidc/callback`);
- marking the session cookie `Secure` when it starts with `https://`;
- accepting form posts whose `Origin` is that address, even if the proxy
  rewrites the `Host` header.

**`DRAWBRIDGE_TRUSTED_PROXIES`** lists the proxy's IP addresses or CIDR ranges,
separated by commas. Only requests that come **directly** from these addresses
may set the forwarding headers; from anywhere else the headers are ignored, so
nobody can forge them. With it set:

| Header from a trusted proxy | Used for |
| --- | --- |
| `X-Forwarded-For` | The visitor's real IP, for the sign-in limit, logs and the session record. Read from the right, skipping trusted hops, so a value the visitor sent cannot be used to impersonate another address. |
| `X-Real-IP` | Fallback when `X-Forwarded-For` has no untrusted address. |
| `X-Forwarded-Proto` | `https` marks the session cookie `Secure`. |
| `X-Forwarded-Host` | The host for the callback URL when `DRAWBRIDGE_BASE_URL` is not set. |

Without trusted proxies, the sign-in limit (5 failures per 15 minutes) counts
everyone as the proxy's address, so one person guessing passwords would lock
everyone out for 15 minutes.

### Finding the proxy's address

If the proxy runs in Docker on the same host, use its network's subnet:

```sh
docker network inspect proxy --format '{{(index .IPAM.Config 0).Subnet}}'
```

Trusting a whole Docker network is fine as long as only the proxy can reach
Drawbridge on it. If other containers share that network and you do not trust
them, use the proxy container's fixed IP instead.

## Live updates (Server-Sent Events)

Pages keep a long-lived request open to `/sse` for live updates. The proxy must
pass it through without buffering:

- Drawbridge sends `X-Accel-Buffering: no`, which tells nginx not to buffer.
- A keep-alive comment is sent every 25 seconds, so proxy read timeouts of 60
  seconds (the usual default) are fine. Do not set them below 30 seconds.

## Traefik

Drawbridge on the same Docker network as Traefik:

```yaml
services:
  drawbridge:
    # …as in the main compose file, without the ports: section…
    networks: [proxy]
    labels:
      drawbridge.self: "true"
      traefik.enable: "true"
      traefik.http.routers.drawbridge.rule: Host(`drawbridge.example.com`)
      traefik.http.routers.drawbridge.entrypoints: websecure
      traefik.http.routers.drawbridge.tls: "true"
      traefik.http.services.drawbridge.loadbalancer.server.port: "8080"
    environment:
      DRAWBRIDGE_BASE_URL: https://drawbridge.example.com
      DRAWBRIDGE_TRUSTED_PROXIES: 192.0.2.0/24   # the proxy network's subnet

networks:
  proxy:
    external: true
```

Traefik sets `X-Forwarded-For` and `X-Forwarded-Proto` and does not buffer
event streams. No further configuration is needed.

## Caddy

```caddy
drawbridge.example.com {
	reverse_proxy drawbridge:8080
}
```

Caddy sets the forwarding headers and flushes event streams immediately.

## nginx

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name drawbridge.example.com;
    # ssl_certificate …; ssl_certificate_key …;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Connection "";
        proxy_read_timeout 120s;
    }
}
```

With nginx on the host itself, `DRAWBRIDGE_TRUSTED_PROXIES` is the address
nginx connects from: the Docker bridge's gateway address when it connects to
the published port (`docker network inspect bridge --format
'{{(index .IPAM.Config 0).Gateway}}'` shows it), or `127.0.0.1` with host
networking.

## Sub-paths are not supported

Drawbridge must be served at the root of its own host name
(`https://drawbridge.example.com/`). A sub-path such as
`https://example.com/drawbridge/` does not work: pages use absolute paths.

## Checking it works

1. Open the public URL and sign in.
2. **Settings → Single sign-on** shows the callback URL; it should start with
   your public URL.
3. In the browser's developer tools, the `drawbridge_session` cookie should be
   marked `Secure`.
4. Leave the dashboard open and click **Check now** in another tab: "Last
   check" in the first tab should update within a second (live updates work).
5. After a wrong password, the log line `failed sign-in … ip=` should show your
   own address, not the proxy's.

## Forward authentication

Drawbridge has its own sign-in and does not read identity headers from a proxy
(`Remote-User` and similar). You can still put forward-auth in front of it as
an extra gate, but people then sign in twice. Single sign-on with the same
provider usually makes that seamless.
