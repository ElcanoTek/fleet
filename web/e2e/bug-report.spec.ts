import { test, expect } from "./fixtures";

// Bug-report → Jules flow, exercised through the real UI.
//
// playwright.config.ts seeds a fake JULES_API_KEY (so the affordance
// renders) and points JULES_API_BASE at an unbound localhost port (so the
// outbound POST deterministically fails). That covers the full UI: button
// visibility, modal open/close, textarea editing, loading state, and error
// surface — without spending a real Jules session per test run.

test.describe("bug report", () => {
  test("button is hidden until there's an active conversation", async ({ page, login }) => {
    await login();

    // Empty state: no active conversation, no bug-report button.
    await expect(
      page.getByRole("button", { name: /report bug with this chat/i }),
    ).toHaveCount(0);

    // Send one message to create a conversation.
    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("hello mocky");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*hello mocky/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Now the button is in the sidebar footer.
    await expect(
      page.getByRole("button", { name: /report bug with this chat/i }),
    ).toBeVisible();
  });

  test("button is hidden entirely when the server has no JULES_API_KEY", async ({ page, login }) => {
    // Stub the capability probe before any page-level fetch fires so the
    // first paint already reflects julesEnabled=false. This simulates a
    // production deploy where the operator never configured Jules.
    await page.route("**/api/server-config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          lockdown_available: false,
          lockdown_only: false,
          lockdown_allowed_models: [],
          julesEnabled: false,
        }),
      });
    });

    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("hidden button");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*hidden button/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Even with an active conversation, the bug-report button stays hidden.
    await expect(
      page.getByRole("button", { name: /report bug with this chat/i }),
    ).toHaveCount(0);
  });

  test("opens the modal and lets the user cancel without sending", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("noisy chat");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*noisy chat/i).first()).toBeVisible({
      timeout: 15_000,
    });

    await page.getByRole("button", { name: /report bug with this chat/i }).click();
    const heading = page.getByRole("heading", { name: /report bug with this chat/i });
    await expect(heading).toBeVisible();
    const modal = heading.locator("..");
    await expect(modal.getByText(/Send the transcript of/i)).toBeVisible();
    // Privacy theatre is gone — no Google/Jules disclosure in the modal.
    await expect(modal).not.toContainText(/jules/i);
    await expect(modal).not.toContainText(/privacy policy/i);

    await page.getByRole("button", { name: /^Cancel$/ }).click();
    await expect(page.getByRole("heading", { name: /report bug with this chat/i })).toHaveCount(0);
  });

  test("surfaces a network error from the modal when the upstream is unreachable", async ({
    page,
    login,
  }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("frustrating");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*frustrating/i).first()).toBeVisible({
      timeout: 15_000,
    });

    await page.getByRole("button", { name: /report bug with this chat/i }).click();
    const modal = page.getByRole("heading", { name: /report bug with this chat/i }).locator("..");
    await expect(modal).toBeVisible();

    await modal.getByRole("textbox").fill("the model kept ignoring my CSV");
    await modal.getByRole("button", { name: /^Send$/ }).click();

    // JULES_API_BASE points at an unbound localhost port → the route
    // returns 502 with the neutral "couldn't reach the bug-report service"
    // message. User-facing copy must not leak the Jules name.
    await expect(modal.getByText(/couldn't reach the bug-report service/i)).toBeVisible({
      timeout: 10_000,
    });
    await expect(modal).not.toContainText(/jules/i);
    // After the failure the button should be re-enabled so the user can
    // retry once an admin fixes the config.
    await expect(modal.getByRole("button", { name: /^Send$/ })).toBeEnabled();
  });

  test("rejects requests without a session cookie at the API level", async ({ page }) => {
    // Visit the login page so page.request inherits a valid same-origin
    // context, then explicitly set Origin so we don't trip the CSRF guard
    // — we want to confirm the auth check fires next.
    await page.goto("/login");
    const origin = new URL(page.url()).origin;
    const response = await page.request.post("/api/bug-report", {
      headers: { Origin: origin, "Content-Type": "application/json" },
      data: { conversationId: "anything" },
    });
    expect(response.status()).toBe(401);
  });
});
