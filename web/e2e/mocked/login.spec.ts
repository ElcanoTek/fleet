import { test, expect } from "@playwright/test";
import type { Route } from "@playwright/test";
import { mintSessionToken, loginViaElcanoCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for the two login paths the unified middleware accepts, plus the
// unauthenticated redirect. The password form's upstream verify call hits the
// Go chat-server, and the "Use Elcano email" button bounces to the external
// auth service — neither exists in CI, so we intercept those browser-originated
// requests and drive the REAL middleware gate with the cookies it accepts.

test("unauthenticated /chat redirects to /login", async ({ page }) => {
  await page.goto("/chat");
  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByRole("heading", { name: /welcome aboard/i })).toBeVisible();
});

test("unauthenticated /orchestrator is gated by the shared middleware", async ({ page }) => {
  // The ONE middleware gates /orchestrator with the SAME session check as /chat
  // (see middleware.test.ts: "gates /orchestrator/* with the SAME session
  // check"). With no session cookie the production server redirects to /login;
  // either way the dashboard must never render for an unauthenticated visitor.
  await page.route("**/api/orchestrator/me", (r: Route) =>
    r.fulfill({ status: 401, json: { authenticated: false } }),
  );
  await page.goto("/orchestrator");
  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByTestId("orchestrator-dashboard")).toHaveCount(0);
});

test("the password form renders both sign-in options", async ({ page }) => {
  await page.goto("/login");
  await expect(page.getByLabel(/email/i)).toBeVisible();
  await expect(page.getByLabel(/password/i)).toBeVisible();
  await expect(page.getByRole("button", { name: /sign in/i })).toBeVisible();
  // AUTH_SIGNING_PUBKEY is set on the server, so the secondary Elcano path shows.
  await expect(page.getByRole("link", { name: /use elcano email/i })).toBeVisible();
});

test("password login: a verified credential lands on /chat", async ({ page }) => {
  // The form POSTs to /api/auth/login, which (in prod) verifies against
  // chat-server then sets the HMAC session cookie. Intercept that POST and
  // fulfill it the way a successful verify would: set elcano_session + redirect
  // home. The middleware then admits the session — the gate under test.
  await page.route("**/api/auth/login", (r: Route) =>
    r.fulfill({
      status: 303,
      headers: {
        location: "/chat",
        "set-cookie": `elcano_session=${mintSessionToken("e2e@example.com")}; Path=/; HttpOnly; SameSite=Lax`,
      },
    }),
  );
  await mockChatBoot(page);

  await page.goto("/login");
  await page.getByLabel(/email/i).fill("e2e@example.com");
  await page.getByLabel(/password/i).fill("e2e-test-password");
  await page.getByRole("button", { name: /sign in/i }).click();

  await page.waitForURL(/\/chat$/, { timeout: 15_000 });
  await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible({
    timeout: 15_000,
  });
});

test("password login: an invalid credential stays on /login with an error", async ({ page }) => {
  // A failed verify redirects back to /login?e=invalid; the card maps that code
  // to a user-facing message.
  await page.route("**/api/auth/login", (r: Route) =>
    r.fulfill({ status: 303, headers: { location: "/login?e=invalid" } }),
  );

  await page.goto("/login");
  await page.getByLabel(/email/i).fill("e2e@example.com");
  await page.getByLabel(/password/i).fill("wrong-password");
  await page.getByRole("button", { name: /sign in/i }).click();

  await page.waitForURL(/\/login/, { timeout: 10_000 });
  await expect(page.getByText(/invalid email or password/i)).toBeVisible();
});

test('"Use Elcano email" bounces to the auth service magic-link flow', async ({ page }) => {
  // The button is an <a href="/api/auth/elcano-login">. That route 303-redirects
  // to auth.elcanotek.com (unreachable in CI), so intercept the navigation and
  // assert it targets the auth service with a signed return_to.
  let bounceUrl = "";
  await page.route("**/api/auth/elcano-login", (r: Route) => {
    bounceUrl = "https://auth.elcanotek.com/?return_to=" + encodeURIComponent("http://127.0.0.1/");
    return r.fulfill({ status: 303, headers: { location: bounceUrl } });
  });
  // Don't actually navigate to the external host.
  await page.route("https://auth.elcanotek.com/**", (r: Route) =>
    r.fulfill({ status: 200, contentType: "text/html", body: "<html><body>auth service</body></html>" }),
  );

  await page.goto("/login");
  const elcanoRequest = page.waitForRequest("**/api/auth/elcano-login");
  await page.getByRole("link", { name: /use elcano email/i }).click();
  await elcanoRequest;
  expect(bounceUrl).toContain("auth.elcanotek.com");
});

test("a signed-in non-member sees the login form WITH a no-access notice, not a redirect or a dead-end (#458)", async ({
  page,
  context,
}) => {
  // A valid chat session cookie whose identity isn't provisioned in the
  // orchestrator: /me returns 403 not_a_member. The middleware still admits the
  // navigation (valid cookie), and the orchestrator must NOT strand the visitor
  // — it shows the login card (so the username/password moc path can still admit
  // a provisioned operator) plus a notice explaining why the prompt appeared.
  await loginViaElcanoCookie(context);
  await page.route("**/api/orchestrator/me", (r: Route) =>
    r.fulfill({ status: 403, json: { error: "not_a_member" } }),
  );

  await page.goto("/orchestrator");

  // Not bounced to the app login, and the dashboard is NOT shown.
  expect(new URL(page.url()).pathname.startsWith("/login")).toBe(false);
  await expect(page.getByTestId("orchestrator-dashboard")).toHaveCount(0);
  // The explanatory notice is shown AND the username/password form remains.
  await expect(page.getByTestId("orchestrator-no-access")).toBeVisible();
  await expect(page.getByLabel("Username", { exact: true })).toBeVisible();
});

test("the elcano_auth (magic-link) cookie authenticates the same as the password session", async ({
  page,
  context,
}) => {
  // The other half of the dual-login middleware: a valid Ed25519 elcano_auth
  // cookie (what the auth service mints after the magic-link round-trip) admits
  // the user without the password form.
  await loginViaElcanoCookie(context);
  await mockChatBoot(page);

  await page.goto("/chat");
  await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible({
    timeout: 15_000,
  });
  expect(new URL(page.url()).pathname).toBe("/chat");
});
