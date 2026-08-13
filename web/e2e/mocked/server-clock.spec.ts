import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for the dashboard's server clock. The unit test proves the
// formatting and skew arithmetic; this proves the wiring — that the clock is
// actually mounted on the dashboard, reads GET /api/orchestrator/config through
// the real proxy route, and ticks.

// The mocked server sits in New York and is deliberately hours away from any
// plausible CI clock, so a render of LOCAL time could not pass by coincidence.
const SERVER_TIME = "2026-08-13T06:23:07-04:00";

async function mockOrchestrator(page: Page) {
  // Registered FIRST so the specific handler wins — Playwright matches routes
  // in reverse registration order.
  await page.route("**/api/**", (route: Route) => route.fulfill({ json: {} }));
  await page.route("**/api/orchestrator/**", async (route: Route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/orchestrator", "");
    if (path === "/me") return route.fulfill({ json: { authenticated: true, username: "e2e" } });
    if (path === "/stats")
      return route.fulfill({
        json: {
          pending_tasks: 2,
          running_tasks: 1,
          completed_tasks_today: 3,
          failed_tasks_today: 0,
          agent_slots: 4,
          active_agents: 1,
        },
      });
    if (path === "/mcp-servers") return route.fulfill({ json: { servers: [] } });
    if (path === "/config")
      return route.fulfill({
        json: {
          version: "test",
          timezone: "America/New_York",
          default_task_timezone: "America/New_York",
          server_time: SERVER_TIME,
        },
      });
    if (path === "/tasks")
      return route.fulfill({ json: { data: [], total: 0, limit: 20, offset: 0 } });
    return route.fulfill({ json: {} });
  });
}

test("the dashboard shows the server's clock, ticking, and names its zone", async ({
  page,
  context,
}) => {
  await loginViaCookie(context);
  await mockOrchestrator(page);
  await page.goto("/orchestrator");
  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();

  const clock = page.getByTestId("server-clock");
  await expect(clock).toBeVisible();
  await expect(clock).toContainText("6:23");
  // The zone is what makes the number actionable — a cron fires in it.
  await expect(clock).toHaveAttribute("title", /America\/New_York/);

  const first = await clock.textContent();
  await expect.poll(() => clock.textContent(), { timeout: 5000 }).not.toBe(first);
});

test("no clock is rendered when the server reports no time", async ({ page, context }) => {
  await loginViaCookie(context);
  await mockOrchestrator(page);
  // An older orchestrator predates server_time. Showing local time under a
  // "Server time" label would be worse than showing nothing.
  await page.route("**/api/orchestrator/config", (route: Route) =>
    route.fulfill({ json: { version: "test", timezone: "America/New_York" } }),
  );

  await page.goto("/orchestrator");
  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();
  await expect(page.getByTestId("stat-slots")).toBeVisible();
  await expect(page.getByTestId("server-clock")).toHaveCount(0);
});
