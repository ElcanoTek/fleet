# AGENTS.md

Operating guide for AI coding agents (Claude Code, Codex, Cursor, opencode, Goose,
Gemini CLI, …) working in the **fleet** repository. It follows the
[agents.md](https://agents.md) convention; `CLAUDE.md` is a symlink to this file so
Claude Code and any `AGENTS.md`-aware tool read the same instructions.

Humans, start with [`README.md`](README.md) and [`CONTRIBUTING.md`](CONTRIBUTING.md);
this file is the agent-facing distillation, not a replacement for them.

## What fleet is (one paragraph)

fleet is a self-hosted, general-purpose agent platform. **One** Go process runs
interactive chat *and* a scheduling engine, driven by **one** unified agent
runtime (`internal/agentcore`). Model-authored local execution — bash, Python,
and file I/O — runs inside a mandatory sandbox; fixed host-side brokers handle
MCP credentials/network and the small control-plane exception set enumerated in
ADR-0036. The sandbox **backend** is pluggable (ADR-0049):
`FLEET_SANDBOX_BACKEND=podman` — the default, rootless Podman co-located with
the process, which is the single-box install — or `kubernetes`, one ephemeral
pod per sandbox exec'd over the apiserver, for the split control-plane
deployment. "On one box" is the default shape, not the only one, so do not write
code (or docs) that assumes podman is the only executor. See the README
"Architecture at a glance" for the full picture.

## Build · test · lint (run before opening any PR)

```sh
make build        # compile-check ./... AND emit ./fleet
make compile      # go build ./...   (release config — see the build tag below)
make test         # go test -p 1 -tags fleet_host_executor ./...   — run in the FOREGROUND
make test-race    # the same, with -race   (use when touching concurrency)
make test-cover   # the same, with -coverprofile/-covermode=atomic (writes coverage.out)
make lint         # golangci-lint + ruff check/format (Python) + migration DDL lint
                  #   + actionlint & shellcheck (workflows + shell) — must pass clean
make govulncheck  # call-graph-aware CVE scan of the dependency tree
make fmt          # gofmt -w .
make tidy         # go mod tidy
make ci-go        # the whole Go gate locally: compile, vet, lint, test, -race, govulncheck
make ci-web       # the Web CI job verbatim (see below)
make ci-local     # ci-go + ci-web — the fast PR gates, locally
```

**`-tags fleet_host_executor` is load-bearing, not decoration.** The unsandboxed
host executor is fenced behind that build tag (#159) so it is *not* compiled into
a release binary: `make compile` deliberately omits it, and `host_disabled.go`
then stubs `newHostSandbox` out and rejects MockMode at boot. Tests opt in — every
`go test`/`go vet` target above carries the tag, and `.golangci.yml` sets
`build-tags` so the linter agrees — so a bare `go test ./...` builds a *different*
tree than CI does (`host.go` unvetted, untested). Use the Makefile targets.

`make lint`'s `lint-python` (ruff) and `lint-actions` (actionlint/shellcheck)
**skip loudly when the tool is missing**, printing the install command. So a green
local `make lint` is not proof — read the output, and remember CI enforces both
regardless.

When you touch `web/` (the Next.js app):

```sh
make ci-web                                        # the Web CI job, verbatim — prefer this
cd web && npm ci && npm run lint && npm run typecheck && npm run test && npm run build
cd web && npx playwright test --project=mocked     # mocked e2e
```

There are **two** npm trees — `web/` and `scripts/rampart-service/` — and CI
audits both (`npm audit --audit-level=low`, lockfile-only) plus
`scripts/check-npm-overrides.sh`, the override canary. `make ci-web` runs all
eight steps; the hand-rolled line above skips the audits and the canary, which is
how a clean local run turns into a red PR.

CI mirrors all of this across **two lanes**, and which one you get depends on the
branch you target:

- **`CI` (`ci.yml`) — `main` only** (pushes to `main` and PRs targeting it). The
  full gate: Go build/vet/lint/test (including a `-race` lane) plus a
  `govulncheck` dependency-CVE scan, a Grype container-image CVE scan (fail on a
  fixable CRITICAL/HIGH) of the sandbox image, a Python lint (ruff), a workflow +
  shell lint (actionlint & shellcheck), web lint (oxlint) / typecheck (TS 7) /
  test / build, Playwright (mocked **and** live, against a real backend +
  sandbox), a Helm chart lint, a migration DDL lint, and a gitleaks secret scan.
  `CI gate` is the **single required status check** on `main`: it `needs` every
  other job and always reports, so a docs-only PR (heavy jobs skipped by the
  `changes` classifier) still merges, while a code PR cannot go green over a skip.
- **`Dev CI (fast lane)` (`dev-ci.yml`) — `dev` only** (pushes to `dev` and PRs
  targeting it). Compile/vet/lint/test against a Postgres service, ruff, the web
  lane, the migration DDL lint, gitleaks, actionlint/shellcheck, the Helm lint,
  CodeQL and Semgrep. Deliberately deferred to the promotion PR: the `-race`
  lane, govulncheck, the Grype image scan, and both Playwright suites. There is
  no docs-only classifier here — the fast lane runs on every change.

`ci.yml` does not fire on `dev` at all, so **the dev→main promotion PR is the
first time the full gate ever sees that code**; expect it to surface things dev
never told you about. **Every job must be green before merge**, and nothing
merges itself — auto-merge was removed, so every PR, dependency bumps included,
waits for a human. Tests are deterministic without a live model: use the fake-LLM
seam (`internal/fakellm` via `OPENROUTER_BASE_URL`), never a real key.

CodeQL (security queries, `security-extended`) and Semgrep (Go/JS/Python SAST +
Actions supply chain) run per PR in **both** lanes: `codeql.yml` and `semgrep.yml`
are reusable workflows that ci.yml/dev-ci.yml call as jobs, so their results roll
up into `CI gate` / `Dev gate` like any other job. `npm audit` (both npm trees,
lockfile-only, any severity) and ruff (`check` **and** `format --check`) gate the
same way.

Their thresholds differ, and the difference is load-bearing:

- **Semgrep, ruff and `npm audit`: zero findings, gating on any finding.**
- **CodeQL: zero *blocking* findings.** It gates on the **High band**
  (`security-severity >= 7.0`, or level `error`/`warning` for a rule publishing
  no security-severity), minus a reviewed register of accepted `(rule, file)`
  pairs in `.github/codeql-accepted-findings.json` — each with a written reason.
  Below the band is **advisory**: printed in the job log and uploaded to the
  Security tab, not blocking. So "CodeQL is green" means "no unwaived High-band
  finding", not "no findings". The reasoning is [ADR-0048](docs/adr/0048-codeql-severity-gating.md).

Two facts an agent must not get wrong here. **A `pull_request` CodeQL run is
diff-informed** — it evaluates every query over the full database, then reports
only results inside the PR's diff — so it certifies a *diff*, never a tree; only
push and scheduled runs give a tree-wide verdict. And **`Dev gate` is not a
required check on `dev`**, so on that branch a scanner failure is a red X beside a
mergeable PR. See [`docs/SCANNING.md`](docs/SCANNING.md) ("Known gaps").

## Repository map

See the README "Repository layout" for the annotated tree. In short: `cmd/` (the
one `fleet` binary — `fleet serve` runs the server, every other verb is the
operator CLI (the `fleet-admin` shim was removed in ADR-0060); plus the
`fleet-bench`, `fake-llm` and `sandbox-probe` harness binaries), `internal/` (`agentcore` the one run loop, `sandbox`,
`mcp`, `creds`, `clientconfig`, `store`, `sched`, `httpapi`, …), `web/` (one
Next.js app: `/chat`, `/orchestrator`, `/settings`, `/admin`), `deploy/` (the
systemd units + Caddyfile for the single-box install and the
`deploy/helm/fleet` chart for the Kubernetes one), and `config/default/` (the
generic client bundle baked in so fleet runs bare).

## Non-negotiable invariants — do NOT weaken these

These are the security and design guarantees the whole project rests on. A change
that breaks one is wrong even if tests pass. The *why* behind several of them is
recorded as Architecture Decision Records in [`docs/adr/`](docs/adr/) — a change
that adds, weakens, or reverses an invariant must add or supersede an ADR in the
same PR.

- **The sandbox is mandatory** — the *backend* is pluggable, the sandbox is not.
  The agent loop runs in the fleet process, but every agent tool call's data-plane
  execution — bash, Python, **and file I/O
  (`view_file`/`write_file`/`edit_file`, via the sandbox FileOp seam, #784)** —
  runs inside the sandbox (rootless Podman by default; an ephemeral Kubernetes pod
  under `FLEET_SANDBOX_BACKEND=kubernetes`, ADR-0049); there is **no** fast path
  that skips it and no host-execution fallback (they fail closed without a
  sandbox). The unsandboxed host executor is compiled in **only** behind the
  `fleet_host_executor` build tag (#159), which is what makes "it cannot ship
  enabled in a production build" a property of the artifact rather than a runtime
  flag — do not widen that fence, and do not add a path that reaches `host.go`
  from an untagged build. Selecting the kubernetes backend runs a fail-closed boot
  preflight (apiserver + credentials, the exact RBAC verbs, the workspace claim,
  the sealed-egress NetworkPolicy object) and refuses podman-only knobs rather
  than ignoring them: no degrade to podman, none to host execution. The
  loop holds no privileged local executor of its own: each tool call is handed
  to the sandbox under host policy. A small set of native tools are host-side
  **control-plane / broker** operations by design (host network fetch, brokered
  credentials, governed datastore writes) — enumerated and threat-modelled in
  [ADR-0036](docs/adr/0036-sandboxed-file-tools-and-host-io-exceptions.md), not
  a silent exception. MCP credentials are brokered **out-of-process** (issue
  #167) and **never** enter the sandbox — the broker injects them only when it
  runs a delegated MCP call host-side.
- **Credentials stay host-side.** MCP/connector credentials are brokered on the
  host and **never** enter the sandbox, the agent container, the model context, or
  logs. Never ship a secret into a container or print one.
- **The broker authorizes, it does not just transport.** The credential-owning
  child re-derives each server's tool allowlist and enabled-server set from its
  OWN bundle and treats the parent's gates as narrowing only, so a parent-side
  gating bug restricts a run instead of unbounding it
  ([ADR-0042](docs/adr/0042-child-side-mcp-scope-authorization.md)). Do not add a
  broker path that binds or dispatches without going through that gate.
- **Governance is one core.** `agentcore.Run` is the single governed loop (policy,
  cost/token ceilings, audit, notes). New entrypoints **adapt I/O around it** —
  they must not fork a second, weaker governance path.
- **No secrets in the repo.** gitleaks gates CI. Use the fake-LLM seam and obvious
  placeholders in tests; the real `OPENROUTER_API_KEY` lives outside the repo.
- **Honesty in docs.** Claim only shipped, tested capabilities. If you add a
  capability, document what it actually does — and what it does not. (Example: a
  skill's `allowed-tools` frontmatter is parsed and **surfaced** for review
  (skills library UI, `/skills` API) but **not** enforced as an authorization
  boundary — the docs say so plainly rather than implying a boundary that isn't
  there. It structurally can't be one: skills are read on-demand mid-turn with
  no single "active skill" to gate a roster against, and a skill can never
  exceed the turn's existing sandbox/MCP/approval limits. See docs/SKILLS.md.)
- **Client content is external.** Branding, the MCP catalog, personas, protocols,
  skills, and the sandbox Containerfile live in an out-of-repo client-config
  bundle (`FLEET_CLIENT_CONFIG_DIR`). fleet ships only the generic `config/default`
  bundle — do not add client-specific content to this repo.

## Conventions

- Single Go module `github.com/ElcanoTek/fleet`, Go 1.27. Keep it `go vet`- and
  `golangci-lint`-clean — lint failures block CI.
- **Coverage (advisory, not a merge gate)**: CI runs the plain `go test` step
  with `-coverprofile=coverage.out -covermode=atomic`, then prints the project
  total to the log and writes the per-package `go tool cover -func` table to the
  Actions job summary. There is **no** external coverage service and no
  coverage threshold anywhere in the pipeline — the Codecov upload and
  `codecov.yml` were removed because the repo has no `CODECOV_TOKEN`, so the
  upload only ever produced a missing-token warning. Treat coverage as a quality
  signal, not a gate: add tests that catch real behavior, not to chase a
  number. (The merge gates are build/vet/lint, ruff — `check` and
  `format --check` — actionlint + shellcheck, the test suites, the `-race` lane,
  govulncheck, Grype, `npm audit` + `scripts/check-npm-overrides.sh`, CodeQL,
  Semgrep, the Helm chart lint, both Playwright suites, the migration
  linter, and gitleaks — all rolled up into the one required `CI gate` check.)
- **Match the surrounding code:** naming, idioms, and comment density. The
  `internal/agentcore` package comments explain *why* each governance invariant
  holds — preserve that level of explanation when you extend it.
- Run tests in the **foreground**. Do not background `go test`, and do not
  `pkill -f 'go test'` (it can kill the shell). Prefer `make test` (it sets
  `-p 1`, which the suite expects).
- **New task fields thread one way.** A new per-task column is one migration
  plus one row in `taskColumnRegistry`
  (`internal/sched/db/task_columns.go`, #1126) plus the `models.Task` field —
  and, for a `read` column, its `taskScanBuf` field (the compiler enforces
  that one). The registry derives the SELECT/scan, insert, upsert and
  `UpdateTaskTx` statements from per-row flags; flag `export` (and add the
  matching `models.TaskExportRecord` field) when the column belongs to the
  portable definition. Result-like columns written after the terminal transition
  (e.g. `error_analysis`) stay excluded from the insert/upsert so a status
  write can never clobber them — every exclusion needs a reason string on
  its row, and the `sched/db` registry tests fail on schema↔registry drift.
- **Ship features with honest scope.** Every feature lands with a design note
  recording what shipped, what deviated from the issue, and what was deliberately
  deferred — in a dedicated `docs/<FEATURE>.md` (plus an ADR when an invariant is
  touched). The PR body's "What changed, and why" is the record of the change
  itself; there is no changelog file to update
  ([ADR-0061](docs/adr/0061-retire-the-changelog.md)). Do **not** append
  per-feature design notes to this file — that is how it grew past 300 lines
  once already; the historical notes live in
  [`docs/FEATURE-NOTES.md`](docs/FEATURE-NOTES.md).
- One focused branch + PR per change; keep diffs scoped. Don't refactor unrelated
  code in a feature PR. `.github/PULL_REQUEST_TEMPLATE.md` asks for exactly the
  three things above — what/why, what you actually ran to verify it, and
  scope-and-deviations — so fill it in rather than deleting it. See
  `CONTRIBUTING.md` for branch/PR conventions.

## Where to look

The docs index is [`docs/README.md`](docs/README.md): one bullet per design
note, runbook and ADR, by question. The entry points an agent needs most:

- **Driving a PR to green after it is opened** — the follow-up loop for any
  coding agent or human. Posture: *we can fix everything*; if you are driving
  the PR, every red check and every open thread on it is yours, whoever wrote
  the code. Procedure: [`.agents/skills/steward/SKILL.md`](.agents/skills/steward/SKILL.md)
  (`.claude/skills/steward` is a symlink to it). Reference:
  [`docs/PR-STEWARDSHIP.md`](docs/PR-STEWARDSHIP.md).
- **Why the invariants are the way they are:** [`docs/adr/`](docs/adr/).
- **Agent runtime mechanics** (per-turn sandbox seal, cost/token ceilings,
  compaction, the MCP credential allowlist): [`docs/AGENT-RUNTIME.md`](docs/AGENT-RUNTIME.md).
- **Contributor workflow, promotions, releases:** [`CONTRIBUTING.md`](CONTRIBUTING.md),
  [`docs/VERSIONING.md`](docs/VERSIONING.md).
- **Testing strategy and the scanning stack:** [`docs/TESTING.md`](docs/TESTING.md),
  [`docs/SCANNING.md`](docs/SCANNING.md).
- **Reporting a vulnerability:** [`SECURITY.md`](SECURITY.md).

## Repo Boundaries & Coupling Doctrine (owner direction, 2026-07-11)

fleet is an **engine**: a client-agnostic harness. Everything customer- or
deployment-specific arrives as a *bundle* (`FLEET_CLIENT_CONFIG_DIR`) — never
as fleet code. To keep the v2 consolidation from becoming a maintenance
tarpit:

1. **Bundles are data, fleet is engine.** Per-customer MCP servers, personas,
   protocols, env/tool contracts, and critical-tool lists live in the
   *-config repos. A new customer is a new bundle, not a fleet change. The
   bundle `manifest.yaml` is the complete contract for what a server needs
   (env keys, tool allowlist, `critical_tools`, `identity_env`) — if fleet
   can't express something a server needs, extend the schema here (as with
   `${FLEET_WORKSPACE}`), don't special-case a customer.
2. **Engines never depend on intake apps.** fleet MUST NOT import, clone, or
   gate CI on manifest (or any future front door). Intake apps integrate by
   calling fleet's API and receiving webhooks — one create seam in, one
   outcome callback out — and they own their contract checks as data
   fixtures in their own repos.
3. **MCP source of truth: the bundle.** Each *-config bundle fully owns the
   MCP servers under its `mcp/`. Fix a server **in the bundle that ships
   it** — that is the only place a fix reaches a customer. The bundles are
   peers: there is no upstream among them and no sync between them, so a
   fix that matters to more than one is hand-ported into each, as its own
   reviewed PR. cutlass is deprecated; its `mcp/` copy serves the legacy v1
   stack only and is not authoritative for any bundle. The cutlass → bundle
   sync was deleted on 2026-08-13 (it had silently reverted a bundle-side
   fix; see cutlass `mcp/docs/CONSUMER_SYNC.md`) — do not re-introduce an
   automated sync, and do not tell anyone to "fix it upstream and re-sync".
4. **Identity/credentials are harness-side.** Host-side broker + suffixed
   account vars + `identity_env` refusal; servers read plain env and stay
   customer-agnostic. Bundle manifests name the *variables*, never values.
5. **Additive-first schema evolution.** New `ServerDef` fields must fail
   loud on old manifests only when actually used (strict decoding is the
   backstop); bundles adopt new fields only after the fleet release that
   understands them ships.
