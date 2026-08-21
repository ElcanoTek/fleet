import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for the admin Overview (#301, settings redesign): the system
// health panel lives at /settings/admin (old /admin redirects there) and
// renders from GET /api/admin/health-summary. Every admin endpoint the page
// (and the settings shell's admin probe) touches is intercepted so the suite
// is deterministic.

async function mockAdmin(page: Page) {
  await page.route("**/api/session", (r: Route) => r.fulfill({ json: { email: "admin@example.com" } }));
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { build_id: "test" } }));
  await page.route("**/api/client-config", (r: Route) => r.fulfill({ json: {} }));
  // The settings sub-nav probes /api/admin/settings to decide Admin
  // visibility — 200 marks this session as an admin.
  await page.route("**/api/admin/settings", (r: Route) => r.fulfill({ json: { settings: [] } }));
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
        disk: {
          available: true,
          path: "/var/lib/fleet",
          total_bytes: 107374182400,
          free_bytes: 64424509440,
          free_percent: 60,
          min_free_percent: 5,
          shedding: false,
        },
        memory_mb: 256,
        goroutines: 73,
      },
    }),
  );
  await page.route("**/api/admin/server-stats", (r: Route) =>
    r.fulfill({
      json: {
        available: true,
        sampled_at: "2026-07-14T12:00:00Z",
        hostname: "fleet-box",
        platform: "linux/amd64",
        uptime_seconds: 90061,
        cpu: { available: true, cores: 8, usage_percent: 21.5, load_1: 0.4, load_5: 0.3, load_15: 0.2 },
        memory: { available: true, total_bytes: 8589934592, used_bytes: 3221225472, available_bytes: 5368709120, swap_total_bytes: 0, swap_used_bytes: 0 },
        disk: { available: true, path: "/", total_bytes: 107374182400, used_bytes: 26843545600, available_bytes: 80530636800, usage_percent: 25 },
        network: { available: true, interfaces: 1, received_bytes: 10737418240, transmitted_bytes: 2147483648, receive_bytes_per_second: 1536, transmit_bytes_per_second: 512 },
        warnings: [],
      },
    }),
  );
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the admin Overview renders live system metrics inside the settings shell", async ({ page }) => {
  await mockAdmin(page);
  await page.goto("/settings/admin");

  const panel = page.getByTestId("health-panel");
  await expect(panel).toBeVisible({ timeout: 15_000 });

  // The refresh control sits in the System health header (not the topbar),
  // icon-only but reachable by its accessible name.
  await expect(page.getByRole("button", { name: "Refresh now" })).toBeVisible();
  await expect(panel).toContainText("test-9.9.9"); // version
  await expect(panel).toContainText("$4.50"); // LLM spend today
  await expect(page.getByText("chat DB healthy")).toBeVisible(); // DB status badge
  await expect(panel).toContainText("256 MB"); // runtime memory
  await expect(page.getByText("email", { exact: true })).toBeVisible(); // MCP catalog chip
});

test("the old /admin URL redirects into the settings area", async ({ page }) => {
  await mockAdmin(page);
  await page.goto("/admin");
  await page.waitForURL("**/settings/admin", { timeout: 15_000 });
  await expect(page.getByTestId("health-panel")).toBeVisible({ timeout: 15_000 });
});

test("the admin Server section shows host resources without SSH", async ({ page }) => {
  await mockAdmin(page);
  await page.goto("/settings/admin/server");

  const panel = page.getByTestId("server-stats-panel");
  await expect(panel).toBeVisible({ timeout: 15_000 });
  await expect(panel).toContainText("21.5%");
  await expect(panel).toContainText("5.00 GB available");
  await expect(panel).toContainText("75.0 GB available");
  await expect(panel).toContainText("1.50 KB/s");
  await expect(page.getByRole("button", { name: "Refresh server stats" })).toBeVisible();
});
