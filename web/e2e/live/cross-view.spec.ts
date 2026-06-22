import { test, expect } from "./fixtures";

// LIVE cross-view session: one real login (the elcano_session cookie) gates
// BOTH the /chat and /orchestrator views off the single Next middleware. This
// navigates between them via the in-app links and asserts it never bounces back
// to /login — proving the shared session against the real backend (the chat
// shell loads its real boot data; the orchestrator probes its real /me).

test.describe("live cross-view session", () => {
  test("one session navigates /chat ↔ /orchestrator without re-login", async ({ page, login }) => {
    await login();

    // We're on /chat (the empty composer). Cross over to the orchestrator.
    await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible();
    await page.getByTestId("nav-to-orchestrator").click();

    await page.waitForURL(/\/orchestrator/, { timeout: 15_000 });
    // The orchestrator either shows its dashboard (if the cookie satisfies the
    // /me probe) or its own login card — but either way it must NOT have bounced
    // to the app /login page: the shared middleware accepted the session.
    expect(new URL(page.url()).pathname.startsWith("/login")).toBe(false);
    await expect(page.getByTestId("nav-to-chat")).toBeVisible({ timeout: 15_000 });

    // Cross back to chat — still no re-login.
    await page.getByTestId("nav-to-chat").click();
    await page.waitForURL((u) => !u.pathname.startsWith("/orchestrator"), { timeout: 15_000 });
    expect(new URL(page.url()).pathname.startsWith("/login")).toBe(false);
    await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible({
      timeout: 15_000,
    });
  });
});
