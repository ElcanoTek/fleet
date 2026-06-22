# Fleet web — Playwright e2e

Browser-driven end-to-end tests for the unified Fleet frontend (the Next.js app
that hosts both the `/chat` and `/orchestrator` views behind one middleware).

There are two ways to run the suite:

| Mode | What runs | Backends | Determinism | CI |
| --- | --- | --- | --- | --- |
| **Mocked** (default) | `e2e/mocked/*.spec.ts` | none — every `/api/*` call is route-intercepted by Playwright | fully deterministic | ✅ runs on every push/PR |
| **Live** | the top-level `e2e/*.spec.ts` | a really booted chat-server + orchestrator (in `CHAT_MOCK_MODE`) | depends on the stack | ❌ never in CI |

`playwright.config.ts` points `testDir` at `./e2e/mocked`, so **only the mocked
suite runs by default and in CI**. The live specs at the top level of `e2e/`
require a real Go backend and are run via a separate harness (below).

---

## Mocked suite (the CI suite)

Deterministic: it drives the *real* Next app but replaces every Go backend
(chat-server `:18080`, orchestrator/moc `:8000`, OpenRouter) with Playwright
route interception. No database, no Podman, no API keys — reproducible on a bare
runner.

```bash
cd web
npm ci
npx playwright install --with-deps chromium

# Fast local loop: drives `next dev`.
npm run test:e2e

# CI parity: builds the app and drives `next start` (the shipped bundle).
E2E_PROD=1 npm run test:e2e
```

`playwright.config.ts` boots the Next server itself (`webServer`) with the
throwaway test env (`CHAT_MOCK_MODE=1`, a test `APP_SESSION_SECRET`, and a
throwaway `AUTH_SIGNING_PUBKEY` so the "Use Elcano email" path renders). In CI
(`process.env.CI`) it uses `next start`; locally it uses `next dev` unless
`E2E_PROD=1` is set.

### How the mocks work

- **Auth** — `e2e/mocked/_session.ts` mints the two session cookies the
  middleware accepts directly: the `elcano_session` HMAC cookie (password path)
  and the `elcano_auth` Ed25519 cookie (magic-link path, signed with the test
  keypair whose public half is in `playwright.config.ts`). So specs start
  "already logged in" and exercise the *real* middleware gate without a backend.
- **API surface** — `e2e/mocked/_mocks.ts` provides `mockChatBoot()` (the chat
  shell's mount calls) and `sse()`/`fulfillSse()` (server-sent-event framing
  matching chat-server's wire format). Each spec layers its scenario-specific
  routes (the chat stream, the orchestrator task list, …) on top.

### Specs

| Spec | Asserts |
| --- | --- |
| `login.spec.ts` | unauthenticated `/chat` redirects to `/login`; unauthenticated `/orchestrator` is gated by the shared middleware; the login card renders both the password form and the "Use Elcano email" link; a verified password login lands on `/chat`; an invalid password stays on `/login` with an error; the "Use Elcano email" button bounces to the auth-service magic-link flow; a valid `elcano_auth` cookie authenticates the same as a password session. |
| `chat.spec.ts` | authenticated `/chat` reaches the empty composer; a sent turn streams text deltas + a final assistant message; the streamed `tool.call`/`tool.result` render in the execution trail (chip + expandable result); config-driven empty-state cards render from a stubbed `/api/client-config` (and the neutral defaults do not). |
| `protocol-pills.spec.ts` | with `/api/client-config` returning no cards, the neutral fallback pills render; the Summarize form templates its prompt and the mocked chat echoes it; the Draft form gates on its required field then sends the template. |
| `orchestrator.spec.ts` | the dashboard loads stats + the task list; a task is created via the shared `<McpServerPicker>` (enable a server + select a credential account), the POST payload carries the `mcp_selection` and **no** `target_node_name`; opening a task renders its react-markdown log viewer. |
| `cross-view.spec.ts` | one authenticated session navigates `/chat` ↔ `/orchestrator` via the in-app links, never bouncing to `/login`, proving the single middleware gates both views off the same cookie. |

---

## Live suite (local only — never in CI)

The top-level `e2e/*.spec.ts` specs (`auth.spec.ts`, `chat.spec.ts`,
`bug-report.spec.ts`, `protocol-pills.spec.ts`, …) drive a really booted fleet.
They use `e2e/fixtures.ts`, which performs a **real** login against a live
chat-server, so they need the Go backend present. In this externalized web tree
there is no Go binary, so the live suite is run from a full fleet checkout where
`scripts/e2e-boot-server.sh` can build and start `chat-server` + `chat-admin`.

To run live against a booted stack, repoint `testDir` (or pass an explicit path)
and set the live env, e.g.:

```bash
cd web

# Mock-mode backend (canned replies, no OpenRouter spend) — the usual live run:
npx playwright test e2e/chat.spec.ts e2e/auth.spec.ts

# Real OpenRouter (costs credits) — gated behind CHAT_E2E_LIVE=1:
CHAT_E2E_LIVE=1 npm run test:e2e:live
```

Relevant env (consumed by `playwright.config.ts` / `scripts/e2e-boot-server.sh`
in a full fleet checkout):

| Var | Purpose |
| --- | --- |
| `CHAT_MOCK_MODE=1` | chat-server returns canned `Mock reply to: <prompt>` instead of calling OpenRouter (deterministic, free). |
| `CHAT_E2E_LIVE=1` | un-skips the `live-smoke` / `live-sandbox` specs that hit **real** OpenRouter. Costs credits. |
| `CHAT_SANDBOX_IMAGE` | e.g. `ghcr.io/elcanotek/sandbox:latest` — required (with `CHAT_E2E_LIVE=1`) for the live sandbox smoke. |
| `OPENROUTER_API_KEY` | required when `CHAT_E2E_LIVE=1`. |
| `APP_SESSION_SECRET` | HMAC secret for the password session cookie. |
| `AUTH_SIGNING_PUBKEY` | Ed25519 public key; when set, the "Use Elcano email" path renders. |
| `CHAT_SERVER_URL` / `CHAT_SERVER_TOKEN` | where the Next `/api/*` proxy reaches chat-server, and the shared secret. |
| `ORCHESTRATOR_SERVER_URL` | where the Next proxy reaches the orchestrator (moc). |
| `E2E_TEST_EMAIL` / `E2E_TEST_PASSWORD` | the seeded test user (defaults `e2e@example.com` / `e2e-test-password`). |

**CI never runs the live suite** — it would need secrets and a private sandbox
image, and OpenRouter calls aren't deterministic. CI runs only the mocked suite.
