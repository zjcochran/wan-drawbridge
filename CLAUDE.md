# WAN Drawbridge: notes for Claude

README.md covers what the app does, its configuration and how it works. This file
records the decisions behind it and how to work in the repo.

## Decisions made with the user (don't revisit them)

- Go + server-rendered `html/template` + vendored HTMX. **No Node.js**, no JS build step.
- SQLite only (`modernc.org/sqlite`, pure Go, WAL mode). No Postgres. The DB lives in
  `/data/db` and nothing else goes there, so the folder can be mounted for backups.
  It also holds `secret.key`.
- Docker access is through a direct `/var/run/docker.sock` mount. There is no socket proxy.
- The app is not built for load balancing: one instance, one Docker host.
- An ISP is a group of ASNs: names are grouped by AS handle (`EXAMPLENET-64500` → `EXAMPLENET`).
  Every comparison is ISP to ISP, never ASN to ASN.
- An unknown ISP counts as a backup. A total outage (nothing conclusive) never triggers
  a failover. Relearning only suggests a new primary; it never switches by itself.
- Stack and container rules:
  - Changing a stack's setting resets all of its containers to inherit it.
  - Overriding one container in a disabled stack converts the stack: the other
    containers become individually disabled.
  - A new service in a split stack keeps running and is flagged for review.
- Detection is time-based with flap damping (`internal/wan/state.go`):
  - Failover happens after the backup has been seen for 45 s.
  - Failback happens after the primary has been stable for 5 min. Only clean checks
    on the primary count; any blip restarts the wait.
  - The failback hold doubles on each flap, capped at 2 h.
- Reverse proxy support: `DRAWBRIDGE_TRUSTED_PROXIES` and `DRAWBRIDGE_BASE_URL`.
- License is AGPL-3.0-or-later. Every Go file starts with an SPDX header, and the
  footer links to the source (AGPL §13).
- CI is GitLab CI (`.gitlab-ci.yml`), not GitHub. Images go to ghcr.io (see below), not the GitLab registry.
- Deferred, not rejected: notifications and multiple Docker hosts.

## Working with the user

- They want things built, not more questions. If something has an obvious right
  answer (for example, grouping a multi-ASN ISP), just do it.
- Don't add anything they didn't ask for: licenses, labels, extra features.
- Commit only when asked. Commit messages are short sentences in the imperative.
- Changes go to `main` through merge requests, never by pushing directly. Each MR
  needs exactly one label, `major`, `minor` or `patch`; merging creates the next
  release automatically (`ci/release.sh`). Pick the label by semver: breaking →
  major, feature → minor, fix/docs/internal → patch. Releases started at v0.1.0.
- Never write the internal GitLab host, its domain or the GitLab project path
  into any file or commit message: everything is published to GitHub. Refer
  to "the GitLab project" or the `origin` remote; in CI use the predefined
  variables (CI_SERVER_HOST, CI_PROJECT_URL). The publish jobs fail if the
  domain appears anywhere in the published history.
- After each release, CI copies main, tags and releases to the private GitHub
  repo zjcochran/wan-drawbridge and pushes the image to ghcr.io (`publish`
  stage, `ci/github-sync.sh`; variables GHCR_REPO, GHCR_USER, GHCR_TOKEN). No
  GitHub Actions, by the user's choice; GitLab stays the source of truth.
- Everything published must be fictional: no user names, people's names,
  real ISP names, ASNs or address ranges, private IP ranges, time zones or
  real-looking self-hosted app names. Use documentation values (192.0.2.0/24,
  198.51.100.0/24, 203.0.113.0/24, ASNs 64496-64511, example.com, alice/bob).
  Screenshots are binary and not checked by the guard: take them only from the
  fictional demo data, check each one, and when replacing screenshots add the
  old blob IDs to ci/publish-strip-blobs.txt.
- Only the user merges. Open the MR, watch its pipeline, fix failures, then tell
  the user it is ready. First push a new branch with `git push -o ci.skip`, then
  `glab mr create`: otherwise a branch pipeline and the MR pipeline run side by
  side (different refs, so auto-cancel does not stop either).

## Layout

- `cmd/drawbridge`: the main binary. Runs `serve` by default and also has CLI subcommands.
- `internal/wan`: the pure state machine. `internal/monitor` owns its state in a single
  goroutine and takes commands over a channel.
- `internal/probe` (public IP quorum), `internal/asn` (iptoasn plus the ipinfo fallback),
  `internal/learner`.
- `internal/docker`: a minimal Engine API client.
- `internal/executor`: stop/start runs in dependency layers. Runs are persisted so it can
  recover after a crash.
- `internal/store`: SQLite, with goose migrations in `migrations/`. Add new migrations;
  never edit old ones.
- `internal/web`: handlers, templates, static files, and SSE via `hub.go`. The CSP is
  strict, so no inline JS: put it in `static/app.js`.
- `internal/e2e`: an in-process end-to-end test against fake Docker and a fake WAN.
- `test/smoke`: a real-Docker smoke test (`smoke.sh`).

## Testing rules (from the user)

- Every package needs at least 80% statement coverage from its own tests.
  `test/coverage.sh` enforces it in CI.
- Unit tests use mocks and fakes, never real services or the network: use
  `httptest` servers, fake interfaces, and mock `http.RoundTripper`s for
  connection failures. Don't dial closed ports. SQLite in a temp dir is fine.
- Every test, scan and coverage result must reach GitLab in a format it reads:
  JUnit, Cobertura, Code Quality, or the security report formats.
- Merges are blocked unless everything passes: tests, 80% coverage per package,
  zero golangci-lint issues (v2.14.0, `.golangci.yml`), and no Critical/High
  SAST or container findings and no secrets (`ci/security-gate.sh`). Fix
  findings; suppress only with a `//nolint:<linter> // reason` or
  `// nosemgrep` plus a reason comment.

## Documentation

User guides are in `docs/user/` and technical docs in `docs/technical/`; the
index is `docs/README.md`, and screenshots are in `docs/images/`. The docs state
exact behaviour: timings, limits, routes, schema, event types, metrics. When
you change behaviour, update the matching page in the same commit. The user
asked for the docs to be detailed and precise, so verify every figure against
the code.

## Checks before committing

```sh
gofmt -l $(git ls-files '*.go') && go vet ./... && golangci-lint run ./... && \
  go test -race -coverprofile=coverage.out ./... && test/coverage.sh coverage.out 80
```

## CI

The source of truth is the private GitLab project behind the `origin` remote,
and `glab` is logged in to it. After pushing, watch the pipeline with
`glab ci get` or `glab ci view`. Read a failed job's log with
`glab api "projects/:id/jobs/<id>/trace"`, fix the cause, and push again.
Don't ask the user to paste logs.

## Not yet verified

- `test/smoke/smoke.sh` has never been run on a real Docker host.
- Single sign-on has only been tested against a fake IdP, not a real Pocket ID.
- The reverse proxy examples in `docs/user/reverse-proxy.md` are written from
  each proxy's documentation and not yet tested against a real proxy.

