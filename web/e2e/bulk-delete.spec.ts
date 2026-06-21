import { test, expect } from "./fixtures";

// "Delete all unpinned" bulk action. Creates two conversations, pins one,
// fires the bulk-delete flow, and confirms that only the pinned one remains.

test.describe("bulk delete", () => {
  test("Delete all unpinned leaves pinned conversations untouched", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);

    // Conversation 1 — will be pinned.
    await composer.fill("keep me");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*keep me/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Conversation 2 — will be deleted.
    await page.getByRole("button", { name: /new chat/i }).click();
    await composer.fill("delete me");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*delete me/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Pin "keep me".
    const sidebar = page.locator("aside").first();
    const pinRow = sidebar
      .locator("div.group")
      .filter({ hasText: "keep me" })
      .first();
    await pinRow.hover();
    await pinRow.getByRole("button", { name: /^Pin keep me$/ }).click({ force: true });
    await expect(pinRow.getByRole("button", { name: /^Unpin keep me$/ })).toBeVisible();

    // Click the sidebar's "Delete all unpinned" button → confirm.
    await page.getByRole("button", { name: /delete all unpinned/i }).click();
    await page.getByRole("button", { name: /^Delete all$/ }).click();

    // "delete me" is gone, "keep me" remains.
    await expect(sidebar.getByText("delete me")).toHaveCount(0, { timeout: 10_000 });
    await expect(sidebar.getByText("keep me")).toBeVisible();
  });
});
