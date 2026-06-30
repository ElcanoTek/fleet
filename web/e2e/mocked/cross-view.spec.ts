import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// The cross-view gate: ONE session (the elcano_session cookie the unified
// middleware accepts) navigates Chat ↔ Orchestrator — using the IN-APP links,
// not just direct gotos — without ever bouncing to /login. This proves the
// single middleware gates both segments off the same cookie.

async function mockOrchestratorShell(page: Page) {
  await page.route("**/api/orchestrator/**", (r: Route) => {
    const path = new URL(r.request().url()).pathname.replace("/api/orchestrator", "");
    if (path === "/me") return r.fulfill({ json: { authenticated: true, username: "e2e" } });
    if (path === "/stats")
      return r.fulfill({
        json: { pending_tasks: 0, running_tasks: 0 },
      });
    if (path === "/tasks") return r.fulfill({ json: { data: [], total: 0, limit: 20, offset: 0 } });
    if (path === "/mcp-servers") return r.fulfill({ json: { servers: [] } });
    if (path === "/config") return r.fulfill({ json: { timezone: "UTC" } });
    return r.fulfill({ json: {} });
  });
}

test("one session crosses Chat ↔ Orchestrator via in-app links without re-login", async ({
  page,
  context,
}) => {
  await loginViaCookie(context);
  await mockChatBoot(page);
  await mockOrchestratorShell(page);

  // Bounce-to-login guard: any navigation to /login during this test fails it.
  page.on("framenavigated", (frame) => {
    if (frame === page.mainFrame() && new URL(frame.url()).pathname === "/login") {
      throw new Error("session bounced to /login mid-navigation");
    }
  });

  // Start in chat.
  await page.goto("/chat");
  await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible({
    timeout: 15_000,
  });
  expect(new URL(page.url()).pathname).toBe("/chat");

  // Cross to the orchestrator via the in-app sidebar link — same cookie.
  await page.getByTestId("nav-to-orchestrator").click();
  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible({ timeout: 15_000 });
  expect(new URL(page.url()).pathname).toBe("/orchestrator");

  // …and back to chat via the orchestrator header's "Go to Chat" link.
  await page.getByTestId("nav-to-chat").click();
  await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible({
    timeout: 15_000,
  });
  expect(new URL(page.url()).pathname).toBe("/chat");
});
