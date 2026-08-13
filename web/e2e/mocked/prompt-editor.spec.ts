import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for the task modal's prompt editor. This lane is the ONLY place
// the behaviour is observable: jsdom reports scrollHeight 0 and computes no
// layout, so a unit test can prove the sizing effect RAN but never that it sized
// anything. The regression it guards is a real report — opening Edit on a long
// protocol prompt showed a three-row box, because auto-grow only ever ran from
// the textarea's onChange and a prefill fires no change event.

const LONG_PROMPT = [
  "raptive_historical_data.csv — Daily-level data containing at minimum: Date (daily), Revenue ($), Margin Share ($), and Impressions.",
  ...Array.from({ length: 60 }, (_, i) => `section ${i + 1}: do the thing carefully and report the result`),
].join("\n");

const TASK = {
  id: "11111111-1111-1111-1111-111111111111",
  title: "Raptive executive margin brief",
  prompt: LONG_PROMPT,
  status: "success",
  created_at: new Date().toISOString(),
  agent_session_id: "sess-1",
};

async function mockOrchestrator(page: Page) {
  // Registered FIRST so the specific handler below wins — Playwright matches
  // routes in reverse registration order.
  await page.route("**/api/**", (route: Route) => route.fulfill({ json: {} }));
  await page.route("**/api/orchestrator/**", async (route: Route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/orchestrator", "");
    if (path === "/me") return route.fulfill({ json: { authenticated: true, username: "e2e" } });
    if (path === "/stats")
      return route.fulfill({
        json: { pending_tasks: 0, running_tasks: 0, completed_tasks_today: 1, failed_tasks_today: 0 },
      });
    if (path === "/mcp-servers") return route.fulfill({ json: { servers: [] } });
    if (path === "/config") return route.fulfill({ json: { timezone: "America/New_York" } });
    if (path === "/tasks")
      return route.fulfill({ json: { data: [TASK], total: 1, limit: 20, offset: 0 } });
    return route.fulfill({ json: {} });
  });
}

async function openEditModal(page: Page) {
  await page.goto("/orchestrator");
  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();
  await page.getByTestId("task-edit-button").first().click();
  const prompt = page.getByLabel("Prompt", { exact: true });
  await expect(prompt).toBeVisible();
  return prompt;
}

test("a prefilled long prompt opens grown, not in a three-row box", async ({ page, context }) => {
  await loginViaCookie(context);
  await mockOrchestrator(page);
  const prompt = await openEditModal(page);

  // The old behaviour was the 4.875rem min-height (~78px, three rows). The
  // auto-grow ceiling is PROMPT_AUTOGROW_MAX_PX (240), which a 60-line prompt
  // reaches.
  const height = await prompt.evaluate((el) => el.getBoundingClientRect().height);
  expect(height).toBeGreaterThan(200);
});

test("the prompt field can be dragged taller, and a dragged height survives typing", async ({
  page,
  context,
}) => {
  await loginViaCookie(context);
  await mockOrchestrator(page);
  const prompt = await openEditModal(page);

  // The native grip is what makes it draggable at all (it was `resize: none`).
  expect(await prompt.evaluate((el) => getComputedStyle(el).resize)).toBe("vertical");

  // Simulate the drag's effect, then type: auto-grow must yield to the operator
  // rather than snapping their chosen height back to the content height.
  await prompt.evaluate((el) => {
    el.style.height = "420px";
    el.dispatchEvent(new Event("input", { bubbles: true }));
  });
  await prompt.focus();
  await page.keyboard.type("xyz");
  await expect
    .poll(() => prompt.evaluate((el) => el.getBoundingClientRect().height))
    .toBeGreaterThan(400);
});

test("Expand gives a tall editing pane and Collapse returns to auto-grow", async ({
  page,
  context,
}) => {
  await loginViaCookie(context);
  await mockOrchestrator(page);
  const prompt = await openEditModal(page);
  const grown = await prompt.evaluate((el) => el.getBoundingClientRect().height);

  const toggle = page.getByTestId("prompt-expand-toggle");
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-pressed", "true");
  const expanded = await prompt.evaluate((el) => el.getBoundingClientRect().height);
  expect(expanded).toBeGreaterThan(grown);

  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-pressed", "false");
  await expect
    .poll(() => prompt.evaluate((el) => el.getBoundingClientRect().height))
    .toBeLessThan(expanded);
});
