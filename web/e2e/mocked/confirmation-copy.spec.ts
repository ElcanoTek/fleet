import { test, expect } from "@playwright/test";
import type { Locator, Page } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

const teamProject = {
  id: "p-team", owner_email: "e2e@example.com", name: "Research",
  team_id: "Platform", instructions: "", mcp_servers: [],
  created_at: 1_700_000_000, updated_at: 1_700_000_500,
};
const personalProject = { ...teamProject, id: "p-private", name: "Drafts", team_id: "" };

async function boot(page: Page) {
  await mockChatBoot(page, { conversations: [
    { id: "c-shared", title: "Shared findings", project_id: teamProject.id, team_visible: true },
  ] });
  await page.route("**/api/conversations/*/queue", (route) => route.fulfill({ json: { messages: [] } }));
  await page.route("**/api/me/team", (route) => route.fulfill({ json: { team_id: "Platform", email: "e2e@example.com" } }));
  await page.route("**/api/projects", (route) => route.fulfill({ json: { projects: [teamProject, personalProject] } }));
  await page.route("**/api/projects/p-team/conversations", (route) => route.fulfill({ json: { conversations: [] } }));
  await page.route("**/api/projects/p-team/team-conversations", (route) => route.fulfill({ json: { conversations: [] } }));
  await page.route("**/api/projects/p-team/files", (route) => route.fulfill({ json: { files: [], truncated: false } }));
  await page.route("**/api/projects/p-team/memories", (route) => route.fulfill({ json: { memories: [] } }));
  await page.route("**/api/projects/p-team/impact", (route) => route.fulfill({ json: { chats_from_teammates: 2 } }));
  await page.route("**/api/projects/p-team/members", (route) => route.fulfill({ json: { members: ["e2e@example.com", "alex@example.com"] } }));
  await page.goto("/chat");
  await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible();
  const openSidebar = page.getByRole("button", { name: "Open sidebar", exact: true });
  if (await openSidebar.isVisible()) await openSidebar.click();
}

// Real layout is essential: jsdom cannot catch grid blockifying an inline chip
// or leaving a comma/question mark on a separate anonymous grid row.
async function expectInlineChips(dialog: Locator, count: number) {
  const chips = dialog.locator("[data-name-chip]");
  await expect(chips).toHaveCount(count);
  const measurements = await chips.evaluateAll((elements) => elements.map((chip) => {
    const rect = chip.getBoundingClientRect();
    const style = getComputedStyle(chip);
    const wrapper = chip.parentElement!;
    const punctuation = chip.nextSibling;
    let suffixOnSameLine = true;
    if (punctuation?.nodeType === Node.TEXT_NODE && punctuation.textContent?.trim()) {
      const range = document.createRange();
      range.selectNodeContents(punctuation);
      const suffixRect = range.getBoundingClientRect();
      suffixOnSameLine = suffixRect.top < rect.bottom && suffixRect.bottom > rect.top && Math.abs(suffixRect.left - rect.right) < 5;
    }
    return {
      display: style.display, width: rect.width,
      border: Number.parseFloat(style.borderTopWidth), background: style.backgroundColor,
      nowrap: getComputedStyle(wrapper).whiteSpace,
      suffixOnSameLine,
      fits: rect.left >= 0 && rect.right <= window.innerWidth,
    };
  }));
  for (const chip of measurements) {
    expect(chip.display).toBe("inline-flex");
    expect(chip.width).toBeLessThan(225);
    expect(chip.border).toBeGreaterThan(0);
    expect(chip.background).not.toBe("rgba(0, 0, 0, 0)");
    expect(chip.nowrap).toBe("nowrap");
    expect(chip.suffixOnSameLine).toBe(true);
    expect(chip.fits).toBe(true);
  }
  const copy = dialog.locator("#move-chat-confirm-body, #rail-delete-project-body");
  if (await copy.count()) {
    await expect(copy).toHaveCSS("display", "block");
    // Sentence copy should stay compact even at phone widths.
    expect((await copy.boundingBox())!.height).toBeLessThan(190);
  }
}

for (const width of [1280, 390]) {
  test.describe(`confirmation copy at ${width}px`, () => {
    // Enter through the same desktop controls; resize the open dialog to
    // isolate responsive confirmation layout from the mobile navigation.
    test.use({ viewport: { width: 1280, height: 900 } });
    test.beforeEach(async ({ context, page }) => {
      await loginViaCookie(context);
      await boot(page);
    });
    for (const action of ["move", "remove", "delete", "transfer", "unshare"] as const) {
      test(`${action} keeps identity chips inline with punctuation`, async ({ page }) => {
        let dialog: Locator;
        let chipCount = 2;
        if (action === "move" || action === "remove") {
          await page.getByRole("button", { name: "Project Research (1 chats)", exact: true }).click();
          await page.getByRole("button", { name: "Conversation options for Shared findings" }).click();
          await page.getByRole("menuitem", { name: "Move to project", exact: true }).click();
          await page.getByRole("menuitem", { name: action === "move" ? "Drafts" : "Remove from project", exact: true }).click();
          dialog = page.getByTestId("move-chat-confirm");
          if (action === "remove") chipCount = 1;
        } else {
          await page.getByRole("button", { name: "Project options for Research" }).click();
          if (action === "delete") {
            await page.getByRole("menuitem", { name: "Delete project", exact: true }).click();
            dialog = page.getByTestId("rail-delete-project-confirm");
            chipCount = 1;
          } else {
            await page.getByRole("menuitem", { name: "Project settings…" }).click();
            const settings = page.getByRole("dialog", { name: "Settings for Research" });
            if (action === "transfer") {
              await settings.getByRole("button", { name: "Transfer ownership…" }).click();
              await settings.getByLabel("Transfer ownership of Research").selectOption("alex@example.com");
              await settings.getByRole("button", { name: "Transfer", exact: true }).click();
              dialog = page.getByRole("dialog", { name: "Transfer Research to alex@example.com?", exact: true });
            } else {
              await settings.getByRole("checkbox", { name: /Share with my team/ }).click();
              dialog = page.getByRole("dialog", { name: "Stop sharing Research with Platform?", exact: true });
              await expect(dialog).toContainText("2 chats from teammates");
            }
          }
        }
        await expect(dialog).toBeVisible();
        await page.setViewportSize({ width, height: 900 });
        await expect(dialog).toHaveCSS("opacity", "1");
        await expectInlineChips(dialog, chipCount);
        await page.screenshot({ path: `/tmp/fleet-confirm-${action}-${width}.png`, animations: "disabled" });
        await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
        await expect(dialog).toBeHidden();
      });
    }
  });
}

test("project settings can be opened directly from the mobile sidebar", async ({ page, context }) => {
  await page.setViewportSize({ width: 390, height: 900 });
  await loginViaCookie(context);
  await boot(page);
  const sidebar = page.getByRole("complementary", { name: "Primary navigation" });
  // Wait for the drawer's entrance animation before targeting its controls;
  // scrolling a still-offscreen control into view changes the drawer itself.
  await expect.poll(async () => (await sidebar.boundingBox())?.x).toBe(0);
  await page.getByRole("button", { name: "Project options for Research" }).click();
  await page.getByRole("menuitem", { name: "Project settings…" }).click();
  await expect(page.getByRole("dialog", { name: "Settings for Research" })).toBeVisible();
});
