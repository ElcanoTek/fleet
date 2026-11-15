import { test, expect } from "@playwright/test";

// Switching Operations Center tabs while scrolled (phones especially) used to
// clamp the scroller to the very top: the incoming panel's brief loading
// state is much shorter than the outgoing content, so the container height
// collapsed under the scroll position. switchTab now pins the tab row to the
// top of the scroller after the swap, and the mobile tab-panel floor keeps
// the container tall enough through loading for the pin to hold.
import { loginViaCookie } from "./_session";
import type { Route } from "@playwright/test";

test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

test("switching tabs on mobile keeps the scroll position", async ({ browser }) => {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 } });
  await loginViaCookie(context);
  const page = await context.newPage();
  await page.route("**/api/orchestrator/**", async (route: Route) => {
    const url = new URL(route.request().url());
    const path = url.pathname.replace("/api/orchestrator", "");
    if (path === "/me") return route.fulfill({ json: { authenticated: true, username: "e2e" } });
    if (path === "/stats")
      return route.fulfill({ json: { pending_tasks: 1, running_tasks: 0, completed_tasks_today: 2, failed_tasks_today: 0, active_agents: 0, agent_slots: 8 } });
    if (path === "/tasks")
      return route.fulfill({
        json: {
          data: Array.from({ length: 12 }, (_, i) => ({
            id: `${String(i).padStart(8, "0")}-1111-1111-1111-111111111111`,
            prompt: `Task number ${i}`,
            status: "success",
            created_at: new Date("2026-07-14T10:00:00").toISOString(),
          })),
          total: 12, limit: 20, offset: 0,
        },
      });
    if (path.startsWith("/tasks/upcoming")) {
      // slow response: the moment the old content is gone is when the page
      // used to collapse and clamp the scroll to the top
      await new Promise((r) => setTimeout(r, 600));
      return route.fulfill({ json: { upcoming: [] } });
    }
    return route.fulfill({ json: { data: [] } });
  });
  await page.goto("/orchestrator");
  await page.waitForSelector(".task-card");
  // Scroll until the tab row sits 100px below the scroller's top edge: still
  // on screen to tap, clearly not at the page top. A fixed 500px only worked
  // while the stat cards above the tabs were taller than that — with the
  // compact phone cards the row scrolled off screen, the click's own
  // scroll-into-view returned the page to the top, and there was nothing left
  // to pin (switchTab leaves a scroller that is already at the top alone).
  await page.evaluate(() => {
    const el = document.querySelector("main .overflow-y-auto") as HTMLElement;
    const bar = document.querySelector(".dashboard-tabs") as HTMLElement;
    el.scrollTop = bar.getBoundingClientRect().top - el.getBoundingClientRect().top - 100;
  });
  expect(
    await page.evaluate(() => (document.querySelector("main .overflow-y-auto") as HTMLElement).scrollTop),
  ).toBeGreaterThan(100);
  await page.waitForTimeout(100);
  await page.getByRole("tab", { name: "Upcoming" }).click();
  const barDelta = () =>
    page.evaluate(() => {
      const el = document.querySelector("main .overflow-y-auto") as HTMLElement;
      const bar = document.querySelector(".dashboard-tabs") as HTMLElement;
      return bar.getBoundingClientRect().top - el.getBoundingClientRect().top;
    });
  // The tab bar gets pinned to (or very near) the top of the scroller. Poll
  // rather than sleep — under a loaded worker pool the rAF pin can land a few
  // frames later. On the pre-fix code this never converges: the scroller
  // clamps to the page top and the bar stays where the page top puts it.
  await expect.poll(barDelta, { timeout: 3000 }).toBeLessThan(60);
  await expect.poll(barDelta, { timeout: 3000 }).toBeGreaterThan(-60);
  // …and it still holds after the new tab's data lands.
  await page.waitForTimeout(800);
  expect(Math.abs(await barDelta())).toBeLessThan(60);
  await context.close();
});
