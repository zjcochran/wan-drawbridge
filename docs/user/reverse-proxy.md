# Behind a reverse proxy

Drawbridge serves plain HTTP on port 8080. To reach it by name over HTTPS, put
it behind your reverse proxy. [Examples](#examples) cover Traefik, Caddy, nginx,
Nginx Proxy Manager, HAProxy and Apache httpd.

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

## Agents

[Agents](hosts.md) on other Docker hosts call the manager at the same address
as the web UI, on paths under `/api/agent/v1/`. Nothing extra is needed on the
proxy:

- An agent keeps one request open for up to 25 seconds while it waits for
  commands, so the usual 60-second read timeouts are fine. Do not set them
  below 30 seconds.
- Agents sign every request themselves and check every command against the
  manager's key, so the proxy may terminate TLS; no passthrough is needed.
- If the proxy also uses the client address (`DRAWBRIDGE_TRUSTED_PROXIES`),
  the Hosts page shows each agent's real address.

## Examples

Written for Traefik 3.7, Caddy 2.11, nginx 1.30, Nginx Proxy Manager 2,
HAProxy 3 and Apache httpd 2.4. Drawbridge and an agent have been run behind
Traefik 3 on a real setup; the other examples are written from each proxy's
documentation and not yet tested against it. Each one serves Drawbridge at
`https://drawbridge.example.com`; replace the host name, and use
[Checking it works](#checking-it-works) afterwards.

In every Docker example Drawbridge has no `ports:` section: only the proxy
reaches it, over a shared Docker network called `proxy`. Create it once with
`docker network create proxy`.

### Traefik

Traefik and Drawbridge on the `proxy` network, with Traefik's Docker provider
reading Drawbridge's labels. If you already run Traefik, only the
`drawbridge` service matters; check that your entry point names match.

```yaml
services:
  traefik:
    image: traefik:v3.7
    restart: unless-stopped
    command:
      - --providers.docker=true
      - --providers.docker.exposedbydefault=false
      - --entrypoints.web.address=:80
      - --entrypoints.web.http.redirections.entrypoint.to=websecure
      - --entrypoints.websecure.address=:443
      # Let's Encrypt; without it Traefik serves its own self-signed certificate:
      # - --certificatesresolvers.le.acme.email=you@example.com
      # - --certificatesresolvers.le.acme.storage=/letsencrypt/acme.json
      # - --certificatesresolvers.le.acme.httpchallenge.entrypoint=web
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      # - ./letsencrypt:/letsencrypt
    networks: [proxy]

  drawbridge:
    image: ghcr.io/zjcochran/wan-drawbridge:latest
    restart: unless-stopped
    labels:
      drawbridge.self: "true"
      traefik.enable: "true"
      traefik.docker.network: proxy
      traefik.http.routers.drawbridge.rule: Host(`drawbridge.example.com`)
      traefik.http.routers.drawbridge.entrypoints: websecure
      traefik.http.routers.drawbridge.tls: "true"
      # traefik.http.routers.drawbridge.tls.certresolver: le
      traefik.http.services.drawbridge.loadbalancer.server.port: "8080"
    volumes:
      - ./db:/data/db
      - cache:/data/cache
      - /var/run/docker.sock:/var/run/docker.sock
    environment:
      TZ: Etc/UTC
      DRAWBRIDGE_BASE_URL: https://drawbridge.example.com
      DRAWBRIDGE_TRUSTED_PROXIES: 192.0.2.0/24   # the proxy network's subnet
    networks: [proxy]

volumes:
  cache:

networks:
  proxy:
    external: true
```

- `traefik.docker.network: proxy` makes Traefik use the `proxy` network. Without it, a
  container on several networks may be reached on one Traefik is not on, and
  requests time out with "Gateway Timeout".
- Traefik sets `X-Forwarded-For`, `X-Forwarded-Proto` and `X-Forwarded-Host`
  and does not buffer event streams. No middleware is needed.
- If something else sits in front of Traefik (a CDN or another proxy), set
  `--entrypoints.websecure.forwardedheaders.trustedips` to its addresses, or
  Traefik replaces the visitor's address with that proxy's.
- Do not put a `compress` middleware on this router without excluding event
  streams: it holds small responses back until it has enough to compress, which
  delays live updates. If you use one, set
  `traefik.http.middlewares.<name>.compress.excludedcontenttypes: text/event-stream`.

### Caddy

Caddy on the `proxy` network:

```caddy
drawbridge.example.com {
	reverse_proxy drawbridge:8080
}
```

With Caddy on the host and Drawbridge's port published, use
`reverse_proxy 127.0.0.1:8080`. Caddy obtains the certificate, sets
`X-Forwarded-For`, `X-Forwarded-Proto` and `X-Forwarded-Host`, and flushes
event streams immediately.

### nginx

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name drawbridge.example.com;
    ssl_certificate     /etc/ssl/drawbridge.example.com/fullchain.pem;
    ssl_certificate_key /etc/ssl/drawbridge.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;   # or http://drawbridge:8080 on the proxy network
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Connection "";
        proxy_read_timeout 120s;
    }
}
```

`http2 on;` needs nginx 1.25.1 or later; on older versions write
`listen 443 ssl http2;` instead. Buffering needs no setting: Drawbridge's
`X-Accel-Buffering: no` header turns it off for `/sse`.

With nginx on the host itself, `DRAWBRIDGE_TRUSTED_PROXIES` is the address
nginx connects from: the Docker bridge's gateway address when it connects to
the published port (`docker network inspect bridge --format
'{{(index .IPAM.Config 0).Gateway}}'` shows it), or `127.0.0.1` with host
networking.

### Nginx Proxy Manager

Put Nginx Proxy Manager and Drawbridge on the `proxy` network, then add a
**Proxy Host**:

| Tab | Field | Value |
| --- | --- | --- |
| Details | Domain Names | `drawbridge.example.com` |
| | Scheme | `http` |
| | Forward Hostname / IP | `drawbridge` (the service name) |
| | Forward Port | `8080` |
| | Websockets Support | not needed (live updates use Server-Sent Events) |
| | Block Common Exploits | either; it does not affect Drawbridge |
| SSL | SSL Certificate | Request a new certificate, or pick an existing one |
| | Force SSL | on |
| | HTTP/2 Support | on |
| Advanced | Custom Nginx Configuration | empty |

Nginx Proxy Manager sends `X-Forwarded-For`, `X-Forwarded-Proto` and
`X-Real-IP` on every proxy host, and its nginx honours Drawbridge's
`X-Accel-Buffering: no`. `DRAWBRIDGE_TRUSTED_PROXIES` is the `proxy`
network's subnet (see [Finding the proxy's address](#finding-the-proxys-address)).

### HAProxy

```haproxy
defaults
    mode http
    timeout connect 5s
    timeout client  60s
    timeout server  60s

frontend https
    bind :443 ssl crt /etc/haproxy/certs/drawbridge.example.com.pem alpn h2,http/1.1
    bind :80
    http-request redirect scheme https unless { ssl_fc }
    option forwardfor
    http-request set-header X-Forwarded-Proto https if { ssl_fc }
    use_backend drawbridge if { req.hdr(host) -i drawbridge.example.com }

backend drawbridge
    server drawbridge 127.0.0.1:8080 check
```

- `option forwardfor` adds the visitor's address to `X-Forwarded-For`.
- `timeout client` and `timeout server` must be longer than the 25-second
  keep-alive on `/sse`; 60 seconds is enough. HAProxy does not buffer
  responses, so event streams pass straight through.
- The `.pem` file holds the certificate chain followed by the private key.
- With HAProxy in Docker on the `proxy` network, use
  `server drawbridge drawbridge:8080 check` and trust that network's subnet.

### Apache httpd

Needs `mod_ssl`, `mod_proxy`, `mod_proxy_http` and `mod_headers`
(`a2enmod ssl proxy proxy_http headers` on Debian and Ubuntu):

```apache
<VirtualHost *:443>
    ServerName drawbridge.example.com
    SSLEngine on
    SSLCertificateFile    /etc/ssl/drawbridge.example.com/fullchain.pem
    SSLCertificateKeyFile /etc/ssl/drawbridge.example.com/privkey.pem
    Protocols h2 http/1.1

    ProxyPreserveHost On
    ProxyTimeout 120
    RequestHeader set X-Forwarded-Proto "https"
    ProxyPass        / http://127.0.0.1:8080/
    ProxyPassReverse / http://127.0.0.1:8080/

    # If mod_deflate is set to compress everything, keep it off the live
    # updates: it would hold them back while it compresses.
    SetEnvIf Request_URI "^/sse$" no-gzip
</VirtualHost>
```

`mod_proxy_http` adds `X-Forwarded-For` and `X-Forwarded-Host` by itself and
passes event streams through as they arrive. `Protocols h2` needs `mod_http2`;
remove the line if it is not loaded.

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
provider usually makes that seamless. Exempt `/api/` from it: the JSON
status API and the agents authenticate themselves and cannot sign in to a
forward-auth page.
