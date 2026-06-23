# Contributing to fleet

Thanks for your interest in fleet! This guide covers how to build, test, and
submit changes. Contributions of all sizes are welcome — bug fixes, docs,
tests, and features.

## Repository layout

fleet is a Go monorepo with a Next.js frontend:

```
cmd/        entrypoints (the single fleet binary, fleet-admin CLI, helpers)
internal/   the Go implementation (agent runtime, sandbox, MCP, scheduler, HTTP API, …)
web/        the Next.js app (the /chat and /orchestrator views)
config/     the generic client-config bundle baked into the repo (config/default)
docs/       architecture and operator documentation
deploy/     systemd unit + Caddyfile for a single-box deployment
scripts/    bootstrap / update / status and e2e helpers
```

See the top-level `README.md` for the architecture overview and
`docs/MIGRATION_PLAN_V2.md` for the deeper design.

## Prerequisites

- **Go** — the version pinned in `go.mod` (currently 1.26.x).
- **Node.js 22** and npm for the `web/` app.
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

# Mocked suite — deterministic, no backend; this is the CI gate:
npx playwright test --project=chromium

# Live suite — requires a real Go backend; see web/e2e/README.md.
```

The mocked suite route-intercepts every backend call, so it runs on a bare
runner with no database, podman, or API keys. The live suite is local-only.

## Continuous integration

Every pull request must be green before merge. CI runs:

- **Go** — `go build`, `go vet`, `golangci-lint` (full gate — fails on any
  finding), and `go test`.
- **Web** — `npm run lint`, vitest, and `npm run build`.
- **Playwright** — the mocked suite, plus a live suite against a real backend
  with a stubbed LLM (no OpenRouter spend).
- **Secret scan (gitleaks)** — fails the build on any new, un-ignored secret.

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
