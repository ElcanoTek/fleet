import { test as base, expect, request } from "@playwright/test";
import { TEST_EMAIL, TEST_PASSWORD } from "../playwright.config";

export { expect };

// Tests share a single chat-server process (one SQLite file). Without a
// clean slate between tests, conversations accumulate and state-sensitive
// tests (persona lock, pin ordering) flake.
//
// wipeUserConversations calls the real DELETE endpoint for every
// conversation on this user. Auth goes through the shared-secret header
// path, which requires the test process to know CHAT_SERVER_TOKEN (set by
// playwright.config in webServer.env, which is NOT visible here). The
// simplest cross-platform solution: do the cleanup _through_ the Next.js
// API, which requires a logged-in cookie. We call it after the login
// helper runs.

async function wipeUserConversationsViaCookies(page: import("@playwright/test").Page) {
  // The API now enforces same-origin via the Origin header (CSRF
  // defense). Playwright's page.request doesn't auto-set Origin, so
  // we do it explicitly from the page's URL.
  const origin = new URL(page.url()).origin;
  const headers = { Origin: origin };

  const resp = await page.request.get("/api/conversations", { headers });
  if (!resp.ok()) return;
  const { conversations } = (await resp.json()) as {
    conversations: Array<{ id: string }> | null;
  };
  for (const c of conversations ?? []) {
    await page.request.delete(`/api/conversations/${c.id}`, { headers });
  }
}

export const test = base.extend<{
  login: () => Promise<void>;
}>({
  login: async ({ page }, use) => {
    // `use` here is Playwright's fixture-setup callback, not the React
    // `use` hook. The linter can't tell them apart from the parameter
    // name alone, so we silence it locally.
    // eslint-disable-next-line react-hooks/rules-of-hooks
    await use(async () => {
      await page.goto("/login");
      await page.getByLabel(/email/i).fill(TEST_EMAIL);
      await page.getByLabel(/password/i).fill(TEST_PASSWORD);
      await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
      await page.waitForURL((url) => !url.pathname.startsWith("/login"), { timeout: 15_000 });
      // Each test starts with zero prior conversations for this user.
      await wipeUserConversationsViaCookies(page);
      // Reload so the sidebar reflects the emptied state, then wait for
      // the app shell to finish its initial data-load (session + personas
      // + conversations). The empty-state heading only renders after
      // isLoadingHistory flips to false.
      await page.goto("/");
      await page
        .getByRole("heading", { name: /what can i help with/i })
        .waitFor({ timeout: 10_000 });
    });
  },
});

// Re-export request for convenience in specs.
export { request };
