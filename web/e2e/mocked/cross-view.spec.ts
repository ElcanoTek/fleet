import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// The P7 cross-view gate: ONE session (the elcano/password cookie the unified
// middleware accepts) navigates Chat ↔ Orchestrator without re-login. We assert
// neither view ever bounces to /login, proving the single middleware gates both
// segments off the same cookie.

async function mockBoth(page: Page) {
  // Chat shell mount.
  await page.route("**/api/session", (r: Route) => r.fulfill({ json: { email: "e2e@example.com" } }));
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { build_id: "test" } }));
  await page.route("**/api/personas", (r: Route) =>
    r.fulfill({ json: { personas: [{ id: "default", name: "Default" }], default: "default" } }),
  );
  await page.route("**/api/server-config", (r: Route) =>
    r.fulfill({
      json: { lockdown_available: false, lockdown_only: false, lockdown_allowed_models: [] },
    }),
  );
  await page.route("**/api/mcp-servers", (r: Route) => r.fulfill({ json: { servers: [] } }));
  await page.route("**/api/conversations", (r: Route) => r.fulfill({ json: { conversations: [] } }));
  await page.route("**/api/model-rankings", (r: Route) => r.fulfill({ json: { rankings: [] } }));
  await page.route("**/api/model-catalog", (r: Route) => r.fulfill({ json: { models: [] } }));

  // Orchestrator shell.
  await page.route("**/api/orchestrator/**", (r: Route) => {
    const path = new URL(r.request().url()).pathname.replace("/api/orchestrator", "");
    if (path === "/me") return r.fulfill({ json: { authenticated: true, username: "e2e" } });
    if (path === "/stats")
      return r.fulfill({
        json: { total_nodes: 0, active_nodes: 0, pending_tasks: 0, running_tasks: 0 },
      });
    if (path === "/nodes") return r.fulfill({ json: { data: [], total: 0, limit: 100, offset: 0 } });
    if (path === "/tasks") return r.fulfill({ json: { data: [], total: 0, limit: 20, offset: 0 } });
    if (path === "/mcp-servers") return r.fulfill({ json: { servers: [] } });
    if (path === "/config") return r.fulfill({ json: { timezone: "UTC" } });
    return r.fulfill({ json: {} });
  });
}

test("one session crosses Chat ↔ Orchestrator without re-login", async ({ page, context }) => {
  await loginViaCookie(context);
  await mockBoth(page);

  // Start in chat.
  await page.goto("/chat");
  await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible({
    timeout: 15_000,
  });
  expect(new URL(page.url()).pathname).toBe("/chat");

  // Cross to the orchestrator — same cookie, no login prompt.
  await page.goto("/orchestrator");
  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible({ timeout: 15_000 });
  expect(new URL(page.url()).pathname).toBe("/orchestrator");

  // Use the in-app link back to chat (header "Go to Chat").
  await page.getByTestId("nav-to-chat").click();
  await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible({
    timeout: 15_000,
  });
  expect(new URL(page.url()).pathname).toBe("/chat");
});
