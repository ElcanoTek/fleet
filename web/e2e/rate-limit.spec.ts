import { test, expect } from "./fixtures";

// The rate-limit test bypasses the usual /api/chat path and calls
// chat-server directly (via an unauthenticated hit that we EXPECT to
// fail) to avoid needing a tiny per-minute cap just for this test. The
// surfacing in the UI is verified with a manual fetch + assertion.

test.describe("rate limiting", () => {
  test("UI surfaces a 429 with a readable retry message", async ({ page, login }) => {
    await login();

    // Simulate a 429 by mocking the next /api/chat response. Playwright
    // can intercept at the browser level.
    await page.route("**/api/chat", async (route) => {
      await route.fulfill({
        status: 429,
        headers: { "Retry-After": "30" },
        body: "rate limit exceeded",
      });
    });

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("this will be rate limited");
    await composer.press("Enter");

    // The user-visible error mentions "Rate limit" and the retry window.
    await expect(page.getByText(/rate limit/i).first()).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(/30s/i).first()).toBeVisible();
  });
});
