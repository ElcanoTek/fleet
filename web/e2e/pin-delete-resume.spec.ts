import { test, expect } from "./fixtures";

// Tests that cover the conversation-management surface: pinning, deleting,
// and resuming a conversation on page reload.

test.describe("conversation management", () => {
  test("pin persists across reload and pinned conversations sort first", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    // Turn 1 — oldest conversation.
    await composer.fill("first chat");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*first chat/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Turn 2 — new conversation (newest by updated_at, so it's on top).
    await page.getByRole("button", { name: /new chat/i }).click();
    await composer.fill("second chat");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:.*second/i).first()).toBeVisible({
      timeout: 15_000,
    });

    const sidebar = page.locator("aside").first();
    // Wait for the sidebar to hydrate with both rows.
    await expect(sidebar.getByText("first chat")).toBeVisible({ timeout: 10_000 });
    await expect(sidebar.getByText("second chat")).toBeVisible();

    // Pin the older row. The pin button is opacity-0 until hovered.
    const firstRow = sidebar
      .locator("div.group")
      .filter({ hasText: "first chat" })
      .first();
    await firstRow.hover();
    const pinBtn = firstRow.getByRole("button", { name: /^Pin first chat$/ });
    // Wait for the pin POST before moving on, so we know persistence happened.
    const pinResponse = page.waitForResponse(
      (res) => /\/api\/conversations\/[^/]+\/pin$/.test(res.url()) && res.request().method() === "POST",
    );
    await pinBtn.click({ force: true });
    const pinResp = await pinResponse;
    expect(pinResp.status()).toBe(200);

    // UI toggle confirms the optimistic state rendered.
    await expect(
      firstRow.getByRole("button", { name: /^Unpin first chat$/ }),
    ).toBeVisible({ timeout: 5_000 });

    // Sanity-check the server: List must report first-chat as pinned.
    const convs = (await (
      await page.request.get("/api/conversations")
    ).json()) as { conversations: Array<{ title: string; pinned: boolean }> };
    const firstConv = convs.conversations.find((c) => c.title === "first chat");
    expect(firstConv?.pinned).toBe(true);

    // Reload and verify pinned sorts first.
    await page.reload();
    const reloadedSidebar = page.locator("aside").first();
    await expect(reloadedSidebar.getByText("first chat")).toBeVisible({ timeout: 10_000 });
    await expect(reloadedSidebar.getByText("second chat")).toBeVisible();

    const sidebarText = await reloadedSidebar.innerText();
    const firstIdx = sidebarText.indexOf("first chat");
    const secondIdx = sidebarText.indexOf("second chat");
    expect(firstIdx).toBeLessThan(secondIdx);
  });

  test("deleting a conversation removes it from the sidebar", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("doomed conversation");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:/i)).toBeVisible({ timeout: 15_000 });

    const row = page.locator("div.group", { hasText: "doomed conversation" }).first();
    await expect(row).toBeVisible();
    await row.hover();
    await row.getByRole("button", { name: /delete doomed/i }).click();

    // Confirm in the modal.
    await page.getByRole("button", { name: /^delete$/i }).click();

    await expect(page.getByText("doomed conversation")).toHaveCount(0, { timeout: 10_000 });
  });

  test("reloading mid-conversation resumes full history from chat-server", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("resume me");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*resume me/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Reload — the most-recent conversation auto-loads.
    await page.reload();

    // The assistant reply bubble must be visible (conversation history replayed).
    await expect(page.getByText(/Mock reply to:\s*resume me/i).first()).toBeVisible({
      timeout: 10_000,
    });

    // Completed turns hide execution details until the footer toggle is opened.
    await page.getByRole("button", { name: /show details/i }).first().click();

    // And the tool chip from the earlier turn should re-render from history.
    await expect(page.getByRole("button", { name: /run_python/i }).first()).toBeVisible();
  });
});
