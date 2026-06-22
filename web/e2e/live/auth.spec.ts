import { test, expect, creds } from "./fixtures";

// LIVE auth journey against the real backend: the real password login mints a
// real session cookie (validated against the real chat users table the boot
// script seeded), and an unauthenticated page request is gated by the real Next
// middleware. No mocks.

test.describe("live auth", () => {
  test("unauthenticated /chat redirects to /login", async ({ page }) => {
    await page.context().clearCookies();
    await page.goto("/chat");
    await page.waitForURL(/\/login/, { timeout: 15_000 });
    await expect(page.getByLabel(/email/i)).toBeVisible();
  });

  test("a real password login lands on the chat composer", async ({ page }) => {
    await page.goto("/login");
    await page.getByLabel(/email/i).fill(creds.email);
    await page.getByLabel(/password/i).fill(creds.password);
    await page.getByRole("button", { name: /sign in|log in|continue/i }).click();

    await page.waitForURL((u) => !u.pathname.startsWith("/login"), { timeout: 20_000 });
    await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible({
      timeout: 15_000,
    });
  });

  test("an invalid password stays on /login", async ({ page }) => {
    await page.goto("/login");
    await page.getByLabel(/email/i).fill(creds.email);
    await page.getByLabel(/password/i).fill("definitely-the-wrong-password");
    await page.getByRole("button", { name: /sign in|log in|continue/i }).click();

    // The app must not authenticate: still on /login after a beat.
    await page.waitForTimeout(1_500);
    expect(new URL(page.url()).pathname).toMatch(/\/login/);
  });
});
