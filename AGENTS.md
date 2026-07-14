# AGENTS.md

Operating guide for AI coding agents (Claude Code, Codex, Cursor, opencode, Goose,
Gemini CLI, …) working in the **fleet** repository. It follows the
[agents.md](https://agents.md) convention; `CLAUDE.md` is a symlink to this file so
Claude Code and any `AGENTS.md`-aware tool read the same instructions.

Humans, start with [`README.md`](README.md) and [`CONTRIBUTING.md`](CONTRIBUTING.md);
this file is the agent-facing distillation, not a replacement for them.

## What fleet is (one paragraph)

fleet is a self-hosted, general-purpose agent platform. **One** Go process runs
interactive chat *and* a scheduling engine on one box, driven by **one** unified
agent runtime (`internal/agentcore`). Every agent tool call — bash, Python, file
I/O, MCP — executes inside a rootless-Podman sandbox; tools and data are reached
through an MCP catalog whose credentials are brokered host-side. See
the README "Architecture at a glance" for the full picture.

## Build · test · lint (run before opening any PR)

```sh
make build        # compile-check ./... AND emit ./fleet + ./fleet-admin
make compile      # go build ./...   (compile-check only; no artifacts)
make test         # go test -p 1 ./...   — run in the FOREGROUND
make test-race    # go test -race -p 1 ./...   (use when touching concurrency)
make test-cover   # run Go tests with coverage profiling (writes coverage.out)
make lint         # golangci-lint + migration DDL lint — must pass clean
make fmt          # gofmt -w .
make tidy         # go mod tidy
```

When you touch `web/` (the Next.js app):

```sh
cd web && npm ci && npm run lint && npm run test && npm run build
cd web && npx playwright test --project=mocked     # mocked e2e
```

CI mirrors all of this — Go build/vet/lint/test (including a `-race` lane) plus a
`govulncheck` dependency-CVE scan, a Grype container-image CVE scan (fail on a
fixable CRITICAL) of the sandbox image, web lint/test/build, Playwright (mocked
**and** live, against a real backend + sandbox), a migration DDL lint, and a
gitleaks secret scan. **Every job must be green before merge.** Tests are
deterministic without a live model: use the fake-LLM seam (`internal/fakellm`
via `OPENROUTER_BASE_URL`), never a real key.

## Repository map

See the README "Repository layout" for the annotated tree. In short: `cmd/` (the
one unified `fleet` binary — `fleet serve` runs the server, every other verb is the
operator CLI; `fleet-admin` is a transitional deprecation shim that still works for
one release), `internal/` (`agentcore` the one run loop, `sandbox`,
`mcp`, `creds`, `clientconfig`, `store`, `sched`, `httpapi`, …), `web/` (one
Next.js app: `/chat` + `/orchestrator`), and `config/default/` (the generic
client bundle baked in so fleet runs bare).

## Non-negotiable invariants — do NOT weaken these

These are the security and design guarantees the whole project rests on. A change
that breaks one is wrong even if tests pass. The *why* behind several of them is
recorded as Architecture Decision Records in [`docs/adr/`](docs/adr/) — a change
that adds, weakens, or reverses an invariant must add or supersede an ADR in the
same PR.

- **The sandbox is mandatory.** The agent loop runs in the fleet process, but
  every agent tool call (bash, Python, file I/O, MCP) runs inside the
  rootless-Podman sandbox — there is **no** fast path that skips it, and the host
  enforces all policy. The loop holds no privileged local executor of its own:
  each tool call is handed to the sandbox under host policy. MCP credentials are
  brokered **out-of-process** (issue #167) and **never** enter the sandbox — the
  broker injects them only when it runs a delegated MCP call host-side.
- **Credentials stay host-side.** MCP/connector credentials are brokered on the
  host and **never** enter the sandbox, the agent container, the model context, or
  logs. Never ship a secret into a container or print one.
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

- Single Go module `github.com/ElcanoTek/fleet`, Go 1.26. Keep it `go vet`- and
  `golangci-lint`-clean — lint failures block CI.
- **Coverage (advisory, not a merge gate)**: CI runs the plain `go test` step
  with `-coverprofile=coverage.out -covermode=atomic` and uploads to Codecov
  with `fail_ci_if_error: false`. The thresholds in `codecov.yml` (project
  drop ≤2%, patch ≥60%) are targets Codecov reports as a PR comment/check when
  a `CODECOV_TOKEN` secret is configured — **they do NOT block merge**, and
  without the token the upload is a silent no-op. Treat coverage as a quality
  signal, not a gate: add tests that catch real behavior, not to chase a
  number. (The real merge gates are build/vet/lint, the test suites, the
  `-race` lane, govulncheck, Grype, the migration linter, and gitleaks.)
- **Match the surrounding code:** naming, idioms, and comment density. The
  `internal/agentcore` package comments explain *why* each governance invariant
  holds — preserve that level of explanation when you extend it.
- Run tests in the **foreground**. Do not background `go test`, and do not
  `pkill -f 'go test'` (it can kill the shell). Prefer `make test` (it sets
  `-p 1`, which the suite expects).
- **New task fields thread one way.** A new per-task column follows the
  `allow_network` pattern: a migration, then `taskColumns`/`scanTask`/`AddTask`/
  `taskInsert*` (+ `UpdateTaskTx` when mutable) and the export/import record.
  Result-like columns written after the terminal transition (e.g.
  `error_analysis`) are excluded from the insert/upsert so a status write can
  never clobber them.
- **Ship features with honest scope.** Every feature lands with a design note
  recording what shipped, what deviated from the issue, and what was deliberately
  deferred — in a dedicated `docs/<FEATURE>.md` (plus an ADR when an invariant is
  touched) and a `CHANGELOG.md` entry. Do **not** append per-feature design notes
  to this file — that is how it grew past 300 lines once already; the historical
  notes now live in [`docs/FEATURE-NOTES.md`](docs/FEATURE-NOTES.md).
- One focused branch + PR per change; keep diffs scoped. Don't refactor unrelated
  code in a feature PR. See `CONTRIBUTING.md` for branch/PR conventions and DCO
  sign-off.

## Where to look

- **Per-feature design notes** (shipped design, deviations from the issue, honest
  scope — one bullet per feature): [`docs/FEATURE-NOTES.md`](docs/FEATURE-NOTES.md).
  Newer features each have a dedicated page in [`docs/`](docs/), and invariant
  changes have an ADR in [`docs/adr/`](docs/adr/).
- **Agent runtime mechanics** (per-turn sandbox seal, cost/token ceilings,
  context compaction, MCP credential allowlist, the scheduled end-of-run
  verifier, the optional "phone a friend" super-LLM review, git-worktree
  isolation): [`docs/AGENT-RUNTIME.md`](docs/AGENT-RUNTIME.md)
- **Architecture overview:** [`README.md`](README.md) ("Architecture at a glance")
- **Why the invariants are the way they are:** [`docs/adr/`](docs/adr/)
  (Architecture Decision Records)
- **Contributor workflow + CI gates:** [`CONTRIBUTING.md`](CONTRIBUTING.md)
- **Testing strategy** (unit / fake-LLM / mocked + live Playwright / canary):
  [`docs/TESTING.md`](docs/TESTING.md)
- **HTTP API versioning** (the `/v1` prefix + `X-Fleet-API-Version` + `/api-info`
  discovery + deprecation contract): [`docs/api-versioning.md`](docs/api-versioning.md)
- **Database migrations** (the two runners, safe-DDL patterns, the migration DDL
  linter, `fleet migrate status`, rollback scope): [`docs/MIGRATIONS.md`](docs/MIGRATIONS.md)
- **MCP server hot-reload** (add/remove/update MCP servers without a restart via
  `fleet mcp reload` / SIGHUP / admin endpoint): [`docs/MCP-RELOAD.md`](docs/MCP-RELOAD.md)
- **Testing MCP servers** (`fleet mcp test` per-server smoke: handshake +
  tools/list with the boot loader's exact env/gates; plus the full testing
  ladder): [`docs/MCP-TESTING.md`](docs/MCP-TESTING.md)
- **The connector directory** (trust classes, the built-in hosted-server
  catalog, provenance tiers) and its **guided onboarding** (setup hints,
  guided tenant/API-key/BYO-client add forms, the per-user api_key auth mode):
  [`docs/MCP-CATALOG.md`](docs/MCP-CATALOG.md) +
  [`docs/CONNECTOR-ONBOARDING.md`](docs/CONNECTOR-ONBOARDING.md)
- **Admin-managed workspace feature settings** (the Settings → Admin Features
  panel: DB override > env var > default, live apply, the registry admission
  rule, what stays env-only): [`docs/ADMIN-SETTINGS.md`](docs/ADMIN-SETTINGS.md)
- **Task notifications** (email/webhook channels, the admin Notifications
  panel with sealed write-only secrets + test sends, env precedence):
  [`docs/NOTIFICATIONS.md`](docs/NOTIFICATIONS.md)
- **Load testing & benchmarks** (`fleet-bench` HTTP chat load via the fake-LLM
  seam + subsystem throughput benchmarks): [`docs/LOAD-TESTING.md`](docs/LOAD-TESTING.md)
- **Prompt-cache prefix-stability contract** (what must stay byte-stable in the
  cacheable prefix so the provider prompt cache keeps hitting):
  [`docs/PROMPT-CACHE-CONTRACT.md`](docs/PROMPT-CACHE-CONTRACT.md)
- **Evals & regression gating** (golden capture, the `evals/` bundle contract,
  scorers + LLM-judge, `fleet eval` CLI):
  [`docs/EVALS.md`](docs/EVALS.md) + [`docs/adr/0018-self-hosted-eval-harness.md`](docs/adr/0018-self-hosted-eval-harness.md)
- **Reporting a vulnerability:** [`SECURITY.md`](SECURITY.md)

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
3. **MCP source of truth.** While v1 (cutlass/moc/gig) is live, the SSP MCP
   servers are authored in cutlass and synced byte-identically into the
   bundles (cutlass `scripts/mcp_sync_consumers.py`). Never patch a synced
   server inside a bundle; fix it upstream and re-sync. After the v2 flip
   the bundles become the only per-customer home.
4. **Identity/credentials are harness-side.** Host-side broker + suffixed
   account vars + `identity_env` refusal; servers read plain env and stay
   customer-agnostic. Bundle manifests name the *variables*, never values.
5. **Additive-first schema evolution.** New `ServerDef` fields must fail
   loud on old manifests only when actually used (strict decoding is the
   backstop); bundles adopt new fields only after the fleet release that
   understands them ships.
