import { test, expect } from "./fixtures";

// Exercises the human-in-the-loop email approval flow end-to-end. Mock
// mode intercepts the "send email" prompt, stages an approval, and emits
// tool.approval_required. The UI renders a Send/Cancel card. Both paths
// (approve + reject) are covered.

test.describe("email approval flow", () => {
  test("Send approves and renders the staged result", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("please send email to demo");
    await composer.press("Enter");

    // Approval card surfaces. The card contains a unique "Send this email?"
    // heading — scope the Send button lookup to that card so we don't pick
    // up the composer's Send button by accident.
    const card = page
      .locator("div", { has: page.getByText(/Send this email\?/i) })
      .filter({ has: page.getByRole("button", { name: /^Send$/ }) })
      .first();
    await expect(card).toBeVisible({ timeout: 15_000 });
    const sendBtn = card.getByRole("button", { name: /^Send$/ });

    // Clicking Send resolves the approval. The card should flip to "Email sent".
    await sendBtn.click();
    await expect(page.getByText(/Email sent/i)).toBeVisible({ timeout: 10_000 });
  });

  test("Cancel rejects the staged send", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("send email to nobody");
    await composer.press("Enter");

    // Card renders. The Cancel button in the approval card is adjacent to Send.
    const card = page.locator("text=/Send this email\\?/i").first();
    await expect(card).toBeVisible({ timeout: 15_000 });

    await page
      .getByRole("button", { name: /^Cancel$/ })
      .first()
      .click();

    await expect(page.getByText(/Send cancelled/i)).toBeVisible({ timeout: 10_000 });
  });

  test("pending approval survives a page reload", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("send email somewhere");
    await composer.press("Enter");

    await expect(page.getByText(/Send this email\?/i)).toBeVisible({ timeout: 15_000 });

    // Don't click anything — reload. The card should come back.
    await page.reload();
    await expect(page.getByText(/Send this email\?/i)).toBeVisible({ timeout: 10_000 });
  });
});
