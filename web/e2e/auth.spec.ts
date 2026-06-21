import { test, expect } from "./fixtures";
import { TEST_EMAIL, TEST_PASSWORD } from "../playwright.config";

test.describe("authentication", () => {
  test("redirects anonymous users from / to /login", async ({ page }) => {
    await page.goto("/");
    await expect(page).toHaveURL(/\/login$/);
    await expect(page.getByRole("heading", { name: /welcome aboard/i })).toBeVisible();
  });

  test("login with valid credentials lands on the chat page", async ({ page, login }) => {
    await login();
    await expect(page).toHaveURL("/");
    // Sidebar "Elcano" brand plus the composer placeholder should be present.
    await expect(page.getByPlaceholder(/message elcano ai/i)).toBeVisible();
  });

  test("login with wrong password stays on /login", async ({ page }) => {
    await page.goto("/login");
    await page.getByLabel(/email/i).fill(TEST_EMAIL);
    await page.getByLabel(/password/i).fill("not-the-password");
    await page.getByRole("button", { name: /sign in/i }).click();
    // The login handler redirects back to /login with an error code.
    await page.waitForURL(/\/login/, { timeout: 5_000 });
  });

  test("login with disallowed email stays on /login", async ({ page }) => {
    await page.goto("/login");
    await page.getByLabel(/email/i).fill("outsider@example.com");
    await page.getByLabel(/password/i).fill(TEST_PASSWORD);
    await page.getByRole("button", { name: /sign in/i }).click();
    await page.waitForURL(/\/login/, { timeout: 5_000 });
  });

  test("sign out clears the session", async ({ page, login }) => {
    await login();
    // Sidebar sign-out button posts to /api/auth/logout.
    await page.getByRole("button", { name: /sign out/i }).click();
    await page.waitForURL(/\/login/, { timeout: 5_000 });
    // And a subsequent root visit still lands at /login.
    await page.goto("/");
    await expect(page).toHaveURL(/\/login$/);
  });
});
