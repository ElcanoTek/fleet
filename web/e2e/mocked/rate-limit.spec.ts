import { test, expect } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// The chat shell's reaction to a provider 429 is a pure front-end affordance, so
// it belongs in the mocked lane: route-intercept /api/chat with a 429 +
// Retry-After and assert the UI surfaces a readable rate-limit message with the
// retry window (chat-experience.tsx renders "Rate limit reached. Try again in
// <n>s." from the Retry-After header).

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the chat UI surfaces a 429 with a readable retry window", async ({ page }) => {
  await mockChatBoot(page);
  await page.route("**/api/chat", (route) =>
    route.fulfill({ status: 429, headers: { "Retry-After": "30" }, body: "rate limit exceeded" }),
  );

  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const composer = page.getByRole("textbox").first();
  await composer.fill("this will be rate limited");
  await composer.press("Enter");

  await expect(page.getByText(/rate limit/i).first()).toBeVisible({ timeout: 10_000 });
  await expect(page.getByText(/30s/i).first()).toBeVisible();
});
