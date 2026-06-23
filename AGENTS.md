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
through an MCP catalog whose credentials are brokered host-side; and other coding
agents (Claude Code, Goose, …) can be driven as sandboxed flavors over ACP. See
the README "Architecture at a glance" and
[`docs/MIGRATION_PLAN_V2.md`](docs/MIGRATION_PLAN_V2.md) for the full picture.

## Build · test · lint (run before opening any PR)

```sh
make build        # compile-check ./... AND emit ./fleet + ./fleet-admin
make compile      # go build ./...   (compile-check only; no artifacts)
make test         # go test -p 1 ./...   — run in the FOREGROUND
make lint         # golangci-lint run    — the lint gate; must pass clean
make test-race    # go test -race -p 1 ./...   (use when touching concurrency)
make fmt          # gofmt -w .
make tidy         # go mod tidy
```

When you touch `web/` (the Next.js app):

```sh
cd web && npm ci && npm run lint && npm run test && npm run build
cd web && npx playwright test --project=mocked     # mocked e2e
```

CI mirrors all of this — Go build/vet/lint/test (including a `-race` lane) plus a
`govulncheck` dependency-CVE scan, web lint/test/build, Playwright (mocked **and**
live, against a real backend + sandbox), and a gitleaks secret scan. **Every job
must be green before merge.** Tests are deterministic without a
live model: use the fake-LLM seam (`internal/fakellm` via `OPENROUTER_BASE_URL`),
never a real key.

## Repository map

See the README "Repository layout" for the annotated tree. In short: `cmd/` (the
`fleet` binary, `fleet-admin` CLI, `fleet-native-agent`, …), `internal/`
(`agentcore` the one run loop, `acpruntime` the ACP client + agent, `sandbox`,
`mcp`, `creds`, `clientconfig`, `store`, `sched`, `httpapi`, …), `web/` (one
Next.js app: `/chat` + `/orchestrator`), and `config/default/` (the generic
client bundle baked in so fleet runs bare).

## Non-negotiable invariants — do NOT weaken these

These are the security and design guarantees the whole project rests on. A change
that breaks one is wrong even if tests pass.

- **The sandbox is mandatory.** Every agent tool call runs inside the
  rootless-Podman sandbox. There is **no** trusted fast path that skips it. The
  native agent itself runs *inside* the sandbox and delegates execution back to
  the host — it holds no privileged local executor.
- **Credentials stay host-side.** MCP/connector credentials are brokered on the
  host and **never** enter the sandbox, the agent container, the model context, or
  logs. Never ship a secret into a container or print one.
- **Governance is one core.** `agentcore.Run` is the single governed loop (policy,
  cost/token ceilings, audit, notes). New entrypoints **adapt I/O around it** —
  they must not fork a second, weaker governance path. (The ACP ingress/runtime
  work is exactly this pattern: same core, different transport.)
- **No secrets in the repo.** gitleaks gates CI. Use the fake-LLM seam and obvious
  placeholders in tests; the real `OPENROUTER_API_KEY` lives outside the repo.
- **Honesty in docs.** Claim only shipped, tested capabilities. (The README
  Standards section lists ACP, MCP, and Agent Skills as shipped.) If you add a
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
- **Match the surrounding code:** naming, idioms, and comment density. The
  `internal/acpruntime` package comments explain *why* each governance invariant
  holds — preserve that level of explanation when you extend it.
- Run tests in the **foreground**. Do not background `go test`, and do not
  `pkill -f 'go test'` (it can kill the shell). Prefer `make test` (it sets
  `-p 1`, which the suite expects).
- One focused branch + PR per change; keep diffs scoped. Don't refactor unrelated
  code in a feature PR. See `CONTRIBUTING.md` for branch/PR conventions and DCO
  sign-off.

## Where to look

- **Driving / adding other agents** (ACP flavors, governance tiers, permission
  model): [`docs/USING-AGENTS.md`](docs/USING-AGENTS.md)
- **Architecture + the phased migration plan:**
  [`docs/MIGRATION_PLAN_V2.md`](docs/MIGRATION_PLAN_V2.md)
- **Contributor workflow + CI gates:** [`CONTRIBUTING.md`](CONTRIBUTING.md)
- **Reporting a vulnerability:** [`SECURITY.md`](SECURITY.md)
