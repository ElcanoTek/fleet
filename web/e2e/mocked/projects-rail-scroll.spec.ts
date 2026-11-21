import { test, expect } from "@playwright/test";
import type { Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// The rail used to show only the five freshest projects; a sixth project, and
// every chat filed in it, vanished from the rail with nothing to scroll to.
// The Projects section now lists the freshest few and says how many there
// are: "All N projects" opens the rest in place, and the sidebar scrolls as
// one (the section is not a scroll box of its own any more). All /api/* calls
// are intercepted.

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

test("the sidebar lists the freshest projects, All N projects shows the rest, and it scrolls as one", async ({ page }) => {
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

  // Most recent first: the eight freshest are listed, the rest are one press
  // away, and the count says they exist.
  const section = page.getByTestId("projects-section");
  const projects = section.getByRole("button", { name: /^Open project / });
  const newest = page.getByRole("button", { name: "Open project Project 12" });
  const oldest = page.getByRole("button", { name: "Open project Project 01" });
  await expect(newest).toBeVisible();
  await expect(projects).toHaveCount(8);
  await expect(oldest).toHaveCount(0);
  const showAll = page.getByTestId("projects-show-all");
  await expect(showAll).toHaveText("All 12 projects");
  await expect(showAll).toHaveAttribute("aria-expanded", "false");
  await showAll.click();
  await expect(projects).toHaveCount(12);
  await expect(showAll).toHaveText("Show fewer");
  await expect(showAll).toHaveAttribute("aria-expanded", "true");

  // One scroller for the whole sidebar: nothing inside it is a scroll box of
  // its own, so Projects and Chats move together.
  const body = page.getByTestId("rail-body");
  const nested = await body.evaluate((el) =>
    [...el.querySelectorAll<HTMLElement>("*")].filter((n) => {
      const o = getComputedStyle(n).overflowY;
      return (o === "auto" || o === "scroll") && n.scrollHeight > n.clientHeight + 1;
    }).length,
  );
  expect(nested).toBe(0);
  const { clientHeight, scrollHeight } = await body.evaluate((el) => ({
    clientHeight: el.clientHeight,
    scrollHeight: el.scrollHeight,
  }));
  expect(scrollHeight).toBeGreaterThan(clientHeight);
  await oldest.scrollIntoViewIfNeeded();
  await expect(oldest).toBeInViewport();
  const lastChat = page.getByRole("button", { name: "Chat 30", exact: true });
  await lastChat.scrollIntoViewIfNeeded();
  await expect(lastChat).toBeInViewport();
  expect(await body.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);

  // A chat filed in the oldest project is reachable from the rail again:
  // expanding its project shows the chat's row.
  const deepRow = page.getByRole("button", { name: "Filed in the oldest project", exact: true });
  await expect(deepRow).toHaveCount(0);
  await page.getByRole("button", { name: /^Project Project 01/ }).click();
  await expect(deepRow).toBeVisible();
});
