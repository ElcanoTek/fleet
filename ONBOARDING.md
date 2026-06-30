# Onboarding: clone to your first sandbox session

A single linear path from a fresh checkout to a passing `make test` and one
real, streamed chat turn through the rootless-Podman sandbox — in well under
30 minutes on a box that already has the toolchain.

This guide is a narrative shortcut, not a replacement for the canonical docs.
It reuses the exact commands from [`README.md`](README.md),
[`CONTRIBUTING.md`](CONTRIBUTING.md), and the [`Makefile`](Makefile); when in
doubt those are the source of truth. For the architecture, read the README
"Architecture at a glance"; for the agent runtime internals, read
[`docs/AGENT-RUNTIME.md`](docs/AGENT-RUNTIME.md). Agents working in this repo
should read [`AGENTS.md`](AGENTS.md) first.

> **What you are about to stand up.** fleet is **one** Go process that runs
> interactive chat *and* a scheduling engine on one box, driven by **one**
> agent runtime (`internal/agentcore`). Every agent tool call — bash, Python,
> file I/O, MCP — executes inside a **rootless-Podman sandbox**; that sandbox is
> mandatory and host policy is enforced around it. Tests stay deterministic
> without a live model by using the fake-LLM seam (`internal/fakellm`, reached
> via `OPENROUTER_BASE_URL`) — never a real key in tests.

---

## 1. Prerequisites

Install these once. The versions match CI (`.github/workflows/ci.yml`) — match
them so a local run agrees with the gate.

| Tool | Version | Why |
| --- | --- | --- |
| **Go** | the version pinned in `go.mod` (currently **1.26.4**) | the backend; CI uses `go-version: 1.26.4` |
| **Node.js** | **22** + npm | the `web/` Next.js app; CI uses `node-version: 22` |
| **golangci-lint** | **v2.12.2** | the lint gate (`.golangci.yml` is the v2 schema and is tuned to this version — keep them in sync) |
| **Podman** (rootless) | recent | the execution sandbox; needed for the sandbox-backed tests / first chat turn. Most unit tests self-skip when podman is absent. |
| **PostgreSQL** | a local cluster you can create DBs on | the chat/scheduler store suites |
| **python3** | system | host-side Python MCP + the sandbox build |

On a bare Fedora/RHEL box the bootstrap script installs the whole toolchain for
you (`git curl jq golang nodejs python3 python3-pip gcc podman`); see the README
"Quick start (one host)". For a **developer** checkout you can install the tools
above directly.

### Rootless Podman: subuid / subgid

Rootless Podman maps container uids/gids into a per-user subordinate range. If
your user has no range yet, add one (this is exactly what `scripts/bootstrap.sh`
writes for the service user):

```sh
# As root, for your login user $USER — skip if /etc/subuid already lists it:
grep -q "^$USER:" /etc/subuid || echo "$USER:100000:65536" | sudo tee -a /etc/subuid
grep -q "^$USER:" /etc/subgid || echo "$USER:100000:65536" | sudo tee -a /etc/subgid
podman system migrate   # pick up a freshly added range
```

Verify rootless Podman works before continuing:

```sh
podman info >/dev/null && echo "rootless podman OK"
```

---

## 2. Clone and build

```sh
git clone https://github.com/ElcanoTek/fleet.git
cd fleet

make build      # compile-check ./... AND emit ./fleet + ./fleet-admin
```

`make build` runs `go build ./...` (compile-check) and then emits the two
deployable artifacts (`./fleet`, `./fleet-admin`). If this is green your Go
toolchain is good. The server runs via `fleet serve` (bare `fleet` also serves,
for back-compat); all other verbs are the operator CLI, and `make install` puts
`fleet` on PATH. (`fleet-admin <verb>` still works but is deprecated and will be
removed.)

---

## 3. Lint

```sh
make lint       # golangci-lint run — the lint gate; must pass clean
```

The lint backlog is at zero — keep it there. If golangci-lint flags something,
fix it or add a `//nolint` **with a reason** (the `nolintlint` linter requires
one).

---

## 4. Local databases (the two-database split)

The store / HTTP / scheduler suites need Postgres. The **chat** and **scheduler**
migration systems both use a table named `schema_migrations` with *incompatible*
schemas, so they **must** point at separate databases. Both suites auto-migrate
from an empty database, so you only need to create two empty DBs.

Create them (adjust user/password to your cluster; CI uses `fleet:fleet`):

```sh
createdb fleet_chat_test
createdb fleet_sched_test
```

Export the DSNs and the client-config dir. These mirror CI exactly — the chat
suites read `FLEET_TEST_DATABASE_URL` (falling back to `CHAT_TEST_DATABASE_URL`);
the scheduler suite reads `DATABASE_URL`:

```sh
export FLEET_TEST_DATABASE_URL="postgres://fleet:fleet@localhost:5432/fleet_chat_test?sslmode=disable"
export CHAT_TEST_DATABASE_URL="$FLEET_TEST_DATABASE_URL"
export DATABASE_URL="postgres://fleet:fleet@localhost:5432/fleet_sched_test?sslmode=disable"
export FLEET_CLIENT_CONFIG_DIR="$(pwd)/config/default"
```

`config/default` is the generic client bundle baked into the repo, so fleet runs
bare without any external config.

---

## 5. Test

```sh
make test       # go test -p 1 -tags fleet_host_executor ./...
```

Run tests in the **foreground** — do not background `go test`. `make test` sets
`-p 1` (the suite expects it) and the `fleet_host_executor` build tag (so the
host-mode fixtures and `MockMode` tests compile; the shipped binary from
`make build` is built **without** that tag, so the unsandboxed host executor
never ships). The podman-gated `internal/sandbox` integration tests self-skip
when podman is masked; the store/HTTP/scheduler suites need the two databases
from step 4.

At this point you have reached a passing `make test` — the first acceptance
milestone.

---

## 6. Build the sandbox image

Agent tool calls (`bash`, `run_python`) execute inside one container image. Build
the image for the bundle `FLEET_CLIENT_CONFIG_DIR` points at (the in-repo
`config/default` by default):

```sh
scripts/build-sandbox-image.sh    # → localhost/fleet-sandbox:latest (podman)
```

The Containerfile lives in the bundle at `<bundle>/sandbox/Containerfile`. The
generic bundle's default tag is `localhost/fleet-sandbox:latest`.

---

## 7. Your first sandbox session

You have two documented ways to drive a real, streamed turn through the real
rootless-Podman sandbox. **The fake-LLM path needs no API key** — start there.

### Option A — full stack with the fake-LLM seam (no key)

This boots the *whole* real stack — Postgres, both Go listeners, SSE streaming,
the scheduler + worker pool, and the Podman sandbox — and stubs **only** the LLM
with the wire-compatible fake (`cmd/fake-llm`, reached via `OPENROUTER_BASE_URL`).
It is the same `e2e-live` job CI runs, and the fastest way to see a streamed chat
turn drive real `bash` + `run_python` calls in the real sandbox:

```sh
cd web
npm ci
npx playwright install --with-deps chromium
npm run test:e2e:live           # E2E_LIVE=1 playwright test --project=live
```

The boot script (`scripts/e2e-boot-server.sh`) builds the binaries, ensures the
sandbox image, starts the fake LLM, boots fleet pointed at it, builds and
`next start`s the web app, seeds the test users, and health-polls everything (no
fixed sleeps). The `chat-sandbox` spec is the one to watch: the fake LLM drives a
real `bash` + `run_python` loop in the **real Podman sandbox** and the real tool
stdout streams back over SSE. See [`web/e2e/live/README.md`](web/e2e/live/README.md)
for the full env table.

### Option B — one task end-to-end with a real model (cutlass)

`cmd/cutlass` runs a single task YAML to completion through the **same governed
scheduled runtime** the production scheduler uses (`agentcore.Run`,
`Mode=Scheduled`; tool calls still run in the sandbox, MCP credentials still
brokered host-side). This path needs a **real** `OPENROUTER_API_KEY` in your
environment (or a `.env` file) and podman:

```sh
export OPENROUTER_API_KEY=...                              # your real key — never commit it
scripts/run_workflow_live.sh docs/examples/cutlass-task.yaml
```

The script ensures the sandbox image exists, mints a fresh isolated workspace,
and points a stable `latest.log` symlink at the run so you can `tail -f` it. The
example task writes and reads back a file in the sandbox workspace. See
[`docs/examples/cutlass-task.yaml`](docs/examples/cutlass-task.yaml) for the
task schema.

> **Why no real key in Option A.** The non-negotiable invariant is *no secrets in
> the repo and the real `OPENROUTER_API_KEY` lives outside it.* Tests and the
> live e2e use the fake-LLM seam so they stay deterministic and free; only the
> hands-on cutlass run (Option B) talks to a real model.

---

## 8. The `web/` app on its own

If you only touched `web/`, the full front-end loop is:

```sh
cd web
npm ci
npm run lint
npm run test                          # vitest unit tests
npm run build                         # Next production build
npx playwright test --project=mocked  # deterministic mocked e2e (no backend, no key)
```

The **mocked** Playwright suite route-intercepts every backend call, so it runs
on a bare runner with no database, podman, or API keys — it is the fast lane.

---

## 9. The PR loop: what gates merge, and how to reproduce it

Every pull request must be green before merge. The CI jobs
(`.github/workflows/ci.yml`) and their local equivalents:

| CI job | What it runs | Reproduce locally |
| --- | --- | --- |
| **Secret scan (gitleaks)** | scans for any new, un-ignored secret | `gitleaks dir . --redact --exit-code 1` |
| **Go** | `go build` (release config, no host-executor tag), `go vet -tags fleet_host_executor`, `golangci-lint`, `go test` (+ a `-race` lane), and a `govulncheck` CVE scan | `make build && go vet -tags fleet_host_executor ./... && make lint && make test` (and `make test-race` when touching concurrency) |
| **Web** | `npm run lint`, vitest, `npm run build` | step 8, lines 1–4 |
| **Playwright (mocked)** | the deterministic mocked suite | `cd web && npm run test:e2e:mocked` |
| **e2e-live** | the full real stack with a stubbed LLM (no OpenRouter spend) | `cd web && npm run test:e2e:live` (Option A above) |

Then follow the contributor conventions in
[`CONTRIBUTING.md`](CONTRIBUTING.md):

- Branch off the latest `main` with a descriptive prefix (`feat/…`, `fix/…`,
  `chore/…`, `docs/…`, `test/…`).
- Keep the PR focused; write a clear description (what changed, why, how
  verified).
- **Sign off every commit** with the Developer Certificate of Origin:
  `git commit -s -m "..."`.

Do not weaken any of the non-negotiable invariants in [`AGENTS.md`](AGENTS.md)
(the sandbox is mandatory, credentials stay host-side, governance is one core,
no secrets in the repo, honest docs, client content stays external).

---

## Quick reference

```sh
make build                              # ./fleet + ./fleet-admin (compile-check too)
make lint                               # golangci-lint (must pass clean)
make test                               # go test -p 1 -tags fleet_host_executor ./...
make test-race                          # same, with the race detector
scripts/build-sandbox-image.sh          # build the sandbox image
gitleaks dir . --redact --exit-code 1   # secret scan (CI gate)
```
