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
make lint         # golangci-lint run    — the lint gate; must pass clean
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
**and** live, against a real backend + sandbox), and a gitleaks secret scan. **Every job
must be green before merge.** Tests are deterministic without a
live model: use the fake-LLM seam (`internal/fakellm` via `OPENROUTER_BASE_URL`),
never a real key.

## Repository map

See the README "Repository layout" for the annotated tree. In short: `cmd/` (the
`fleet` binary, `fleet-admin` CLI, …), `internal/`
(`agentcore` the one run loop, `sandbox`,
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
- **Honesty in docs.** Claim only shipped, tested capabilities. (The README
  Standards section lists MCP and Agent Skills as shipped.) If you add a
  capability, document what it actually does — and what it does not. (Example: a
  skill's `allowed-tools` frontmatter is parsed but **not** enforced as a hard
  authorization gate — the docs say so plainly rather than implying a boundary
  that isn't there.)
- **Client content is external.** Branding, the MCP catalog, personas, protocols,
  skills, and the sandbox Containerfile live in an out-of-repo client-config
  bundle (`FLEET_CLIENT_CONFIG_DIR`). fleet ships only the generic `config/default`
  bundle — do not add client-specific content to this repo.

## Conventions

- Single Go module `github.com/ElcanoTek/fleet`, Go 1.26. Keep it `go vet`- and
  `golangci-lint`-clean — lint failures block CI.
- **Go Test Coverage**: CI runs with `-coverprofile=coverage.out -covermode=atomic` on the plain `go test` step. Local coverage can be run using `make test-cover`. Project coverage must not drop more than 2% compared to the base branch, and new patch code in PRs must have at least 60% coverage, configured via `codecov.yml` at the repo root.
- **Match the surrounding code:** naming, idioms, and comment density. The
  `internal/agentcore` package comments explain *why* each governance invariant
  holds — preserve that level of explanation when you extend it.
- Run tests in the **foreground**. Do not background `go test`, and do not
  `pkill -f 'go test'` (it can kill the shell). Prefer `make test` (it sets
  `-p 1`, which the suite expects).
- **Composer & Textarea heights**: In `web/src/app/chat/ui/Composer.tsx` and `chat-experience.tsx`, avoid mixing JS-based auto-grow style height clamping with Tailwind `max-h-*` CSS classes on the composer textarea, to prevent layout flickering. Use `MAX_COMPOSER_HEIGHT_PX` (200px) as the single source of truth.
- **Composer keybindings & preferences**: The user's send preference (`fleet.sendKey` in localStorage) controls whether Enter or Ctrl+Enter (with Cmd+Enter) sends a message. The keydown handler adjusts based on this config and skips sending on touch devices to let mobile keyboards use native enter keys.
- **Bulk Conversation Operations**:
  - **Soft Delete**: Setting `FLEET_CONVERSATION_SOFT_DELETE=true` switches deletes to soft-deletes (`deleted_at` timestamp). A 30-day background sweeper permanently purges soft-deleted rows. Soft-deleted conversations are excluded from retrieval and search query filters.
  - **Bulk APIs**: Bulk delete and patch actions are rate-limited to 100 IDs per request. Transactional pre-checks protect ownership; any single foreign ID fails the entire request (403 forbidden) to ensure consistency.
  - **Bulk UI**: Uses checkboxes for selection, warning users when selecting more than 50 conversations. The confirmation modal enforces a 3-second disabled countdown for delete actions.
- **Conditional Task Execution (run_if)**:
  - Pre-run shell gates (`run_if`) are evaluated serially on the host as the fleet process user prior to task promotion.
  - The evaluation is restricted to `PATH=/usr/bin:/bin` and `HOME=/tmp` with a custom timeout.
  - If a gate skips a recurring task, its status stays `scheduled`, and `scheduled_for` is advanced to the next cron occurrence. For one-shot tasks, status remains `scheduled` but the time is not advanced, acting as a soft hold.
- **Batch Task Operations**:
  - **Batch APIs**: `POST /tasks/batch` allows batch task creation of up to 100 tasks. In atomic mode (`atomic: true`), all tasks are validated up front and created in a single DB transaction (returning `422 Unprocessable Entity` with errors if any fail). In non-atomic mode (`atomic: false`), it behaves best-effort and returns `207 Multi-Status` for partial success.
  - **Rate Limiting**: Rate limiter consumes `N` tokens for `N` tasks in a batch (instead of 1 token per batch request) via `apikeys.Manager.ConsumeN`.
  - **Single Multi-row Insert**: `db.AddTaskBatch` inserts multiple tasks in a single query via parameterized multi-row insert rather than individual sequential inserts.
  - **CLI**: `fleet-admin task batch-create --from-file tasks.json [--atomic]` allows creating tasks from a JSON file (or stdin via `-`).
- **Task Definition Import/Export**:
  - **Task Names**: Scheduled tasks now support an optional `name` field (mapped to the database `tasks.name` column with a partial unique index). Empty/empty-string names represent "unnamed" tasks and are always created fresh on import. Non-empty names are unique and serve as conflict keys.
  - **Payload Limits**: Imports are rate-limited/capped to at most 100 task records per request (matching bulk API policies). Payload-internal duplicate name entries are validation errors.
  - **Conflict Behaviors**: Mode `conflict=error` aborts the batch on any collision; `conflict=skip` skips colliding entries and writes others; `conflict=replace` performs an in-place update of colliding entries. Mode `conflict=replace` requires admin permission.
  - **Formats**: Support both JSON and YAML envelopes (via `github.com/goccy/go-yaml`). Version is set to `"1"`.
- One focused branch + PR per change; keep diffs scoped. Don't refactor unrelated
  code in a feature PR. See `CONTRIBUTING.md` for branch/PR conventions and DCO
  sign-off.


## Where to look

- **Agent runtime mechanics** (per-turn sandbox seal, cost/token ceilings,
  context compaction, MCP credential allowlist, the scheduled end-of-run
  verifier, the optional "phone a friend" super-LLM review, git-worktree
  isolation): [`docs/AGENT-RUNTIME.md`](docs/AGENT-RUNTIME.md)
- **Architecture overview:** [`README.md`](README.md) ("Architecture at a glance")
- **Why the invariants are the way they are:** [`docs/adr/`](docs/adr/)
  (Architecture Decision Records)
- **Contributor workflow + CI gates:** [`CONTRIBUTING.md`](CONTRIBUTING.md)
- **Reporting a vulnerability:** [`SECURITY.md`](SECURITY.md)
