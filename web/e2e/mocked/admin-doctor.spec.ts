import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for Settings → Admin → Doctor: the box-health report renders
// from GET /api/admin/doctor, failing checks show their on-box fix command,
// and the deep run happens only via the explicit button (forwarding ?deep=1).

const QUICK_REPORT = {
  generated_at: "2026-07-22T12:00:00Z",
  duration_ms: 850,
  deep: false,
  healthy: false,
  summary: { ok: 7, warn: 1, fail: 1, skip: 2 },
  checks: [
    { name: "chat database", status: "ok", detail: "reachable via the server pool" },
    { name: "sched database", status: "ok", detail: "reachable" },
    { name: "sandbox image", status: "ok", detail: "localhost/fleet-sandbox:latest present (deep run skipped)" },
    {
      name: "subuid range",
      status: "fail",
      detail: "/etc/subuid has no range for fleet — rootless podman cannot map the userns",
      fix: "run on the box: sudo fleet doctor",
    },
    { name: "disk: data dir", status: "warn", detail: "/var/lib/fleet: 87.0% used, 6.1 GiB free", fix: "consider: sudo fleet cleanup" },
    { name: "unit caddy", status: "skip", detail: "caddy.service not installed (optional tier)" },
  ],
};

const DEEP_REPORT = {
  ...QUICK_REPORT,
  deep: true,
  healthy: true,
  duration_ms: 41000,
  summary: { ok: 9, warn: 0, fail: 0, skip: 1 },
  checks: [
    { name: "chat database", status: "ok", detail: "reachable via the server pool" },
    { name: "sandbox image", status: "ok", detail: "localhost/fleet-sandbox:latest present + runnable (deep smoke passed)" },
  ],
};

async function mockAdmin(page: Page) {
  await page.route("**/api/session", (r: Route) => r.fulfill({ json: { email: "admin@example.com" } }));
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { build_id: "test" } }));
  await page.route("**/api/client-config", (r: Route) => r.fulfill({ json: {} }));
  // The settings sub-nav probes /api/admin/settings to decide Admin visibility.
  await page.route("**/api/admin/settings", (r: Route) => r.fulfill({ json: { settings: [] } }));
  // One matcher for both modes: the deep run is the same endpoint with ?deep=1.
  await page.route("**/api/admin/doctor**", (r: Route) => {
    const deep = r.request().url().includes("deep=1");
    return r.fulfill({ json: deep ? DEEP_REPORT : QUICK_REPORT });
  });
  // The Overview page (used by the sub-nav test) fetches these on load.
  await page.route("**/api/admin/stats", (r: Route) => r.fulfill({ json: { users: [] } }));
  await page.route("**/api/admin/health-summary", (r: Route) =>
    r.fulfill({
      json: {
        fleet_version: "test-9.9.9",
        uptime_seconds: 60,
        db: { chat: "healthy", pool_size: 1, in_use: 0, idle: 1 },
        workers: null,
        llm: { calls_today: 0, cost_today_usd: 0, avg_cost_per_call: 0 },
        mcp_servers: [],
        conversations_active: 0,
        sandbox_pool: null,
        memory_mb: 64,
        goroutines: 10,
      },
    }),
  );
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the Doctor panel renders box checks with fix commands for failures", async ({ page }) => {
  await mockAdmin(page);
  await page.goto("/settings/admin/doctor");

  const panel = page.getByTestId("doctor-panel");
  await expect(panel).toBeVisible({ timeout: 15_000 });

  await expect(panel).toContainText("chat database");
  await expect(panel).toContainText("subuid range");
  await expect(panel).toContainText("run on the box: sudo fleet doctor");
  await expect(page.getByTestId("doctor-attention")).toContainText("1 check(s) failing");
  await expect(page.getByText("1 failing")).toBeVisible();
});

test("the deep run goes through the explicit button and reports the smoke", async ({ page }) => {
  await mockAdmin(page);
  await page.goto("/settings/admin/doctor");
  await expect(page.getByTestId("doctor-panel")).toBeVisible({ timeout: 15_000 });

  await page.getByTestId("doctor-run-deep").click();
  await expect(page.getByTestId("doctor-panel")).toContainText("deep smoke passed", { timeout: 15_000 });
  await expect(page.getByText("healthy", { exact: true })).toBeVisible();
});

test("Doctor appears in the settings admin sub-nav", async ({ page }) => {
  await mockAdmin(page);
  await page.goto("/settings/admin");
  await expect(page.getByTestId("setnav-admin")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByRole("link", { name: "Doctor" })).toBeVisible();
});
