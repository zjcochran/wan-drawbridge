# Security

## What is being protected

Drawbridge has the Docker socket, which is equivalent to root on the host. Its
web UI can stop and start containers and change what is stopped. The threat
model:

| Threat | Mitigation |
| --- | --- |
| Someone on the network uses the UI | Authentication required for every page except `/login`, `/setup` (until an admin exists), `/healthz`, `/metrics` (token), `/api/status` (API token), static files |
| Password guessing | argon2id hashes, minimum 12 characters, rate limit per IP and per username |
| Stolen session cookie | `HttpOnly`, `SameSite=Lax`, `Secure` over HTTPS; only a hash stored server-side; sessions end on password change or disable |
| Cross-site request forgery | Per-session CSRF token on every POST, plus Go's cross-origin protection |
| Script injection | `html/template` escaping; strict Content-Security-Policy without inline scripts |
| Clickjacking | `X-Frame-Options: DENY`, `frame-ancestors 'none'` |
| Forged proxy headers | Only honoured from configured trusted proxies |
| Single sign-on attacks | Authorization code + PKCE (S256), state, nonce, ID-token verification, issuer+subject matching |
| Leaked database backup | Passwords hashed; session tokens hashed; client secret encrypted with a key stored separately (or in the environment) |
| Open redirect after sign-in | `next` must be a local path |

What Drawbridge does **not** protect against: someone with access to the host,
the Docker socket or the database file; a malicious OIDC provider you
configured; and network attackers when served over plain HTTP. Serve it over
HTTPS (see [Behind a reverse proxy](../user/reverse-proxy.md)) whenever it is
reachable by anyone you do not trust.

## Setup window

Until the first user exists, every request is redirected to `/setup`, and
`POST /setup/admin` creates an admin and `POST /setup/restore` restores a
backup file, both **without authentication**. After that both return 403.
Complete setup immediately after the first start.

## Passwords

- Minimum length 12 (`store.MinPasswordLength`); no other composition rules.
- Hashed with **argon2id**, memory 64 MiB, 3 iterations, parallelism 2, 16-byte
  random salt, 32-byte key (RFC 9106's second recommended parameter set),
  stored as a PHC string:
  `$argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>`.
- Verification uses the parameters stored in the hash, so they can be raised
  later without invalidating existing hashes, and compares in constant time.
- A sign-in for an unknown username verifies against a dummy hash, so it takes
  as long as a real one and does not reveal which usernames exist.
- Usernames are case-insensitive (`COLLATE NOCASE`).

## Two-factor sign-in

Optional per account, for password sign-in (`internal/totp`, RFC 6238):

- HMAC-SHA1, 6 digits, 30-second steps, 160-bit random secret; one step either
  side is accepted. The secret is sealed with the instance key like other
  stored secrets.
- `users.totp_step` keeps the last accepted step, updated with
  `UPDATE … WHERE totp_step < ?`, so each code (and any older one) works once,
  even with two sign-ins racing.
- 10 recovery codes of 12 characters (about 59 bits each) from a 31-character
  alphabet, stored as SHA-256 hashes and deleted when used.
- After the right password, no session is made: a random 256-bit token in the
  `drawbridge_2fa` cookie (path `/login`, HttpOnly, SameSite=Strict) points to
  an in-memory pending sign-in that lasts 5 minutes and allows 5 codes. Wrong
  codes also count toward the sign-in rate limit, against the same IP and
  username keys. `last_login` is set only when the code is right.
- Setting it up, making new recovery codes and turning it off need the
  current password. During setup the secret travels to the browser and back
  sealed, so it is stored only after a right code.
- It is not offered to accounts linked to single sign-on, and linking turns it
  off: those accounts rely on the provider's second factor.
- An admin can turn it off for another user; `drawbridge reset-2fa` does it
  from the command line.

## Sign-in rate limit

In memory, per process (cleared on restart). A failure is counted against two
keys: `ip:<client IP>` and `user:<lower-cased username>`. A sign-in attempt is
refused with 429 while **either** key has 5 or more failures in the last 15
minutes (sliding window). A successful sign-in clears both keys. The client IP
comes from trusted proxies' headers when configured (see below).

## Sessions

- Created at sign-in: a 32-byte random token (base64url) in the cookie
  `drawbridge_session`. The database stores only its SHA-256, so a database
  leak does not reveal usable tokens.
- Cookie: `Path=/`, `HttpOnly`, `SameSite=Lax`, `Max-Age` 7 days, `Secure` when
  the request arrived over HTTPS (directly, through a trusted proxy with
  `X-Forwarded-Proto: https`, or when `DRAWBRIDGE_BASE_URL` is `https://…`).
- Lifetime: 7 days, sliding. Each authenticated request extends the expiry,
  at most once per hour.
- Ended by: signing out (row deleted, cookie cleared), changing or resetting
  the password (all of that user's sessions deleted), disabling the user (all
  deleted), expiry (pruned daily).
- A disabled user's existing session is rejected even before it is deleted.

## CSRF and cross-origin requests

Two independent layers:

1. **CSRF token.** Each session has a random token. Every authenticated
   `POST` must carry it in the `X-CSRF-Token` header (HTMX sends it from the
   `hx-headers` attribute on `<body>`) or the `_csrf` form field; otherwise 403
   "invalid or missing CSRF token; reload the page". Compared exactly.
2. **Cross-origin protection.** Go's `http.CrossOriginProtection` wraps the
   whole handler and rejects unsafe-method requests that browsers mark as
   cross-site (`Sec-Fetch-Site`) or whose `Origin` does not match the `Host`.
   `DRAWBRIDGE_BASE_URL` is added as a trusted origin, for proxies that rewrite
   `Host`.

`POST /login` and `POST /setup/admin` have no session yet and rely on layer 2.

## Authorisation

| Role | Can |
| --- | --- |
| viewer | Read every page except Users; change own password; link or unlink own single sign-on |
| admin | Everything |

Enforced per route by middleware (`auth`, `admin`), not only by hiding
controls. Safeguards in the store: the last enabled admin cannot be disabled
or demoted; users cannot disable themselves (web UI).

## Response headers

Every response:

```
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
Referrer-Policy: same-origin
Content-Security-Policy: default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'
```

There is no inline JavaScript: `script-src` falls back to `'self'`, and all
scripts are served from `/static/`. `'unsafe-inline'` for styles is needed by
HTMX's injected indicator styles and the timeline's computed tick positions;
only numbers generated by the server are placed in style attributes, never
user input.

`Strict-Transport-Security` is not set by Drawbridge; set it at your HTTPS
reverse proxy.

## Trusted proxies

`DRAWBRIDGE_TRUSTED_PROXIES` (IPs or CIDR ranges). For a request whose direct
peer is trusted:

- **Client IP:** walk `X-Forwarded-For` (all values joined) from the right,
  skipping addresses that are themselves trusted; the first untrusted address
  is the client. Stop at an unparseable entry. If none is found, use
  `X-Real-IP`, else the peer.
- **HTTPS:** `X-Forwarded-Proto: https`.
- **Host for the callback URL:** `X-Forwarded-Host`.

From any other peer, these headers are ignored and the peer address is used.
Walking from the right means a client cannot choose its apparent address by
sending its own `X-Forwarded-For`: proxies append, so the rightmost untrusted
entry is the one the trusted proxy saw.

## Single sign-on

- Discovery from `<issuer>/.well-known/openid-configuration`, cached until the
  settings change (10 s timeout).
- Authorization code flow with **PKCE S256**. Per sign-in: random `state`
  (24 bytes), `nonce` (24 bytes) and verifier, kept in memory for 10 minutes
  and usable once.
- The ID token is verified by go-oidc: signature against the provider's JWKS,
  `iss` equal to the configured issuer, `aud` containing the client ID, expiry.
  Drawbridge then checks the `nonce`.
- Signature algorithms: those go-oidc supports (RS, ES and PS 256/384/512 and
  EdDSA) that the discovery document lists. HS256 is added only when the
  provider lists it and a client secret is saved; its key is the client
  secret (OIDC Core 10.1) and must be at least 32 bytes (RFC 7518). An HS256
  token is never checked against the JWKS and an asymmetric token never
  against the secret, so the public keys cannot be used as an HMAC key
  (algorithm confusion). Public clients cannot use HS256.
- Scopes: `openid profile email`, plus `groups` when `scopes_supported` lists
  it or is absent.
- Accounts are matched by `(issuer, subject)`. Username and email are never
  used to match, so an attacker who controls an account named like a local
  user at the provider gets a separate, provisioned account at most.
- Allowed group and admin group are compared with the `groups` claim
  ignoring case (`strings.EqualFold`). Groups come from the claim (ID token, or
  userinfo if absent).
- Linking requires being signed in already, and the callback checks that the
  same user is still signed in.
- "SSO only" can be enabled only by an admin whose own account is linked.
  `DRAWBRIDGE_LOCAL_LOGIN=force` overrides it from the environment.
- Profile pictures: the URL comes from the verified ID token's `picture`
  claim and must be `http` or `https`. The access token is sent only when the
  URL has the issuer's scheme and host. At most 1 MiB is read, within 10 s,
  and only PNG, JPEG, GIF and WebP (sniffed with `http.DetectContentType`)
  are stored; SVG is refused because, served from Drawbridge's origin, it
  could run script. Uploads follow the same limits. Pictures are served only
  to their user and to admins.
- The post-login redirect (`next`) is restricted to local paths: it must start
  with `/`, must not start with `//`, and must not contain `\`.

## Stored secrets

The OIDC client secret, each notification channel's URL, token and user, and
each user's two-factor secret are
sealed with AES-256-GCM (a webhook URL, a Discord one in particular, is itself
a credential):

- Key: SHA-256 of `DRAWBRIDGE_SECRET_KEY`, or of the contents of
  `$DRAWBRIDGE_DB_DIR/secret.key`. If neither exists, 32 random bytes are
  generated, base64-encoded and written to `secret.key` with mode 0600.
- Format: `v1:` + base64(12-byte random nonce ‖ ciphertext ‖ tag).
- Secrets are never sent back to the browser: the forms only show whether one
  is saved, and a notification channel shows only its URL's host.

The key file is in the database folder so that backups include it. If you
store backups somewhere less trusted than the host, set `DRAWBRIDGE_SECRET_KEY`
instead and keep it out of the backup.

## Backup files

Backup files (Settings → Backup and restore) hold password hashes, the
single sign-on client secret and notification URLs, tokens and users, so the data is
encrypted with the passphrase given at download:

- Key: argon2id(passphrase, 16-byte random salt, time 3, memory 64 MiB,
  2 threads) → 32 bytes. The parameters are stored in the file.
- Data: the gzipped JSON payload, sealed with AES-256-GCM (12-byte random
  nonce, the format name as additional data). A wrong passphrase and a
  modified file fail the same way.
- The client secret and the notification URLs, tokens and users are unsealed with
  this instance's key before encryption, and sealed with the restoring
  instance's key on restore, so `secret.key` is not needed to move a backup.
- Readable without the passphrase: the format name and version, the
  Drawbridge version, the creation time, the schema version and the history
  range. The encrypted copy of these is the one used.
- Passphrases must be at least 12 characters.

Restoring is limited so a crafted file cannot exhaust the host: the upload is
at most 128 MB, the decompressed payload at most 1 GiB, and files asking for
more than 256 MiB of memory, 10 passes or 8 threads of argon2id are rejected
before any key is derived. Restored rows may only name existing tables and
columns, and are written in one transaction that is rolled back if any row is
rejected or no enabled admin remains.

Scheduled backups use a passphrase stored in the database, sealed with the
secret key like the other secrets, because nobody is there to type it. The
backup files alone stay protected by it; whoever has the database folder and
the key can recover it. Old files are deleted only if their names match
`drawbridge-backup-YYYYMMDD-HHMMSS.json`, and downloads of stored files accept
only such names, so no other path can be read.

## Notifications

Notification requests go only to the URLs admins configure (Pushover's and
Telegram's own APIs for those kinds). They carry the event's title, message
and fields (ISP names, ASNs, usernames in `by`), never secrets. Email is only
sent encrypted: STARTTLS is required on `smtp://`, `smtps://` uses TLS from the
start, certificates are verified (TLS 1.2 or newer), and the password is sent
only after that. Errors show only the scheme and host of a URL, never a path
that may hold a token. Test messages use the URL from the form, so only admins can send
them. Answers are read only for the status code and the first 300 bytes of an
error.

## Metrics endpoint

Off unless `DRAWBRIDGE_METRICS_TOKEN` is set (404). With a token, requires
`Authorization: Bearer <token>`, compared in constant time; otherwise 401 with
`WWW-Authenticate`. Metrics reveal the ISP name, ASN and container counts, not
container names or IPs.

## API tokens

`/api/status` needs a token made by an admin in Settings → API tokens:
`dbt_` followed by 32 random bytes in base64url. The token is shown once;
only its SHA-256 hash is stored, and a request is matched by hashing the
presented token. A token can only read the status (the same facts as the
dashboard, including container names and the public IP); it cannot sign in
or change anything. Revoking deletes it immediately. Tokens are not in backup
files.

## Docker access

Drawbridge uses these Docker API calls only: version, list containers, inspect
container, stop, start, and the container event stream. It never creates,
removes, or execs into containers and never reads their logs or environment.
The socket nonetheless grants full control; protect the UI accordingly.

## Logging

Logs contain usernames and client IPs for failed sign-ins, container names, and
public IPs from checks. They never contain passwords, session tokens, CSRF
tokens or the client secret.

With `DRAWBRIDGE_SYSLOG` the same lines go to the syslog server. Over `udp://`
and `tcp://` they cross the network unencrypted. `tls://` uses TLS 1.2 or
later and verifies the server's certificate and host name against the system
CAs plus `DRAWBRIDGE_SYSLOG_CA_FILE`; there is no option to skip verification
and no client certificate.

## Reporting vulnerabilities

Open a confidential issue in the GitLab project.
