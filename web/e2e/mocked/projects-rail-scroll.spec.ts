import { test, expect } from "@playwright/test";
import type { Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// The rail used to show only the five freshest projects; a sixth project, and
// every chat filed in it, vanished from the rail with nothing to scroll to.
// Every project now renders inside the Projects section's own scroller, which
// scrolls like the Chats list below it. All /api/* calls are intercepted.

const PROJECTS = Array.from({ length: 12 }, (_, i) => ({
  id: `p-${i}`,
  owner_email: "e2e@example.com",
  name: `Project ${String(i + 1).padStart(2, "0")}`,
  instructions: "",
  mcp_servers: [],
  created_at: 1_700_000_000 + i,
  updated_at: 1_700_000_500 + i,
}));

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the rail lists every project and the Projects section scrolls", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 720 });
  await page.route("**/api/projects", (r: Route) => r.fulfill({ json: { projects: PROJECTS } }));
  await mockChatBoot(page, {
    // The filed chat comes last so it is not the conversation the page opens.
    conversations: [
      ...Array.from({ length: 30 }, (_, i) => ({ id: `c${i}`, title: `Chat ${i + 1}` })),
      { id: "c-deep", title: "Filed in the oldest project", project_id: "p-0" },
    ],
  });
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  // Most recent first; the oldest (twelfth) project is rendered too.
  const newest = page.getByRole("button", { name: "Open project Project 12" });
  const oldest = page.getByRole("button", { name: "Open project Project 01" });
  await expect(newest).toBeVisible();
  await expect(oldest).toHaveCount(1);

  // The section is its own scroller: it overflows, and scrolling it brings the
  // oldest project into view while the Chats list keeps its place below.
  const scroller = page.locator("div.overflow-y-auto", { has: newest });
  const { clientHeight, scrollHeight } = await scroller.evaluate((el) => ({
    clientHeight: el.clientHeight,
    scrollHeight: el.scrollHeight,
  }));
  expect(scrollHeight).toBeGreaterThan(clientHeight);
  await oldest.scrollIntoViewIfNeeded();
  await expect(oldest).toBeInViewport();
  expect(await scroller.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
  await expect(page.getByRole("button", { name: "Chat 2", exact: true })).toBeInViewport();

  // A chat filed in the oldest project is reachable from the rail again:
  // expanding its project shows the chat's row.
  const deepRow = page.getByRole("button", { name: "Filed in the oldest project", exact: true });
  await expect(deepRow).toHaveCount(0);
  await page.getByRole("button", { name: /^Project Project 01/ }).click();
  await expect(deepRow).toBeVisible();
});
