# Fleet web — Playwright e2e

Browser-driven end-to-end tests for the unified Fleet frontend (the Next.js app
that hosts both the `/chat` and `/orchestrator` views behind one middleware).

There are three lanes, each a separate Playwright project selected by env in
`playwright.config.ts`:

| Lane | Dir | What runs | Backends | LLM | CI |
| --- | --- | --- | --- | --- | --- |
| **mocked** (default) | `e2e/mocked/` | the real Next app | every `/api/*` call is route-intercepted by Playwright | — | ✅ PR gate (fast lane) |
| **live** | `e2e/live/` | the *fully real* stack — Postgres, both Go listeners, SSE, scheduler/worker pool, rootless-Podman sandbox | **real** | faked by `cmd/fake-llm` over `OPENROUTER_BASE_URL` | ✅ PR gate (`e2e-live` job) |
| **canary** | `e2e/canary/` | the same real stack | **real** | **real** cheap OpenRouter model | ❌ nightly/manual, secret-gated, never a PR gate |

```bash
cd web
npm ci
npx playwright install --with-deps chromium

npm run test:e2e            # mocked (default project)
npm run test:e2e:mocked     # mocked, explicit
npm run test:e2e:live       # live — boots the real stack (E2E_LIVE=1, --project=live)
npm run test:e2e:canary     # canary — real model (E2E_CANARY=1; needs OPENROUTER_API_KEY)
```

Both real-stack lanes are booted by `scripts/e2e-boot-server.sh`, which
`playwright.config.ts` launches as the `webServer` in live/canary mode. See
[`live/README.md`](live/README.md) for the full live/canary contract (what is
real, the `[[scenario:NAME]]` / `[[echo:TEXT]]` fake-LLM markers, required env,
and determinism measures).

---

## Mocked suite (the fast CI lane)

Deterministic: it drives the *real* Next app but replaces every Go backend
(chat-server `:18080`, orchestrator `:18000`, OpenRouter) with Playwright route
interception. No database, no Podman, no API keys — reproducible on a bare
runner.

```bash
cd web

# Fast local loop: drives `next dev`.
npm run test:e2e:mocked

# CI parity: builds the app and drives `next start` (the shipped bundle).
E2E_PROD=1 npm run test:e2e:mocked
```

In CI (`process.env.CI`) the config uses `next start`; locally it uses `next dev`
unless `E2E_PROD=1` is set. The Next server boots with the throwaway test env
(`CHAT_MOCK_MODE=1`, a test `APP_SESSION_SECRET`, and a throwaway
`AUTH_SIGNING_PUBKEY` so the "Use Elcano email" path renders).

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
| `login.spec.ts` | unauthenticated `/chat` redirects to `/login`; `/orchestrator` is gated by the same middleware; the login card renders the password form and the "Use Elcano email" link; a verified password login lands on `/chat`; an invalid password stays on `/login`; the magic-link button bounces to the auth-service flow; a valid `elcano_auth` cookie authenticates the same as a password session. |
| `chat.spec.ts` | authenticated `/chat` reaches the empty composer; a sent turn streams text deltas + a final assistant message; the streamed `tool.call`/`tool.result` render in the execution trail; config-driven empty-state cards render from a stubbed `/api/client-config`. |
| `protocol-pills.spec.ts` | the neutral fallback pills render; the Summarize form templates its prompt and the mocked chat echoes it; the Draft form gates on its required field then sends the template. |
| `orchestrator.spec.ts` | the dashboard loads stats + the task list; a task is created via `<McpServerPicker>` with an `mcp_selection` and no `target_node_name`; opening a task renders its log viewer. |
| `cross-view.spec.ts` | one authenticated session navigates `/chat` ↔ `/orchestrator` via in-app links, never bouncing to `/login`. |

---

## Live & canary suites (real stack)

These boot the whole real stack and are documented in
[`live/README.md`](live/README.md). The live lane runs on every PR (`e2e-live`);
the canary is a nightly/manual drift detector that skips cleanly without an
`OPENROUTER_API_KEY` secret and is never a PR gate.
