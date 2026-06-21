import { test, expect } from "./fixtures";

// Empty-state protocol pills. Against the MOCKED chat-server the canned reply
// is "Mock reply to: <prompt>", so we can assert that a pill submitted the
// expected templated prompt by matching the echoed text. These tests cover
// the form↔conversation hybrid: a form pill (Weekly Report) and the
// conversation-first pill with its optional form (Performance Diagnostic).

test.describe("protocol pills", () => {
  test("empty state surfaces the four workflow pills", async ({ page, login }) => {
    await login();
    await expect(page.getByRole("button", { name: /weekly performance report/i })).toBeVisible();
    await expect(page.getByRole("button", { name: /performance diagnostic/i })).toBeVisible();
    await expect(page.getByRole("button", { name: /end-of-campaign wrap/i })).toBeVisible();
    await expect(page.getByRole("button", { name: /optimization report/i })).toBeVisible();
  });

  test("weekly report form templates the DSP trigger and sends it", async ({ page, login }) => {
    await login();
    await page.getByRole("button", { name: /weekly performance report/i }).click();

    // The inline form replaces the card grid.
    await page.getByLabel(/client name/i).fill("TestCo");
    await page.getByLabel(/campaign code/i).fill("ELC-9999");

    // Prompt preview reflects the templated trigger before sending.
    await expect(
      page.getByText(/Run the DSP reporting protocol for TestCo \(ELC-9999\)\./i),
    ).toBeVisible();

    await page.getByRole("button", { name: /run report/i }).click();

    // Mock server echoes the submitted prompt back.
    await expect(
      page.getByText(/Mock reply to:\s*Run the DSP reporting protocol for TestCo \(ELC-9999\)/i),
    ).toBeVisible({ timeout: 15_000 });
  });

  test("performance diagnostic starts the conversational intake", async ({ page, login }) => {
    await login();
    await page.getByRole("button", { name: /performance diagnostic/i }).click();

    // The form leads now; the "Skip the form, start in Chat" link is the chat path.
    await page.getByRole("button", { name: /skip the form, start in chat/i }).click();

    await expect(
      page.getByText(/run a performance diagnostic on a campaign/i).first(),
    ).toBeVisible({ timeout: 15_000 });
  });

  test("optimization report folds the optional campaign code into the prompt", async ({ page, login }) => {
    await login();
    await page.getByRole("button", { name: /optimization report/i }).click();

    await page.getByLabel(/mailbox alias/i).fill("twc.acme.elc00001@victoria.elcanotek.com");
    await page.getByLabel(/recipient/i).fill("brad@elcanotek.com");
    await page.getByLabel(/campaign code/i).fill("AUTO-2024");

    // Prompt preview reflects the optional campaign code before sending.
    await expect(page.getByText(/Campaign code: AUTO-2024\./i)).toBeVisible();

    await page.getByRole("button", { name: /generate report/i }).click();

    await expect(
      page.getByText(/Mock reply to:\s*Run the optimization protocol.*Campaign code: AUTO-2024\./i),
    ).toBeVisible({ timeout: 15_000 });
  });

  test("performance diagnostic form folds details into the prompt", async ({ page, login }) => {
    await login();
    await page.getByRole("button", { name: /performance diagnostic/i }).click();

    // The form is primary now — point Victoria at a specific campaign directly.
    await page.getByLabel(/client name/i).fill("Acme");
    await page.getByLabel(/campaign code/i).fill("ELC-1");

    await page.getByRole("button", { name: /run diagnostic/i }).click();

    await expect(
      page.getByText(/Mock reply to:\s*Run a performance diagnostic\..*Campaign: Acme \(ELC-1\)/i),
    ).toBeVisible({ timeout: 15_000 });
  });
});
