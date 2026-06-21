import { test, expect } from "./fixtures";

// These tests exercise the full chat flow end-to-end against the MOCKED
// chat-server (CHAT_MOCK_MODE=1). The canned reply is "Mock reply to: <prompt>".

test.describe("chat flow", () => {
  test("sends a message, streams mock response, renders tool chip + python block", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("hello from playwright");
    await composer.press("Enter");

    // Assistant reply should include the canned mock text.
    await expect(page.getByText(/Mock reply to:\s*hello from playwright/i)).toBeVisible({
      timeout: 15_000,
    });

    // Details (thinking, stats, tool calls) hidden by default; flip the
    // global header toggle to reveal them.
    await page.getByRole("button", { name: /show details/i }).click();

    // The mock turn also emits a run_python tool chip.
    await expect(page.getByRole("button", { name: /run_python/i })).toBeVisible();

    // Expanding the chip reveals the kernel stdout.
    await page.getByRole("button", { name: /run_python/i }).click();
    await expect(page.getByText(/mock result/i).first()).toBeVisible();
  });

  test("conversation appears in the sidebar after sending a message", async ({ page, login }) => {
    await login();
    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("sidebar smoke test");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:/i).first()).toBeVisible({ timeout: 15_000 });

    // The sidebar title is the first user message (truncated if long).
    const sidebarLink = page
      .getByRole("button", { name: /sidebar smoke test/i })
      .first();
    await expect(sidebarLink).toBeVisible({ timeout: 10_000 });
  });

  test("persona selector shows available personas (victoria + generic)", async ({ page, login }) => {
    await login();
    // The picker is now a popover button (aria-haspopup="listbox"), not a
    // native <select>. Click to open the listbox, then read its options.
    const personaButton = page.locator('button[aria-haspopup="listbox"]');
    await expect(personaButton).toBeVisible();
    await personaButton.click();

    const listbox = page.getByRole("listbox", { name: /persona/i });
    await expect(listbox).toBeVisible();

    const optionTexts = (await listbox.getByRole("option").allTextContents()).map((t) =>
      t.trim().toLowerCase(),
    );
    expect(optionTexts).toContain("victoria");
    expect(optionTexts).toContain("generic");
  });

  test("persona selector hides after the first turn", async ({ page, login }) => {
    await login();
    // Persona is locked server-side after the first turn. The UI used to
    // render a disabled <select>; PR #54 hides the picker entirely once
    // the conversation has any turns. Verify visible-before, gone-after.
    const personaButton = page.locator('button[aria-haspopup="listbox"]');
    await expect(personaButton).toBeVisible();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("locking persona");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:/i)).toBeVisible({ timeout: 15_000 });

    await expect(personaButton).toBeHidden();
  });
});
