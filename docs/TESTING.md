# Testing fleet

fleet's test suite is the set of CI lanes that gate every pull request, plus a
non-blocking nightly drift canary. This document is the map of those lanes: what
each one checks, the **exact** command CI runs, and how to reproduce it locally.

The guiding principle is **CI == local**: the convenience `make` targets here
delegate to the same commands the workflows run, so "make it green locally" and
"make CI green" are the same act. The source of truth is, and remains, the
workflow files themselves:

- [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) — the PR gates
  (every job must be green to merge).
- [`.github/workflows/e2e-canary.yml`](../.github/workflows/e2e-canary.yml) —
  the nightly real-model canary (never a PR gate).
- [`.github/workflows/grype-scheduled.yml`](../.github/workflows/grype-scheduled.yml)
  — a weekly, non-blocking container-image vulnerability scan (never a PR
  gate).

If a command here ever disagrees with those files, the workflow wins — please
fix this doc (and the `make` targets) to match.

## The lanes at a glance

| Lane | CI job | What it gates | Local |
| --- | --- | --- | --- |
| Secret scan | `gitleaks` | No secrets committed | `gitleaks dir . --redact --exit-code 1` |
| Go build | `go` | Release binary compiles (host executor fenced out) | `make compile` |
| Go vet | `go` | `go vet` clean (tagged) | part of `make ci-go` |
| Go lint | `go` | `golangci-lint` full gate (zero findings) | `make lint` |
| Go test | `go` | Unit + integration suites + coverage profile (needs Postgres) | `make test` |
| Go coverage | `go` | Coverage profile summarised in the log + job summary (advisory, no threshold) | `make test-cover` |
| Go test -race | `go` | Race detector on the same suites | `make test-race` |
| govulncheck | `go` | Dependency CVEs reachable from fleet | `make govulncheck` |
| Grype (image) | `grype-scan` | CVEs in the sandbox container image (fail on a fixable CRITICAL) | see below |
| Web lint/test/build | `web` | ESLint + vitest + `next build` | `make ci-web` |
| Playwright (mocked) | `playwright` | Deterministic browser e2e, no backend | `make ci-e2e-mocked` |
| Playwright (live) | `e2e-live` | Real stack + rootless-Podman sandbox, fake LLM | `npm run test:e2e:live` |
| Playwright (canary) | `canary` (nightly) | Real cheap OpenRouter model, drift detection | `npm run test:e2e:canary` |

The fast PR-gate subset (everything except the browser/sandbox e2e lanes) is one
command: `make ci-local`.

## Which lanes run where — `dev` vs `main`

The table above is [`ci.yml`](../.github/workflows/ci.yml), and **`ci.yml` only
fires on `main`** (its `push` and `pull_request` triggers filter to it). Work
normally lands on the `dev` integration branch first — Dependabot targets `dev`
by default, see [`dependabot.yml`](../.github/dependabot.yml) — so `dev` has its
own lane, [`dev-ci.yml`](../.github/workflows/dev-ci.yml), and the two tiers
divide the work like this:

| | `dev-ci.yml` (fast lane) | `ci.yml` (full gate) |
| --- | --- | --- |
| **Fires on** | PRs into `dev`, and pushes to `dev` | PRs into `main`, and pushes to `main` — in practice, the dev→main promotion PR |
| **Runs** | Go compile / vet / lint / test (with Postgres), web lint / test / build, migration DDL lint, gitleaks | everything in the table above |
| **Skips** | `-race`, govulncheck, Grype, both Playwright suites, CodeQL | nothing |
| **Aggregate check** | `Dev gate` | `CI gate` |

The split is "does it compile, lint, and pass tests" on `dev`; "is it safe to
ship" on the promotion. The skipped lanes are the slow ones, and none of them is
what a routine change breaks.

> **Both triggers on the fast lane matter.** The `pull_request` trigger was added
> after a period when PRs into `dev` were gated by nothing but CodeQL, which made
> `dev` itself the first thing to ever build a change. Every Dependabot PR merged
> unverified, and a `next` 16.3.0 bump landed a TypeScript error that only
> surfaced at the promotion — where it is far more expensive to unpick, because
> several changes are already stacked on it. The web lane was missing from this
> file entirely for the same reason: nothing on `dev` ran `web/` at all.

## Convenience targets that mirror CI

The Makefile exposes targets that run the same commands as the CI jobs above.
They delegate to the real commands — they do not reimplement them:

```sh
make govulncheck     # the 'go' job's govulncheck step, verbatim
make ci-go           # the full 'go' job: build → vet → lint → test → test-race → govulncheck
make ci-web          # the 'web' job: npm ci → lint → vitest → build (in web/)
make ci-e2e-mocked   # the 'playwright' job: the mocked Playwright project (in web/)
make ci-local        # the fast PR gates: ci-go + ci-web (no browser/sandbox e2e)
```

`make ci-go` requires the two Postgres test databases and the env vars below.
`make ci-e2e-mocked` requires the Playwright Chromium browser (see that section).
The live and canary lanes are not wrapped in a `make` target because they boot a
real stack (Postgres + both Go listeners + a ~1.3 GB rootless-Podman sandbox
image); run them via the `npm` scripts documented below.

## Prerequisites

- **Go** — 1.21 or newer. You do not need the exact patch release pinned in
  `go.mod` (CI uses `1.26.6`): the Makefile exports `GOTOOLCHAIN=auto`, so the
  pinned toolchain is downloaded on demand. 1.21 is the floor only because
  that is when `GOTOOLCHAIN` — and therefore the ability to fetch — landed.
- **golangci-lint `v2.12.2`** — CI pins this exact version (it matches
  `run.go` in [`.golangci.yml`](../.golangci.yml)); a different version may flag
  or miss findings.
- **Node.js 22** and npm for the `web/` lanes.
- **PostgreSQL 18** for the Go suites that touch the chat/scheduler stores. CI
  uses the `postgres:18` service container.
- **Podman (rootless) + pasta** for the live e2e and sandbox-invariant tests.
  Most unit tests self-skip when podman is absent; the `go` job actively masks
  podman so the container-sandbox tests skip in that fast lane.
- **gitleaks** for the secret-scan lane (CI pins `8.30.1`).

---

## Secret scan (gitleaks) — CI job `gitleaks`

Scans the working tree for committed secrets. Fails on any new, un-ignored
finding. It reads [`.gitleaks.toml`](../.gitleaks.toml) (the full default ruleset
plus a path allowlist) and [`.gitleaksignore`](../.gitleaksignore) (fingerprints
of confirmed **fake** test fixtures) from the repo root automatically.

Reproduce locally (run from the repo root):

```sh
gitleaks dir . --redact --exit-code 1
```

`--redact` keeps any match out of the log; `--exit-code 1` makes a finding fail
the command.

---

## Go build / vet / lint / test — CI job `go`

This is the largest lane. It runs against a `postgres:18` service container with
two test databases. The full job, in CI's order, is wrapped by **`make ci-go`**.

### The two Postgres DSNs (required)

The chat suites (`internal/store`, `internal/httpapi`) and the scheduler suite
(`internal/sched/*`, `internal/runner`) both manage a table named
`schema_migrations` with **incompatible** schemas, so they must point at
**separate** databases. Both suites auto-migrate from an empty database. CI
creates both databases and sets these env vars:

```sh
# Chat suites read FLEET_TEST_DATABASE_URL, falling back to CHAT_TEST_DATABASE_URL.
export FLEET_TEST_DATABASE_URL="postgres://fleet:fleet@localhost:5432/fleet_chat_test?sslmode=disable"
export CHAT_TEST_DATABASE_URL="$FLEET_TEST_DATABASE_URL"
# The sched suite reads DATABASE_URL — a DIFFERENT database.
export DATABASE_URL="postgres://fleet:fleet@localhost:5432/fleet_sched_test?sslmode=disable"
# Point the binaries at the in-repo default bundle.
export FLEET_CLIENT_CONFIG_DIR="$(pwd)/config/default"
```

> **Parallel checkouts / agent worktrees:** the suites MIGRATE and TRUNCATE
> their databases, so two workstreams sharing the default pair corrupt each
> other — a checkout on an older migration set refuses to run against a DB a
> newer branch migrated ("database is at schema version N+1 … refusing to
> downgrade"), and concurrent truncates race. Give each workstream its own
> pair with [`scripts/test-db-setup.sh`](../scripts/test-db-setup.sh):
>
> ```sh
> eval "$(scripts/test-db-setup.sh | tail -3)"   # suffix defaults to the branch name
> make test
> scripts/test-db-setup.sh --drop "$(git rev-parse --abbrev-ref HEAD)"   # when done
> ```

Adjust user/password/host to your local Postgres. To create the two databases
(mirroring CI's "Create test databases" step) against a server where role
`fleet` exists:

```sh
PGPASSWORD=fleet psql -h localhost -U fleet -d fleet -v ON_ERROR_STOP=1 \
  -c 'CREATE DATABASE fleet_chat_test;' \
  -c 'CREATE DATABASE fleet_sched_test;'
```

### The steps (and their local equivalents)

1. **go build** (release config — the host executor must NOT be compiled in).
   No `fleet_host_executor` tag, so the unsandboxed host executor is fenced out
   and the `host_disabled.go` stub stands in (issue #159).

   ```sh
   go build ./...        # or: make compile
   ```

   `web/go.mod` is an intentional empty module boundary. It prevents root
   `./...` patterns from discovering Go source that an npm dependency happens
   to ship under `web/node_modules`, so Go gates cover the same Fleet-owned
   packages before and after `npm ci`.

2. **go vet** — tagged so `host.go` (gated behind `fleet_host_executor`) is
   vetted too:

   ```sh
   go vet -tags fleet_host_executor ./...
   ```

3. **golangci-lint** — the **full gate**: it fails on **any** finding (the
   backlog is at zero; `only-new-issues` is intentionally off). Fix the finding
   or add a `//nolint` with a reason (`nolintlint` requires the reason).

   ```sh
   golangci-lint run     # or: make lint
   ```

4. **go test** — tagged, so the host-mode fixtures and MockMode tests compile.
   `-p 1` is required (the suite expects serial package execution); CI also adds
   `-count=1` to defeat the test cache. CI instruments this step with
   `-coverprofile=coverage.out -covermode=atomic` (issue #249); the race step
   below is intentionally NOT instrumented — the non-race profile is enough for
   trend tracking, and `-coverprofile` under `-race` would double its run.

   ```sh
   go test -p 1 -tags fleet_host_executor ./... -count=1   # `make test` runs this without -count=1
   # with coverage (matches CI's 'go test' step):
   make test-cover   # writes coverage.out, prints the project total
   ```

5. **go test -race** — the race detector is the gate for fleet's in-process
   coordination (worker pool, SSE fan-out, single-owner DB leases, the admission
   semaphore). Same DSNs and tag:

   ```sh
   go test -race -p 1 -tags fleet_host_executor ./... -count=1   # or: make test-race
   ```

6. **govulncheck** — call-graph-aware scan of the dependency tree for known CVEs
   reachable from fleet's code:

   ```sh
   go run golang.org/x/vuln/cmd/govulncheck@latest ./...   # or: make govulncheck
   ```

   **This gate is time-varying on purpose, and that is worth understanding
   before you treat a red run as a flake.** Both halves float, unlike the pinned
   gitleaks and Grype releases:

   - **The database is live.** govulncheck fetches advisories from
     <https://vuln.go.dev> on every run (`govulncheck -version` prints the DB and
     its last-updated timestamp; `-db` overrides the URL). Pinning it would blind
     the gate to everything disclosed after the pin, which is the whole point of
     the check — so it stays live, and the same commit can pass today and fail
     tomorrow. That is the gate working.

     The worked example: `GO-2026-6166`, `GO-2026-6168`–`GO-2026-6173` were
     published against `github.com/lib/pq`, a dependency fleet already had. CI on
     `main` went red with nothing in the diff to blame. All seven were
     `Fixed in: N/A` (lib/pq is unmaintained), so no bump could clear them and
     the correct fix was to remove the dependency — not to quiet the scanner.

   - **The scanner stays `@latest`**, which is upstream Go's own documented
     invocation and effectively what `golang/govulncheck-action` does (it exposes
     no govulncheck version input). A pin here could only be a hand-maintained
     string — Dependabot does not rewrite versions inside a `run:` block, and the
     go.mod `tool` directive is worse, because tool dependencies share the main
     module's build list and the scanner would start dictating the product's
     `x/net` version. A pin nobody bumps quietly costs detection as the
     reachability analysis improves, which is the wrong trade for a security
     gate. If a bad govulncheck release ever blocks every PR at once, pinning
     this one line is the rollback.

   ### Daily scheduled scan — `.github/workflows/govulncheck-scheduled.yml`

   Because the database floats, the only thing that would otherwise tell this
   project a new advisory applies to it is an unrelated PR turning red. A
   **non-blocking** daily scan (08:00 UTC; also `workflow_dispatch`) runs the
   same command against the tip of `main` and reports to the Security tab
   (category `govulncheck-scheduled`), so an advisory is found on a schedule
   instead of ambushing the next contributor. It makes nothing greener — the
   gate still blocks a reachable vulnerability.

   It also sees strictly more than the gate does: `-format sarif` reports
   vulnerabilities in modules the build merely *requires* but never calls, at
   SARIF level `note`, which the blocking text-mode step ignores entirely. Those
   notes are the leading indicator — an unmaintained dependency accumulating
   advisories is worth knowing about before a refactor makes one reachable.

> **Coverage (CI-only steps).** After the `go test` step CI runs two coverage
> steps (issue #249): `Coverage summary` (prints the project total + writes
> `coverage.html`) and `Per-package coverage summary` (writes the
> `go tool cover -func` table to `$GITHUB_STEP_SUMMARY`). Both are advisory:
> **no coverage threshold gates a merge**, and there is no external coverage
> service — the `Upload coverage to Codecov` step and `codecov.yml` were removed
> because the repo has no `CODECOV_TOKEN`, so the upload only ever emitted a
> missing-token warning. Locally `make test-cover` produces the same
> `coverage.out`; the two summary steps are CI-side presentation on top of it.

> **Podman in this lane.** CI masks `podman` so the container-sandbox
> integration tests in `internal/sandbox` cleanly self-skip (building the
> ~1.3 GB sandbox image per unit-test run would dominate this fast lane; the real
> sandbox is exercised in `e2e-live`). The host-backend sandbox tests (bash /
> python3 via `os/exec`) still run. Locally, the container tests self-skip when
> podman is absent or when you run as root (an euid guard). To run the container
> invariants for real, see the live lane below.

Run the whole job locally:

```sh
make ci-go
```

---

## Web lint / test / build — CI job `web`

Runs from `web/`. The job is: `npm ci` → `npm run lint` (ESLint) →
`npx vitest run` (unit tests) → `npm run build` (`next build`).

```sh
cd web
npm ci
npm run lint
npx vitest run
npm run build
```

Or, from the repo root:

```sh
make ci-web
```

---

## Web e2e (Playwright, mocked) — CI job `playwright`

The mocked suite drives the **real** Next.js app but route-intercepts every Go
backend, so it needs no chat-server, orchestrator, OpenRouter, or Podman — it is
fully deterministic on a bare runner. It runs the `mocked` Playwright project
(`web/e2e/mocked`) against a production build (`next build` then `next start` in
`CHAT_MOCK_MODE=1`, so it validates the shipped bundle).

```sh
cd web
npm ci
npx playwright install --with-deps chromium   # CI does this in its own step
npm run build
npm run test:e2e:mocked                        # = playwright test --project=mocked
```

Or, once browsers and deps are installed, from the repo root:

```sh
make ci-e2e-mocked
```

---

## Container image vulnerability scan (Grype) — CI job `grype-scan`

`govulncheck` only sees Go modules; it cannot see the ~400 RPMs (and the Python
packages they ship) the sandbox image installs via `microdnf` from
`config/default/sandbox/Containerfile` (Python 3, the scientific Python stack,
ImageMagick, pandoc, git, …). That large image attack surface is what this scan
covers. The job runs after `e2e-live` (which already builds the image), rebuilds
the same default-bundle sandbox image, `podman save`s it to a docker-archive
tarball, and scans the tarball — so the scan is self-contained and needs no
running container daemon or socket.

**Why Grype and not Trivy.** The sandbox base is
`registry.fedoraproject.org/fedora-minimal:latest`, and **Trivy has no Fedora
advisory feed**: it detects the OS but then logs `WARN Unsupported os
family="fedora"` and scans **0** of the image's packages — a gate that always
passes regardless of what CVEs ship (a false green). Grype matches the image's
RPM **and** Python packages against NVD/GHSA, so it actually covers this image.
Grype is pinned to a release version and verified by `sha256sum` (the same
supply-chain discipline as the gitleaks gate) rather than run as a third-party
action on a mutable ref — the scanner is part of the merge gate, so its own
supply chain matters as much as what it scans.

The per-PR scan collects and uploads **all** findings, including unfixed and
non-blocking language-package records. A separate repository-owned policy
(`scripts/check-grype-policy.sh`) fails only on a **CRITICAL Fedora RPM** with a
non-empty fix version. This distinction is intentional: Fedora RPMs sometimes
also expose Python `dist-info`, which Grype catalogs as a second PyPI artifact;
an upstream PyPI fix does not mean Fedora has published an installable RPM. The
generic image follows Fedora latest, so an actionable failure should be fixed by
rebuilding/updating the RPM rather than by layering a pip wheel over files owned
by the distro. The weekly scan uses the same complete reporting model (see
below). Narrow, reviewed suppressions live in [`.grype.yaml`](../.grype.yaml)
(one `ignore:` entry per CVE, with a rationale comment); Grype auto-reads it from
the repository root.

Findings are uploaded as SARIF to **GitHub Security → Code scanning** (category
`grype-sandbox-image`) so CVE details, affected packages, and fix versions are
visible without parsing log output. (Secret scanning stays with `gitleaks`, the
authoritative gate; misconfig/IaC scanning is out of scope for this lane. Grype
downloads its vulnerability DB from Anchore's CDN at scan time, so a transient CDN
outage can redden the gate independent of any code change.)

It needs `security-events: write` (scoped to the job, not the workflow) to upload
SARIF. Reproduce locally (needs podman + the Grype binary):

```sh
# Build the same image the job scans, and export it to a docker-archive tarball.
IMAGE_NAME=localhost/fleet-sandbox scripts/build-sandbox-image.sh latest
podman save --format docker-archive -o sandbox-image.tar localhost/fleet-sandbox:latest
# Install grype first (see .github/workflows/ci.yml for the pinned version+sha),
# then scan exactly as the gate does:
grype docker-archive:sandbox-image.tar --only-fixed --fail-on critical \
  --output table --output sarif=grype-results.sarif
```

There is no `make` target for this lane because it boots a podman image build;
run it in CI or with the commands above.

### Weekly scheduled scan — `.github/workflows/grype-scheduled.yml`

A **non-blocking** weekly scan (Mondays 09:00 UTC; also `workflow_dispatch`)
rebuilds the image against the latest unpinned `fedora-minimal:latest` base and
reports **all** findings — including unfixed ones, with no `--fail-on` /
`--only-fixed` — to the Security tab (category `grype-scheduled`), so a
newly-disclosed CVE against the existing image surfaces as an informational alert
rather than blocking `main`. It is never a PR gate.

---

## Web e2e (Playwright, live) — CI job `e2e-live`

The full real stack: Postgres, both Go listeners, SSE, the scheduler + worker
pool, and the **real rootless-Podman sandbox**. Only the LLM is stubbed — by the
wire-compatible fake (`cmd/fake-llm`, reached via `OPENROUTER_BASE_URL`) — so
there is no OpenRouter spend and the suite stays deterministic.

This lane also asserts the project's #1 invariant: it runs the
sandbox-isolation tests in `internal/sandbox` for real (non-root, rootless
podman, against the locally-built default-bundle image) and treats a **SKIP as a
failure** (issue #34) — the only always-on lane that does so.

`playwright.config.ts` (with `E2E_LIVE=1`) launches
[`scripts/e2e-boot-server.sh`](../scripts/e2e-boot-server.sh) as its
`webServer`; the script boots everything health-gated (no sleeps), then the
`live` project runs against it. Locally:

```sh
cd web
npm ci
npx playwright install --with-deps chromium
npm run test:e2e:live          # = E2E_LIVE=1 playwright test --project=live
```

See [`web/e2e/live/README.md`](../web/e2e/live/README.md) for the boot script's
prerequisites (podman + pasta, a Postgres reachable via `E2E_DATABASE_DSN`, and
the sandbox image it builds from `config/default/sandbox/Containerfile`).

---

## Web e2e (Playwright, canary, real model) — nightly, `e2e-canary.yml`

A **non-blocking** drift canary, never a PR gate. It boots the same live stack
but swaps the fake LLM for a real, cheap OpenRouter model to catch
upstream/provider drift the deterministic `e2e-live` lane cannot see. It runs on
a nightly schedule and on manual dispatch, and is secret-gated: without the
`OPENROUTER_API_KEY` repo secret it skips cleanly (green), never failing for a
missing secret.

To run it locally you supply a real key (this **does** spend on OpenRouter):

```sh
cd web
export OPENROUTER_API_KEY="…"        # your real key — kept outside the repo
npm run test:e2e:canary              # = E2E_CANARY=1 playwright test --project=canary
```

---

## What to run before opening a PR

- **Always:** `gitleaks dir . --redact --exit-code 1`.
- **If you touched Go:** `make ci-go` (build, vet, lint, test, -race,
  govulncheck) — with the two Postgres DSNs and `FLEET_CLIENT_CONFIG_DIR` set as
  above.
- **If you touched `web/`:** `make ci-web`, plus `make ci-e2e-mocked` if you
  changed UI behavior the mocked suite covers.
- **Fast everything-but-e2e:** `make ci-local`.

CI runs the rest (the live and canary browser/sandbox lanes) for you, but they
are not local-only — `e2e-live` is an always-on PR gate.
