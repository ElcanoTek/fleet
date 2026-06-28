import { test as base, expect, request } from "@playwright/test";

export { expect, request };

// Live-suite fixtures. Unlike the mocked suite (e2e/mocked, which intercepts
// every /api/* call) and the older mock-mode live specs (e2e/*.spec.ts, which
// hit a CHAT_MOCK_MODE backend), these run against a FULLY REAL stack booted by
// scripts/e2e-boot-server.sh: real Postgres, real Go chat + orchestrator
// listeners, real SSE, the real scheduler + worker pool, and the real Podman
// sandbox. ONLY the LLM is stubbed — by the wire-compatible fake (cmd/fake-llm)
// that fleet reaches via OPENROUTER_BASE_URL. Specs drive deterministic model
// behaviour by embedding a "[[scenario:NAME]]" marker in the prompt.

const TEST_EMAIL = process.env.E2E_TEST_EMAIL ?? "e2e@example.com";
const TEST_PASSWORD = process.env.E2E_TEST_PASSWORD ?? "e2e-test-password";
const SCHED_USERNAME = process.env.E2E_SCHED_USERNAME ?? "e2e";

export const creds = { email: TEST_EMAIL, password: TEST_PASSWORD, schedUsername: SCHED_USERNAME };

// wipeConversations deletes every conversation for the logged-in user via the
// real DELETE endpoint, so each test starts from a clean slate (conversations
// share one chat DB across the suite). It must clear BOTH active and archived
// (#282) — archived conversations are hidden from the default list, so fetching
// only the active list would leak them across tests and skew archive counts.
// The API enforces same-origin CSRF, so we set Origin explicitly (page.request
// does not auto-set it).
async function wipeConversations(page: import("@playwright/test").Page) {
  const origin = new URL(page.url()).origin;
  const headers = { Origin: origin };
  for (const path of ["/api/conversations", "/api/conversations?archived=true"]) {
    const resp = await page.request.get(path, { headers });
    if (!resp.ok()) continue;
    const body = (await resp.json()) as { conversations: Array<{ id: string }> | null };
    for (const c of body.conversations ?? []) {
      await page.request.delete(`/api/conversations/${c.id}`, { headers });
    }
  }
}

export const test = base.extend<{
  // login performs a REAL password login (the chat/elcano_session cookie path),
  // which gates both /chat and /orchestrator off one middleware, then lands on
  // the empty chat composer.
  login: () => Promise<void>;
}>({
  // Reveal the per-turn "details" (tool chips + reasoning + stats) by default so
  // tool-output assertions don't depend on a click; specs can still toggle it.
  page: async ({ page }, use) => {
    await page.addInitScript(() => {
      try {
        window.localStorage.setItem("chat-show-stats", "1");
      } catch {
        /* storage may be unavailable pre-navigation; specs fall back to the toggle */
      }
    });
    // Playwright fixture-setup callback, not the React `use` hook.
    // eslint-disable-next-line react-hooks/rules-of-hooks
    await use(page);
  },

  login: async ({ page }, use) => {
    // eslint-disable-next-line react-hooks/rules-of-hooks
    await use(async () => {
      await page.goto("/login");
      await page.getByLabel(/email/i).fill(TEST_EMAIL);
      await page.getByLabel(/password/i).fill(TEST_PASSWORD);
      await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
      await page.waitForURL((url) => !url.pathname.startsWith("/login"), { timeout: 20_000 });
      await wipeConversations(page);
      await page.goto("/");
      await page
        .getByRole("heading", { name: /what can i help with/i })
        .waitFor({ timeout: 15_000 });
    });
  },
});
