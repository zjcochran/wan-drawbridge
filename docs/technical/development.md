# Development

## Prerequisites

- Go 1.26 (`go.mod`), no cgo needed.
- Docker with Compose, for the image and the smoke test.
- Optional: `sqlite3` for inspecting databases; Google Chrome for UI
  screenshots.

## Build and run locally

```sh
go build -o drawbridge ./cmd/drawbridge

DRAWBRIDGE_DB_DIR=./db DRAWBRIDGE_CACHE_DIR=./cache LOG_LEVEL=debug ./drawbridge
```

Without Docker on the machine, Drawbridge logs that it cannot reach Docker and
continues. To exercise container control without real containers, point
`DOCKER_HOST` at a fake Engine API (any HTTP server implementing the seven calls
in [Container control](container-control.md#docker-client)), and point
`DRAWBRIDGE_IP_PROVIDERS` at a server returning the IP you want to simulate.

Build the image with version information:

```sh
docker build --build-arg VERSION=v1.2.3 --build-arg SOURCE_URL=https://github.com/zjcochran/wan-drawbridge -t drawbridge .
```

The image is `gcr.io/distroless/static-debian13` with the single static binary
at `/drawbridge`, the `HEALTHCHECK`, `EXPOSE 8080`, volumes `/data/db` and
`/data/cache`, and OCI labels (including `licenses=AGPL-3.0-or-later`).

## Code layout

```
cmd/drawbridge/        entry point, CLI subcommands
internal/
  config/              environment, defaults, validation
  probe/               public IP quorum
  asn/                 IP → ASN (iptoasn table, ipinfo fallback)
  learner/             primary ISP decision (pure)
  wan/                 failover state machine (pure)
  monitor/             check loop, owns WAN state
  executor/            container runs, inventory
  docker/              Engine API client
  store/               SQLite: all persistence
    migrations/        goose SQL migrations (embedded)
  secrets/             AES-GCM sealing
  web/                 HTTP, templates, OIDC, metrics, SSE
    templates/         html/template pages and partials
    static/            htmx, sse extension, app.js, app.css, icon
  e2e/                 end-to-end test
test/
  coverage.sh          per-package coverage gate
  smoke/               real-Docker smoke test
docs/                  this documentation
```

## Conventions

- **Go style:** `gofmt`, `go vet` and `golangci-lint` with no issues (see
  [Testing and CI → The linters](testing-and-ci.md#the-linters)). Small packages with small interfaces at the
  consumer (`executor.Docker`, `web.Monitor`, …). Errors are wrapped with
  context; user-facing messages are full sentences.
- **Licence headers:** every Go file starts with
  `// SPDX-License-Identifier: AGPL-3.0-or-later`.
- **Comments** explain why, not what. Package comments describe the package's
  role.
- **Pure decisions:** keep decision logic (like `wan` and `learner`) free of
  I/O so it can be tested with simulated time.
- **One owner for WAN state:** only the monitor loop touches the state machine.
  New commands go through `do` (wait) or `post` (fire and forget).
- **No JavaScript build.** Pages are server-rendered; interactivity is HTMX
  attributes. Any script goes in `static/app.js` (the CSP forbids inline
  scripts). New partials go in `partials.html` or a page's file as named
  `define` blocks, returned to HTMX requests with `s.partial`.
- **Live updates:** publish `TopicState` or `TopicInventory` on the hub after a
  change; fragments that depend on it use `hx-trigger="sse:<topic>"`.
- **Events:** record user-visible changes with `Store.AddEvent(type, payload)`
  and put the acting user in `by`. Document new event types in
  [Monitoring](../user/monitoring.md#event-types).
- **Commits:** short imperative sentences.

## Adding a migration

1. Create `internal/store/migrations/0000N_description.sql` with the next
   number:

   ```sql
   -- +goose Up
   ALTER TABLE containers ADD COLUMN note TEXT NOT NULL DEFAULT '';

   -- +goose Down
   ALTER TABLE containers DROP COLUMN note;
   ```

2. Never edit a migration that has been released; add a new one.
3. Make new columns `NOT NULL` with a default where possible, so existing rows
   are valid.
4. Update [Data model](data-model.md) and the store code and tests.

Migrations run in a transaction per file at start-up.

## Adding a setting

- **Environment only:** add it to `config.Config` and `config.Load`, document
  it in [Configuration reference](configuration-reference.md), and add it to
  `config_test.go`.
- **Detection (editable in the UI):** add the field to `config.Detection` with
  a JSON tag, a default in `DefaultDetection`, a limit in `Limits` and a check
  in `Validate`; the form field in `detection-fields` (partials) and
  `applyDetectionForm`/`formFromDetection`; the environment variable in
  `Load`. Old saved settings lack the field and get the default, because they
  are decoded on top of `DefaultDetection()`.

## UI screenshots

The screenshots in `docs/images` were taken with headless Chrome (chromedp)
against a running Drawbridge with a fake Docker API (nine containers in
`media`, `proxy` and two standalone) and a fake IP service switched between
`203.0.113.3` (Examplenet, AS64500) and `198.51.100.2` (Skylink, AS64510). Retake them
when the UI changes visibly.

## Workflow and releasing

All changes reach the default branch through merge requests:

1. Branch from `main`, commit, push.
2. Open a merge request and give it one label: `major`, `minor` or `patch`
   (see [Testing and CI → Releases](testing-and-ci.md#releases)).
3. When the pipeline is green, the owner merges. The merge creates the next
   release automatically (for example `v0.1.0` → `v0.2.0` for a `minor` merge
   request), copies it to GitHub and publishes the image to ghcr.io.

GitLab is the source of truth. Never commit or tag on GitHub: the next sync
would be rejected. Never write the internal GitLab host or domain into a file
or commit message; publishing fails if it appears (see
[Testing and CI → Nothing internal is published](testing-and-ci.md#nothing-internal-is-published)).

With `glab`:

```sh
git switch -c better-timeline
git push -u origin better-timeline
glab mr create --fill --label minor --target-branch main
glab mr merge --when-pipeline-succeeds
```

Do not create version tags by hand; the release job derives each version from
the latest tag.

## Licence

AGPL-3.0-or-later. Every page links to the source of the running version
(`DRAWBRIDGE_SOURCE_URL`, set from the CI project URL at build time), which
satisfies section 13 for unmodified builds. Anyone running a modified version
for others must point that link at their modified source.
