# Testing and CI

## Rules

1. **Unit tests never reach real services or the network.** Docker, the IP
   providers, iptoasn, ipinfo and the OIDC provider are replaced by in-process
   fakes (`httptest` servers or fake implementations of small interfaces).
   Connection failures are simulated with a mock `http.RoundTripper` that
   returns an error without dialing. Tests do not dial closed ports.
   SQLite in a temporary directory is used directly: it is an embedded
   library, not a service.
2. **Every package keeps at least 80% statement coverage from its own
   tests.** CI enforces this per package, not just in total.
3. **No lint issues.** `golangci-lint` with `.golangci.yml` must report zero
   issues.
4. **Every result reaches GitLab in a format it reads:** JUnit for test
   results, Cobertura for coverage, GitLab's security report formats for scans.

## Test layers

| Layer | Where | What it covers |
| --- | --- | --- |
| Pure logic | `internal/wan`, `internal/learner`, `internal/probe` (`Agree`, `ParseBody`), `store.GroupKey` | Exhaustive cases with simulated time. The state machine tests drive 15-second check sequences: delays, short drops, inconclusive checks, stale evidence, failback strictness, flapping back-off over 8 cycles, hold doubling and reset, zero delays. |
| Components | each package's `_test.go` | Real SQLite in `t.TempDir()`; fake Docker; fake WAN servers; fake OIDC provider (signs ID tokens with `go-jose`, supports PKCE, groups, userinfo); fake clock. |
| Web | `internal/web` | Full HTTP round trips with a cookie-jar client: setup, sign-in and rate limit, CSRF, cross-origin, roles, every page, every action, SSE, metrics, proxy headers, SSO flows (including state and nonce tampering), database failures (tables dropped under a live server). |
| CLI | `cmd/drawbridge` | Every subcommand run in-process through `cli(args, stdin, stdout, stderr)` against a fake Docker API and fake WAN; `serve` started and stopped on a free port; the health check. |
| End to end | `internal/e2e` | The real store, monitor, executor, Docker client and web server together, against a fake Docker Engine and fake IP services, with short timings (100 ms checks): setup and stack selection through the web UI, a reconnect onto another ASN of the same ISP (no failover), failover, a restart in the middle of a failover, failback. |
| Smoke | `test/smoke/smoke.sh` | The real image on a real Docker engine (below). |

## Running tests

```sh
go test -race ./...                                    # everything except smoke
go test -race -run TestFlapping ./internal/wan         # one test
go test -race -coverprofile=coverage.out ./... && test/coverage.sh coverage.out 80
go tool cover -html=coverage.out                       # browse coverage
```

The suite takes about 40 seconds with `-race` (most of it in `internal/web`,
which hashes many argon2id passwords).

### test/coverage.sh

```sh
test/coverage.sh <profile> <minimum percent>
```

Reads a Go cover profile, sums statements and covered statements per package
directory (counting each block once), prints one line per package and exits 1
if any package is below the minimum. Packages without statements (such as
`internal/e2e`) are skipped. Each package's coverage comes from its own tests
only, because the profile is produced without `-coverpkg`.

### Writing tests

- Prefer fakes of the small interfaces packages already define:
  `executor.Docker`, `monitor.Actor`, `web.Monitor`, `web.Docker`,
  `web.Syncer`, `executor.EventSource`.
- For HTTP dependencies, start an `httptest.Server`. For "unreachable", set the
  client's `Transport` to a mock that returns an error.
- Inject time: `Monitor.now`, `Executor.Now`, `Server.now` are function fields.
- In `internal/web`, `newEnv(t)` gives a server with a fresh database, a fake
  monitor and fake Docker; `e.admin()` returns a signed-in admin client;
  `c.htmx(path, form)` posts like HTMX with the CSRF header;
  `e.breakTables(...)` drops tables to exercise database errors after sign-in.

## Smoke test

`test/smoke/smoke.sh` builds the image and runs it against a real Docker
engine:

1. Starts a "victims" stack (`db`, `app` depending on `db`, and `idle`, which is
   stopped beforehand) and Drawbridge plus `fakeip`, a BusyBox web server whose
   answer can be switched between `8.8.8.8` (AS15169, set as primary) and
   `1.1.1.1` (AS13335). The primary is pinned with `DRAWBRIDGE_PRIMARY_ASN=15169`.
2. Creates the admin through the wizard's HTTP endpoints, selects the victims
   stack, finishes setup.
3. Waits for the primary state through `/metrics`.
4. Switches the IP to the backup and waits for `db` and `app` to stop; checks
   that Drawbridge, `fakeip` and nothing else was touched, and that
   `drawbridge_containers_stopped` is 2.
5. Switches back and waits for `db` and `app` to start; checks `idle` stayed
   stopped.

Failover and failback delays are 0 for speed; it takes about two minutes and
removes everything it created (`trap cleanup EXIT`). It needs Docker with
Compose, `curl`, and internet access (Drawbridge downloads the ASN database).

Variables: `SMOKE_HOST` and `SMOKE_BIND` (where published ports are reached; in
Docker-in-Docker the `docker` service host), and `SMOKE_JUNIT=<path>` to write
each step as a JUnit test case; a failure includes the last 60 lines of
Drawbridge's log.

## GitLab CI

`.gitlab-ci.yml` stages: `test`, `smoke`, `release`, `publish`. Versioning and releases are described in [Releases](#releases).

| Job | Stage | Image | What it does | Reports |
| --- | --- | --- | --- | --- |
| `test` | test | `golang:1.26` | gofmt check on tracked `.go` files, `go vet`, `gotestsum` running `go test -race -coverprofile`, total coverage line, Cobertura conversion, per-package 80% gate | `junit: report.xml`, `coverage_report: coverage.xml` (Cobertura), `coverage:` regex on `total: (statements) N%` |
| `code-quality` | test | `golang:1.26` | `golangci-lint` v2.14.0 with `.golangci.yml`; fails on any issue | `codequality` (Code Climate JSON) |
| `semgrep-sast` | test | GitLab template | SAST (Semgrep rules for Go). Not allowed to fail. | `sast` |
| `secret_detection` | test | GitLab template | Secret scanning of the repository. Not allowed to fail. | `secret_detection` |
| `build-image` | test | `docker:28` + dind | Builds the image and saves it as `drawbridge.tar` (artifact, 1 day) | |
| `container_scanning` | smoke | GitLab template | Scans `archive://drawbridge.tar`, including Go modules in the binary (`CS_DISABLE_LANGUAGE_VULNERABILITY_SCAN: "false"`). Not allowed to fail: findings do not fail it, a broken scan does. | `container_scanning`, `cyclonedx` SBOM |
| `security-gate` | smoke | `alpine:3` | `ci/security-gate.sh` on the three scan reports; fails on Critical or High SAST and container findings, any secret, or a missing or unreadable report. | |
| `smoke` | smoke | `docker:28` + dind | `test/smoke/smoke.sh` | `junit: smoke-report.xml` |
| `release-label` | test | `alpine:3` | Merge request pipelines only: fails unless the MR has exactly one of the labels `major`, `minor`, `patch`. | |
| `release` | release | `alpine:3` | Pushes to the default branch only: `ci/release.sh` creates the next version's tag and GitLab release. | `dotenv: release.env` |
| `github-sync` | publish | `alpine:3` | Default branch only: copies code, tags and releases to GitHub. See [Publishing to GitHub](#publishing-to-github). | |
| `github-image` | publish | `docker:28` + dind | Default branch only, when a release was made: multi-arch image to ghcr.io. | |

Tool versions pinned in the `test` job: `gotest.tools/gotestsum@v1.13.0`,
`github.com/boumenot/gocover-cobertura@v1.5.0`.

### Docker-in-Docker

The `.docker` template runs a `docker:28-dind` service with alias `docker`,
`DOCKER_HOST=tcp://docker:2375` and TLS off (`DOCKER_TLS_CERTDIR: ""`); the
daemon is reachable only inside the job's network. Jobs wait up to 30 seconds
for the daemon. **The runner must allow privileged containers**
(`privileged = true` in `config.toml`).

### Pipelines run on

Merge request pipelines, branch pipelines (skipped for branches with an open
merge request, which get the MR pipeline instead). Tags start no pipeline:
the release job creates them on a commit the merge pipeline has just tested.

### What blocks a merge

Every job in a merge request pipeline must pass. Together they enforce:

| Check | Job | Fails when |
| --- | --- | --- |
| Formatting | `test` | Any tracked `.go` file is not gofmt-formatted |
| Static checks | `test` | `go vet` reports anything |
| Tests | `test` | Any test fails (with the race detector) |
| Coverage | `test` | Any package is below 80% |
| Code quality | `code-quality` | `golangci-lint` reports any issue |
| Security | `semgrep-sast`, `secret_detection`, `container_scanning`, `security-gate` | A scanner fails, or reports a Critical/High finding or any secret |
| Image and real Docker | `build-image`, `smoke` | The image does not build or the smoke test fails |
| Version | `release-label` | The MR lacks exactly one of `major`, `minor`, `patch` |

GitLab enforces the pipeline result through two project settings (Maintainer
role), under **Settings → Merge requests → Merge checks**:

- **Pipelines must succeed**: on.
- **Skipped pipelines are considered successful**: off.

Only Maintainers may merge into `main` (protected branch), and no one may push
to it directly.

### The linters

`.golangci.yml` enables Go's standard set (errcheck, govet, ineffassign,
staticcheck, unused) plus bodyclose, copyloopvar, errorlint, gocritic, gocyclo
(complexity above 30 fails), gosec, misspell, nilerr, noctx, rowserrcheck,
sqlclosecheck, unconvert, unparam and usestdlibvars, and the gofmt formatter.
Deliberate exceptions:

- errcheck ignores a deferred `Tx.Rollback`, writes to an HTTP response
  (they fail only when the client has gone) and per-row CSV writes (the error
  is checked once after `Flush`). gosec's G104 duplicates errcheck and is off.
- Test files are exempt from errcheck, bodyclose, gosec and noctx: they talk
  only to in-process fakes.
- Single lines are excused with `//nolint:<linter> // <reason>`; the reason is
  required. Current ones: file paths that come from configuration (G304), the
  bounded argon2 key length (G115), cookies whose `Secure` flag follows the
  request's scheme (G124), and redirects to local paths only (G710).

Run it locally with the same version:

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
golangci-lint run ./...
```

### Where to see the results

| Result | In GitLab |
| --- | --- |
| Test results (unit and smoke) | Pipeline → **Tests** tab; merge request test summary |
| Coverage percentage | Pipeline list and job; can be used for a badge |
| Line coverage | Merge request diff gutter (Cobertura) |
| Lint issues | Merge request **Code Quality** widget (new and fixed issues compared with the target branch) |
| SAST, secret detection, container scanning | Job artifacts (JSON) on every tier; the Security tab and MR widgets on Ultimate |

### Semgrep suppressions

Four `// nosemgrep` comments in `internal/store` mark parameterised queries
that Semgrep flags because a value built with `fmt.Sprintf` is passed as a
bound argument, and one in `internal/web/handlers_test.go` marks a test helper
that drops fixed table names. Each has a comment explaining why. Do not add
suppressions without the same justification: the security gate fails on High
findings.

### Publishing to GitHub

After each release, the `publish` stage copies everything to the private
GitHub repository and publishes the container image. All of it runs in GitLab
CI; **no GitHub Actions run** (the repository has no workflows, and Actions can
be disabled in its settings).

| Job | What it does |
| --- | --- |
| `github-sync` | `ci/github-sync.sh`: publishes a **sanitized copy** of the default branch and every `vX.Y.Z` tag (see below), then creates each GitLab release that GitHub lacks (oldest first, same name, sanitized notes). Never force-pushes: if someone changed the GitHub repository, the push is rejected and the job fails. |
| `github-image` | Builds `linux/amd64` and `linux/arm64` with buildx (QEMU via `tonistiigi/binfmt`) and pushes `ghcr.io/<repo>:X.Y.Z`, `:X.Y` and `:latest`. Built with `VERSION=vX.Y.Z` and `SOURCE_URL=https://github.com/<repo>`; the source label links the package to the repository, so it shares the repository's (private) visibility. Skipped when the pipeline released nothing. |

#### Nothing internal is published

The GitLab server's host name and domain must never reach GitHub. Two
mechanisms ensure it:

1. **Sanitizing.** `github-sync` copies the branch and tags into a scratch
   repository and rewrites it with `git filter-repo`:
   - in every file version and commit message, the GitLab project URL,
     `host/project-path` and the project path become the GitHub repository's,
     and the rules in `PUBLISH_REPLACEMENTS` are applied (private values that
     older commits contain, mapped to the fictional values used today);
   - every author and committer becomes `GHCR_USER` with GitHub's no-reply
     address;
   - `Claude-Session:` trailers are removed from messages;
   - the image versions listed in `ci/publish-strip-blobs.txt` (old
     screenshots) are dropped.

   The built-in rules are built at run time from GitLab's predefined
   variables (`CI_PROJECT_URL`, `CI_SERVER_HOST`,
   `CI_PROJECT_PATH`), so the internal names are never stored in the
   repository. Release notes get the same replacements (GitLab compare links
   become GitHub compare links). The rewrite is deterministic: each commit has
   the same GitHub hash on every run, so syncs are fast-forwards. GitHub hashes
   differ from GitLab's; commit order, dates and tags are preserved.
2. **A guard.** `ci/leak-check.sh` fails the job, before anything is pushed,
   if the internal host or its domain, or any pattern in `PUBLISH_DENYLIST`,
   appears (case-insensitive) anywhere in the sanitized history (every commit's
   author, committer, message and file versions), in a release note, or in the
   files the image is built from. A leak the rules do not cover therefore stops
   publishing instead of going out. If the check itself fails (for example an
   invalid denylist pattern), publishing stops too.

The source itself contains no internal names: the Go module is
`github.com/zjcochran/wan-drawbridge`, and docs and `CLAUDE.md` refer to "the
GitLab project" or the `origin` remote.

Both run only on pushes to the default branch, after `release` (which passes
`RELEASE_TAG` to them through a dotenv artifact). `github-sync` uses
`resource_group: github`, so syncs never overlap.

CI/CD variables (Settings → CI/CD → Variables):

| Variable | Value | Flags |
| --- | --- | --- |
| `GHCR_REPO` | `zjcochran/wan-drawbridge` (also accepted: `ghcr.io/…` or `https://github.com/…`) | |
| `GHCR_USER` | the GitHub user name that owns the token | |
| `GHCR_TOKEN` | GitHub **classic** personal access token with scopes `repo` and `write:packages` | Masked, Protected |
| `PUBLISH_REPLACEMENTS` | Optional. `git filter-repo --replace-text` rules, one per line (`literal==>replacement` or `regex:pattern==>replacement`), for private values in older commits. Kept only in GitLab. | Type **File** recommended |
| `PUBLISH_DENYLIST` | Optional. One extended regular expression per line for anything private: host names, LAN ranges, names, email addresses. Kept only in GitLab, never in the repository. | Type **File** recommended |
| `GITHUB_FORCE_SYNC` | `true` only to overwrite GitHub once, for example if the GitHub repository was created with a README; remove it afterwards | |

A classic token is needed because ghcr.io does not accept fine-grained tokens.
When it expires, both jobs fail with 401; create a new one and update
`GHCR_TOKEN`. Re-running `github-sync` is always safe.

## Releases

Drawbridge uses [semantic versioning](https://semver.org): `vX.Y.Z`. Every
merge to the default branch creates a release; nothing is released by hand.

### The label decides the version

Each merge request carries exactly one of these labels:

| Label | Use it for | Example |
| --- | --- | --- |
| `major` | Changes that break existing setups: removed or renamed settings, changed defaults that alter behaviour, a database change that cannot be downgraded | v0.4.2 → v1.0.0 |
| `minor` | New features that keep existing setups working | v0.4.2 → v0.5.0 |
| `patch` | Fixes, documentation, internal changes | v0.4.2 → v0.4.3 |

The `release-label` job fails the merge request pipeline until exactly one is
set. GitLab does not re-run a pipeline when labels change: after adding the
label, click **Run pipeline** on the merge request.

### What happens on merge

The default-branch pipeline runs every test and scan; if they pass, the
`release` job runs `ci/release.sh`:

1. Skip if the commit already has a `vX.Y.Z` tag (a retried job).
2. Find the merge request that produced the commit
   (`GET /projects/:id/repository/commits/:sha/merge_requests`, or else the
   recent merged MRs whose merge, squash or head commit is this commit). No
   merge request (a direct push) means no release.
3. Read its release label; none or several fails the job.
4. Take the highest `vX.Y.Z` tag (`v0.0.0` if none) and bump the labelled part,
   resetting the parts after it.
5. Create the GitLab release with the API; GitLab creates the tag on the merge
   commit. The release notes contain the MR's title and description, the
   commits since the previous release, and a compare link.

The `publish` stage then copies the release to GitHub and pushes the image to
ghcr.io (see [Publishing to GitHub](#publishing-to-github)).

The job uses `resource_group: release`, so merges in quick succession are
released one at a time, in order. API errors fail the job instead of skipping
the release; retrying is safe.

### RELEASE_TOKEN

The CI job token cannot read merge requests, so the release job uses a
project access token in the CI/CD variable `RELEASE_TOKEN`:

| Setting | Value |
| --- | --- |
| Token | Project access token named `release-bot`, role **Developer**, scope **`api`** |
| Variable | `RELEASE_TOKEN`, **masked**, **protected** (only available on protected branches such as the default branch) |

Project access tokens expire (at most a year); when it does, the release job
fails with 401. Create a new token and update the variable.

### Checking it locally

`ci/release.sh` only needs `sh`, `git`, `curl` and `jq`, and the CI variables it
reads (`CI_API_V4_URL`, `CI_PROJECT_ID`, `CI_PROJECT_URL`, `CI_COMMIT_SHA`,
`CI_DEFAULT_BRANCH`, `RELEASE_TOKEN`), so it can be run against a fake API to
test changes.
