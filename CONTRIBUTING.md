# Contributing to fleet

Thanks for your interest in fleet! This guide covers how to build, test, and
submit changes. Contributions of all sizes are welcome — bug fixes, docs,
tests, and features.

New here? [`ONBOARDING.md`](ONBOARDING.md) is a single linear path from clone to
your first sandbox session (a passing `make test` and one streamed chat turn).

## Repository layout

fleet is a Go monorepo with a Next.js frontend:

```
cmd/        entrypoints (the one unified fleet binary — server + operator CLI; fleet-admin is a transitional deprecation shim; helpers)
internal/   the Go implementation (agent runtime, sandbox, MCP, scheduler, HTTP API, …)
web/        the Next.js app (the /chat and /orchestrator views)
config/     the generic client-config bundle baked into the repo (config/default)
docs/       architecture and operator documentation
deploy/     systemd unit + Caddyfile for a single-box deployment
scripts/    bootstrap / update / status and e2e helpers
```

See the top-level `README.md` for the architecture overview. The server runs via
`fleet serve` (bare `fleet` also serves, for back-compat); all other verbs are the
operator CLI. (`fleet-admin <verb>` still works but is deprecated and will be
removed.)

## Prerequisites

- **Go** — the version pinned in `go.mod` (currently 1.26.x).
- **Node.js** — the major in [`web/.nvmrc`](web/.nvmrc) (currently 24) — and npm, for the `web/` app.
- **Podman** (rootless) for the execution sandbox — only needed to run the
  sandbox-backed tests/e2e locally; most unit tests self-skip when podman is
  absent.
- **PostgreSQL** for the Go suites that touch the chat/scheduler stores.

## Building and testing

### Go

```bash
go build ./...     # or: make build
go vet ./...
golangci-lint run  # or: make lint  (golangci-lint v2.x is the lint gate)
go test ./...      # or: make test
```

The store / HTTP / scheduler suites need Postgres. Point them at throwaway
databases via environment variables (these mirror CI):

```bash
export FLEET_TEST_DATABASE_URL="postgres://<user>:<pass>@localhost:5432/fleet_chat_test?sslmode=disable"
export CHAT_TEST_DATABASE_URL="$FLEET_TEST_DATABASE_URL"
export DATABASE_URL="postgres://<user>:<pass>@localhost:5432/fleet_sched_test?sslmode=disable"
export FLEET_CLIENT_CONFIG_DIR="$(pwd)/config/default"
go test -p 1 ./... -count=1
```

The chat and scheduler migration systems both use a `schema_migrations` table
with incompatible schemas, so they must point at **separate** databases. Both
suites auto-migrate from an empty database.

### Web

```bash
cd web
npm ci
npm run lint
npm run test     # vitest unit tests
npm run build
```

### Playwright (browser e2e)

```bash
cd web
npx playwright install --with-deps chromium

# Mocked suite — deterministic, no backend; the fast CI lane:
npx playwright test --project=mocked   # or: npm run test:e2e:mocked

# Live suite — boots the real stack (Postgres + Go + Podman), fakes only the
# LLM; see web/e2e/live/README.md:
npm run test:e2e:live
```

The mocked suite route-intercepts every backend call, so it runs on a bare
runner with no database, podman, or API keys. The live suite boots the whole
real stack and runs in CI too (the `e2e-live` job) — it is not local-only.

## Continuous integration

[`docs/TESTING.md`](docs/TESTING.md) documents every CI lane, the exact command
each runs, and the `make` targets that mirror them locally (`make ci-go`,
`make ci-web`, `make ci-e2e-mocked`, `make ci-local`).

Every pull request must be green before merge. CI runs:

- **Go** — `go build`, `go vet`, `golangci-lint` (full gate — fails on any
  finding), `go test`, and a `-race` lane.
- **Python** — `ruff check` **and** `ruff format --check` over the tree's Python
  files (`make lint-python` runs both, and skips loudly if ruff is not installed).
- **Web** — `npm run lint` (oxlint), `npm run typecheck` (`tsc --noEmit`), vitest,
  and `npm run build`.
- **Playwright** — the mocked suite, plus a live suite against a real backend
  with a stubbed LLM (no OpenRouter spend).
- **Secret scan (gitleaks)** — fails the build on any new, un-ignored secret.
- **SAST (CodeQL and Semgrep)** — both are reusable workflows called by `ci.yml`
  and `dev-ci.yml`, so they sit inside the aggregate gate. **Semgrep** fails on
  any unsuppressed finding across `p/github-actions`, `p/golang`, `p/javascript`
  and `p/python`; a false positive is waived at the line with
  `nosemgrep: <rule-id>` plus a reason. **CodeQL** (`security-extended` over go /
  python / javascript-typescript / actions) fails on an unwaived finding in the
  **High band** — `security-severity >= 7.0`, or level `error`/`warning` for a
  rule that publishes no security-severity — with lower-severity findings
  reported as advisory. A false positive is waived either by an in-source
  `// codeql[rule-id]` comment or by an entry in
  `.github/codeql-accepted-findings.json` **with a written reason**; both are
  reviewable in the diff, and fixing the code is always preferred. See
  [`docs/SCANNING.md`](docs/SCANNING.md), [`docs/CODEQL.md`](docs/CODEQL.md) and
  [ADR-0048](docs/adr/0048-codeql-severity-gating.md).
- **Dependency CVEs** — `govulncheck` for the Go module, and
  `npm audit --audit-level=low` (lockfile-only, **any** severity) for both
  `web/` and `scripts/rampart-service`, alongside
  `scripts/check-npm-overrides.sh`, which fails once upstream ships fixes that
  make the pinned security `overrides` droppable.
- **Container image scan (Grype)** — fails the build on a fixable **CRITICAL or
  HIGH** CVE in an **RPM** of the sandbox image built from
  `config/default/sandbox/Containerfile`. MEDIUM and below are reported, not
  blocking, and the Python `dist-info` records Grype catalogs alongside the RPMs
  never block whatever their severity (they are uploaded to SARIF — the rationale
  is in [`docs/TESTING.md`](docs/TESTING.md)). Findings upload to GitHub Security
  → Code scanning. A separate weekly scheduled scan (non-blocking) catches new
  CVEs against the existing image between PRs. (Grype, not Trivy: the image's
  `fedora-minimal` base has no Trivy advisory feed, so Trivy would scan none of
  its packages; Grype matches its RPM + Python packages against NVD/GHSA.)

One qualifier on "must be green", because it differs by branch: `CI gate` is a
**required** status check on `main`, so a red lane genuinely blocks the merge
there. The `dev` ruleset requires no status checks, so on `dev` a red `Dev gate`
is a signal rather than a block — please treat it as one anyway. See
[`docs/SCANNING.md`](docs/SCANNING.md) ("Known gaps").

If golangci-lint flags something, either fix it or add a `//nolint` with a
reason (the `nolintlint` linter requires the reason). The lint backlog is at
zero — please keep it there.

## Branch and pull-request conventions

- Branch off the latest `main`. Use a short, descriptive prefix, e.g.
  `feat/…`, `fix/…`, `chore/…`, `docs/…`, `test/…`.
- Keep pull requests focused. A PR that does one thing well is easier to review
  and revert than a grab-bag.
- Write a clear PR description: what changed, why, and how you verified it.
- Make sure the full local suite (Go + web + mocked Playwright) is green before
  you push.

## Commit messages and sign-off

- Write clear, imperative commit subjects ("Add X", not "Added X").
- Sign off your commits with the Developer Certificate of Origin
  (<https://developercertificate.org/>) by adding a `Signed-off-by` trailer:

  ```bash
  git commit -s -m "Your message"
  ```

  By signing off you certify that you wrote the patch (or otherwise have the
  right to submit it) under the project's MIT license.

## Reporting bugs and proposing features

Open a GitHub issue with a clear title and enough context to reproduce or
understand the request. For security issues, do **not** open a public issue —
see `SECURITY.md`.

## License

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE) that covers this project.
