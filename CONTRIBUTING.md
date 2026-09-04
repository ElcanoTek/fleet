# Contributing to fleet

Thanks for your interest in fleet! This guide covers how to build, test, and
submit changes. Contributions of all sizes are welcome — bug fixes, docs,
tests, and features.

New here? [`ONBOARDING.md`](ONBOARDING.md) is a single linear path from clone to
your first sandbox session (a passing `make test` and one streamed chat turn).

## Repository layout

fleet is a Go monorepo with a Next.js frontend:

```
cmd/        entrypoints (the one fleet binary — server + operator CLI; helpers)
internal/   the Go implementation (agent runtime, sandbox, MCP, scheduler, HTTP API, …)
web/        the Next.js app (the /chat and /orchestrator views)
config/     the generic client-config bundle baked into the repo (config/default)
docs/       architecture and operator documentation
deploy/     systemd unit + Caddyfile for a single-box deployment
scripts/    bootstrap / update / status and e2e helpers
```

See the top-level `README.md` for the architecture overview. The server runs via
`fleet serve` (bare `fleet` also serves, for back-compat); all other verbs are the
operator CLI. (The `fleet-admin` shim was removed — see
`docs/adr/0060-remove-the-fleet-admin-shim.md`.)

## Prerequisites

- **Go** — the version pinned in `go.mod` (currently 1.27.x).
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
  reported as advisory. A false positive is waived by an entry in
  `.github/codeql-accepted-findings.json` **with a written reason** — that is the
  only waiver route that works here; an in-source `// codeql[rule-id]` comment
  does not (measured on #1249). The register entry is reviewable in the diff, and
  fixing the code is always preferred. See
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

### If CI is red and you don't recognise the failure

Three lanes here depend on **live external data**, so they can go red on a diff
that did not cause it — including a one-line documentation change. This is by
design (a new advisory *should* redden an unchanged tree), but it means a red
check is not automatically yours:

- **`govulncheck`** queries the Go vulnerability database on every run.
- **`npm audit`** runs over both npm trees at `--audit-level=low` and fails on
  any severity.
- **Semgrep** fetches its rule packs from the registry. They cannot be pinned by
  vendoring — the Semgrep Rules License forbids redistribution — so a
  registry-side rule addition can turn CI red with no commit to blame.

If the failure names a package, advisory or rule you did not touch, say so in the
PR rather than trying to fix it; a maintainer will confirm and handle it.

Two other things that surprise first-time contributors, neither of them a problem
with your change: a first PR waits for a maintainer to approve the workflow run
before CI starts at all, and the full `main` suite is around a dozen jobs
including a ~1.3&nbsp;GB sandbox image build, so it is thorough rather than fast.
`make lint && make test && make ci-web` locally will catch nearly everything
first.

## Branch and pull-request conventions

- Branch off the latest `main`. Use a short, descriptive prefix, e.g.
  `feat/…`, `fix/…`, `chore/…`, `docs/…`, `test/…`.
- Keep pull requests focused. A PR that does one thing well is easier to review
  and revert than a grab-bag.
- Write a clear PR description: what changed, why, and how you verified it.
- Make sure the full local suite (Go + web + mocked Playwright) is green before
  you push.

### Promotions (dev → main)

Feature work merges into `dev` (the fast lane); `dev` is promoted to `main`
via a **squash**-merge PR — the squash titles are the promotion log. Squashing
has one structural side effect: the branches' merge-base never advances, so
any region dev changes, promotes, and later changes again would read as
both-sides-modified (a spurious conflict) on the next promotion PR.

The fix is an **ancestry merge** recorded on `dev` right after each promotion:
`git merge -s ours main` makes main an ancestor of dev while changing nothing
in dev's tree. The `Promotion ancestry` workflow
([`.github/workflows/promotion-ancestry.yml`](.github/workflows/promotion-ancestry.yml))
does this automatically on every push to `main`, gated on **tree identity** —
`-s ours` is only provably safe when `main^{tree}` is byte-identical to
`dev^{tree}`, i.e. main carries nothing dev lacks. If the trees differ (dev
moved mid-promotion), the workflow fails loudly instead of guessing.

If it fails and you have looked, the manual step it automates is:

```bash
git fetch origin main dev
# Precondition — both must print the same tree hash:
git rev-parse origin/main^{tree} origin/dev^{tree}
git checkout -B dev origin/dev
git merge -s ours --no-ff origin/main -m "Merge main back into dev after promotion (ancestry only; tree unchanged)"
git push origin dev
```

If the trees differ, do **not** use `-s ours` — resolve the divergence for
real first (dev commits that landed mid-promotion are content main genuinely
lacks; `-s ours` from main's side would be wrong, and from dev's side it is
only safe once you have confirmed main brings nothing new).

### Releases (there is nothing to do)

You never cut a release, tag one, or bump a version. Every promotion that goes
green on `main` is tagged `vYYYY.MM.DD.N` by the `Release` workflow
([`.github/workflows/release.yml`](.github/workflows/release.yml)) — UTC date,
plus an ordinal because several promotions in a day is normal — and published as
a GitHub release whose notes are generated from the commits since the previous
tag. There is no `VERSION` file to touch and no semver decision to make; a red
`main` simply gets no tag, and the next green push takes the next ordinal.

Two consequences for a PR:

- **Do not add a hand-authored version number** anywhere — not in a doc, a
  chart, a `package.json`, or a Go string. Builds derive their identity from the
  tags (`scripts/version.sh`); `scripts/check_release_version_test.go` fails if
  one comes back.
- **Date your deprecation windows**, never number them: "removed in the first
  release on or after `YYYY-MM-DD`". A window keyed to a release number can
  never come due here.

`CHANGELOG.md` is still yours to update for a user-visible change — it carries
the *why* that generated notes cannot. See
[`docs/VERSIONING.md`](docs/VERSIONING.md).

## Commit messages

- Write clear, imperative commit subjects ("Add X", not "Added X").
- Explain *why* in the body when the change is not self-evident. The diff
  already says what changed.

## Reporting bugs and proposing features

Open a GitHub issue with a clear title and enough context to reproduce or
understand the request. For security issues, do **not** open a public issue —
see `SECURITY.md`.

## License

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE) that covers this project.
