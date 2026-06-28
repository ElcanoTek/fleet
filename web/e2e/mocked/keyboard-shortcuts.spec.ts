import { test, expect } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for the keyboard-shortcut layer (#306): "?" opens the discoverable
// help overlay, it lists the wired shortcuts grouped by area, an inline filter
// narrows the list, and Escape closes it. The sidebar "?" button is the mouse
// equivalent. Every /api/* call is intercepted by mockChatBoot, so the suite is
// deterministic (no Go chat-server).

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test('"?" opens the shortcuts help overlay and Escape closes it', async ({ page }) => {
  await mockChatBoot(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  // Press "?" from the document body (no input focused) — the overlay appears.
  await page.keyboard.press("?");
  await expect(page.getByTestId("shortcuts-overlay")).toBeVisible();
  // It documents the global actions, grouped.
  await expect(page.getByTestId("shortcuts-list")).toContainText("New conversation");
  await expect(page.getByTestId("shortcuts-list")).toContainText("Open search");
  expect(await page.getByTestId("shortcut-row").count()).toBeGreaterThan(0);

  await page.keyboard.press("Escape");
  await expect(page.getByTestId("shortcuts-overlay")).toBeHidden();
});

test("the sidebar shortcuts button opens the overlay (mouse equivalent)", async ({ page }) => {
  await mockChatBoot(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  await page.getByTestId("shortcuts-button").click();
  await expect(page.getByTestId("shortcuts-overlay")).toBeVisible();

  // Inline filter narrows the list to matching rows.
  await page.getByTestId("shortcuts-filter").fill("newline");
  const rows = page.getByTestId("shortcut-row");
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("newline");

  // A non-matching filter shows the empty state.
  await page.getByTestId("shortcuts-filter").fill("zzz-nope");
  await expect(page.getByTestId("shortcuts-empty")).toBeVisible();

  // The explicit close button dismisses it.
  await page.getByTestId("shortcuts-close").click();
  await expect(page.getByTestId("shortcuts-overlay")).toBeHidden();
});

test('"?" typed into the composer does NOT open the overlay (no hijack while typing)', async ({
  page,
}) => {
  await mockChatBoot(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const composer = page.getByRole("textbox").first();
  await composer.click();
  await composer.type("why? ");
  // The "?" landed in the textarea as text; no overlay was triggered.
  await expect(composer).toHaveValue("why? ");
  await expect(page.getByTestId("shortcuts-overlay")).toBeHidden();
});
