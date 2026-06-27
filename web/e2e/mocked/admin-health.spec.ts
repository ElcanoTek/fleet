import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for the admin health dashboard (#301): /admin renders the system
// health panel from GET /api/admin/health-summary. Both admin endpoints are
// intercepted so the suite is deterministic.

async function mockAdmin(page: Page) {
  await page.route("**/api/session", (r: Route) => r.fulfill({ json: { email: "admin@example.com" } }));
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { build_id: "test" } }));
  await page.route("**/api/admin/stats", (r: Route) => r.fulfill({ json: { users: [] } }));
  await page.route("**/api/admin/health-summary", (r: Route) =>
    r.fulfill({
      json: {
        fleet_version: "test-9.9.9",
        uptime_seconds: 3661,
        db: { chat: "healthy", pool_size: 5, in_use: 1, idle: 4 },
        workers: {
          total: 8,
          active: 2,
          idle: 6,
          queued_tasks: 3,
          running_tasks: 2,
          completed_today: 41,
          failed_today: 1,
        },
        llm: { calls_today: 120, cost_today_usd: 4.5, avg_cost_per_call: 0.0375 },
        mcp_servers: [{ name: "email", enabled: true }],
        conversations_active: 2,
        sandbox_pool: { size: 3, available: 1 },
        memory_mb: 256,
        goroutines: 73,
      },
    }),
  );
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the admin health panel renders live system metrics", async ({ page }) => {
  await mockAdmin(page);
  await page.goto("/admin");

  const panel = page.getByTestId("health-panel");
  await expect(panel).toBeVisible({ timeout: 15_000 });
  await expect(panel).toContainText("test-9.9.9"); // version
  await expect(panel).toContainText("$4.50"); // LLM spend today
  await expect(panel).toContainText("chat DB healthy"); // DB status pill
  await expect(panel).toContainText("256 MB"); // runtime memory
  await expect(panel).toContainText("email"); // MCP server pill
});
