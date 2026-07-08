import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for the settings shell (fleet-unified settings redesign): every
// /settings/* page renders inside the unified NavRail + "Settings" topbar with
// the sticky sub-nav. Admin visibility is probed from /api/admin/settings —
// 200 shows the expandable Admin parent (five children), 403 hides it and
// bounces /settings/admin/* back to /settings.

async function mockShell(page: Page, opts: { admin: boolean }) {
  await page.route("**/api/session", (r: Route) =>
    r.fulfill({ json: { email: "e2e@example.com" } }),
  );
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { build_id: "test" } }));
  await page.route("**/api/client-config", (r: Route) => r.fulfill({ json: {} }));
  await page.route("**/api/push/vapid-public-key", (r: Route) =>
    r.fulfill({ status: 501, json: { error: "not configured" } }),
  );
  await page.route("**/api/admin/settings", (r: Route) =>
    opts.admin
      ? r.fulfill({ json: { settings: [] } })
      : r.fulfill({ status: 403, body: "forbidden — not an admin" }),
  );
  // The admin Overview's data endpoints (visited in the admin variant).
  await page.route("**/api/admin/stats", (r: Route) => r.fulfill({ json: { users: [] } }));
  await page.route("**/api/admin/health-summary", (r: Route) =>
    r.fulfill({
      json: {
        fleet_version: "test",
        uptime_seconds: 60,
        db: { chat: "healthy", pool_size: 5, in_use: 1, idle: 4 },
        workers: null,
        llm: { calls_today: 0, cost_today_usd: 0, avg_cost_per_call: 0 },
        mcp_servers: [],
        conversations_active: 0,
        sandbox_pool: null,
        memory_mb: 1,
        goroutines: 1,
      },
    }),
  );
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("settings render inside the unified shell with the section sub-nav", async ({ page }) => {
  await mockShell(page, { admin: true });
  await page.goto("/settings");

  // The unified chrome: rail + "Settings" topbar; no legacy pill buttons.
  await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByRole("link", { name: "Chat" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Back to chat" })).toHaveCount(0);

  const nav = page.getByRole("navigation", { name: "Settings sections" });
  await expect(nav.getByRole("link", { name: "General" })).toHaveAttribute("aria-current", "page");
  await expect(nav.getByRole("link", { name: "Connections" })).toBeVisible();
  await expect(nav.getByRole("link", { name: "Skills" })).toBeVisible();

  // General's preference rows.
  await expect(page.getByRole("group", { name: "Theme" })).toBeVisible();
  await expect(page.getByRole("switch", { name: "Send on Enter" })).toBeVisible();
  await expect(page.getByRole("switch", { name: "Browser notifications" })).toBeVisible();
});

test("the Admin parent expands to its five children for admins", async ({ page }) => {
  await mockShell(page, { admin: true });
  await page.goto("/settings");

  const nav = page.getByRole("navigation", { name: "Settings sections" });
  const admin = page.getByTestId("setnav-admin");
  await expect(admin).toBeVisible({ timeout: 15_000 });
  await expect(admin).toHaveAttribute("aria-expanded", "false");

  // Clicking Admin lands on Overview and reveals the children.
  await admin.click();
  await page.waitForURL("**/settings/admin");
  await expect(admin).toHaveAttribute("aria-expanded", "true");
  for (const child of ["Overview", "Users", "Features", "Providers", "Notifications"]) {
    await expect(nav.getByRole("link", { name: child })).toBeVisible();
  }
  await expect(nav.getByRole("link", { name: "Overview" })).toHaveAttribute(
    "aria-current",
    "page",
  );
});

test("non-admins see no Admin section and admin routes bounce to /settings", async ({ page }) => {
  await mockShell(page, { admin: false });
  await page.goto("/settings");

  await expect(page.getByRole("switch", { name: "Send on Enter" })).toBeVisible({
    timeout: 15_000,
  });
  await expect(page.getByTestId("setnav-admin")).toHaveCount(0);

  await page.goto("/settings/admin");
  await page.waitForURL(/\/settings$/, { timeout: 15_000 });
});
