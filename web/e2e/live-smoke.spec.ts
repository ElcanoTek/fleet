import { test, expect } from "./fixtures";

// One "live" test that hits real OpenRouter through the real chat-server.
// Guarded by CHAT_E2E_LIVE=1 (set via `npm run test:e2e:live`) so PR/watch
// runs don't burn credits.

const live = process.env.CHAT_E2E_LIVE === "1";

test.describe("live OpenRouter smoke", () => {
  test.skip(!live, "set CHAT_E2E_LIVE=1 to run the live smoke");

  test("real turn streams a non-mock response", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("Reply with the word: sunrise. Do not call any tools.");
    await composer.press("Enter");

    // The live reply MUST not contain the mock sentinel.
    const body = page.locator("body");
    await expect(body).toContainText(/sunrise/i, { timeout: 60_000 });
    await expect(body).not.toContainText(/Mock reply to:/);
  });
});
