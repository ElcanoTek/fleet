import { test, expect } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// The user guides, reached the way a user reaches them: from the rail, mid-task,
// without leaving the app. The guides are plain Markdown rendered by a server
// component, so this spec is really asserting three joins — the rail link, the
// route, and that the Markdown on disk actually made it onto the page.

test("a signed-in user can open the guides from the rail and read a guide", async ({
  page,
  context,
}) => {
  await loginViaCookie(context);
  await mockChatBoot(page);

  page.on("framenavigated", (frame) => {
    if (frame === page.mainFrame() && new URL(frame.url()).pathname === "/login") {
      throw new Error("session bounced to /login mid-navigation");
    }
  });

  await page.goto("/chat");
  await page.getByTestId("nav-to-help").click();

  // The index: both guides, as cards.
  await expect(page.getByTestId("guide-card-chat")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("guide-card-operations-center")).toBeVisible();

  // Into the Operations Center guide — the run-state table is the page's
  // reason for existing, so assert the content, not just the heading.
  await page.getByTestId("guide-card-operations-center").click();
  await expect(page.getByRole("heading", { name: "4. Run states" })).toBeVisible({
    timeout: 15_000,
  });
  await expect(page.getByTestId("guide-prose").getByText("DEAD_LETTERED").first()).toBeVisible();

  // The contents rail jumps within the guide.
  await page.getByRole("link", { name: "7. Working conventions" }).click();
  expect(new URL(page.url()).hash).toBe("#7-working-conventions");

  // And the rail still crosses back to chat from here.
  await page.getByTestId("nav-to-chat").click();
  await page.waitForURL("**/chat");
});

// A slug outside the GUIDES table must 404 rather than reach the page — the
// guides are read from disk by filename, so "no request-supplied string ever
// becomes a path" is worth holding to rather than trusting.
test("an unknown guide slug 404s", async ({ page, context }) => {
  await loginViaCookie(context);
  await mockChatBoot(page);
  const response = await page.goto("/help/not-a-guide");
  expect(response?.status()).toBe(404);
});
