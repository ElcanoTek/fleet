import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { loginViaCookie } from "./_session";

// Mocked e2e for the Patrol Launchpad (docs/PATROL-LAUNCHPAD.md): the
// top-bar button opens the wizard on any surface (the Guides here), it walks
// to a launched patrol through the real bundle and Go gateway, and the dialog
// passes axe in both colour schemes.

type Call = { url: string; method: string; body: unknown };

async function mockShell(page: Page, calls: Call[], patrols: unknown[] = []) {
  const record = (r: Route) => {
    const req = r.request();
    let body: unknown = undefined;
    try {
      body = req.postDataJSON();
    } catch {
      body = undefined;
    }
    calls.push({ url: new URL(req.url()).pathname, method: req.method(), body });
  };
  await page.route("**/api/session", (r) => r.fulfill({ json: { email: "e2e@example.com" } }));
  await page.route("**/api/version", (r) => r.fulfill({ json: { build_id: "test" } }));
  await page.route("**/api/client-config", (r) => r.fulfill({ json: {} }));
  await page.route("**/api/push/vapid-public-key", (r) => r.fulfill({ status: 501, json: { error: "not configured" } }));
  await page.route("**/api/mcp-servers", (r) =>
    r.fulfill({
      json: {
        servers: [
          { name: "gmail", display_name: "Gmail", description: "Read and search mail", enabled: true, tools: [], tool_count: 3 },
          { name: "dealdesk", display_name: "Deal Desk", description: "PMP deal management", enabled: false, tools: [], tool_count: 5 },
          { name: "trade", display_name: "Trade Desk", description: "DSP campaigns", status: "needs_setup", tools: [], tool_count: 0 },
        ],
      },
    }),
  );
  await page.route("**/api/memories", (r) => {
    if (r.request().method() === "POST") {
      record(r);
      return r.fulfill({ json: { id: "m1", content: "x" } });
    }
    return r.fulfill({ json: { memories: [{ id: "m0", content: "Report money in EUR." }] } });
  });
  await page.route("**/api/memories/*", (r) => {
    record(r);
    return r.fulfill({ json: { id: "m1" } });
  });
  await page.route("**/api/patrols", (r) => r.fulfill({ json: { patrols } }));
  await page.route("**/api/me", (r) => r.fulfill({ json: { email: "e2e@example.com", role: "member" } }));
  await page.route("**/api/personas", (r) =>
    r.fulfill({ json: { personas: ["assistant", "trader"], default: "assistant", user_personas: [] } }),
  );
  await page.route("**/api/conversations", (r) => {
    record(r);
    return r.fulfill({ json: { id: "conv-9" } });
  });
  await page.route("**/api/conversations/conv-9/**", (r) => {
    record(r);
    const path = new URL(r.request().url()).pathname;
    if (path.endsWith("/schedules")) return r.fulfill({ json: { schedule: { id: "s-1" } } });
    return r.fulfill({ json: {} });
  });
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the Launchpad walks from nothing to a launched patrol", async ({ page }) => {
  const calls: Call[] = [];
  await mockShell(page, calls);
  await page.goto("/help");

  const button = page.getByTestId("launchpad-button");
  await expect(button).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("launchpad-new-dot")).toBeVisible();
  await button.click();

  const dialog = page.getByRole("dialog", { name: "Patrol Launchpad" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByTestId("launchpad-mind")).toBeVisible();
  await dialog.getByTestId("launchpad-next").click();
  await expect(dialog.getByText("2 tools ready")).toBeVisible();
  await dialog.getByTestId("launchpad-next").click();

  await expect(dialog.getByTestId("launchpad-tool-gmail")).toBeChecked();
  await dialog.getByTestId("launchpad-tool-dealdesk").check();
  await dialog.getByTestId("launchpad-next").click();

  await dialog.getByRole("button", { name: /Never send an email/ }).click();
  await dialog.getByTestId("launchpad-memory-add").click();
  await expect.poll(() => calls.filter((c) => c.url === "/api/memories/m1").length).toBe(1);
  await dialog.getByTestId("launchpad-next").click();

  await dialog.getByTestId("launchpad-goal-deals").click();
  await dialog.getByTestId("launchpad-goal-spend").click();
  await dialog.getByTestId("launchpad-next").click();

  await dialog.getByTestId("launchpad-cadence-twice-daily").click();
  await dialog.getByTestId("launchpad-next").click();

  await expect(dialog.getByTestId("launchpad-title")).toHaveValue("Deals & spend patrol");
  await dialog.getByTestId("launchpad-launch").click();
  await expect(dialog.getByTestId("launchpad-done")).toBeVisible();
  await expect(dialog.getByText(/Next rung of the ladder/)).toBeVisible();

  const writes = calls.filter((c) => c.method === "POST" && !c.url.startsWith("/api/memories")).map((c) => c.url);
  expect(writes).toEqual([
    "/api/conversations",
    "/api/conversations/conv-9/mcp-servers",
    "/api/conversations/conv-9/schedules",
    "/api/conversations/conv-9/schedules/s-1/run-now",
  ]);
  const sched = calls.find((c) => c.url.endsWith("/schedules"))?.body as Record<string, unknown>;
  expect(sched).toMatchObject({ cron: "0 9,16 * * 1-5", auto_approve: false });
});

test("?launchpad=1 opens the Launchpad directly", async ({ page }) => {
  await mockShell(page, []);
  await page.goto("/help?launchpad=1");
  await expect(page.getByRole("dialog", { name: "Patrol Launchpad" })).toBeVisible({ timeout: 15_000 });
  await expect(page).toHaveURL(/\/help$/);
});

for (const scheme of ["light", "dark"] as const) {
  test.describe(`${scheme} theme`, () => {
    test.use({ colorScheme: scheme });

    test("the Launchpad has no axe violations", async ({ page }) => {
      await mockShell(page, []);
      await page.goto("/help?launchpad=1");
      const dialog = page.getByRole("dialog", { name: "Patrol Launchpad" });
      await expect(dialog.getByTestId("launchpad-mind")).toBeVisible({ timeout: 15_000 });
      const steps = ["why", "welcome", "tools", "rules", "watch", "schedule"];
      for (const step of steps) {
        await expect(dialog.getByTestId(`launchpad-step-${step}`)).toBeVisible();
        if (step === "watch") {
          await dialog.getByTestId("launchpad-goal-inbox").click();
          // Picking a goal enables Next, whose opacity then transitions from
          // the disabled 0.5 to 1. Axe measured mid-fade reads a contrast
          // failure no one ever sees at rest, so wait for the settled state.
          const nextButton = dialog.getByTestId("launchpad-next");
          await expect(nextButton).toBeEnabled();
          await expect
            .poll(() => nextButton.evaluate((el) => getComputedStyle(el).opacity))
            .toBe("1");
        }
        const results = await new AxeBuilder({ page })
          .include('[data-testid="patrol-launchpad"]')
          .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
          .analyze();
        const summary = results.violations.map(
          (v) => `${step}: ${v.id} (${v.impact}): ${v.help} — ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`,
        );
        expect(summary, summary.join("\n")).toEqual([]);
        await dialog.getByTestId("launchpad-next").click();
      }
      await expect(dialog.getByTestId("launchpad-step-launch")).toBeVisible();
      const results = await new AxeBuilder({ page })
        .include('[data-testid="patrol-launchpad"]')
        .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
        .analyze();
      expect(results.violations.map((v) => `launch: ${v.id}: ${v.help}`)).toEqual([]);
    });
  });
}

test("with patrols already running, Patrols lists them and offers New patrol", async ({ page }) => {
  await mockShell(page, [], [
    { id: "s1", conversation_id: "c1", title: "Deal sweep", conversation_title: "Deals chat", cron: "0 8 * * 1-5", status: "active" },
    { id: "s2", conversation_id: "c2", title: "Inbox triage", conversation_title: "Inbox triage", cron: "0 */2 * * *", status: "paused" },
  ]);
  await page.goto("/help");
  await page.getByTestId("launchpad-button").click();
  const menu = page.getByRole("menu", { name: "Patrols" });
  await expect(menu).toBeVisible({ timeout: 15_000 });
  await expect(menu.getByText("Each patrol is a chat")).toBeVisible();
  await expect(page.getByTestId("patrols-menu-item-s2")).toContainText("Paused");
  await expect(page.getByRole("dialog", { name: "Patrol Launchpad" })).toHaveCount(0);
  await page.getByTestId("patrols-menu-new").click();
  await expect(page.getByRole("dialog", { name: "Patrol Launchpad" })).toBeVisible();
});

test("a long Patrols menu stays inside the viewport and scrolls to New patrol", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 520 });
  const many = Array.from({ length: 40 }, (_, i) => ({
    id: `s${i}`, conversation_id: `c${i}`, title: `Patrol ${i}`, conversation_title: "x", cron: "0 8 * * *", status: "active",
  }));
  await mockShell(page, [], many);
  await page.goto("/help");
  await page.getByTestId("launchpad-button").click();
  const menu = page.getByRole("menu", { name: "Patrols" });
  await expect(menu).toBeVisible({ timeout: 15_000 });
  const box = await menu.boundingBox();
  expect(box && box.y >= 0 && box.y + box.height <= 520).toBe(true);
  const newPatrol = page.getByTestId("patrols-menu-new");
  await newPatrol.scrollIntoViewIfNeeded();
  await expect(newPatrol).toBeInViewport();
});

test("on a phone, closing the wizard puts the focus on the visible Open sidebar button", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await mockShell(page, []);
  await page.goto("/help");
  await page.getByRole("button", { name: "Open sidebar", exact: true }).click();
  await page.getByTestId("launchpad-button").click(); // no patrols: the wizard opens, the drawer closes
  const dialog = page.getByRole("dialog", { name: "Patrol Launchpad" });
  await expect(dialog).toBeVisible({ timeout: 15_000 });
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Open sidebar", exact: true })).toBeFocused();
});
