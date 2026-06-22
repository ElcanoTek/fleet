# Fleet web — LIVE Playwright e2e (real backend, fake LLM)

These specs run against a **fully real fleet stack** — everything except the LLM
is genuine. The product owner dislikes mocked-only tests; this is the answer.

| Layer | Real? | Notes |
| --- | --- | --- |
| Postgres (chat + sched DBs) | ✅ | local instance or `$E2E_DATABASE_DSN`; fresh DBs per run |
| Go chat listener (`/chat`, SSE) | ✅ | real auth, real streaming |
| Go orchestrator listener (`/tasks`, logs) | ✅ | real scheduler + worker pool |
| Podman sandbox (`bash`, `run_python`) | ✅ | per-turn container, hardened rootless flags |
| Next.js web app | ✅ | `next build` + `next start` |
| **LLM** | ❌ **STUBBED** | the wire-compatible fake `cmd/fake-llm`, reached via `OPENROUTER_BASE_URL` |

Only the LLM is replaced — by a fake that speaks the OpenRouter chat-completions
wire format (SSE streaming + assistant `tool_calls`). The real fantasy provider,
SSE parser, tool loop, scheduler and sandbox all run unchanged. Specs steer the
fake deterministically by embedding a `[[scenario:NAME]]` marker in the prompt.

## Run it locally

```bash
cd web
npm ci
npx playwright install --with-deps chromium

# Boots the whole stack via scripts/e2e-boot-server.sh, then runs the live project.
npm run test:e2e:live:real
# == E2E_LIVE=1 playwright test --project=live
```

The boot script (`scripts/e2e-boot-server.sh`) builds the binaries, ensures the
sandbox image (builds it if absent), starts the fake LLM, boots fleet (both
listeners) pointed at the fake, builds + `next start`s the web app, seeds the
test users, health-polls everything (no fixed sleeps), and installs a cleanup
trap. It is also what `playwright.config.ts` launches as the live `webServer`.

## Specs

| Spec | Journey (all real except the LLM) |
| --- | --- |
| `auth.spec.ts` | real password login mints a real session; unauth `/chat` → `/login`; bad password stays on `/login`. |
| `chat-sandbox.spec.ts` | the fake LLM drives a real `bash` + `run_python` loop in the **real Podman sandbox**; the real tool stdout streams over SSE and renders in the execution trail. |
| `scheduled-task.spec.ts` | a task created in the orchestrator UI is leased by the **real worker pool** and run to `success` through the same sandbox (the fake calls `confirm_audit` to clear scheduled-mode enforcement); logs are retrievable. |
| `cross-view.spec.ts` | one real session navigates `/chat` ↔ `/orchestrator` without re-login (the single middleware gates both). |

## Required / notable env (all defaulted by the boot script)

| Var | Default | Purpose |
| --- | --- | --- |
| `E2E_LIVE` | — | set to `1` to select the live project + boot webServer. |
| `OPENROUTER_BASE_URL` | set by boot | points fleet's provider at the fake LLM (the one stub seam). Bare var, honored before the `FLEET_`/`CHAT_`/`CUTLASS_` family. |
| `FLEET_SANDBOX_IMAGE` | `localhost/fleet-sandbox:latest` | the sandbox image; CI pulls from GHCR, else builds locally. |
| `E2E_DATABASE_DSN` | local pg @ 5432 | admin DSN with CREATEDB; the boot makes `fleet_e2e_chat` + `fleet_e2e_sched`. |
| `E2E_WORKSPACE_BASE` | `$TMPDIR/fleet-e2e-<uid>` | sandbox workspace base; **must be world-traversable** (the container runs as uid 1000 and the workspace is bind-mounted at the same host path, so a path under a `0550` `$HOME` like `/root` breaks `run_python`'s chdir). |
| `NEXT_PORT` / `CHAT_PORT` / `ORCH_PORT` / `FAKE_LLM_ADDR` | 3100 / 18080 / 18000 / 127.0.0.1:18090 | fixed ports for determinism. |
| `E2E_TEST_EMAIL` / `E2E_TEST_PASSWORD` | `e2e@example.com` / `e2e-test-password` | seeded chat user. |
| `E2E_SCHED_USERNAME` | `e2e` | seeded orchestrator (sched) admin user. |
| `FLEET_SERVER_TOKEN` / `ADMIN_API_KEY` / `APP_SESSION_SECRET` / `AUTH_SIGNING_PUBKEY` | throwaway test values | shared secrets; never real. |
| `E2E_CANARY` | `0` | `1` swaps the fake for a REAL model (see the canary note below). |

## Canary (NOT a PR gate)

`e2e/canary/sandbox-smoke.spec.ts` runs the same stack against a **real cheap
OpenRouter model** instead of the fake (`E2E_CANARY=1`, needs a real
`OPENROUTER_API_KEY`). It is a drift detector run nightly/manually by the
`e2e-canary` workflow, which **skips cleanly** when the key secret is absent.

## Determinism measures

- Fixed ports; no `reuseExistingServer` in CI.
- Health-gated boot (poll `/healthz`, `/health`, the Next `/login`, the fake's
  `/healthz`) — never a fixed sleep.
- A `sandbox-probe` gate fails the boot early if the container can't run bash +
  python (normal + lockdown) before any spec runs.
- Web-first assertions; bounded `expect.poll`/`toPass` for the worker-pool task
  status; per-test timeouts raised for the cold-container journeys.
- Cleanup trap kills procs and removes sandbox containers by the
  `chat-sandbox-` name prefix.
